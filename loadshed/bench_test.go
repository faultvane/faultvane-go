package loadshed

import (
	"context"
	"testing"
)

func benchmarkShedder(b *testing.B, opts ...Option) *Shedder {
	b.Helper()
	shedder, err := New(opts...)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	return shedder
}

func BenchmarkDoAdmitted(b *testing.B) {
	shedder := benchmarkShedder(b)
	ctx := context.Background()
	for b.Loop() {
		_ = shedder.Do(ctx, succeed)
	}
}

func BenchmarkDoAdmittedWithPriority(b *testing.B) {
	shedder := benchmarkShedder(b)
	ctx := with(High)
	for b.Loop() {
		_ = shedder.Do(ctx, succeed)
	}
}

func BenchmarkDoShed(b *testing.B) {
	shedder := benchmarkShedder(b, WithMaxInFlight(1))
	ctx := context.Background()
	whileInFlight(b, shedder, 1, func() {
		for b.Loop() {
			_ = shedder.Do(ctx, succeed)
		}
	})
}

func BenchmarkDoAdmittedParallel(b *testing.B) {
	shedder := benchmarkShedder(b, WithMaxInFlight(1_000_000))
	ctx := context.Background()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = shedder.Do(ctx, succeed)
		}
	})
}
