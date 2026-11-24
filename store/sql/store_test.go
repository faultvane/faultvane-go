package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/faultvane/faultvane-go"
	"github.com/faultvane/faultvane-go/circuitbreaker"
	"github.com/faultvane/faultvane-go/circuitbreaker/storetest"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
)

var (
	namespaceCounter atomic.Uint64
	errDependency    = errors.New("dependency failed")
	quiet            = slog.New(slog.DiscardHandler)
)

type database struct {
	name    string
	dialect Dialect
	driver  string
	dsn     string
}

func databases() []database {
	env := func(variable, fallback string) string {
		if value := os.Getenv(variable); value != "" {
			return value
		}
		return fallback
	}
	return []database{
		{"PostgreSQL", PostgreSQL, "pgx", env("FAULTVANE_POSTGRES_DSN", "postgres://postgres:faultvane@127.0.0.1:5499/faultvane?sslmode=disable")},
		{"MySQL", MySQL, "mysql", env("FAULTVANE_MYSQL_DSN", "root:faultvane@tcp(127.0.0.1:3399)/faultvane")},
	}
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func open(t *testing.T, d database) *sql.DB {
	t.Helper()
	db, err := sql.Open(d.driver, d.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	eventually(t, d.name+" answers", func() bool { return db.Ping() == nil })
	return db
}

func newStore(t *testing.T, d database, db *sql.DB, opts ...Option) *Store {
	t.Helper()
	namespace := fmt.Sprintf("test-%d-%d", time.Now().UnixNano(), namespaceCounter.Add(1))
	store, err := New(db, d.dialect, append([]Option{WithNamespace(namespace)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store
}

func report(failures int64, ttl time.Duration) circuitbreaker.Report {
	return circuitbreaker.Report{
		Calls:         failures,
		Failures:      failures,
		Thresholds:    circuitbreaker.Thresholds{FailureThreshold: 5, MinimumCalls: 1000, FailureRate: 1},
		WindowSize:    time.Minute,
		WindowBuckets: 6,
		OpenDuration:  time.Hour,
		StateTTL:      ttl,
	}
}

func forEachDatabase(t *testing.T, name string, test func(t *testing.T, d database, db *sql.DB)) {
	for _, d := range databases() {
		t.Run(name+" with "+d.name, func(t *testing.T) { test(t, d, open(t, d)) })
	}
}

func TestConformance(t *testing.T) {
	forEachDatabase(t, "QA-8 DS-6 GEN-2 the store contract", func(t *testing.T, d database, db *sql.DB) {
		store := newStore(t, d, db)
		storetest.Run(t, func(*testing.T) circuitbreaker.Store { return store })
	})
}

func TestNew(t *testing.T) {
	db, err := sql.Open("pgx", "postgres://invalid")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	invalid := map[string]func() (*Store, error){
		"CFG-3 nil db":                     func() (*Store, error) { return New(nil, PostgreSQL, WithNamespace("a")) },
		"CFG-3 unknown dialect":            func() (*Store, error) { return New(db, MySQL+1, WithNamespace("a")) },
		"CFG-3 no namespace":               func() (*Store, error) { return New(db, PostgreSQL) },
		"CFG-3 namespace with a quote":     func() (*Store, error) { return New(db, PostgreSQL, WithNamespace("a'b")) },
		"CFG-3 namespace with 65 letters":  func() (*Store, error) { return New(db, PostgreSQL, WithNamespace(strings.Repeat("a", 65))) },
		"CFG-3 empty table":                func() (*Store, error) { return New(db, PostgreSQL, WithNamespace("a"), WithTable("")) },
		"CFG-3 table with a space":         func() (*Store, error) { return New(db, PostgreSQL, WithNamespace("a"), WithTable("a b")) },
		"CFG-3 table with a hyphen":        func() (*Store, error) { return New(db, PostgreSQL, WithNamespace("a"), WithTable("a-b")) },
		"CFG-3 table with a semicolon":     func() (*Store, error) { return New(db, MySQL, WithNamespace("a"), WithTable("a;DROP")) },
		"CFG-3 namespace outside of ASCII": func() (*Store, error) { return New(db, PostgreSQL, WithNamespace("café")) },
	}
	for name, create := range invalid {
		t.Run(name, func(t *testing.T) {
			if store, err := create(); store != nil || !errors.Is(err, faultvane.ErrInvalidConfig) {
				t.Fatalf("New() = %v, %v, want nil and ErrInvalidConfig", store, err)
			}
		})
	}

	t.Run("KEY-1 the statements of PostgreSQL use numbered parameters and the table name", func(t *testing.T) {
		store, err := New(db, PostgreSQL, WithNamespace("azAZ09._-"), WithTable("Breakers_1"))
		if err != nil {
			t.Fatal(err)
		}
		if want := "UPDATE Breakers_1 SET record = $1, expires_at = $2 WHERE namespace = $3 AND name = $4"; store.queries.update != want {
			t.Fatalf("update = %q, want %q", store.queries.update, want)
		}
		if want := "CREATE TABLE IF NOT EXISTS Breakers_1 (namespace VARCHAR(64) NOT NULL, name VARCHAR(64) NOT NULL, record TEXT NOT NULL, expires_at BIGINT NOT NULL, PRIMARY KEY (namespace, name))"; store.Schema() != want {
			t.Fatalf("Schema() = %q", store.Schema())
		}
		if !strings.Contains(store.queries.insert, "ON CONFLICT (namespace, name) DO NOTHING") || !strings.Contains(store.queries.lock, "clock_timestamp()") {
			t.Fatalf("insert = %q, lock = %q", store.queries.insert, store.queries.lock)
		}
	})

	t.Run("KEY-1 the statements of MySQL use question marks and the default table", func(t *testing.T) {
		store, err := New(db, MySQL, WithNamespace("a"))
		if err != nil {
			t.Fatal(err)
		}
		if want := "UPDATE faultvane_circuit_breakers SET record = ?, expires_at = ? WHERE namespace = ? AND name = ?"; store.queries.update != want {
			t.Fatalf("update = %q, want %q", store.queries.update, want)
		}
		if !strings.HasPrefix(store.queries.insert, "INSERT IGNORE INTO faultvane_circuit_breakers") || !strings.Contains(store.queries.lock, "NOW(6)") {
			t.Fatalf("insert = %q, lock = %q", store.queries.insert, store.queries.lock)
		}
	})
}

func TestData(t *testing.T) {
	ctx := context.Background()

	forEachDatabase(t, "KEY-2 DS-2 one row for each breaker holds the record", func(t *testing.T, d database, db *sql.DB) {
		store := newStore(t, d, db)
		if err := store.Migrate(ctx); err != nil {
			t.Fatalf("second Migrate() = %v", err)
		}
		if snapshot, err := store.Load(ctx, "a"); err != nil || snapshot.Generation != 0 || snapshot.StoreTime.IsZero() {
			t.Fatalf("Load() of a breaker without a row = %+v, %v", snapshot, err)
		}
		if _, err := store.Report(ctx, "a", report(2, time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Report(ctx, "b", report(5, time.Hour)); err != nil {
			t.Fatal(err)
		}
		var rows int
		var record string
		query := store.placeholders("SELECT COUNT(*), MAX(record) FROM " + store.table + " WHERE namespace = ? AND name = ?")
		if err := db.QueryRow(query, store.namespace, "a").Scan(&rows, &record); err != nil {
			t.Fatal(err)
		}
		if rows != 1 || !strings.Contains(record, `"Failures":2`) || !strings.Contains(record, `"State":0`) {
			t.Fatalf("rows = %d, record = %s", rows, record)
		}
		first, _ := store.Load(ctx, "a")
		second, _ := store.Load(ctx, "b")
		if first.State != circuitbreaker.Closed || second.State != circuitbreaker.Open || second.Generation != 1 {
			t.Fatalf("states = %+v, %+v", first, second)
		}
		if skew := time.Since(first.StoreTime).Abs(); skew > time.Hour {
			t.Fatalf("store time %v is %v from the local time", first.StoreTime, skew)
		}
	})

	forEachDatabase(t, "TTL-1 TTL-2 cleanup removes only rows that are not in use", func(t *testing.T, d database, db *sql.DB) {
		store := newStore(t, d, db, WithTable(fmt.Sprintf("cleanup_%d", time.Now().UnixNano())))
		t.Cleanup(func() { _, _ = db.Exec("DROP TABLE " + store.table) })
		if _, err := store.Report(ctx, "used", report(5, time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Report(ctx, "unused", report(5, time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
		removed, err := store.Cleanup(ctx)
		if err != nil || removed != 1 {
			t.Fatalf("Cleanup() = %d, %v, want 1 row", removed, err)
		}
		used, _ := store.Load(ctx, "used")
		unused, _ := store.Load(ctx, "unused")
		if used.Generation != 1 || unused.Generation != 0 {
			t.Fatalf("generations = %d, %d, want 1, 0", used.Generation, unused.Generation)
		}
	})

	forEachDatabase(t, "TTL-1 a report runs the cleanup at most one time in each interval", func(t *testing.T, d database, db *sql.DB) {
		store := newStore(t, d, db, WithTable(fmt.Sprintf("interval_%d", time.Now().UnixNano())))
		t.Cleanup(func() { _, _ = db.Exec("DROP TABLE " + store.table) })
		count := func() (rows int) {
			_ = db.QueryRow("SELECT COUNT(*) FROM " + store.table).Scan(&rows)
			return rows
		}
		if _, err := store.Restore(ctx, "old", circuitbreaker.Snapshot{Generation: 1, TransitionAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("UPDATE " + store.table + " SET expires_at = 1"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Report(ctx, "a", report(1, time.Hour)); err != nil || count() != 1 {
			t.Fatalf("rows = %d, err = %v, want 1 row after the first report", count(), err)
		}
		if _, err := db.Exec("UPDATE " + store.table + " SET expires_at = 1"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Report(ctx, "b", report(1, time.Hour)); err != nil || count() != 2 {
			t.Fatalf("rows = %d, err = %v, want 2 rows inside the cleanup interval", count(), err)
		}
	})

	forEachDatabase(t, "TTL-2 a cleanup does not remove a row that an operation just created", func(t *testing.T, d database, db *sql.DB) {
		store := newStore(t, d, db, WithTable(fmt.Sprintf("created_%d", time.Now().UnixNano())))
		t.Cleanup(func() { _, _ = db.Exec("DROP TABLE " + store.table) })
		if _, err := db.Exec(store.queries.insert, store.namespace, "a"); err != nil {
			t.Fatal(err)
		}
		if removed, err := store.Cleanup(ctx); err != nil || removed != 0 {
			t.Fatalf("Cleanup() = %d, %v, want 0 rows", removed, err)
		}
		var remaining time.Duration
		query := "SELECT expires_at FROM " + store.table
		var expires int64
		if err := db.QueryRow(query).Scan(&expires); err != nil {
			t.Fatal(err)
		}
		remaining = time.Until(time.UnixMicro(expires))
		if remaining < 30*time.Minute || remaining > 90*time.Minute {
			t.Fatalf("a new row expires in %v, want about 1h", remaining)
		}
	})

	forEachDatabase(t, "RF-1 a record that is not valid is a store fault", func(t *testing.T, d database, db *sql.DB) {
		store := newStore(t, d, db)
		if _, err := store.Report(ctx, "a", report(1, time.Hour)); err != nil {
			t.Fatal(err)
		}
		query := store.placeholders("UPDATE " + store.table + " SET record = 'not json' WHERE namespace = ? AND name = ?")
		if _, err := db.Exec(query, store.namespace, "a"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(ctx, "a"); !errors.Is(err, faultvane.ErrStoreUnavailable) {
			t.Fatalf("Load() error = %v, want ErrStoreUnavailable", err)
		}
		if _, err := store.Report(ctx, "a", report(1, time.Hour)); !errors.Is(err, faultvane.ErrStoreUnavailable) {
			t.Fatalf("Report() error = %v, want ErrStoreUnavailable", err)
		}
	})
}

func TestFaults(t *testing.T) {
	ctx := context.Background()

	forEachDatabase(t, "RF-1 ERR-3 each operation on a closed database returns ErrStoreUnavailable", func(t *testing.T, d database, db *sql.DB) {
		closed, err := sql.Open(d.driver, d.dsn)
		if err != nil {
			t.Fatal(err)
		}
		store := newStore(t, d, closed)
		_ = closed.Close()
		_, loadErr := store.Load(ctx, "a")
		_, reportErr := store.Report(ctx, "a", report(1, time.Hour))
		grant, acquireErr := store.AcquireProbe(ctx, "a", circuitbreaker.ProbeRequest{Token: 1, Limit: 1})
		_, completeErr := store.CompleteProbe(ctx, "a", circuitbreaker.ProbeResult{Token: 1})
		_, restoreErr := store.Restore(ctx, "a", circuitbreaker.Snapshot{Generation: 1})
		_, cleanupErr := store.Cleanup(ctx)
		failures := map[string]error{"load": loadErr, "report": reportErr, "acquire probe": acquireErr, "complete probe": completeErr, "restore": restoreErr, "cleanup": cleanupErr, "migrate": store.Migrate(ctx)}
		for operation, err := range failures {
			if !errors.Is(err, faultvane.ErrStoreUnavailable) || !strings.HasPrefix(err.Error(), "sqlstore: "+operation+": ") {
				t.Errorf("%s: error = %v", operation, err)
			}
		}
		if grant.Granted {
			t.Error("a failed probe request is granted")
		}
	})

	forEachDatabase(t, "RF-1 a table that does not exist is a store fault", func(t *testing.T, d database, db *sql.DB) {
		store, err := New(db, d.dialect, WithNamespace("a"), WithTable("missing_table"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Report(ctx, "a", report(1, time.Hour)); !errors.Is(err, faultvane.ErrStoreUnavailable) {
			t.Fatalf("Report() error = %v, want ErrStoreUnavailable", err)
		}
	})

	forEachDatabase(t, "RF-2 API-3 an operation stops when its context ends", func(t *testing.T, d database, db *sql.DB) {
		store := newStore(t, d, db)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := store.Report(cancelled, "a", report(5, time.Hour)); !errors.Is(err, context.Canceled) {
			t.Fatalf("Report() error = %v, want context.Canceled", err)
		}
		if snapshot, err := store.Load(ctx, "a"); err != nil || snapshot.Generation != 0 {
			t.Fatalf("Load() = %+v, %v", snapshot, err)
		}
	})
}

func newBreakers(t *testing.T, store *Store, count int, opts ...circuitbreaker.Option) []*circuitbreaker.Breaker {
	t.Helper()
	breakers := make([]*circuitbreaker.Breaker, count)
	for i := range breakers {
		base := []circuitbreaker.Option{
			circuitbreaker.WithStore(store), circuitbreaker.WithLogger(quiet),
			circuitbreaker.WithFailureThreshold(5), circuitbreaker.WithMinimumCalls(1000),
			circuitbreaker.WithWindowSize(10 * time.Minute), circuitbreaker.WithOpenDuration(500 * time.Millisecond),
			circuitbreaker.WithSyncInterval(100 * time.Millisecond), circuitbreaker.WithStoreTimeout(90 * time.Millisecond),
			circuitbreaker.WithEarlyReportInterval(10 * time.Millisecond), circuitbreaker.WithFaultThreshold(1000),
			circuitbreaker.WithProbeAttemptInterval(0), circuitbreaker.WithStartTimeout(5 * time.Second),
		}
		breaker, err := circuitbreaker.New("dependency", append(base, opts...)...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = breaker.Close(context.Background()) })
		breakers[i] = breaker
	}
	return breakers
}

func all(breakers []*circuitbreaker.Breaker, state circuitbreaker.State) func() bool {
	return func() bool {
		for _, b := range breakers {
			if b.State() != state || b.Mode() != circuitbreaker.Distributed {
				return false
			}
		}
		return true
	}
}

func TestAcceptance(t *testing.T) {
	ctx := context.Background()
	fail := func(context.Context) error { return errDependency }

	forEachDatabase(t, "AT-1 FD-6 ten instances with one failure each open the circuit", func(t *testing.T, d database, db *sql.DB) {
		store := newStore(t, d, db)
		breakers := newBreakers(t, store, 10, circuitbreaker.WithOpenDuration(time.Hour))
		for _, b := range breakers {
			_ = b.Do(ctx, fail)
		}
		eventually(t, "all instances are open", all(breakers, circuitbreaker.Open))
		if snapshot, err := store.Load(ctx, "dependency"); err != nil || snapshot.State != circuitbreaker.Open || snapshot.Generation != 1 {
			t.Fatalf("the database has %+v, %v, want open generation 1", snapshot, err)
		}
	})

	forEachDatabase(t, "AT-2 HO-1 exactly one of ten instances gets the probe", func(t *testing.T, d database, db *sql.DB) {
		store := newStore(t, d, db)
		breakers := newBreakers(t, store, 10, circuitbreaker.WithProbeTimeout(time.Hour))
		for range 5 {
			_ = breakers[0].Do(ctx, fail)
		}
		eventually(t, "all instances are open", all(breakers, circuitbreaker.Open))
		time.Sleep(600 * time.Millisecond)
		var permits []circuitbreaker.Permit
		var mu sync.Mutex
		var group sync.WaitGroup
		for _, b := range breakers {
			group.Add(1)
			go func() {
				defer group.Done()
				if permit, err := b.Allow(ctx); err == nil {
					mu.Lock()
					permits = append(permits, permit)
					mu.Unlock()
				}
			}()
		}
		group.Wait()
		if len(permits) != 1 {
			t.Fatalf("%d instances got a probe, want 1", len(permits))
		}
		permits[0].Success()
		eventually(t, "all instances are closed", all(breakers, circuitbreaker.Closed))
		if snapshot, _ := store.Load(ctx, "dependency"); snapshot.Generation != 3 {
			t.Fatalf("the database has generation %d, want 3", snapshot.Generation)
		}
	})

	forEachDatabase(t, "RF-5 an instance restores the state after the database lost the row", func(t *testing.T, d database, db *sql.DB) {
		store := newStore(t, d, db)
		breakers := newBreakers(t, store, 2, circuitbreaker.WithOpenDuration(time.Hour))
		for range 5 {
			_ = breakers[0].Do(ctx, fail)
		}
		eventually(t, "all instances are open", all(breakers, circuitbreaker.Open))
		query := store.placeholders("DELETE FROM " + store.table + " WHERE namespace = ? AND name = ?")
		if _, err := db.Exec(query, store.namespace, "dependency"); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the database has the open state again", func() bool {
			snapshot, err := store.Load(ctx, "dependency")
			return err == nil && snapshot.State == circuitbreaker.Open && snapshot.Generation == 1
		})
	})
}

func BenchmarkOperations(b *testing.B) {
	for _, d := range databases() {
		db, err := sql.Open(d.driver, d.dsn)
		if err != nil {
			b.Fatal(err)
		}
		store, err := New(db, d.dialect, WithNamespace(fmt.Sprintf("bench-%d", time.Now().UnixNano())))
		if err != nil {
			b.Fatal(err)
		}
		if err := store.Migrate(context.Background()); err != nil {
			b.Fatal(err)
		}
		b.Run(d.name+"/Load", func(b *testing.B) {
			for b.Loop() {
				if _, err := store.Load(context.Background(), "a"); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(d.name+"/Report", func(b *testing.B) {
			for b.Loop() {
				if _, err := store.Report(context.Background(), "a", report(0, time.Hour)); err != nil {
					b.Fatal(err)
				}
			}
		})
		_ = db.Close()
	}
}
