package redisstore

import (
	"context"
	_ "embed"
	"fmt"
	"strconv"
	"time"

	"github.com/faultvane/faultvane-go"
	"github.com/faultvane/faultvane-go/circuitbreaker"
	"github.com/redis/go-redis/v9"
)

const (
	defaultPrefix = "faultvane"
	maxPartLength = 64
	restoreTTL    = time.Hour
)

var (
	//go:embed scripts/common.lua
	common string
	//go:embed scripts/load.lua
	loadSource string
	//go:embed scripts/report.lua
	reportSource string
	//go:embed scripts/acquire_probe.lua
	acquireProbeSource string
	//go:embed scripts/complete_probe.lua
	completeProbeSource string
	//go:embed scripts/restore.lua
	restoreSource string

	loadScript          = redis.NewScript(common + loadSource)
	reportScript        = redis.NewScript(common + reportSource)
	acquireProbeScript  = redis.NewScript(common + acquireProbeSource)
	completeProbeScript = redis.NewScript(common + completeProbeSource)
	restoreScript       = redis.NewScript(common + restoreSource)
)

type Subscriber interface {
	Subscribe(ctx context.Context, channels ...string) *redis.PubSub
}

type Store struct {
	client        redis.Scripter
	prefix        string
	namespace     string
	subscriber    Subscriber
	notifications bool
}

var _ circuitbreaker.Notifier = (*Store)(nil)

type Option func(*Store)

func WithNotifications(subscriber Subscriber) Option {
	return func(s *Store) {
		s.subscriber = subscriber
		s.notifications = true
	}
}

func WithNamespace(namespace string) Option {
	return func(s *Store) { s.namespace = namespace }
}

func WithKeyPrefix(prefix string) Option {
	return func(s *Store) { s.prefix = prefix }
}

