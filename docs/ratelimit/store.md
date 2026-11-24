# Rate limit store

This document describes the store of the shared rate limit.
The [rate limiter guide](README.md) describes how a limiter uses the store.
The [requirements](requirements.md) RL-20 to RL-41 apply.

After version 1.0, the interface, its contract, and the data layout of the Redis store are compatibility commitments.

## 1. Stores

| Store | Package | Use |
|---|---|---|
| In-memory | `github.com/faultvane/faultvane-go/ratelimit` | Tests, and limiters in one process. It is the reference implementation. |
| Redis | `github.com/faultvane/faultvane-go/store/redis` (package `redisstore`) | Instances in many processes. |

No other store exists.

## 2. Interface

```go
type Store interface {
    Acquire(ctx context.Context, name string, request Acquire) (Grant, error)
}

type Acquire struct {
    Wanted   int64
    Returned int64
    Rate     float64
    Burst    int64
    StateTTL time.Duration
}

type Grant struct {
    Tokens    int64
    StoreTime time.Time
}
```

| Field | Content |
|---|---|
| `Acquire.Wanted` | The number of tokens that the limiter asks for. |
| `Acquire.Returned` | The number of tokens that the limiter gives back. |
| `Acquire.Rate` | The number of tokens that the shared bucket gets each second. |
| `Acquire.Burst` | The largest number of tokens that the shared bucket can hold. |
| `Acquire.StateTTL` | The time after which the store can remove a bucket that no limiter uses. |
| `Grant.Tokens` | The number of tokens that the store gives to the limiter. |
| `Grant.StoreTime` | The time of the store clock in this operation. |

The store keeps one bucket for each name. The bucket has two values: the number of tokens and the time of the last operation.

One `Acquire` operation does these steps as one atomic change:

1. It reads the time of the store clock. When this time is before the time of the last operation, it uses the time of the last operation.
2. It adds `Rate` multiplied by the elapsed seconds to the tokens.
3. It adds `Returned` to the tokens.
4. It decreases the tokens to `Burst` when they are above `Burst`.
5. It grants the smaller of `Wanted` and the whole tokens of the bucket. It grants 0 when `Wanted` is below 0.
6. It removes the granted tokens from the bucket and stores the time.

The interface has one operation because the limiter needs one atomic step: give back, refill, and take.
The limiter sends the rate and the burst in each operation. The store keeps no configuration.

The limiter sets `StateTTL` to the time that an empty bucket needs to become full, plus one minute.
A bucket that the store removes after this time is equal to a full bucket. Thus the removal changes no result.

## 3. Contract

A store must satisfy these rules.

| ID | Rule |
|---|---|
| RS1 | **Atomic.** Each operation applies all its changes or none. Concurrent operations on one name never grant more tokens in total than the bucket holds. They also lose no token. |
| RS2 | **Full start.** A name without state has a full bucket. The bucket holds `Burst` tokens. |
| RS3 | **Grant.** The grant is never above `Wanted`, never above the whole tokens of the bucket, and never below 0. |
| RS4 | **One clock.** The bucket refills by the clock of the store, at `Rate` tokens each second, up to `Burst`. Each response contains the time of this clock. The time does not go back. |
| RS5 | **Returned tokens.** The store adds the returned tokens before it calculates the grant. The bucket never holds more than `Burst`. |
| RS6 | **Isolation.** Two names never share data. |
| RS7 | **Context.** An operation stops when its context ends. It returns an error and changes nothing. |
| RS8 | **Errors.** A fault of the store returns an error that matches `faultvane.ErrStoreUnavailable`. |
| RS9 | **Cleanup.** The data of a name that no limiter uses does not stay forever. The store keeps the data at least for the time `StateTTL` after the last operation. |

The in-memory store does not satisfy RS9. It keeps one small record for each name until the process ends.

## 4. Conformance suite

The package `ratelimit/storetest` contains one test suite for the contract.

```go
func TestRateLimitConformance(t *testing.T) {
    client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6399"})
    t.Cleanup(func() { _ = client.Close() })

    storetest.Run(t, func(t *testing.T) ratelimit.Store {
        namespace := fmt.Sprintf("test-%d", time.Now().UnixNano())
        store, err := redisstore.NewRateLimitStore(client, redisstore.WithNamespace(namespace))
        if err != nil {
            t.Fatal(err)
        }
        return store
    })
}
```

`Run` calls the function one time for each check. Each check uses its own name.
The name of each check starts with the contract rule that it proves, for example `RS4`.

The suite has checks for RS1 to RS7:

- The full start, the limits of a grant, the refill by the store time, the limit of the burst, the returned tokens, the isolation of names, the store time, and an ended context.
- A concurrency check: 32 concurrent operations get exactly the tokens of the bucket.

The suite cannot control the clock of a real store. The refill check waits 20 ms and then compares the grant with the store time of the two responses.

The suite has no check for RS8 and RS9. The tests of each adapter check these rules.

The tests of the suite run it against the in-memory store and against 14 broken stores. Each broken store must fail a check, and each check must reject a broken store.

## 5. Redis store

```go
store, err := redisstore.NewRateLimitStore(client, redisstore.WithNamespace("production"))
```

`NewRateLimitStore` accepts each client of `go-redis` that can run scripts.
It uses the options of the Redis adapter of the circuit breaker: `WithNamespace` is required, and `WithKeyPrefix` has the default `faultvane`.
It returns an error that matches `faultvane.ErrInvalidConfig` when the client is `nil`, or when the namespace or the key prefix is not valid.

### Key

Each name uses one key:

| Key | Type |
|---|---|
| `<prefix>:{<namespace>:<name>}:rl:bucket` | Hash |

Example: `faultvane:{production:payment-api}:rl:bucket`

The braces are a hash tag. The circuit breaker keys of the same namespace and name are in the same hash slot.

| Field | Content |
|---|---|
| `v` | The version of the layout. The value is `1`. |
| `tk` | The number of tokens. The value can have a fraction. |
| `ts` | The time of the last operation, in microseconds of Unix time, from the Redis clock. |

### Script

The file `store/redis/scripts/ratelimit_acquire.lua` contains the one script of the store.
Redis runs a script as one atomic operation.

1. The script reads the time with the Redis command `TIME`.
2. It reads `tk` and `ts`. When the key does not exist, the bucket is full and `ts` is the current time.
3. It does the steps of section 2.
4. It writes `v`, `tk`, and `ts`, and sets the time to live of the key with `PEXPIRE` to `StateTTL`.
5. It returns the grant and the time as text. Text keeps all digits of a large number.

The store sends the script again when Redis lost its script cache.

### Errors

A fault of Redis and a reply that is not valid return an error of this form:

```text
redisstore: rate limit acquire: faultvane: store unavailable: <cause>
```

The error matches `faultvane.ErrStoreUnavailable`. It also matches the error of the context when the context ended.

### Limits

- The tests use one Redis server without cluster mode and one Redis server with cluster mode on. The server with cluster mode owns all hash slots. The conformance suite and the key test use it through a cluster client.
- No test uses a cluster with many nodes, a move of a hash slot, or a failover. The store uses one key for each name, so each operation is in one hash slot.
- After a failover to a replica that is behind, the bucket can hold older values. The bucket then holds at most `Burst` tokens. The guarantee of the guide starts again from that time.
- When the Redis clock goes back, the bucket gets no tokens until the clock reaches the stored time again.
