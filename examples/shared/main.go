package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/faultvane/faultvane-go/circuitbreaker"
	redisstore "github.com/faultvane/faultvane-go/store/redis"
	"github.com/redis/go-redis/v9"
)

func main() {
	address := os.Getenv("FAULTVANE_REDIS_ADDR")
	if address == "" {
		address = "127.0.0.1:6399"
	}
	if err := run(os.Stdout, address, fmt.Sprintf("example-%d", time.Now().UnixNano())); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(out io.Writer, address, namespace string) error {
	client := redis.NewClient(&redis.Options{Addr: address})
	defer client.Close()
	store, err := redisstore.New(client, redisstore.WithNamespace(namespace))
	if err != nil {
		return err
	}
	ctx := context.Background()
	instances := make([]*circuitbreaker.Breaker, 5)
	for i := range instances {
		instances[i], err = circuitbreaker.New("inventory-service",
			circuitbreaker.WithStore(store),
			circuitbreaker.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
			circuitbreaker.WithFailureThreshold(5),
			circuitbreaker.WithMinimumCalls(1000),
			circuitbreaker.WithOpenDuration(time.Minute),
			circuitbreaker.WithSyncInterval(200*time.Millisecond),
		)
		if err != nil {
			return err
		}
		defer instances[i].Close(ctx)
	}

	outage := errors.New("inventory service: connection refused")
	for i, instance := range instances {
		_ = instance.Do(ctx, func(context.Context) error { return outage })
		fmt.Fprintf(out, "instance %d: 1 failure, local view %s\n", i+1, instance.State())
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		open := 0
		for _, instance := range instances {
			if instance.State() == circuitbreaker.Open {
				open++
			}
		}
		if open == len(instances) {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("the instances did not open the circuit")
		}
		time.Sleep(10 * time.Millisecond)
	}
	fmt.Fprintln(out, "5 failures in the shared window: the circuit is open for all 5 instances")
	err = instances[0].Do(ctx, func(context.Context) error { return nil })
	fmt.Fprintf(out, "next call on instance 1: %v\n", err)
	return nil
}
