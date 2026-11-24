package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/faultvane/faultvane-go"
	"github.com/faultvane/faultvane-go/circuitbreaker"
)

const (
	defaultTable    = "faultvane_circuit_breakers"
	maxPartLength   = 64
	restoreTTL      = time.Hour
	cleanupInterval = time.Minute
)

type Dialect uint8

const (
	PostgreSQL Dialect = iota
	MySQL
)

type queries struct {
	schema  string
	insert  string
	lock    string
	update  string
	load    string
	cleanup string
}

type Store struct {
	db          *sql.DB
	dialect     Dialect
	table       string
	namespace   string
	queries     queries
	lastCleanup atomic.Int64
}

type Option func(*Store)

func WithNamespace(namespace string) Option {
	return func(s *Store) { s.namespace = namespace }
}

func WithTable(table string) Option {
	return func(s *Store) { s.table = table }
}

func validPart(part, extra string) bool {
	if part == "" || len(part) > maxPartLength {
		return false
	}
	for _, r := range part {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', strings.ContainsRune(extra, r):
		default:
			return false
		}
	}
	return true
}

func New(db *sql.DB, dialect Dialect, opts ...Option) (*Store, error) {
	s := &Store{db: db, dialect: dialect, table: defaultTable}
	for _, opt := range opts {
		opt(s)
	}
	switch {
	case db == nil:
		return nil, fmt.Errorf("%w: db must not be nil", faultvane.ErrInvalidConfig)
	case dialect > MySQL:
		return nil, fmt.Errorf("%w: Dialect must be PostgreSQL or MySQL", faultvane.ErrInvalidConfig)
	case !validPart(s.namespace, "._-"):
		return nil, fmt.Errorf("%w: Namespace must have 1 to 64 characters from a-z, A-Z, 0-9, '.', '_', '-'", faultvane.ErrInvalidConfig)
	case !validPart(s.table, "_"):
		return nil, fmt.Errorf("%w: Table must have 1 to 64 characters from a-z, A-Z, 0-9, '_'", faultvane.ErrInvalidConfig)
	}
	s.queries = s.build()
	return s, nil
}

func (s *Store) build() queries {
	now := "(EXTRACT(EPOCH FROM clock_timestamp()) * 1000000)::bigint"
	insert := "INSERT INTO %[1]s (namespace, name, record, expires_at) VALUES (?, ?, '{}', %[2]s + %[3]d) ON CONFLICT (namespace, name) DO NOTHING"
	if s.dialect == MySQL {
		now = "CAST(UNIX_TIMESTAMP(NOW(6)) * 1000000 AS SIGNED)"
		insert = "INSERT IGNORE INTO %[1]s (namespace, name, record, expires_at) VALUES (?, ?, '{}', %[2]s + %[3]d)"
	}
	q := queries{
		schema:  "CREATE TABLE IF NOT EXISTS %[1]s (namespace VARCHAR(64) NOT NULL, name VARCHAR(64) NOT NULL, record TEXT NOT NULL, expires_at BIGINT NOT NULL, PRIMARY KEY (namespace, name))",
		insert:  insert,
		lock:    "SELECT record, %[2]s FROM %[1]s WHERE namespace = ? AND name = ? FOR UPDATE",
		update:  "UPDATE %[1]s SET record = ?, expires_at = ? WHERE namespace = ? AND name = ?",
		load:    "SELECT %[2]s, (SELECT record FROM %[1]s WHERE namespace = ? AND name = ?)",
		cleanup: "DELETE FROM %[1]s WHERE expires_at < %[2]s",
	}
	for _, query := range []*string{&q.schema, &q.insert, &q.lock, &q.update, &q.load, &q.cleanup} {
		*query = s.placeholders(fmt.Sprintf(*query, s.table, now, restoreTTL.Microseconds()))
	}
	return q
}

func (s *Store) placeholders(query string) string {
	if s.dialect == MySQL {
		return query
	}
	var b strings.Builder
	index := 0
	for _, r := range query {
		if r == '?' {
			index++
			fmt.Fprintf(&b, "$%d", index)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (s *Store) Schema() string { return s.queries.schema }

func (s *Store) fail(operation string, err error) error {
	return fmt.Errorf("sqlstore: %s: %w: %w", operation, faultvane.ErrStoreUnavailable, err)
}

func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, s.queries.schema); err != nil {
		return s.fail("migrate", err)
	}
	return nil
}

func (s *Store) Cleanup(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, s.queries.cleanup)
	if err != nil {
		return 0, s.fail("cleanup", err)
	}
	return result.RowsAffected()
}

