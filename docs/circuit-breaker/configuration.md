# Configuration

All options have defaults.
`New` validates the configuration and fails immediately when it is not valid.

Status: all options of this document are implemented.

Each option is a function of the package `circuitbreaker`. The application gives the options to `New`:

```go
breaker, err := circuitbreaker.New("payment-service",
    circuitbreaker.WithFailureRateThreshold(0.5),
    circuitbreaker.WithOpenDuration(30*time.Second),
)
```

## 1. Breaker options

### Failure detection

| Option | Default | Meaning |
|---|---|---|
| `WithFailureRateThreshold(rate)` | `0.5` | The circuit opens when the failure rate of the window is equal to or larger than this value. Range: above 0, at most 1. This condition is always on. With the value `1`, it applies only when all calls fail. |
| `WithMinimumCalls(calls)` | `20` | The rate thresholds apply only when the window has this number of calls or more. |
| `WithFailureThreshold(failures)` | Off | The circuit opens when the number of failures in the window is equal to or larger than this value. |
| `WithSlowCallDuration(d)` | Off | A call with this duration or longer is a slow call. |
| `WithSlowCallRateThreshold(rate)` | Off | The circuit opens when the slow call rate of the window is equal to or larger than this value. |
| `WithWindowSize(d)` | `10s` | The duration of the sliding window. |
| `WithWindowBuckets(buckets)` | `10` | The number of buckets in the window. |
| `WithClassifier(classifier)` | `DefaultClassifier` | The function that classifies the result of a call. |

### Open and half-open

| Option | Default | Meaning |
|---|---|---|
| `WithOpenDuration(d)` | `30s` | The time in `OPEN` before a probe is possible. |
| `WithHalfOpenMaxProbes(probes)` | `1` | The maximum number of probes at one time, for all instances together. |
| `WithHalfOpenSuccessThreshold(successes)` | Equal to the maximum number of probes | The number of probe successes that closes the circuit. |
| `WithProbeTimeout(d)` | `30s` | The duration of a probe lease. |
| `WithProbeAttemptInterval(d)` | `100ms` | The minimum time between two probe requests of one instance to the store. The value `0` turns this limit off. |

### Local mode

| Option | Default | Meaning |
|---|---|---|
| `WithLocalFallback(enabled)` | `true` | When `false`, a change to local mode applies the expired policy immediately. |
| `WithMaxLocalFallbackDuration(d)` | `5m` | The maximum time in local mode. |
| `WithLocalModeExpiredPolicy(policy)` | `KeepLocalBreaker` | The behavior after the maximum time: `KeepLocalBreaker`, `RejectAll`, or `AllowAll`. |
| `WithStartPolicy(policy)` | `StartWithProbe` | The behavior when the first read fails: `StartWithProbe`, `StartClosed`, or `StartRejecting`. |
| `WithLocalFailureThreshold(failures)` | Equal to the failure count threshold | The failure count threshold for the local window. |
| `WithLocalMinimumCalls(calls)` | Equal to the minimum calls | The minimum number of calls for the local window. |

A breaker without a store always uses the local window. When the two local values are set, such a breaker uses them.

### Integration

| Option | Default | Meaning |
|---|---|---|
| `WithStore(store)` | None | The shared store. Without a store, the breaker is a local breaker. |
| `WithObserver(observer)` | None | The receiver of events. |
| `WithLogger(logger)` | `slog.Default()` | The logger for state changes and faults. |
| `WithClock(clock)` | `faultvane.SystemClock()` | The clock of the instance. Tests use it. |

### Synchronization

These options apply to a breaker with a store. They are the same for each adapter.

| Option | Default | Meaning |
|---|---|---|
| `WithStoreTimeout(d)` | `100ms` | The time limit of each store operation. |
| `WithSyncInterval(d)` | `500ms` | The interval of reports and snapshot refreshes. |
| `WithEarlyReportInterval(d)` | `50ms` | The minimum time between two synchronizations when a failure causes an early report. |
| `WithFaultThreshold(faults)` | `3` | The number of store faults in sequence that starts local mode. |
| `WithStartTimeout(d)` | `1s` | The time limit of the first read in `New`. |
| `WithStateTTL(d)` | `1h` | The time after which the store can remove the data of a breaker without traffic. See the effective value below. |

### Effective state TTL

The breaker does not validate the state TTL. It calculates an effective value:

```text
effective state TTL = max(StateTTL, 2 × (OpenDuration + WindowSize))
```

The breaker sends the effective value to the store with each report and each probe operation.
A low or negative `StateTTL` is not an error. The breaker then uses the second term.
With the defaults, the effective value is 1 hour.

## 2. Adapter options

Each adapter has its own options. The namespace is a required option of each adapter that shares state.
The `New` function of an adapter validates its options and returns an error that matches `faultvane.ErrInvalidConfig`.

### Redis adapter

```go
store, err := redisstore.New(client, redisstore.WithNamespace("production"))
```

| Option | Default | Meaning |
|---|---|---|
| `WithNamespace(namespace)` | Required | The scope of the breaker names, for example `production`. |
| `WithKeyPrefix(prefix)` | `faultvane` | The first part of each key. |

### SQL adapter

```go
store, err := sqlstore.New(db, sqlstore.PostgreSQL, sqlstore.WithNamespace("production"))
```

