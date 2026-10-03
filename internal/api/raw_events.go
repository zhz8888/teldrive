package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tgdrive/teldrive/v2/internal/api/gen"
	userevents "github.com/tgdrive/teldrive/v2/internal/events"
)

// streamEventEnvelope is the JSON document carried in the data field of every
// server-sent event. Its field tags are part of the stream contract published to
// clients, so they must stay in sync with the generated types rather than being
// renamed freely.
type streamEventEnvelope struct {
	// Version is the envelope schema version; it is currently always 1 and lets
	// clients reject payloads they do not understand.
	Version int `json:"version"`
	// OccurredAt is when the domain event happened, in UTC.
	OccurredAt time.Time `json:"occurredAt"`
	// ResourceType names the kind of resource the event is about, for example
	// "file" or "upload".
	ResourceType string `json:"resourceType"`
	// ResourceID identifies that resource and is empty for events that are not
	// about a single resource.
	ResourceID string `json:"resourceId,omitempty"`
	// Generation carries the resource generation after the change when the event
	// tracks one; nil means the event has no generation.
	Generation *int64 `json:"generation,omitempty"`
	// Payload is the event-specific JSON body. It is never null: a missing stored
	// payload is replaced by an empty object.
	Payload json.RawMessage `json:"payload"`
}

