// Package cache provides the process-local cache shared by the domain services.
//
// Only an in-memory Ristretto backend is implemented: entries are lost on
// restart and are never coordinated between processes, so the cache may only be
// used for data that can be rebuilt from PostgreSQL. Values are msgpack-encoded,
// which means anything stored here must survive a round trip through msgpack
// with the same field names.
//
// Every entry is also budgeted by cost, and Ristretto may evict it early under
// memory pressure. A cache miss is therefore always a normal outcome, never an
// error condition.
package cache

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/dgraph-io/ristretto/v2"
	"github.com/vmihailenco/msgpack/v5"
)

// ErrNotFound is returned by Cacher.Get when the key is absent or its entry has
// already been evicted. Callers must test it with errors.Is rather than by
// comparing error values, because implementations are free to wrap it.
var ErrNotFound = errors.New("cache: not found")

// Cacher is the global in-memory cache contract restored from v1.8.3.
type Cacher interface {
	// Get decodes the entry stored under key into value, which must be a pointer
	// to the stored type. It returns ErrNotFound for a cache miss and a
	// deserialisation error when the entry does not match value.
	Get(ctx context.Context, key string, value any) error

	// Set stores value under key with the given time-to-live. A zero expiration
	// means the entry does not expire on its own, though it may still be evicted.
	// Values must be msgpack-serialisable.
	Set(ctx context.Context, key string, value any, expiration time.Duration) error

	// Delete removes keys from the cache. Deleting a key that is absent is not an
	// error, so callers do not need to check for existence first.
	Delete(ctx context.Context, keys ...string) error
}

// MemoryCache is the Ristretto-backed Cacher implementation. It is safe for
// concurrent use by multiple goroutines and must be closed to release its
// background resources.
type MemoryCache struct {
	cache *ristretto.Cache[string, []byte]
	// prefix namespaces every key written by this instance, so cache contents can
	// be told apart from anything else sharing the process.
	prefix string
}

// NewMemoryCache creates a Ristretto-backed memory cache.
// size is the maximum cache cost in bytes, e.g. 5*1024*1024 for 5MB.
func NewMemoryCache(size int) *MemoryCache {
	numCounters := max(int64(size/1024)*10, 10_000)
	c, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
		NumCounters: numCounters,
		MaxCost:     int64(size),
		BufferItems: 64,
	})
	if err != nil {
		panic(fmt.Sprintf("create memory cache: %v", err))
	}
	return &MemoryCache{cache: c, prefix: "teldrive:"}
}

// NewCache is compatibility helper from v1.8.3. Now only memory is supported.
func NewCache(_ context.Context, maxSize int) Cacher {
	return NewMemoryCache(maxSize)
}

// Get decodes the entry stored under key into value. A miss returns ErrNotFound;
// a key that exists but does not decode into value returns the msgpack error.
func (m *MemoryCache) Get(_ context.Context, key string, value any) error {
	key = m.prefix + key
	data, ok := m.cache.Get(key)
	if !ok {
		return ErrNotFound
	}
	return msgpack.Unmarshal(data, value)
}

// Set msgpack-encodes value and stores it under key with the given time-to-live.
// The entry is charged len(key)+len(payload) against the cache budget.
//
// The only error reported is a serialisation failure. Ristretto admission and
// eviction are asynchronous and lossy by design, so a nil error means "accepted
// for storage", not "guaranteed to still be readable afterwards".
func (m *MemoryCache) Set(_ context.Context, key string, value any, expiration time.Duration) error {
	key = m.prefix + key
	data, err := msgpack.Marshal(value)
	if err != nil {
		return err
	}
	cost := int64(len(key) + len(data))
	if m.cache.SetWithTTL(key, data, cost, expiration) {
		m.cache.Wait()
	}
	return nil
}

// Delete evicts each of the listed keys. Keys that are absent or already expired
// are ignored, so the call never fails and callers do not need to check first.
func (m *MemoryCache) Delete(_ context.Context, keys ...string) error {
	for _, key := range keys {
		m.cache.Del(m.prefix + key)
	}
	return nil
}

// Close releases the Ristretto store and its background goroutines. It is
// nil-safe, so a nil *MemoryCache or a zero-value instance can be closed
// defensively. The cache must not be used after Close returns.
func (m *MemoryCache) Close() {
	if m != nil && m.cache != nil {
		m.cache.Close()
	}
}

// Fetch is generic read-through helper: try cache, on miss call fn and cache result.
func Fetch[T any](ctx context.Context, c Cacher, key string, expiration time.Duration, fn func() (T, error)) (T, error) {
	var zero, value T
	err := c.Get(ctx, key, &value)
	if err == nil {
		return value, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return zero, err
	}
	value, err = fn()
	if err != nil {
		return zero, err
	}
	_ = c.Set(ctx, key, &value, expiration)
	return value, nil
}

// FetchArg is Fetch for read-through functions that already take one argument.
// The argument is bound into a closure so callers do not have to write one at
// every call site. Its error and caching behaviour are exactly Fetch's.
func FetchArg[T any, A any](ctx context.Context, c Cacher, key string, expiration time.Duration, fn func(a A) (T, error), a A) (T, error) {
	return Fetch(ctx, c, key, expiration, func() (T, error) {
		return fn(a)
	})
}

// Key builds a colon-joined cache key from arbitrary args, sorted for maps.
func Key(args ...any) string {
	parts := make([]string, len(args))
	for i, arg := range args {
		parts[i] = formatValue(arg)
	}
	return strings.Join(parts, ":")
}

// formatValue renders v as a stable, human-readable fragment of a cache key.
// Pointers are dereferenced, and slices, arrays and maps are rendered
// recursively so composite arguments do not collide through fmt's default
// formatting. Map entries are sorted, which makes the key independent of Go's
// randomised map iteration order; slice order is preserved because it is
// meaningful to the caller.
func formatValue(v any) string {
	if v == nil {
		return "nil"
	}
	val := reflect.ValueOf(v)
	switch val.Kind() {
	case reflect.Pointer:
		if val.IsNil() {
			return "nil"
		}
		return formatValue(val.Elem().Interface())
	case reflect.Array, reflect.Slice:
		parts := make([]string, val.Len())
		for i := range val.Len() {
			parts[i] = formatValue(val.Index(i).Interface())
		}
		return fmt.Sprintf("[%s]", strings.Join(parts, ","))
	case reflect.Map:
		parts := make([]string, 0, val.Len())
		for _, k := range val.MapKeys() {
			parts = append(parts, fmt.Sprintf("%s=%s", formatValue(k.Interface()), formatValue(val.MapIndex(k).Interface())))
		}
		slices.Sort(parts)
		return fmt.Sprintf("{%s}", strings.Join(parts, ","))
	case reflect.Struct:
		return fmt.Sprintf("%+v", v)
	default:
		return fmt.Sprintf("%v", v)
	}
}
