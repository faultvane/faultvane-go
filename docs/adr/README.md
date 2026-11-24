# Architecture decision records

A decision record describes one important design decision.
It gives the context, the decision, the alternatives, and the consequences.
It also gives the conditions under which the team must examine the decision again.

| Number | Decision | Status |
|---|---|---|
| [0001](0001-local-decision-shared-aggregation.md) | Local decisions with batched shared aggregation | Accepted |
| [0002](0002-lua-scripts-and-generation.md) | Atomic transitions in Lua scripts with a generation | Accepted |
| [0003](0003-redis-server-time.md) | Redis server time for all shared timestamps | Accepted |
| [0004](0004-local-breaker-fallback.md) | Local breaker as the fallback | Accepted |
| [0005](0005-module-layout.md) | One repository with a dependency-free core module | Accepted |

## Add a record

1. Copy the structure of an existing record.
2. Use the next number.
3. Set the status to `Proposed`. Change it to `Accepted` when the owner approves it.
4. Do not change an accepted record. Write a new record that replaces it.
