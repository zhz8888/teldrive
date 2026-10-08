// Package querytrace counts how often each named SQL query is executed, which
// lets tests assert that a code path issues a bounded number of statements and
// catches N+1 regressions.
package querytrace

import (
	"context"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
)

// Counter records per-query execution counts. It implements pgx.QueryTracer and
// is safe for concurrent use, so a single Counter may be attached to a pool
// serving parallel goroutines. The zero value is ready to use; counts are
// accumulated for the lifetime of the Counter and are never reset.
type Counter struct {
	// mu guards counts, because pgx may call the tracer from several goroutines
	// at once.
	mu sync.Mutex
	// counts maps a query name to the number of times it started. It is created
	// on the first counted query, which is what makes the zero Counter usable.
	counts map[string]int
}

// TraceQueryStart counts the query identified by the "-- name: ..." header of
// data.SQL and returns ctx unchanged. Unnamed statements, such as ad-hoc SQL and
// those issued by migrations, are not counted.
func (c *Counter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	name := queryName(data.SQL)
	if name == "" {
		return ctx
	}
	c.mu.Lock()
	if c.counts == nil {
		c.counts = make(map[string]int)
	}
	c.counts[name]++
	c.mu.Unlock()
	return ctx
}

// TraceQueryEnd does nothing; Counter needs only the start of a query.
func (*Counter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// Count returns how many times the query named name has started since the
// Counter was created, or zero if it has never run.
func (c *Counter) Count(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[name]
}

// queryName extracts the query name from a sqlc statement header of the form
// "-- name: ListFiles :many". It returns an empty string when sql does not start
// with that header, and it tolerates a missing parameter list by accepting
// whatever follows the name.
func queryName(sql string) string {
	const prefix = "-- name: "
	if !strings.HasPrefix(sql, prefix) {
		return ""
	}
	name := strings.TrimPrefix(sql, prefix)
	if index := strings.IndexByte(name, ' '); index >= 0 {
		name = name[:index]
	}
	if index := strings.IndexByte(name, '\n'); index >= 0 {
		name = name[:index]
	}
	return name
}
