package redisstore

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func BenchmarkOperations(b *testing.B) {
	client := redis.NewClient(&redis.Options{Addr: standalone()})
	defer func() { _ = client.Close() }()
	store, err := New(client, WithNamespace("bench"))
	if err != nil {
		b.Fatal(err)
	}
	b.Run("Load", func(b *testing.B) {
		for b.Loop() {
			if _, err := store.Load(context.Background(), "a"); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Report", func(b *testing.B) {
		for b.Loop() {
			if _, err := store.Report(context.Background(), "a", report(0, 0, time.Hour)); err != nil {
				b.Fatal(err)
			}
		}
	})
}