// StreamEvents serves the authenticated server-sent event feed, replaying events
// after the Last-Event-ID/after cursor and then pushing new batches until the
// client disconnects. A cursor that has fallen out of the retention window
// produces a sync.required control event instead of a replay.
func (h *RawHandler) StreamEvents(ctx context.Context, params gen.StreamEventsParams, w http.ResponseWriter) error {
	userID, err := UserIDFromContext(ctx)
	if err != nil {
		return mapServiceError(err)
	}
	if h == nil || h.handler == nil || h.handler.Events == nil {
		return mapServiceError(ErrOperationUnavailable)
	}
	cursor, cursorSet, err := eventCursor(params)
	if err != nil {
		return problem(http.StatusUnprocessableEntity, "invalid_event_cursor", "event cursor is invalid", err)
	}
	eventTypes, err := normalizeEventTypes(params.Types)
	if err != nil {
		return problem(http.StatusUnprocessableEntity, "invalid_event_types", "event types are invalid", err)
	}

	if !cursorSet {
		// The live watermark is taken before subscribing on purpose. Subscribing
		// first would fold an event that arrives while the wake channel is being
		// set up into the watermark, and the read below only returns events
		// after it, so that event would be skipped as if it had been delivered.
		// This order can deliver an event twice, which a client deduplicates by
		// cursor, and never drops one.
		cursor, err = h.handler.Events.CurrentCursor(ctx, userID)
		if err != nil {
			return mapServiceError(err)
		}
	}
	wake, unsubscribe, err := h.handler.Events.Subscribe(userID)
	if err != nil {
		return mapServiceError(err)
	}
	defer unsubscribe()

	expired, err := h.handler.Events.CursorExpired(ctx, userID, cursor)
	if err != nil {
		return mapServiceError(err)
	}
	initial, err := h.handler.Events.ListAfter(ctx, userID, cursor, eventTypes)
	if err != nil {
		return mapServiceError(err)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	if err := writeAndFlushStream(w, h.handler.Events.WriteTimeout(), func() error {
		w.WriteHeader(http.StatusOK)
		_, err := fmt.Fprint(w, "retry: 3000\n\n")
		return err
	}); err != nil {
		return nil
	}

	if expired {
		_ = writeAndFlushStream(w, h.handler.Events.WriteTimeout(), func() error {
			return writeStreamControl(w, "sync.required", map[string]string{"reason": "cursor_expired"})
		})
		return nil
	}

	heartbeat := time.NewTicker(h.handler.Events.Heartbeat())
	defer heartbeat.Stop()
	pending := initial
	for {
		for len(pending) > 0 {
			if err := writeAndFlushStream(w, h.handler.Events.WriteTimeout(), func() error {
				for _, event := range pending {
					if err := writeStreamEvent(w, event); err != nil {
						return err
					}
					cursor = event.ID
				}
				return nil
			}); err != nil {
				return nil
			}
			if len(pending) < int(h.handler.Events.BatchSize()) {
				pending = nil
				break
			}
			pending, err = h.handler.Events.ListAfter(ctx, userID, cursor, eventTypes)
			if err != nil {
				_ = writeAndFlushStream(w, h.handler.Events.WriteTimeout(), func() error {
					return writeStreamControl(w, "stream.error", map[string]string{"code": "service_unavailable"})
				})
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-h.handler.Events.Done():
			return nil
		case _, ok := <-wake:
			if !ok {
				return nil
			}
			pending, err = h.handler.Events.ListAfter(ctx, userID, cursor, eventTypes)
			if err != nil {
				_ = writeAndFlushStream(w, h.handler.Events.WriteTimeout(), func() error {
					return writeStreamControl(w, "stream.error", map[string]string{"code": "service_unavailable"})
				})
				return nil
			}
		case <-heartbeat.C:
			pending, err = h.handler.Events.ListAfter(ctx, userID, cursor, eventTypes)
			if err != nil {
				_ = writeAndFlushStream(w, h.handler.Events.WriteTimeout(), func() error {
					return writeStreamControl(w, "stream.error", map[string]string{"code": "service_unavailable"})
				})
				return nil
			}
			if len(pending) == 0 {
				if err := writeAndFlushStream(w, h.handler.Events.WriteTimeout(), func() error {
					_, err := fmt.Fprint(w, ": keep-alive\n\n")
					return err
				}); err != nil {
					return nil
				}
			}
		}
	}
}

// eventCursor resolves the replay position requested by the client from the
// Last-Event-ID header and the after query parameter. The boolean reports whether
// the client asked for a position at all, which lets the stream start from the
// current cursor instead of replaying history; it fails when a value is
// malformed, negative, or when the two sources disagree.
func eventCursor(params gen.StreamEventsParams) (cursor int64, set bool, err error) {
	if header, ok := params.LastEventID.Get(); ok {
		value := strings.TrimSpace(header)
		if value == "" {
			return 0, false, errors.New("empty Last-Event-ID")
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 0 {
			return 0, false, errors.New("invalid Last-Event-ID")
		}
		cursor, set = parsed, true
	}
	if after, ok := params.After.Get(); ok {
		if after < 0 {
			return 0, false, errors.New("negative event cursor")
		}
		if set && cursor != after {
			return 0, false, errors.New("conflicting event cursors")
		}
		cursor, set = after, true
	}
	return cursor, set, nil
}

// normalizeEventTypes trims and de-duplicates the client's event type filter
// while preserving the order it was given in. It rejects empty entries and more
// than 50 filters, which bounds the SQL array parameter of every later query.
func normalizeEventTypes(values []string) ([]string, error) {
	if len(values) > 50 {
		return nil, errors.New("too many event types")
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, errors.New("event type is empty")
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}

// writeStreamEvent writes one server-sent event frame with the event ID, type and
// JSON envelope to w. It refuses events whose ID is not positive or whose type is
// not a valid event name, and substitutes an empty object for a missing payload.
func writeStreamEvent(w http.ResponseWriter, event userevents.Event) error {
	if event.ID <= 0 || !validEventName(event.Type) {
		return errors.New("invalid event")
	}
	payload := json.RawMessage(event.Payload)
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	envelope, err := json.Marshal(streamEventEnvelope{
		Version:      1,
		OccurredAt:   event.OccurredAt,
		ResourceType: event.ResourceType,
		ResourceID:   event.ResourceID,
		Generation:   event.Generation,
		Payload:      payload,
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.ID, event.Type, envelope)
	return err
}

// writeStreamControl writes a control frame such as sync.required or stream.error.
// Control frames carry no event ID, so they never advance the client's replay
// cursor; the name must be a valid event name.
func writeStreamControl(w http.ResponseWriter, name string, payload any) error {
	if !validEventName(name) {
		return errors.New("invalid control event name")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, encoded)
	return err
}

// validEventName reports whether value may be emitted as the event name of a
// server-sent event. It must be non-empty and contain only letters, digits, dots,
// underscores and hyphens, so a value can never inject additional frames through
// whitespace or a newline.
func validEventName(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

// writeAndFlushStream runs write and flushes the response, applying a write
// deadline of timeout beforehand so a stuck client cannot hold the streaming
// goroutine forever. A timeout of zero leaves the deadline untouched, and a
// ResponseWriter that does not support deadlines (http.ErrNotSupported) is
// tolerated; any other error from write or Flush is returned.
func writeAndFlushStream(w http.ResponseWriter, timeout time.Duration, write func() error) error {
	controller := http.NewResponseController(w)
	deadlineSet := false
	if timeout > 0 {
		if err := controller.SetWriteDeadline(time.Now().Add(timeout)); err == nil {
			deadlineSet = true
		} else if !errors.Is(err, http.ErrNotSupported) {
			return err
		}
	}
	if deadlineSet {
		defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	}
	if err := write(); err != nil {
		return err
	}
	return controller.Flush()
}
