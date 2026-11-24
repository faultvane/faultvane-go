package timeout

import (
	"context"
	"testing"
	"time"
)

func benchmarkTimeout(b *testing.B) *Timeout {
	b.Helper()
	policy, err := New(time.Hour)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	return policy
}

func BenchmarkDo(b *testing.B) {
	policy := benchmarkTimeout(b)
	ctx := context.Background()
	call := func(context.Context) error { return nil }
	for b.Loop() {
		_ = policy.Do(ctx, call)
	}
}

func BenchmarkDoWithCallerDeadline(b *testing.B) {
	policy := benchmarkTimeout(b)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	call := func(context.Context) error { return nil }
	for b.Loop() {
		_ = policy.Do(ctx, call)
	}
}

func BenchmarkDoParallel(b *testing.B) {
	policy := benchmarkTimeout(b)
	ctx := context.Background()
	call := func(context.Context) error { return nil }
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = policy.Do(ctx, call)
		}
	})
}
