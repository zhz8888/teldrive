package health

import (
	"context"
	"errors"
	"testing"
)

type pingerFunc func(context.Context) error

func (f pingerFunc) Ping(ctx context.Context) error { return f(ctx) }

func TestServiceHealth(t *testing.T) {
	t.Parallel()

	svc := NewService("v2-test", pingerFunc(func(context.Context) error { return nil }))
	if got := svc.Live(); got.State != "ok" || got.Version != "v2-test" {
		t.Fatalf("Live() = %#v", got)
	}
	if got, err := svc.Ready(context.Background()); err != nil || got.State != "ok" {
		t.Fatalf("Ready() = %#v, %v", got, err)
	}
}

// TestServiceReadyFailures pins the distinction between the two degraded causes: an
// unwired database is reported as ErrNotConfigured, while a database that fails its
// ping wraps the ping error and is not reported as a configuration gap.
func TestServiceReadyFailures(t *testing.T) {
	t.Parallel()

	got, err := NewService("v2-test", nil).Ready(context.Background())
	if !errors.Is(err, ErrNotConfigured) || got.State != "degraded" || got.Version != "v2-test" {
		t.Fatalf("nil database readiness = %#v, %v", got, err)
	}
	boom := errors.New("boom")
	got, err = NewService("v2-test", pingerFunc(func(context.Context) error { return boom })).Ready(context.Background())
	if !errors.Is(err, boom) || errors.Is(err, ErrNotConfigured) || got.State != "degraded" {
		t.Fatalf("failed database readiness = %#v, %v", got, err)
	}
}
