package mongostore

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go/circuitbreaker"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var errLinkDown = errors.New("the link to MongoDB is down")

type link struct {
	mu    sync.Mutex
	down  bool
	conns []net.Conn
}

func (l *link) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.down {
		_ = conn.Close()
		return nil, errLinkDown
	}
	l.conns = append(l.conns, conn)
	return conn, nil
}

func (l *link) stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.down = true
	for _, conn := range l.conns {
		_ = conn.Close()
	}
	l.conns = nil
}

func (l *link) start() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.down = false
}

func connectThrough(t *testing.T, l *link) *mongo.Collection {
	t.Helper()
	client, err := mongo.Connect(options.Client().ApplyURI(uri()).SetDialer(l).SetServerSelectionTimeout(500 * time.Millisecond).SetHeartbeatInterval(500 * time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	return client.Database("faultvane_test").Collection("circuit_breakers")
}

type instance struct {
	breaker *circuitbreaker.Breaker
	link    *link
}

func newInstances(t *testing.T, count int, opts ...circuitbreaker.Option) ([]instance, *Store) {
	t.Helper()
	direct := newStore(t, collection(t))
	if err := direct.EnsureIndexes(context.Background()); err != nil {
		t.Fatal(err)
	}
	opts = append([]circuitbreaker.Option{circuitbreaker.WithOpenDuration(time.Hour), circuitbreaker.WithFaultThreshold(2)}, opts...)
	instances := make([]instance, count)
	for i := range instances {
		l := &link{}
		store, err := New(connectThrough(t, l), WithNamespace(direct.namespace))
		if err != nil {
			t.Fatal(err)
		}
		instances[i] = instance{breaker: newBreakers(t, store, 1, opts...)[0], link: l}
	}
	return instances, direct
}

func call(b *circuitbreaker.Breaker, result error) error {
	return b.Do(context.Background(), func(context.Context) error { return result })
}

func allIn(instances []instance, state circuitbreaker.State, mode circuitbreaker.Mode) func() bool {
	return func() bool {
		for _, i := range instances {
			if i.breaker.State() != state || i.breaker.Mode() != mode {
				return false
			}
		}
		return true
	}
}

func assertStored(t *testing.T, store *Store, state circuitbreaker.State, generation uint64) {
	t.Helper()
	snapshot, err := store.Load(context.Background(), "dependency")
	if err != nil || snapshot.State != state || snapshot.Generation != generation {
		t.Fatalf("MongoDB has %+v, %v, want %v generation %d", snapshot, err, state, generation)
	}
}

func TestAT3_RequestsContinueWithoutTheStore(t *testing.T) {
	instances, _ := newInstances(t, 1, circuitbreaker.WithLocalFailureThreshold(2))
	cut := instances[0]
	eventually(t, "distributed mode", allIn(instances, circuitbreaker.Closed, circuitbreaker.Distributed))
	cut.link.stop()
	eventually(t, "local mode", allIn(instances, circuitbreaker.Closed, circuitbreaker.Local))
	for range 20 {
		if err := call(cut.breaker, nil); err != nil {
			t.Fatalf("Do() = %v without MongoDB, want nil", err)
		}
	}
	if s := cut.breaker.Stats(); s.StoreErrors < 2 || s.LocalModeEntries != 1 || s.Successes != 20 {
		t.Fatalf("stats = %+v, want 2 or more store errors, 1 local mode entry, and 20 successes", s)
	}
	for range 2 {
		_ = call(cut.breaker, errDependency)
	}
	if err := call(cut.breaker, nil); cut.breaker.State() != circuitbreaker.Open || err != circuitbreaker.ErrCircuitOpen {
		t.Fatalf("the local breaker is %v and Do() = %v, want open and ErrCircuitOpen", cut.breaker.State(), err)
	}
}

func TestAT4_ReconciliationKeepsTheNewerState(t *testing.T) {
	instances, direct := newInstances(t, 2)
	cut, connected := instances[0], instances[1]
	cut.link.stop()
	eventually(t, "local mode", func() bool { return cut.breaker.Mode() == circuitbreaker.Local })
	for range 5 {
		_ = call(connected.breaker, errDependency)
	}
	eventually(t, "the connected instance is open", allIn(instances[1:], circuitbreaker.Open, circuitbreaker.Distributed))
	assertStored(t, direct, circuitbreaker.Open, 1)
	if err := call(cut.breaker, nil); err != nil || cut.breaker.State() != circuitbreaker.Closed {
		t.Fatalf("the stale local state is %v and Do() = %v, want closed and nil", cut.breaker.State(), err)
	}
	cut.link.start()
	eventually(t, "both instances are open in distributed mode", allIn(instances, circuitbreaker.Open, circuitbreaker.Distributed))
	assertStored(t, direct, circuitbreaker.Open, 1)
	if s := cut.breaker.Stats(); s.LocalModeEntries != 1 || s.Opened != 1 {
		t.Fatalf("stats = %+v, want 1 local mode entry and 1 change to open", s)
	}
}

func TestAT5_StaleWriterIsRejected(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, collection(t))
	if _, err := store.Restore(ctx, "dependency", circuitbreaker.Snapshot{State: circuitbreaker.HalfOpen, Generation: 100, TransitionAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	const stale, newer = 1, 2
	for _, token := range []uint64{stale, newer} {
		grant, err := store.AcquireProbe(ctx, "dependency", circuitbreaker.ProbeRequest{Token: token, Limit: 2, LeaseDuration: time.Minute, StateTTL: time.Hour})
		if err != nil || !grant.Granted || grant.Snapshot.Generation != 100 {
			t.Fatalf("AcquireProbe() = %+v, %v, want a grant at generation 100", grant, err)
		}
	}
	result := circuitbreaker.ProbeResult{Generation: 100, Token: stale, Result: circuitbreaker.Success, SuccessThreshold: 1, OpenDuration: time.Hour, StateTTL: time.Hour}
	attempts := 0
	snapshot, err := store.update(ctx, "complete probe", "dependency", time.Hour, func(r circuitbreaker.Record, now time.Time) circuitbreaker.Record {
		if attempts++; attempts == 1 {
			failure := result
			failure.Token, failure.Result = newer, circuitbreaker.Failure
			if snapshot, err := store.CompleteProbe(ctx, "dependency", failure); err != nil || snapshot.State != circuitbreaker.Open || snapshot.Generation != 101 {
				t.Errorf("CompleteProbe() of the newer instance = %+v, %v, want open generation 101", snapshot, err)
			}
		}
		return r.CompleteProbe(now, result)
	})
	if err != nil || attempts != 2 || snapshot.State != circuitbreaker.Open || snapshot.Generation != 101 {
		t.Fatalf("the stale write = %+v, %v after %d attempts, want open generation 101 after 2", snapshot, err, attempts)
	}
	assertStored(t, store, circuitbreaker.Open, 101)
}

func TestAT6_InstancesConvergeAfterTheOutage(t *testing.T) {
	instances, direct := newInstances(t, 4, circuitbreaker.WithLocalFailureThreshold(2))
	for _, i := range instances {
		i.link.stop()
	}
	eventually(t, "all instances are in local mode", allIn(instances, circuitbreaker.Closed, circuitbreaker.Local))
	for range 2 {
		_ = call(instances[0].breaker, errDependency)
		_ = call(instances[1].breaker, errDependency)
	}
	_ = call(instances[2].breaker, errDependency)
	if !allIn(instances[:2], circuitbreaker.Open, circuitbreaker.Local)() || !allIn(instances[2:], circuitbreaker.Closed, circuitbreaker.Local)() {
		t.Fatal("the local states are not open, open, closed, closed")
	}
	if snapshot, err := direct.Report(context.Background(), "dependency", report(5, time.Hour)); err != nil || snapshot.State != circuitbreaker.Open {
		t.Fatalf("Report() of an instance with a connection = %+v, %v, want open", snapshot, err)
	}
	for _, i := range instances {
		i.link.start()
	}
	eventually(t, "all instances are open in distributed mode", allIn(instances, circuitbreaker.Open, circuitbreaker.Distributed))
	assertStored(t, direct, circuitbreaker.Open, 1)
}
