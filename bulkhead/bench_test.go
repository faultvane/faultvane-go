package bulkhead

import (
	"context"
	"testing"
)

func benchmarkBulkhead(b *testing.B, opts ...Option) *Bulkhead {
	b.Helper()
	bulkhead, err := New(opts...)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	return bulkhead
}

func BenchmarkDo(b *testing.B) {
	bulkhead := benchmarkBulkhead(b)
	ctx := context.Background()
	for b.Loop() {
		_ = bulkhead.Do(ctx, succeed)
	}
}

func BenchmarkDoParallel(b *testing.B) {
	bulkhead := benchmarkBulkhead(b, WithMaxConcurrent(1<<20))
	ctx := context.Background()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = bulkhead.Do(ctx, succeed)
		}
	})
}

func BenchmarkDoFull(b *testing.B) {
	bulkhead := benchmarkBulkhead(b, WithMaxConcurrent(1))
	ctx := context.Background()
	_ = bulkhead.Do(ctx, func(context.Context) error {
		for b.Loop() {
			_ = bulkhead.Do(ctx, succeed)
		}
		return nil
	})
}
