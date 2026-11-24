package redisstore

import (
	"context"
	_ "embed"
	"fmt"
	"strconv"
	"time"

	"github.com/faultvane/faultvane-go"
	"github.com/faultvane/faultvane-go/ratelimit"
	"github.com/redis/go-redis/v9"
)

//go:embed scripts/ratelimit_acquire.lua
var rateLimitAcquireSource string

var rateLimitAcquireScript = redis.NewScript(rateLimitAcquireSource)

type RateLimitStore struct {
	store *Store
}

func NewRateLimitStore(client redis.Scripter, opts ...Option) (*RateLimitStore, error) {
	store, err := New(client, opts...)
	if err != nil {
		return nil, err
	}
	return &RateLimitStore{store: store}, nil
}

func (s *RateLimitStore) key(name string) string {
	return s.store.prefix + ":{" + s.store.namespace + ":" + name + "}:rl:bucket"
}

func parseRateLimitGrant(raw any) (ratelimit.Grant, error) {
	values, ok := raw.([]any)
	if !ok || len(values) != 2 {
		return ratelimit.Grant{}, fmt.Errorf("unexpected reply %v", raw)
	}
	tokens, err := integer(values[0])
	if err != nil {
		return ratelimit.Grant{}, fmt.Errorf("unexpected reply %v: %w", raw, err)
	}
	storeTime, err := integer(values[1])
	if err != nil {
		return ratelimit.Grant{}, fmt.Errorf("unexpected reply %v: %w", raw, err)
	}
	return ratelimit.Grant{Tokens: tokens, StoreTime: time.UnixMicro(storeTime)}, nil
}

func (s *RateLimitStore) Acquire(ctx context.Context, name string, request ratelimit.Acquire) (ratelimit.Grant, error) {
	raw, err := rateLimitAcquireScript.Run(ctx, s.store.client, []string{s.key(name)},
		request.Wanted, request.Returned, strconv.FormatFloat(request.Rate, 'g', -1, 64), request.Burst, millis(request.StateTTL),
	).Result()
	if err == nil {
		var grant ratelimit.Grant
		if grant, err = parseRateLimitGrant(raw); err == nil {
			return grant, nil
		}
	}
	return ratelimit.Grant{}, fmt.Errorf("redisstore: rate limit acquire: %w: %w", faultvane.ErrStoreUnavailable, err)
}
