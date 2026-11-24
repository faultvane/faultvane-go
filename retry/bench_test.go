package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

func benchmarkRetry(b *testing.B, opts ...Option) *Retry {
	b.Helper()
	r, err := New(opts...)
	if err != nil {
		b.Fatal(err)
	}
	r.wait = func(context.Context, time.Duration) bool { return true }
	b.ReportAllocs()
	return r
}

func BenchmarkDoSuccess(b *testing.B) {
	r := benchmarkRetry(b)
	ctx := context.Background()
	call := func(context.Context) error { return nil }
	for b.Loop() {
		_ = r.Do(ctx, call)
	}
}

func BenchmarkDoSuccessWithBudget(b *testing.B) {
	r := benchmarkRetry(b, WithBudget(0.1))
	ctx := context.Background()
	call := func(context.Context) error { return nil }
	for b.Loop() {
		_ = r.Do(ctx, call)
	}
}

func BenchmarkDoSuccessWithBudgetParallel(b *testing.B) {
	r := benchmarkRetry(b, WithBudget(0.1))
	ctx := context.Background()
	call := func(context.Context) error { return nil }
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = r.Do(ctx, call)
		}
	})
}

func BenchmarkDoThreeFailedAttempts(b *testing.B) {
	r := benchmarkRetry(b)
	ctx := context.Background()
	failure := errors.New("dependency failed")
	call := func(context.Context) error { return failure }
	for b.Loop() {
		_ = r.Do(ctx, call)
	}
}

func BenchmarkDoFailedWithEmptyBudgetParallel(b *testing.B) {
	r := benchmarkRetry(b, WithBudget(0.1))
	ctx := context.Background()
	failure := errors.New("dependency failed")
	call := func(context.Context) error { return failure }
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = r.Do(ctx, call)
		}
	})
}
