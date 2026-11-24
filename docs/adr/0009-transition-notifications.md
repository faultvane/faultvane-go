# ADR 0009: Optional transition notifications as a signal to synchronize

Status: Proposed

## Context

An instance observes a transition of a different instance only at its next synchronization.
The delay is at most one sync interval. The default is 500 ms.
During this time, the instance can still send calls to an unhealthy dependency, and it can reject calls after the dependency recovered.

Some stores can tell a client about a change immediately. Redis has Pub/Sub.
ADR 0006 permits such a function only as an optional addition to an adapter. The core cannot depend on it.

The limits for a solution are:

- The store holds the authoritative state. The generation rules decide each conflict (ADR 0001, ADR 0002).
- The `Store` interface, the keys, and the stored data are commitments. A change can add to them. It cannot change them.
- Old and new library versions must run against the same store at the same time.
- A message of Redis Pub/Sub has no delivery guarantee. A subscriber that is not connected loses it.
- The request path makes no store operation in the `CLOSED` state.
- The store load must have a limit that does not depend on the number of events.
- The Redis adapter gets a `redis.Scripter`. This type cannot subscribe.

## Decision

1. **One optional interface in the core.** The core defines `Notifier` with one method: `Subscribe(ctx, name, changed) (stop, err)`. The `Store` interface does not change. A breaker examines its store when it starts. When the store implements `Notifier`, the synchronizer goroutine subscribes. `Close` stops the subscription.
2. **A notification is only a signal.** `changed` has no parameter. It wakes the synchronizer through the signal that a recorded failure uses. The synchronizer then sends a report or reads the state, as at each other synchronization. The breaker gets the state only from the response of the store.
3. **Correctness does not depend on a notification.** A notification that is lost, late, repeated, or false changes no state and no permit decision. The sync interval stays the upper limit of the propagation time.
4. **The early report interval limits the rate.** An instance makes at most one synchronization in each early report interval, for each number of notifications. The default is 50 ms.
5. **An instance in local mode ignores notifications.** It counts them and does nothing more. The health check of local mode keeps its increasing interval.
6. **A failed subscription is visible and has no other effect.** The breaker writes one `WARN` log and sets `Stats().SubscribeFailed`. It does not fail `New`, it fails and delays no call, and it does not subscribe again. `Stats().Notifications` counts the notifications.
7. **The Redis adapter publishes from its scripts.** With the new option `WithNotifications(subscriber)`, the adapter gives each script a fourth key, the channel name `<prefix>:{<namespace>:<name>}:cb:transitions`. A script that changes the generation publishes one message to this channel: the new generation as decimal text. This applies to the transitions T1 to T4 and to a `restore` that writes. The scripts publish with `redis.pcall`, so a message that Redis refuses does not fail the operation. Without the option, a script gets three keys and publishes nothing.
8. **`PUBLISH`, not `SPUBLISH`.** `SPUBLISH` needs Redis 7.0, and the minimum version of the adapter is Redis 6.2. In a Redis Cluster, `PUBLISH` reaches the subscribers on all nodes. The channel name has the hash tag of the keys, so a later change to `SPUBLISH` stays possible.
9. **One subscription for each breaker.** The adapter uses the subscription of the client library, with one goroutine of its own. The client library connects again after a connection loss.
10. **Only the Redis adapter has the function.** The in-memory, SQL, and MongoDB adapters do not implement `Notifier`.

## Alternatives

