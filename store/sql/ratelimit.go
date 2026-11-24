package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/faultvane/faultvane-go/ratelimit"
)

const defaultRateLimitTable = "faultvane_rate_limits"

type rateLimitQueries struct {
	schema string
	insert string
	lock   string
	update string
}

type RateLimitStore struct {
	store   *Store
	queries rateLimitQueries
}

func NewRateLimitStore(db *sql.DB, dialect Dialect, opts ...Option) (*RateLimitStore, error) {
	store, err := New(db, dialect, append([]Option{WithTable(defaultRateLimitTable)}, opts...)...)
	if err != nil {
		return nil, err
	}
	insert := "INSERT INTO %[1]s (namespace, name, tokens, refilled_at, expires_at) VALUES (?, ?, ?, %[2]s, %[2]s + ?) ON CONFLICT (namespace, name) DO NOTHING"
	if dialect == MySQL {
		insert = "INSERT IGNORE INTO %[1]s (namespace, name, tokens, refilled_at, expires_at) VALUES (?, ?, ?, %[2]s, %[2]s + ?)"
	}
	q := rateLimitQueries{
		schema: "CREATE TABLE IF NOT EXISTS %[1]s (namespace VARCHAR(64) NOT NULL, name VARCHAR(64) NOT NULL, tokens DOUBLE PRECISION NOT NULL, refilled_at BIGINT NOT NULL, expires_at BIGINT NOT NULL, PRIMARY KEY (namespace, name))",
		insert: insert,
		lock:   "SELECT tokens, refilled_at, %[2]s FROM %[1]s WHERE namespace = ? AND name = ? FOR UPDATE",
		update: "UPDATE %[1]s SET tokens = ?, refilled_at = ?, expires_at = ? WHERE namespace = ? AND name = ?",
	}
	for _, query := range []*string{&q.schema, &q.insert, &q.lock, &q.update} {
		*query = store.placeholders(fmt.Sprintf(*query, store.table, store.now()))
	}
	return &RateLimitStore{store: store, queries: q}, nil
}

func (s *RateLimitStore) Schema() string { return s.queries.schema }

func (s *RateLimitStore) Migrate(ctx context.Context) error {
	if _, err := s.store.db.ExecContext(ctx, s.queries.schema); err != nil {
		return s.store.fail("migrate", err)
	}
	return nil
}

func (s *RateLimitStore) Cleanup(ctx context.Context) (int64, error) {
	return s.store.Cleanup(ctx)
}

func (s *RateLimitStore) change(ctx context.Context, name string, request ratelimit.Acquire) (ratelimit.Grant, bool, error) {
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return ratelimit.Grant{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var tokens float64
	var refilledAt, now int64
	if err = tx.QueryRowContext(ctx, s.queries.lock, s.store.namespace, name).Scan(&tokens, &refilledAt, &now); err != nil {
		return ratelimit.Grant{}, errors.Is(err, sql.ErrNoRows), err
	}
	bucket, grant := ratelimit.Bucket{Tokens: tokens, At: time.UnixMicro(refilledAt)}.Acquire(time.UnixMicro(now), request)
	if _, err = tx.ExecContext(ctx, s.queries.update, bucket.Tokens, bucket.At.UnixMicro(), bucket.At.Add(request.StateTTL).UnixMicro(), s.store.namespace, name); err != nil {
		return ratelimit.Grant{}, false, err
	}
	return grant, false, tx.Commit()
}

func (s *RateLimitStore) Acquire(ctx context.Context, name string, request ratelimit.Acquire) (ratelimit.Grant, error) {
	grant, missing, err := s.change(ctx, name, request)
	if missing {
		if _, err = s.store.db.ExecContext(ctx, s.queries.insert, s.store.namespace, name, float64(request.Burst), request.StateTTL.Microseconds()); err == nil {
			grant, _, err = s.change(ctx, name, request)
		}
	}
	if err != nil {
		return ratelimit.Grant{}, s.store.fail("rate limit acquire", err)
	}
	s.store.cleanupAt(ctx, grant.StoreTime)
	return grant, nil
}
