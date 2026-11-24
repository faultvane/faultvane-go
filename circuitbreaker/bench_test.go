package circuitbreaker

import (
	"context"
	"testing"
)

func benchmarkBreaker(b *testing.B, open bool) *Breaker {
	b.Helper()
	breaker, err := New("benchmark")
	if err != nil {
		b.Fatal(err)
	}
	if open {
		breaker.trip(0, breaker.cfg.clock.Now())
	}
	b.ReportAllocs()
	return breaker
}

func BenchmarkDoClosed(b *testing.B) {
	breaker := benchmarkBreaker(b, false)
	ctx := context.Background()
	call := func(context.Context) error { return nil }
	for b.Loop() {
		_ = breaker.Do(ctx, call)
	}
}

func BenchmarkExecuteClosed(b *testing.B) {
	breaker := benchmarkBreaker(b, false)
	ctx := context.Background()
	call := func(context.Context) (int, error) { return 1, nil }
	for b.Loop() {
		_, _ = Execute(ctx, breaker, call)
	}
}

func BenchmarkAllowClosed(b *testing.B) {
	breaker := benchmarkBreaker(b, false)
	ctx := context.Background()
	for b.Loop() {
		permit, _ := breaker.Allow(ctx)
		permit.Success()
	}
}

func BenchmarkDoClosedParallel(b *testing.B) {
	breaker := benchmarkBreaker(b, false)
	ctx := context.Background()
	call := func(context.Context) error { return nil }
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = breaker.Do(ctx, call)
		}
	})
}

func BenchmarkDoOpen(b *testing.B) {
	breaker := benchmarkBreaker(b, true)
	ctx := context.Background()
	call := func(context.Context) error { return nil }
	for b.Loop() {
		_ = breaker.Do(ctx, call)
	}
}
