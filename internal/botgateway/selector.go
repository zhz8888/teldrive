package botgateway

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/zhz8888/teldrive/v2/internal/db/sqlcgen"
	"github.com/zhz8888/teldrive/v2/internal/telegramstore"
)

const (
	// RotationMemory keeps the per-user rotation counters in process memory, so
	// they reset on restart and each TelDrive instance spreads load
	// independently. It is the default rotation backend.
	RotationMemory = "memory"
	// RotationDatabase keeps the per-user rotation counters in the database, so
	// every instance advances one shared sequence and round-robin order
	// survives restarts.
	RotationDatabase = "database"
)

// botSelector supplies the rotation counter that picks the bot for one
// operation. Implementations must be safe for concurrent use.
type botSelector interface {
	// Next returns the next counter value for the user and operation. Callers
	// reduce it modulo the number of candidate bots, so it only has to advance,
	// and two callers that get the same value may pick the same bot.
	Next(context.Context, int64, telegramstore.Operation) (uint64, error)
}

// memoryBotSelector keeps one atomic counter per user and operation for the
// lifetime of the process.
type memoryBotSelector struct {
	// counters holds one *atomic.Uint64 per memoryCounterKey. Entries are never
	// evicted, so the map grows with the number of distinct users seen.
	counters sync.Map
}

// memoryCounterKey identifies one in-memory rotation counter.
type memoryCounterKey struct {
	// userID is the TelDrive user the counter belongs to, so two users never
	// share a rotation sequence.
	userID int64
	// operation separates the counters of one user, because uploads and
	// downloads choose from different candidate lists.
	operation telegramstore.Operation
}

// Next returns a counter that starts at zero and increases by one per call for
// the user and operation. It returns an error for a non-positive user ID,
// because such a counter could not be attributed to a user.
func (s *memoryBotSelector) Next(_ context.Context, userID int64, operation telegramstore.Operation) (uint64, error) {
	if userID <= 0 {
		return 0, errors.New("invalid bot selection user")
	}
	value, _ := s.counters.LoadOrStore(memoryCounterKey{userID: userID, operation: operation}, new(atomic.Uint64))
	return value.(*atomic.Uint64).Add(1) - 1, nil
}

// databaseBotSelector keeps the rotation counters in the bot_selection_counters
// table, so instances sharing a database advance one sequence per user and
// operation.
type databaseBotSelector struct {
	// queries advances the persisted counter rows.
	queries *sqlcgen.Queries
}

// Next atomically increments the persisted counter of the user and operation
// and returns its previous value, so concurrent callers receive distinct
// values. The upsert is a single statement and the query error is returned
// unwrapped; the counter is unchanged when the statement fails.
func (s databaseBotSelector) Next(ctx context.Context, userID int64, operation telegramstore.Operation) (uint64, error) {
	value, err := s.queries.NextBotSelectionValue(ctx, sqlcgen.NextBotSelectionValueParams{
		UserID: userID, Operation: string(operation),
	})
	return uint64(value), err
}