| Alternative | Reason for rejection |
|---|---|
| No notifications | This is the condition before the decision. It is correct and simple. But the propagation needs a median of approximately 300 ms with the default options, and all calls of that time can reach an unhealthy dependency. With notifications, the measured median is approximately 1 ms. The function is optional, so an application can keep this alternative. |
| Notifications that carry the state. The instance applies the state of the message and makes no store operation. | A message has no delivery guarantee and no order guarantee between connections. The instance would need the generation rules for messages also, as a second path to change its snapshot. A wrong or forged message could open or close a circuit. The gain is one small store operation for each instance and transition. Transitions are rare. |
| Redis keyspace notifications on the state key | They need the server setting `notify-keyspace-events`. The application frequently cannot change it on a managed Redis. The state key changes at each accepted report, because each report writes the fingerprint and the TTL. The instances would then get a notification for each report of each instance, not only for a transition. In a Redis Cluster, a keyspace notification stays on the node of the key. |
| A shorter sync interval | The store load increases in proportion for all instances at all times, not only at a transition. A sync interval of 5 ms gives 200 operations each second for each instance and breaker. The window rule also needs a bucket duration that is not shorter than the sync interval. |
| The core polls the store for a change of the generation at a short interval | It is a shorter sync interval with a different name. It has the same store load. |
| A `Publish` operation in the `Store` interface | It changes a commitment and forces each adapter to implement it. ADR 0006 rejected a low interface with `Publish`. The transition and the message would also be two operations and not atomic. |
| Notifications that are on for each user of the Redis adapter | The adapter gets a `redis.Scripter`, which cannot subscribe. A change of the parameter type of `New` breaks the API. Each subscription also holds one connection. That cost must be a decision of the application. |
| One shared subscription for all breakers of one store | It needs one connection for each instance, not one for each breaker. But the adapter must then keep a table of the breakers, with a lock and a start and stop sequence. The decision uses the smallest solution first. See "Examine again when". |
| `SPUBLISH` with sharded channels | It needs Redis 7.0. It would raise the minimum version of the adapter, or the scripts would need two paths. |

## Consequences

Positive:

- With the Redis adapter and the option on, an instance observes a transition of a different instance after approximately 1 ms in the usual condition, not after a part of the sync interval.
- The overshoot of calls to an unhealthy dependency during the propagation decreases in the same proportion.
- The state has one path into the instance: the response of a store operation. The generation rules, local mode, and reconciliation do not change.
- The `Store` interface, the keys, and the stored data do not change. An application that does not set the option gets the behavior from before.
- Old and new versions can use the same Redis. An old version ignores the channel.
- A different adapter can implement `Notifier` later without a change of the core.

Negative:

- For each transition, each instance with a subscription makes one store operation at the same time. For N instances, Redis gets N small operations in a short time. The instance that made the transition also makes one operation that changes nothing.
- When an instance synchronized less than one early report interval before a notification, it waits for the end of that interval. The measured maximum of the propagation time was 43 ms with the default 50 ms.
- Each breaker of each instance holds one more connection to Redis, and three goroutines.
- The adapter is no longer free of goroutines. The contract rule C12 still applies to the operations.
- The scripts have a new text. During a rolling deployment, Redis holds the old scripts and the new scripts.
- The function works fully only when all instances set the option. An instance without the option, and an old version, publish no message for their transitions.
- A wrapper around the store hides the interface `Notifier` unless it passes `Subscribe` to the store. `faultvaneotel.InstrumentStore` is such a wrapper. A breaker with an instrumented store gets no notifications until that wrapper passes the method.
- The channel name and the message are data in Redis that other tools can read. After version 1.0 they are a compatibility commitment.
- With Redis access control lists, the user needs a permission for the channels. Without it, no message goes out, and no signal shows this condition except the statistic `Notifications` that stays 0.
- In a Redis Cluster, `PUBLISH` sends each message to all nodes.
- The tests run against one standalone server and one server with cluster mode on. No test shows the delivery between two nodes of a cluster.
- A breaker does not subscribe again after `Subscribe` returned an error. The Redis adapter returns no error for a connection fault, because the client library connects again. A different adapter can need a different rule.

## Examine again when

- The number of connections for subscriptions is a problem for a Redis. A possible change is one shared subscription for all breakers of one store.
- The store operations of all instances at the same time after a transition are a problem. A possible change is a random delay before the synchronization.
- The minimum version of the adapter becomes Redis 7.0 or later. `SPUBLISH` then keeps a message in one shard.
- A user needs notifications for the SQL adapter or the MongoDB adapter, for example with `LISTEN` and `NOTIFY` or with change streams.
- An operator needs a metric for the notifications. The OpenTelemetry module can then export `Notifications` and `SubscribeFailed`.
