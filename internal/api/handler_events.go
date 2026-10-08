package api

import (
	"context"

	"github.com/zhz8888/teldrive/v2/internal/api/gen"
)

// CreateEventStreamTicket issues a short-lived ticket that the browser can pass as
// a query parameter on the event stream, since EventSource cannot set headers.
// It requires an authenticated identity and h.Events, otherwise it reports 503.
func (h *Handler) CreateEventStreamTicket(ctx context.Context) (gen.CreateEventStreamTicketRes, error) {
	userID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if h == nil || h.Events == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	ticket, err := h.Events.IssueTicket(ctx, userID)
	if err != nil {
		return nil, mapServiceError(err)
	}
	return &gen.EventStreamTicket{Ticket: ticket.Value, ExpiresAt: ticket.ExpiresAt}, nil
}
