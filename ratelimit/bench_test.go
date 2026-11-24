package ratelimit

import (
	"context"
	"testing"

	"github.com/faultvane/faultvane-go"
)

func benchmarkLimiter(b *testing.B, opts ...Option) *Limiter {
	b.Helper()
	limiter, err := New(opts...)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	return limiter
}

func BenchmarkDoPermitted(b *testing.B) {
	limiter := benchmarkLimiter(b, WithRate(maxRate))
	ctx := context.Background()
	call := func(context.Context) error { return nil }
	for b.Loop() {
		_ = limiter.Do(ctx, call)
	}
}

func BenchmarkAllowPermitted(b *testing.B) {
	limiter := benchmarkLimiter(b, WithRate(maxRate))
	for b.Loop() {
		_ = limiter.Allow()
	}
}

func BenchmarkDoRejected(b *testing.B) {
	limiter := benchmarkLimiter(b, WithRate(1e-9), WithBurst(1))
	ctx := context.Background()
	call := func(context.Context) error { return nil }
	for b.Loop() {
		_ = limiter.Do(ctx, call)
	}
}

func BenchmarkDoPermittedParallel(b *testing.B) {
	limiter := benchmarkLimiter(b, WithRate(maxRate))
	ctx := context.Background()
	call := func(context.Context) error { return nil }
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = limiter.Do(ctx, call)
		}
	})
}

func BenchmarkAllowPermittedParallel(b *testing.B) {
	limiter := benchmarkLimiter(b, WithRate(maxRate))
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = limiter.Allow()
		}
	})
}

func benchmarkSharedLimiter(b *testing.B) *Limiter {
	b.Helper()
	limiter := benchmarkLimiter(b, WithRate(maxRate), WithStore(NewMemoryStore(faultvane.SystemClock())), WithName("benchmark"))
	b.Cleanup(func() { _ = limiter.Close(context.Background()) })
	return limiter
}

func BenchmarkDoPermittedShared(b *testing.B) {
	limiter := benchmarkSharedLimiter(b)
	ctx := context.Background()
	call := func(context.Context) error { return nil }
	for b.Loop() {
		_ = limiter.Do(ctx, call)
	}
	if stats := limiter.Stats(); stats.Rejected != 0 || stats.Mode != Distributed {
		b.Fatalf("stats = %+v, want no rejection in distributed mode", stats)
	}
}

func BenchmarkAllowPermittedShared(b *testing.B) {
	limiter := benchmarkSharedLimiter(b)
	for b.Loop() {
		_ = limiter.Allow()
	}
	if stats := limiter.Stats(); stats.Rejected != 0 || stats.Mode != Distributed {
		b.Fatalf("stats = %+v, want no rejection in distributed mode", stats)
	}
}

func BenchmarkAllowPermittedSharedParallel(b *testing.B) {
	limiter := benchmarkSharedLimiter(b)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = limiter.Allow()
		}
	})
	if stats := limiter.Stats(); stats.Rejected != 0 || stats.Mode != Distributed {
		b.Fatalf("stats = %+v, want no rejection in distributed mode", stats)
	}
}