The second argument is the dialect: `sqlstore.PostgreSQL` or `sqlstore.MySQL`.

| Option | Default | Meaning |
|---|---|---|
| `WithNamespace(namespace)` | Required | The scope of the breaker names. |
| `WithTable(table)` | `faultvane_circuit_breakers` | The name of the table. |

### MongoDB adapter

```go
store, err := mongostore.New(collection, mongostore.WithNamespace("production"))
```

| Option | Default | Meaning |
|---|---|---|
| `WithNamespace(namespace)` | Required | The scope of the breaker names. |

### In-memory adapter

```go
store := circuitbreaker.NewMemoryStore(faultvane.SystemClock())
```

The in-memory adapter has no options and no namespace. Its argument is the clock of the store.

## 3. Names from the requirements

| Requirement name | Option |
|---|---|
| `failureThreshold` | `WithFailureThreshold` |
| `failureRateThreshold` | `WithFailureRateThreshold` |
| `minimumNumberOfCalls` | `WithMinimumCalls` |
| `slidingWindowSize` | `WithWindowSize` and `WithWindowBuckets` |
| `openStateDuration` | `WithOpenDuration` |
| `halfOpenMaxProbes` | `WithHalfOpenMaxProbes` |
| `probeTimeout` | `WithProbeTimeout` |
| `redisTimeout` | `WithStoreTimeout` |
| `redisConnectionTimeout` | A setting of the store client that the application supplies |
| `localFallbackEnabled` | `WithLocalFallback` |
| `maxLocalFallbackDuration` | `WithMaxLocalFallbackDuration` |
| `stateRefreshInterval` | `WithSyncInterval` |
| `stateTTL` | `WithStateTTL` |

## 4. Validation rules

`New` returns an error that matches `faultvane.ErrInvalidConfig` when one of these rules is not satisfied.
The message names the option without the prefix `With`, for example `OpenDuration must be above 0`.

### Rules for each breaker

| Rule | Reason |
|---|---|
| The name contains only `a-z`, `A-Z`, `0-9`, `.`, `_`, `-`, and has 1 to 64 characters. | Keys cannot collide. Metric attributes stay small. |
| The failure rate threshold is above 0 and at most 1. | Other values have no meaning. |
| The minimum calls value is 1 or more. | Other values have no meaning. |
| The failure count threshold is 1 or more, when it is set. | Other values have no meaning. |
| The slow call duration and the slow call rate threshold are set together. | One without the other has no effect. |
| The slow call duration is above 0. The slow call rate threshold is above 0 and at most 1. | Other values have no meaning. |
| The number of window buckets is 1 or more. | A window needs a bucket. |
| The window size gives each bucket a duration above 0. | A bucket needs a duration. |
| The open duration is above 0. | Other values have no meaning. |
| The maximum number of probes is 1 or more. The half-open success threshold is 1 or more. | The circuit must be able to close. |
| The probe timeout is above 0. | A lease needs a duration. |
| The probe attempt interval is 0 or more. | A negative interval has no meaning. |
| The classifier, the clock, and the logger are not `nil`. | The breaker cannot work without them. |
| The observer is not `nil`, when it is set. | The breaker calls it. |
| The local failure count threshold is 1 or more, when it is set. The local minimum calls value is 1 or more. | Other values have no meaning. |

### More rules for a breaker with a store

`New` applies these rules only when the application sets `WithStore`.

| Rule | Reason |
|---|---|
| The store is not `nil`. | The option has no meaning without a store. |
| The store timeout is above 0. | Each store operation needs a limit. |
| The sync interval is longer than the store timeout. | One slow operation must not delay the next cycle. |
| The early report interval is above 0 and not longer than the sync interval. | An early report must be earlier. |
| The window size divided by the number of buckets is equal to or longer than the sync interval. | A report must go into the correct bucket. |
| The fault threshold is 1 or more. | Other values have no meaning. |
| The start timeout is above 0. | The first read needs a limit. |
| The maximum local fallback duration is above 0. | Other values have no meaning. |
| The expired policy is `KeepLocalBreaker`, `RejectAll`, or `AllowAll`. The start policy is `StartWithProbe`, `StartClosed`, or `StartRejecting`. | Other values are not defined. |
| The expired policy is not `KeepLocalBreaker` when the local fallback is off. | The two values disagree. |

The state TTL has no validation rule. Section 1 gives its effective value.

### Rules of the adapters

| Adapter | Rule |
|---|---|
| Redis | The client is not `nil`. The namespace and the key prefix contain only `a-z`, `A-Z`, `0-9`, `.`, `_`, `-`, and have 1 to 64 characters. |
| SQL | The database handle is not `nil`. The dialect is `PostgreSQL` or `MySQL`. The namespace has the same rule as for Redis. The table name contains only `a-z`, `A-Z`, `0-9`, `_`, and has 1 to 64 characters. |
| MongoDB | The collection is not `nil`. The namespace has the same rule as for Redis. |

## 5. Guidance

- Start with the defaults. Change a value only when a measurement shows a problem.
- Set the open duration to the usual recovery time of the dependency.
- Set the probe timeout longer than the longest possible call. Use a timeout policy to limit the call.
- A lower sync interval gives faster propagation and more store load. [Architecture](architecture.md) gives the formula.
- Use the same configuration for one breaker name in all instances. The library does not detect a difference. See [failure detection](failure-detection.md).
