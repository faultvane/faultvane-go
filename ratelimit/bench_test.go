package ratelimit

import (
	"context"
	"testing"
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
