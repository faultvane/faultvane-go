package concurrencylimit

import (
	"context"
	"testing"
	"time"
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

func BenchmarkDo(b *testing.B) {
	limiter := benchmarkLimiter(b)
	ctx := context.Background()
	for b.Loop() {
		_ = limiter.Do(ctx, succeed)
	}
}

func BenchmarkDoLatencyThreshold(b *testing.B) {
	limiter := benchmarkLimiter(b, WithLatencyThreshold(time.Hour))
	ctx := context.Background()
	for b.Loop() {
		_ = limiter.Do(ctx, succeed)
	}
}

func BenchmarkDoAdapting(b *testing.B) {
	limiter := benchmarkLimiter(b, WithInitialLimit(1), WithMaxLimit(2))
	ctx := context.Background()
	for b.Loop() {
		_ = limiter.Do(ctx, succeed)
	}
}

func BenchmarkDoParallel(b *testing.B) {
	limiter := benchmarkLimiter(b, WithInitialLimit(1<<20), WithMaxLimit(1<<20))
	ctx := context.Background()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = limiter.Do(ctx, succeed)
		}
	})
}

func BenchmarkDoRejected(b *testing.B) {
	limiter := benchmarkLimiter(b, WithMinLimit(1), WithInitialLimit(1), WithMaxLimit(1))
	ctx := context.Background()
	_ = limiter.Do(ctx, func(context.Context) error {
		for b.Loop() {
			_ = limiter.Do(ctx, succeed)
		}
		return nil
	})
}
