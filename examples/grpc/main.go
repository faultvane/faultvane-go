package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync/atomic"
	"time"

	"github.com/faultvane/faultvane-go"
	"github.com/faultvane/faultvane-go/circuitbreaker"
	"github.com/faultvane/faultvane-go/timeout"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func main() {
	if err := run(os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func classify(err error) circuitbreaker.Result {
	switch status.Code(err) {
	case codes.OK:
		return circuitbreaker.Success
	case codes.Canceled, codes.InvalidArgument, codes.NotFound, codes.AlreadyExists, codes.PermissionDenied, codes.Unauthenticated, codes.FailedPrecondition, codes.OutOfRange:
		return circuitbreaker.Ignored
	}
	return circuitbreaker.Failure
}

func resilient(policy faultvane.Policy) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, request, reply any, connection *grpc.ClientConn, invoker grpc.UnaryInvoker, options ...grpc.CallOption) error {
		return policy.Do(ctx, func(ctx context.Context) error {
			return invoker(ctx, method, request, reply, connection, options...)
		})
	}
}

func run(out io.Writer) error {
	var healthy atomic.Bool
	healthy.Store(true)
	outage := func(ctx context.Context, request any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !healthy.Load() {
			return nil, status.Error(codes.Unavailable, "outage")
		}
		return handler(ctx, request)
	}
	listener := bufconn.Listen(1 << 16)
	server := grpc.NewServer(grpc.UnaryInterceptor(outage))
	grpc_health_v1.RegisterHealthServer(server, health.NewServer())
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()

	breaker, err := circuitbreaker.New("inventory-service",
		circuitbreaker.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		circuitbreaker.WithClassifier(classify),
		circuitbreaker.WithMinimumCalls(4),
		circuitbreaker.WithOpenDuration(200*time.Millisecond),
	)
	if err != nil {
		return err
	}
	limited, err := timeout.New(time.Second)
	if err != nil {
		return err
	}
	connection, err := grpc.NewClient("passthrough:///inventory",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(resilient(faultvane.Combine(breaker, limited))),
	)
	if err != nil {
		return err
	}
	defer func() { _ = connection.Close() }()
	client := grpc_health_v1.NewHealthClient(connection)

	request := func(label, service string) {
		_, err := client.Check(context.Background(), &grpc_health_v1.HealthCheckRequest{Service: service})
		switch {
		case err == nil:
			_, _ = fmt.Fprintf(out, "%s: ok (circuit %s)\n", label, breaker.State())
		case errors.Is(err, faultvane.ErrRejected):
			_, _ = fmt.Fprintf(out, "%s: rejected without a call (circuit %s)\n", label, breaker.State())
		default:
			_, _ = fmt.Fprintf(out, "%s: %s (circuit %s)\n", label, status.Code(err), breaker.State())
		}
	}

	request("healthy dependency", "")
	for i := 1; i <= 4; i++ {
		request(fmt.Sprintf("unknown service, request %d", i), "unknown")
	}
	healthy.Store(false)
	for i := 1; i <= 4; i++ {
		request(fmt.Sprintf("outage, request %d", i), "")
	}
	healthy.Store(true)
	request("dependency recovered, open duration not ended", "")
	time.Sleep(250 * time.Millisecond)
	request("after the open duration", "")
	request("normal operation", "")
	return nil
}
