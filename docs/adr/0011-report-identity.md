# ADR 0011: A report has an identity, and the store applies it one time

Status: Proposed

## Context

An instance sends a failed report again ([ADR 0010](0010-resend-failed-reports.md)).
The instance does not know if the store applied a failed report.
When the store applied it and the response did not arrive, the store added the same counts again with the next report.

ADR 0010 accepted this limit for a correction in a minor version:

- With the default fault threshold of 3, the store added one result at most 3 times before the instance was in local mode, and one more time for each report of a reconciliation.
- A failure count threshold and the minimum number of calls were then reached too early. The circuit of a healthy dependency could open.

ADR 0010 names the exact solution: an identity in the report. The owner selected this work item on 2026-11-28.

The limits for a solution are:

- The `Store` interface and the stored data are commitments. Versions 0.1.0 and 0.2.0 are released.
- Old and new versions of the library use the same store during a deployment.
- The request path makes no store operation and no allocation.
- The number of store operations for each synchronization keeps its limit of one.
- The memory of the stored data has a limit.

## Decision

1. **A report has an identity.** The type `Report` has two new fields: `Instance` and `Sequence`. `Instance` is a random 64-bit number that a breaker selects when it starts. `Sequence` starts at 1 and increases by 1 for each new report of that breaker. A sequence of 0 means that the report has no identity.
2. **The store applies a report with an identity one time.** For each instance, the store keeps the sequence of the last report that it applied. A report with a sequence that is equal to or lower than this value changes nothing. The store returns the current state, as for each event that it ignores. Rule C14 of the [store contract](../stores/README.md) states this. The conformance suite has checks for it.
3. **The store forgets an instance with its counts.** The store keeps the entry of an instance while the counts of its last report can be in the shared window. A transition and a restore remove all entries, together with the window. The number of entries is then at most the number of instances that sent a report in one window size.
4. **An instance sends a failed report again without a change.** The instance keeps a failed report as the unconfirmed report: the same counts, the same generation, the same sequence. The next synchronization sends this report again, and no other report. New results stay in the pending counts. When the store confirms the unconfirmed report, the synchronizer gets the signal for an early report, and the next synchronization sends the pending counts with the next sequence. This replaces point 1 of ADR 0010, which put the counts back into the pending counts.
5. **An instance discards an unconfirmed report in two conditions.** The first condition: the instance has a different generation than the report (GEN-3). The second condition: the report is as old as the window size or older. The counts then left the shared window, or they must not enter it.
6. **The reconciliation uses the same rule.** A reconciliation first sends the unconfirmed report again, if one exists. Its pending counts get the limit of the last local `CLOSED` window, as before, and go with the next synchronization. A reconciliation without an unconfirmed report sends the limited pending counts as a new report.
7. **The adapters written in Go get the rule through `Record`.** The type `Record` has the new field `Reporters`. The method `Record.Report` contains the rule. The in-memory, SQL, and MongoDB adapters need no change of their code.
8. **The Redis adapter keeps the entries in a fourth key.** The key `<prefix>:{<namespace>:<name>}:cb:reporters` is a hash. The field is the instance number, and the value is the bucket and the sequence of its last report. The report script removes old entries one time for each new bucket, not for each report.

## Alternatives

| Alternative | Reason for rejection |
|---|---|
| Keep the limit of ADR 0010 | The circuit of a healthy dependency can open when the store is slow. This is the only known error of the shared decision. |
| A random identity for each report, and the store keeps each identity for one window size | The store then keeps one entry for each report, not for each instance. With 1000 instances, a sync interval of 1 s, and a window of 60 s, that is 60 000 entries for one breaker. The SQL and MongoDB adapters write the full record with each report. |
| Cumulative totals: each report carries the totals of the instance since its start, and the store adds the difference | A report is then idempotent without a sequence, and a failed report needs no special rule. But the store must keep the totals of an instance for the full life of a generation. After a loss of the entry, the store adds the full totals of the instance. The limit of the reconciliation (RC-5) also has no clear meaning for totals. |
| Send the kept counts and the new counts in one report, with the identities of all reports before | The store must then find which of the earlier reports it applied. The report grows with each failed report. |
| Send the unconfirmed report and the pending counts in one synchronization, as two store operations | The second operation shares the store timeout with the first. With a slow store, the second operation fails after the first was successful. The fault count then increases although the store answered. |
| Add the identity to the fields of the window hash in Redis | A script of version 0.2.0 fails when it reads a window field that it does not know. A separate key has no effect on an old script. |

## Consequences

Positive:

- A result reaches the shared window at most one time, also when the response of a report does not arrive. The worst case of ADR 0010 (3 times, and one more time for each reconciliation) no longer applies to a store that satisfies rule C14.
- A late copy of a report also changes nothing, because the store compares the sequence with "equal or lower".
- The `Store` interface has the same methods. A store that ignores the new fields compiles and behaves as in version 0.2.0.
- The request path does not change. Each synchronization still makes one store operation.
- The SQL and MongoDB adapters need no migration. A record without `Reporters` is valid.

Negative:

- After a failed report, new results wait for one more synchronization. The signal for an early report limits this time to the early report interval.
- The stored data grows by one entry for each instance that sent a report in the last window size. An entry in the JSON record has about 70 bytes. With 1000 instances that is about 70 kB in each write of the SQL and MongoDB adapters.
- The Redis adapter uses four keys for each breaker, not three. The report script reads all entries of the reporters hash one time for each bucket duration.
- A third-party store must implement rule C14 to pass the conformance suite. Without it, the store has the limit of ADR 0010.
- Two instances with the same instance number hide the reports of each other. The number is random with 64 bits, so this is not probable.
- An instance that restarts gets a new instance number. A report that it sent before the restart is not sent again, so no count is added two times.

Mixed versions during a deployment:

- An instance of version 0.2.0 sends no identity. The store adds each of its reports, as before. The limit of ADR 0010 stays for these instances until they have the new version.
- Redis: a script of version 0.2.0 does not know the reporters key. It does not delete the key at a transition and does not set its TTL. The entries that stay cause no error: the sequence of an instance only increases, and the next report in a new bucket removes old entries. A new script does not read data that an old script cannot write.
- SQL and MongoDB: an instance of version 0.2.0 writes the record without the field `Reporters`. The entries are then lost, and a report that is sent again after that write is added a second time. This is the behavior of version 0.2.0. It ends when all instances have the new version.
- A rollback to version 0.2.0 needs no change of the data.

## Examine again when

- The number of instances for one breaker makes the record of the SQL or MongoDB adapter too large. A separate table or collection for the entries is then the next step.
- A profile shows the scan of the reporters hash in the Redis report script.
- A version 2 of the stored data starts.
