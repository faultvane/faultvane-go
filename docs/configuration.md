# Configuration

All options have defaults.
`New` validates the configuration and fails immediately when it is not valid.

## 1. Breaker options

### Failure detection

| Option | Default | Meaning |
|---|---|---|
| `FailureRateThreshold` | `0.5` | The circuit opens when the failure rate of the window is equal to or larger than this value. Range: above 0, at most 1. |
| `MinimumCalls` | `20` | The rate thresholds apply only when the window has this number of calls or more. |
| `FailureThreshold` | Off | The circuit opens when the number of failures in the window is equal to or larger than this value. |
| `SlowCallDuration` | Off | A call with this duration or longer is a slow call. |
| `SlowCallRateThreshold` | Off | The circuit opens when the slow call rate of the window is equal to or larger than this value. |
| `WindowSize` | `10s` | The duration of the sliding window. |
| `WindowBuckets` | `10` | The number of buckets in the window. |
| `Classifier` | Default rule | The function that classifies the result of a call. |

### Open and half-open

| Option | Default | Meaning |
|---|---|---|
| `OpenDuration` | `30s` | The time in `OPEN` before a probe is possible. |
| `HalfOpenMaxProbes` | `1` | The maximum number of probes at one time, for all instances together. |
| `HalfOpenSuccessThreshold` | Equal to `HalfOpenMaxProbes` | The number of probe successes that closes the circuit. |
| `ProbeTimeout` | `30s` | The duration of a probe lease. |
| `ProbeAttemptInterval` | `100ms` | The minimum time between two probe requests of one instance. |

### Local mode

| Option | Default | Meaning |
|---|---|---|
| `LocalFallbackEnabled` | `true` | When `false`, a store fault applies the `LocalModeExpiredPolicy` immediately. |
| `MaxLocalFallbackDuration` | `5m` | The maximum time in local mode. |
| `LocalModeExpiredPolicy` | `KeepLocalBreaker` | The behavior after the maximum time: `KeepLocalBreaker`, `RejectAll`, or `AllowAll`. |
| `StartPolicy` | `StartWithProbe` | The behavior when the first read fails: `StartWithProbe`, `StartClosed`, or `StartRejecting`. |
| `LocalFailureThreshold` | Equal to `FailureThreshold` | The failure count threshold in local mode. |
| `LocalMinimumCalls` | Equal to `MinimumCalls` | The minimum number of calls in local mode. |

### Integration

| Option | Default | Meaning |
|---|---|---|
| `Store` | None | The shared store. Without a store, the breaker is a local breaker. |
| `Observer` | None | The receiver of events and metrics. |
| `Logger` | `slog.Default()` | The logger for transitions and faults. |

### Synchronization

These options apply to a breaker with a store. They are the same for each adapter.

| Option | Default | Meaning |
|---|---|---|
| `StoreTimeout` | `100ms` | The time limit of each store operation. |
| `SyncInterval` | `500ms` | The interval of reports and snapshot refreshes. |
| `EarlyReportInterval` | `50ms` | The minimum time between two reports that a failure causes. |
| `FaultThreshold` | `3` | The number of store faults in sequence that starts local mode. |
| `StartTimeout` | `1s` | The time limit of the first read. |
| `StateTTL` | `1h` | The time after which the store removes the data of a breaker without traffic. |

## 2. Adapter options

Each adapter has its own options. The namespace is an option of each adapter.

### Redis adapter

| Option | Default | Meaning |
|---|---|---|
| `Namespace` | Required | The scope of the breaker names, for example `production`. |
| `KeyPrefix` | `faultvane` | The first part of each key. |

### In-memory adapter

The in-memory adapter has no options.

## 3. Names from the requirements

| Requirement name | Option |
|---|---|
| `failureThreshold` | `FailureThreshold` |
| `failureRateThreshold` | `FailureRateThreshold` |
| `minimumNumberOfCalls` | `MinimumCalls` |
| `slidingWindowSize` | `WindowSize` and `WindowBuckets` |
| `openStateDuration` | `OpenDuration` |
| `halfOpenMaxProbes` | `HalfOpenMaxProbes` |
| `probeTimeout` | `ProbeTimeout` |
| `redisTimeout` | `StoreTimeout` |
| `redisConnectionTimeout` | A setting of the store client that the application supplies |
| `localFallbackEnabled` | `LocalFallbackEnabled` |
| `maxLocalFallbackDuration` | `MaxLocalFallbackDuration` |
| `stateRefreshInterval` | `SyncInterval` |
| `stateTTL` | `StateTTL` |

## 4. Validation rules

`New` returns `faultvane.ErrInvalidConfig` when one of these rules is not satisfied:

| Rule | Reason |
|---|---|
| The name and the namespace contain only `a-z`, `A-Z`, `0-9`, `.`, `_`, `-`, and have 1 to 64 characters. | Keys cannot collide. Metric labels stay small. |
| At least one failure condition is on. | A breaker that cannot open gives no protection. |
| Each rate threshold is above 0 and at most 1. | Other values have no meaning. |
| `MinimumCalls`, `FailureThreshold`, `WindowBuckets`, `HalfOpenMaxProbes` are 1 or more. | Other values have no meaning. |
| `SlowCallRateThreshold` and `SlowCallDuration` are set together. | One without the other has no effect. |
| `HalfOpenSuccessThreshold` is 1 or more. | The circuit must be able to close. |
| All durations are above 0. | Other values have no meaning. |
| `WindowSize / WindowBuckets` is equal to or longer than `SyncInterval`. | A report must go into the correct bucket. |
| `EarlyReportInterval` is not longer than `SyncInterval`. | An early report must be earlier. |
| `StoreTimeout` is shorter than `SyncInterval`. | One slow operation must not delay the next cycle. |
| `StateTTL` is at least `2 × (OpenDuration + WindowSize)`. | The TTL must not delete a state in use. |
| `LocalModeExpiredPolicy` is not `KeepLocalBreaker` when `LocalFallbackEnabled` is `false`. | The two values disagree. |

## 5. Guidance

- Start with the defaults. Change a value only when a measurement shows a problem.
- Set `OpenDuration` to the usual recovery time of the dependency.
- Set `ProbeTimeout` longer than the longest possible call. Use a timeout policy to limit the call.
- A lower `SyncInterval` gives faster propagation and more store load. [Architecture](architecture.md) gives the formula.
- Use the same configuration for one breaker name in all instances. The library reports a difference. See [failure detection](circuit-breaker/failure-detection.md).
