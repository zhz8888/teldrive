// Package events implements the TelDrive server-sent event feed. A PostgreSQL
// trigger publishes a wake-up through LISTEN/NOTIFY whenever an event row is
// inserted, and the per-process Hub turns that into a signal for the local
// subscribers of the affected user. Wake-ups deliberately carry no payload:
// subscribers always re-read the durable user_events rows from their own cursor
// through Service.ListAfter, so a dropped or coalesced notification can only
// delay a client until its next poll, never lose an event.
package events

import (
	"errors"
	"sync"
)

var (
	// ErrTooManyConnections is returned when a user already holds
	// MaxConnectionsPerUser open streams. Callers must test it with errors.Is;
	// the HTTP layer maps it to 429 Too Many Requests.
	ErrTooManyConnections = errors.New("too many event stream connections")
	// ErrServiceClosed reports that the service or its hub has been closed.
	// Subscribe, Start and IssueTicket return it instead of restarting work, and
	// the HTTP layer maps it to 503 Service Unavailable.
	ErrServiceClosed = errors.New("event service is closed")
)

// Hub coalesces PostgreSQL wake-ups for local SSE subscribers. It never carries
// event payloads; subscribers always read durable rows from PostgreSQL.
type Hub struct {
	// mu guards nextID, subscribers and closed. It is also held during the
	// non-blocking sends in Notify, so locking it never blocks on a subscriber.
	mu sync.Mutex
	// nextID is the id handed to the next subscription; ids are never reused.
	nextID uint64
	// subscribers maps a user id to that user's live wake channels, keyed by
	// subscription id.
	subscribers map[int64]map[uint64]chan struct{}
	// maxPerUser caps the subscriptions one user may hold; zero or less means no
	// cap at all.
	maxPerUser int
	// closed records that Close ran: new subscriptions are refused and every
	// wake channel has already been closed.
	closed bool
}

// NewHub returns an empty hub that admits at most maxPerUser concurrent
// subscribers per user; zero or less removes the per-user cap. The hub starts
// open and stays usable until Close is called.
func NewHub(maxPerUser int) *Hub {
	return &Hub{subscribers: make(map[int64]map[uint64]chan struct{}), maxPerUser: maxPerUser}
}

// Subscribe registers a wake-up channel for userID and returns it together with
// the function that removes the registration. The channel is buffered with one
// slot, so repeated notifications coalesce instead of queueing and a subscriber
// that stops reading cannot block Notify; it is closed by Close and must not be
// closed by the caller. The unsubscribe function is idempotent, remains safe to
// call after Close, and should be deferred. Subscribe fails with
// ErrServiceClosed once the hub is closed, with ErrTooManyConnections at the
// per-user cap, and with a plain error for a nil hub or a non-positive user ID.
func (h *Hub) Subscribe(userID int64) (<-chan struct{}, func(), error) {
	if h == nil || userID <= 0 {
		return nil, nil, errors.New("invalid event subscriber")
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, nil, ErrServiceClosed
	}
	if h.maxPerUser > 0 && len(h.subscribers[userID]) >= h.maxPerUser {
		h.mu.Unlock()
		return nil, nil, ErrTooManyConnections
	}
	h.nextID++
	id := h.nextID
	wake := make(chan struct{}, 1)
	if h.subscribers[userID] == nil {
		h.subscribers[userID] = make(map[uint64]chan struct{})
	}
	h.subscribers[userID][id] = wake
	h.mu.Unlock()

	var once sync.Once
	return wake, func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			userSubscribers := h.subscribers[userID]
			if userSubscribers == nil {
				return
			}
			delete(userSubscribers, id)
			if len(userSubscribers) == 0 {
				delete(h.subscribers, userID)
			}
		})
	}, nil
}

// Notify tells every local subscriber of userID that new events may be
// available. The signal carries no event data: subscribers re-read durable
// PostgreSQL rows from their own cursor. Delivery is best effort and
// non-blocking, so a subscriber that is already signaled, or that has stopped
// reading, is skipped rather than stalling the caller. A nil hub, a
// non-positive user ID and a closed hub are all no-ops.
func (h *Hub) Notify(userID int64) {
	if h == nil || userID <= 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	for _, wake := range h.subscribers[userID] {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

// Close drops every subscription, closes their wake channels and marks the hub
// closed. It is idempotent and safe on a nil hub, and is normally called by
// Service.Close during shutdown. Closing under the same mutex that Notify takes
// guarantees a wake-up is never sent on a closed channel; unblocked readers see
// the close and stop their streams. Afterwards Subscribe fails with
// ErrServiceClosed and previously returned unsubscribe functions are no-ops.
func (h *Hub) Close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for _, userSubscribers := range h.subscribers {
		for _, wake := range userSubscribers {
			close(wake)
		}
	}
	h.subscribers = nil
}