func decode(text string) (circuitbreaker.Record, error) {
	var record circuitbreaker.Record
	err := json.Unmarshal([]byte(text), &record)
	return record, err
}

func (s *Store) change(ctx context.Context, name string, ttl time.Duration, apply func(circuitbreaker.Record, time.Time) circuitbreaker.Record) (circuitbreaker.Snapshot, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return circuitbreaker.Snapshot{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var text string
	var micros int64
	if err = tx.QueryRowContext(ctx, s.queries.lock, s.namespace, name).Scan(&text, &micros); err != nil {
		return circuitbreaker.Snapshot{}, errors.Is(err, sql.ErrNoRows), err
	}
	record, err := decode(text)
	if err != nil {
		return circuitbreaker.Snapshot{}, false, err
	}
	now := time.UnixMicro(micros)
	record = apply(record, now)
	encoded, _ := json.Marshal(record)
	if _, err = tx.ExecContext(ctx, s.queries.update, string(encoded), now.Add(ttl).UnixMicro(), s.namespace, name); err != nil {
		return circuitbreaker.Snapshot{}, false, err
	}
	return record.Snapshot(now), false, tx.Commit()
}

func (s *Store) update(ctx context.Context, operation, name string, ttl time.Duration, apply func(circuitbreaker.Record, time.Time) circuitbreaker.Record) (circuitbreaker.Snapshot, error) {
	snapshot, missing, err := s.change(ctx, name, ttl, apply)
	if missing {
		if _, err = s.db.ExecContext(ctx, s.queries.insert, s.namespace, name); err == nil {
			snapshot, _, err = s.change(ctx, name, ttl, apply)
		}
	}
	if err != nil {
		return circuitbreaker.Snapshot{}, s.fail(operation, err)
	}
	return snapshot, nil
}

func (s *Store) Load(ctx context.Context, name string) (circuitbreaker.Snapshot, error) {
	var micros int64
	var text sql.NullString
	err := s.db.QueryRowContext(ctx, s.queries.load, s.namespace, name).Scan(&micros, &text)
	var record circuitbreaker.Record
	if err == nil && text.Valid {
		record, err = decode(text.String)
	}
	if err != nil {
		return circuitbreaker.Snapshot{}, s.fail("load", err)
	}
	return record.Snapshot(time.UnixMicro(micros)), nil
}

func (s *Store) Report(ctx context.Context, name string, report circuitbreaker.Report) (circuitbreaker.Snapshot, error) {
	snapshot, err := s.update(ctx, "report", name, report.StateTTL, func(r circuitbreaker.Record, now time.Time) circuitbreaker.Record {
		return r.Report(now, report)
	})
	if last := s.lastCleanup.Load(); err == nil && snapshot.StoreTime.UnixMicro()-last >= cleanupInterval.Microseconds() && s.lastCleanup.CompareAndSwap(last, snapshot.StoreTime.UnixMicro()) {
		_, _ = s.Cleanup(ctx)
	}
	return snapshot, err
}

func (s *Store) AcquireProbe(ctx context.Context, name string, request circuitbreaker.ProbeRequest) (circuitbreaker.ProbeGrant, error) {
	granted := false
	snapshot, err := s.update(ctx, "acquire probe", name, request.StateTTL, func(r circuitbreaker.Record, now time.Time) circuitbreaker.Record {
		r, granted = r.AcquireProbe(now, request)
		return r
	})
	return circuitbreaker.ProbeGrant{Snapshot: snapshot, Granted: granted && err == nil}, err
}

func (s *Store) CompleteProbe(ctx context.Context, name string, result circuitbreaker.ProbeResult) (circuitbreaker.Snapshot, error) {
	return s.update(ctx, "complete probe", name, result.StateTTL, func(r circuitbreaker.Record, now time.Time) circuitbreaker.Record {
		return r.CompleteProbe(now, result)
	})
}

func (s *Store) Restore(ctx context.Context, name string, snapshot circuitbreaker.Snapshot) (circuitbreaker.Snapshot, error) {
	return s.update(ctx, "restore", name, restoreTTL, func(r circuitbreaker.Record, _ time.Time) circuitbreaker.Record {
		return r.Restore(snapshot)
	})
}
