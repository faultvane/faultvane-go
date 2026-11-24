package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go/circuitbreaker"
	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

var errLinkDown = errors.New("the link to the database is down")

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

func openThrough(t *testing.T, d database, l *link) *sql.DB {
	t.Helper()
	var db *sql.DB
	if d.dialect == MySQL {
		config, err := mysql.ParseDSN(d.dsn)
		if err != nil {
			t.Fatal(err)
		}
		config.DialFunc, config.Logger = l.DialContext, &mysql.NopLogger{}
		connector, err := mysql.NewConnector(config)
		if err != nil {
			t.Fatal(err)
		}
		db = sql.OpenDB(connector)
	} else {
		config, err := pgx.ParseConfig(d.dsn)
		if err != nil {
			t.Fatal(err)
		}
		config.DialFunc = l.DialContext
		db = stdlib.OpenDB(*config)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type instance struct {
	breaker *circuitbreaker.Breaker
	link    *link
}

func newInstances(t *testing.T, d database, db *sql.DB, count int, opts ...circuitbreaker.Option) ([]instance, *Store) {
	t.Helper()
	direct := newStore(t, d, db)
	opts = append([]circuitbreaker.Option{circuitbreaker.WithOpenDuration(time.Hour), circuitbreaker.WithFaultThreshold(2)}, opts...)
	instances := make([]instance, count)
	for i := range instances {
		l := &link{}
		store, err := New(openThrough(t, d, l), d.dialect, WithNamespace(direct.namespace))
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
		t.Fatalf("the database has %+v, %v, want %v generation %d", snapshot, err, state, generation)
	}
}

func TestAT3_RequestsContinueWithoutTheStore(t *testing.T) {
	forEachDatabase(t, "AT-3 DG-1 an instance works in local mode without the database", func(t *testing.T, d database, db *sql.DB) {
		instances, _ := newInstances(t, d, db, 1, circuitbreaker.WithLocalFailureThreshold(2))
		cut := instances[0]
		eventually(t, "distributed mode", allIn(instances, circuitbreaker.Closed, circuitbreaker.Distributed))
		cut.link.stop()
		eventually(t, "local mode", allIn(instances, circuitbreaker.Closed, circuitbreaker.Local))
		for range 20 {
			if err := call(cut.breaker, nil); err != nil {
				t.Fatalf("Do() = %v without the database, want nil", err)
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
	})
}

func TestAT4_ReconciliationKeepsTheNewerState(t *testing.T) {
	forEachDatabase(t, "AT-4 RC-1 an instance takes the state of the database after the outage", func(t *testing.T, d database, db *sql.DB) {
		instances, direct := newInstances(t, d, db, 2)
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
	})
}

func TestAT5_StaleWriterIsRejected(t *testing.T) {
	forEachDatabase(t, "AT-5 GEN-3 a probe result of generation 100 does not change generation 101", func(t *testing.T, d database, db *sql.DB) {
		ctx := context.Background()
		store := newStore(t, d, db)
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
		result := circuitbreaker.ProbeResult{Generation: 100, Token: newer, Result: circuitbreaker.Failure, SuccessThreshold: 1, OpenDuration: time.Hour, StateTTL: time.Hour}
		if snapshot, err := store.CompleteProbe(ctx, "dependency", result); err != nil || snapshot.State != circuitbreaker.Open || snapshot.Generation != 101 {
			t.Fatalf("CompleteProbe() of the newer instance = %+v, %v, want open generation 101", snapshot, err)
		}
		result.Token, result.Result = stale, circuitbreaker.Success
		if snapshot, err := store.CompleteProbe(ctx, "dependency", result); err != nil || snapshot.State != circuitbreaker.Open || snapshot.Generation != 101 {
			t.Fatalf("CompleteProbe() of the stale instance = %+v, %v, want open generation 101", snapshot, err)
		}
		assertStored(t, store, circuitbreaker.Open, 101)
	})
}

func TestAT6_InstancesConvergeAfterTheOutage(t *testing.T) {
	forEachDatabase(t, "AT-6 RC-4 instances with different local states get one state", func(t *testing.T, d database, db *sql.DB) {
		instances, direct := newInstances(t, d, db, 4, circuitbreaker.WithLocalFailureThreshold(2))
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
	})
}