func validPart(part string) bool {
	if part == "" || len(part) > maxPartLength {
		return false
	}
	for _, r := range part {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func New(client redis.Scripter, opts ...Option) (*Store, error) {
	s := &Store{client: client, prefix: defaultPrefix}
	for _, opt := range opts {
		opt(s)
	}
	switch {
	case client == nil:
		return nil, fmt.Errorf("%w: client must not be nil", faultvane.ErrInvalidConfig)
	case !validPart(s.namespace):
		return nil, fmt.Errorf("%w: Namespace must have 1 to 64 characters from a-z, A-Z, 0-9, '.', '_', '-'", faultvane.ErrInvalidConfig)
	case !validPart(s.prefix):
		return nil, fmt.Errorf("%w: KeyPrefix must have 1 to 64 characters from a-z, A-Z, 0-9, '.', '_', '-'", faultvane.ErrInvalidConfig)
	case s.notifications && s.subscriber == nil:
		return nil, fmt.Errorf("%w: Notifications must have a subscriber that is not nil", faultvane.ErrInvalidConfig)
	}
	return s, nil
}

func (s *Store) base(name string) string {
	return s.prefix + ":{" + s.namespace + ":" + name + "}:cb:"
}

func (s *Store) keys(name string) []string {
	base := s.base(name)
	return []string{base + "state", base + "window", base + "probes"}
}

func (s *Store) channel(name string) string {
	return s.base(name) + "transitions"
}

func (s *Store) Subscribe(ctx context.Context, name string, changed func()) (func(), error) {
	if !s.notifications {
		return func() {}, nil
	}
	subscription := s.subscriber.Subscribe(ctx, s.channel(name))
	messages, done := subscription.Channel(), make(chan struct{})
	go func() {
		defer close(done)
		for range messages {
			changed()
		}
	}()
	return func() {
		_ = subscription.Close()
		<-done
	}, nil
}

func micros(d time.Duration) int64 {
	if d > 0 && d < time.Microsecond {
		return 1
	}
	return d.Microseconds()
}

func millis(d time.Duration) int64 {
	return max(d.Milliseconds(), 1)
}

func integer(value any) (int64, error) {
	switch v := value.(type) {
	case int64:
		return v, nil
	case string:
		return strconv.ParseInt(v, 10, 64)
	}
	return 0, fmt.Errorf("unexpected value %v", value)
}

func parse(raw any) (circuitbreaker.Snapshot, bool, error) {
	values, ok := raw.([]any)
	if !ok || len(values) != 7 {
		return circuitbreaker.Snapshot{}, false, fmt.Errorf("unexpected reply %v", raw)
	}
	fingerprint, ok := values[6].(string)
	if !ok {
		return circuitbreaker.Snapshot{}, false, fmt.Errorf("unexpected reply %v", raw)
	}
	var fields [6]int64
	for i, value := range values[:6] {
		field, err := integer(value)
		if err != nil {
			return circuitbreaker.Snapshot{}, false, fmt.Errorf("unexpected reply %v: %w", raw, err)
		}
		fields[i] = field
	}
	return circuitbreaker.Snapshot{
		State:        circuitbreaker.State(fields[0]),
		Generation:   uint64(fields[1]),
		TransitionAt: time.UnixMicro(fields[2]),
		OpenDuration: time.Duration(fields[3]) * time.Microsecond,
		StoreTime:    time.UnixMicro(fields[4]),
		Fingerprint:  fingerprint,
	}, fields[5] == 1, nil
}

func (s *Store) run(ctx context.Context, operation string, script *redis.Script, name string, args ...any) (circuitbreaker.Snapshot, bool, error) {
	keys := s.keys(name)
	if s.notifications {
		keys = append(keys, s.channel(name))
	}
	raw, err := script.Run(ctx, s.client, keys, args...).Result()
	if err == nil {
		var snapshot circuitbreaker.Snapshot
		var granted bool
		if snapshot, granted, err = parse(raw); err == nil {
			return snapshot, granted, nil
		}
	}
	return circuitbreaker.Snapshot{}, false, fmt.Errorf("redisstore: %s: %w: %w", operation, faultvane.ErrStoreUnavailable, err)
}

func (s *Store) Load(ctx context.Context, name string) (circuitbreaker.Snapshot, error) {
	snapshot, _, err := s.run(ctx, "load", loadScript, name)
	return snapshot, err
}

func (s *Store) Report(ctx context.Context, name string, report circuitbreaker.Report) (circuitbreaker.Snapshot, error) {
	snapshot, _, err := s.run(ctx, "report", reportScript, name,
		report.Generation, report.Calls, report.Failures, report.SlowCalls,
		report.Thresholds.FailureThreshold, report.Thresholds.MinimumCalls,
		strconv.FormatFloat(report.Thresholds.FailureRate, 'g', -1, 64),
		strconv.FormatFloat(report.Thresholds.SlowCallRate, 'g', -1, 64),
		micros(report.WindowSize/time.Duration(report.WindowBuckets)), report.WindowBuckets,
		micros(report.OpenDuration), millis(report.StateTTL), report.Fingerprint,
	)
	return snapshot, err
}

func (s *Store) AcquireProbe(ctx context.Context, name string, request circuitbreaker.ProbeRequest) (circuitbreaker.ProbeGrant, error) {
	snapshot, granted, err := s.run(ctx, "acquire probe", acquireProbeScript, name,
		strconv.FormatUint(request.Token, 10), request.Limit, micros(request.LeaseDuration), millis(request.StateTTL), request.Fingerprint,
	)
	return circuitbreaker.ProbeGrant{Snapshot: snapshot, Granted: granted}, err
}

func (s *Store) CompleteProbe(ctx context.Context, name string, result circuitbreaker.ProbeResult) (circuitbreaker.Snapshot, error) {
	snapshot, _, err := s.run(ctx, "complete probe", completeProbeScript, name,
		result.Generation, strconv.FormatUint(result.Token, 10), int(result.Result), result.SuccessThreshold,
		micros(result.OpenDuration), millis(result.StateTTL), result.Fingerprint,
	)
	return snapshot, err
}

func (s *Store) Restore(ctx context.Context, name string, snapshot circuitbreaker.Snapshot) (circuitbreaker.Snapshot, error) {
	restored, _, err := s.run(ctx, "restore", restoreScript, name,
		int(snapshot.State), snapshot.Generation, snapshot.TransitionAt.UnixMicro(), micros(snapshot.OpenDuration), millis(restoreTTL),
	)
	return restored, err
}
