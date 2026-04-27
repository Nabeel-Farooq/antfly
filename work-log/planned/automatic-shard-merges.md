# Automatic Shard Merges

## Context

Antfly already has:

- Automatic shard splits driven by `MaxShardSizeBytes`
- An empty-shard merge path
- A real zero-downtime split state machine with replicated `SplitState`

What it does **not** have yet is a correct online merge path for **non-empty under-utilized shards**.

Today, merge planning only selects shards marked `Empty`, and merge execution is effectively:

1. Rewrite metadata to remove the donor shard
2. Stop the donor shard
3. Call `MergeRange` on the survivor

`MergeRange` currently just changes the survivor's byte range. It does not move donor data. That is only correct when the donor shard is actually empty.

## What We Decided

### 1. Add a minimum-size merge knob

We should add `MinShardSizeBytes`.

This is the main missing policy knob and gives Antfly the same basic split/merge shape as CockroachDB's `range_max_bytes` / `range_min_bytes`.

`MaxShardSizeBytes` remains the split trigger.
`MinShardSizeBytes` becomes the merge eligibility trigger.

### 2. Reuse some existing automatic split knobs, but not all of them

We should reuse:

- `DisableShardAlloc`
- `ShardCooldownPeriod`
- The new split budgeting knobs, but generalized so splits and merges share a single range-transition budget

That means `AutoSplitPerTableLimit` / `AutoSplitClusterLimit` should eventually become something like:

- `AutoRangeTransitionPerTableLimit`
- `AutoRangeTransitionClusterLimit`

We should **not** reuse split-specific timing knobs as-is:

- `SplitTimeout`
- `SplitFinalizeGracePeriod`

Those are tied to split cutover semantics. Real merges will need their own timeout/grace settings, or a later generalized range-transition timeout model.

### 3. Use hysteresis, not a single threshold

We should not make merges trigger at the same threshold used for splits.

The intended policy is:

- Split when shard size exceeds `MaxShardSizeBytes`
- Merge only when a shard falls well below `MaxShardSizeBytes`
- Only merge adjacent shards when their combined size remains comfortably below the split threshold

Reasonable default behavior:

- Merge candidate if `shard.Size < 0.25x-0.40x MaxShardSizeBytes`
- Merge pair only if `left.Size + right.Size < 0.70x-0.80x MaxShardSizeBytes`

Exact defaults are still TBD, but the design decision is that Antfly should have a deadband between "too large" and "too small" to avoid split/merge oscillation.

### 4. Roll this out in two phases

We decided on a phased implementation:

#### Phase 1: Policy + empty-shard automatic merges

This phase adds the config surface and planner behavior first, while keeping execution constrained to the already-safe case:

- Add `MinShardSizeBytes`
- Generalize split budgets into shared range-transition budgets
- Keep merge execution limited to empty donors
- Use the new min-size policy to decide when empty shards are eligible for merge

This gets the knob and planner model right without pretending Antfly can already merge non-empty shards safely.

#### Phase 2: Real online merges for under-utilized non-empty shards

This phase adds a real merge state machine, data movement, and merge-aware routing.

That is the actual feature needed for under-utilized shard merges.

### 5. Merge and split must share one transition lane

Merges should not be implemented as a second independent automaton that happens to touch shards.

Instead, split and merge should be treated as one class of range transition:

- A shard can be in at most one range transition at a time
- A shard participating in a split cannot start a merge
- A shard participating in a merge cannot start a split
- Split children still replaying parent deltas are not merge candidates
- Cooldown applies after both split and merge completion/rollback

In practice, the current split-state planning slot should eventually become a more general range-transition planner.

### 6. Do not build real merges on top of the current `MergeRange`

The current `MergeRange` path is not sufficient for non-empty merges because it only widens the survivor's range.

A real merge must keep donor data available until the receiver has copied and caught up.

### 7. Do not use the current `Scan` RPC as the only merge transport

The current `Scan` path is document-oriented. It scans main document keys, not all on-disk metadata, transaction state, or other internal structures.

That means:

- `Scan` is acceptable for inspection and maybe for a future quiesced, document-level merge tool
- `Scan` is not sufficient as the primary transport for a correct online merge

For real non-empty merges, we will need either:

- A raw range export/import path
- A shard archive + replay approach
- Another internal transfer mechanism that preserves the necessary state

## Target Design

## Policy Model

Antfly should eventually have the following policy surface:

```go
type ReconciliationConfig struct {
    ReplicationFactor uint64

    MaxShardSizeBytes uint64 // split threshold
    MinShardSizeBytes uint64 // merge threshold

    MinShardsPerTable uint64
    MaxShardsPerTable uint64
    DisableShardAlloc bool
    ShardCooldownPeriod time.Duration

    AutoRangeTransitionPerTableLimit int
    AutoRangeTransitionClusterLimit  int

    // Later, for real online merges:
    MergeTimeout time.Duration
    MergeFinalizeGracePeriod time.Duration
}
```

Notes:

- `MaxShardSizeBytes` remains the split threshold
- `MinShardSizeBytes` is the merge threshold
- `MinShardsPerTable` prevents automatic merges from collapsing a table too far
- Merge planning must also check the merged size of the pair
- The split budget knobs should become shared range-transition budget knobs

## Planner Behavior

For automatic merges, candidate selection should require:

- same table
- adjacent byte ranges
- full healthy replication quorum
- elected leader on both shards
- no active split or merge state
- not currently transitioning
- fresh shard stats
- both shards outside cooldown
- table shard count remains above `MinShardsPerTable`
- merged pair stays below the merged-size guard

For Phase 1, donor must still be `Empty`.

For Phase 2, donor may be non-empty if the online merge machinery exists.

## Merge Execution Design

### Phase 1: Empty-Only Merge Path

This is close to what Antfly already does, but should be cleaned up to fit the new policy surface:

1. Planner finds adjacent pair under merge policy
2. Metadata marks merge intent
3. Stop donor shard on all peers
4. Widen receiver range
5. Delete donor metadata
6. Apply cooldown

This remains valid only when donor is empty.

### Phase 2: Online Non-Empty Merge Path

For non-empty under-utilized shards, Antfly needs a merge state machine roughly symmetric to split:

1. `PREPARE`
2. `COPYING`
3. `CATCHUP`
4. `FINALIZING`
5. `ROLLING_BACK`

#### `PREPARE`

- Choose receiver and donor
- Persist replicated merge state
- Fence new transition attempts
- Keep donor authoritative for writes until receiver is ready

#### `COPYING`

- Bulk-copy donor state into receiver using a real internal transfer path
- Receiver is not authoritative yet

#### `CATCHUP`

- Capture and replay donor-side deltas until receiver catches up
- Similar in spirit to split replay, but in the opposite direction

#### `FINALIZING`

- Cut routing over to receiver
- Widen receiver byte range
- Stop and remove donor peers
- Delete donor metadata
- Clear merge state

#### `ROLLING_BACK`

- Abort the merge
- Keep both original shards serving
- Clear merge state
- Apply cooldown

## Routing Rules

Current routing is split-aware, not merge-aware.

For online merges we decided the routing rule should be:

- During split: parent remains authoritative until child is ready
- During merge: donor remains authoritative until receiver is ready

That means online merges cannot safely reuse the current "rewrite metadata first" pattern.

Metadata cutover must happen at finalize time, not at merge start.

## Data Movement Rules

We explicitly decided:

- The existing `MergeRange` API is not the right primitive for non-empty merges
- The current `Scan` RPC is not sufficient as the main merge transport

So Phase 2 will require a new data-transfer primitive.

We decided the preferred design is:

1. Seed the receiver from a donor archive or archive-like internal transfer
2. Replay donor-side deltas until the receiver is caught up
3. Cut routing over and retire the donor

This aligns best with Antfly's existing split model, which already uses archive seeding plus replay/cutover semantics.

Likely implementation options:

1. Shard archive seeding plus donor delta replay
2. Another internal state transfer path that preserves required metadata
3. Raw range export/import as a fallback option, but not the preferred first design

We are explicitly **not** planning to lead with raw range copy as the main online merge transport.

We have **not** yet chosen which of these to implement first.

## State Machine Interaction with Splits

Split and merge should be mutually exclusive on any shard.

Rules:

- Active split blocks merge start
- Active merge blocks split start
- Split children still replaying parent deltas are not merge candidates
- Merge participants are not eligible for further split planning
- Cooldown prevents immediate split-after-merge or merge-after-split thrash

This is an explicit design decision and should be enforced in the planner.

## Merge Identity Rules

Merge execution should preserve an existing shard ID rather than creating a third replacement shard.

We decided:

- merge into an existing receiver shard ID
- choose the receiver deterministically
- the simplest deterministic rule is: the left/lower-range shard survives

That means:

- lower metadata churn
- simpler routing updates
- stable deterministic behavior
- no need to invent a synthetic merged shard ID

## Files Likely to Change

### Phase 1

| File | Change |
|------|--------|
| `src/metadata/reconciler/types.go` | Add `MinShardSizeBytes` and `MinShardsPerTable`; generalize split budget knobs into shared range-transition budget knobs |
| `src/metadata/reconciler/reconciler.go` | Update planner to use `MinShardSizeBytes` + hysteresis and shared transition budgets |
| `src/metadata/reconciler/reconciler_splits_test.go` | Add tests for min-size merge policy and hysteresis |
| `src/metadata/reconciler/executor.go` | Keep empty-only merge execution aligned with planner semantics |

### Phase 2

| File | Change |
|------|--------|
| `src/store/db/ops.proto` | Add replicated merge state and merge operations |
| `src/store/db/storedb.go` | Implement merge phases, transfer/catchup/finalize/rollback behavior |
| `src/store/client/interfaces.go` | Add merge RPCs beyond `MergeRange` |
| `src/store/client/store_client.go` | Implement new merge RPCs |
| `src/store/api.go` | Add merge endpoints |
| `src/metadata/reconciler/reconciler.go` | Generalize split-state planning into range-transition planning |
| `src/metadata/reconciler/executor.go` | Execute merge state actions |
| `src/metadata/write_routing.go` | Add merge-aware write routing |
| `src/metadata/shard_routing.go` | Add merge-aware read routing if needed |
| `src/tablemgr/table.go` | Delay donor deletion until finalize; current `ReassignShardsForMerge` is too eager for online merges |

## Resolved Decisions

1. Add `MinShardsPerTable` in the first pass. It is the natural companion to `MaxShardsPerTable` and gives automatic merges a hard floor.
2. Shared transition budgeting should replace split-specific naming now. The intended model is one shared range-transition budget, not separate split and merge budgets.
3. For online merges, prefer archive seeding plus donor delta replay. This aligns best with Antfly's existing split design and recovery model.
4. Merge into an existing shard ID, not a replacement shard. Use a deterministic survivor rule, with the left/lower-range shard surviving by default.

## Recommended Implementation Order

1. Add `MinShardSizeBytes`
2. Add `MinShardsPerTable`
3. Generalize split budgets into shared range-transition budgets and rename them accordingly
4. Update planner/tests so automatic merge policy has the right hysteresis model
5. Keep execution limited to empty donors for now
6. Design and implement archive-plus-replay merge state machine for non-empty donors
7. Add merge-aware routing
8. Add online merge tests

## Verification

### Phase 1

- Unit tests for merge candidate selection
- Unit tests for hysteresis behavior
- Unit tests for shared transition budgeting
- Existing split tests still pass

### Phase 2

- State machine tests for merge prepare/copy/catchup/finalize/rollback
- Routing tests for donor-authoritative writes during merge
- Failure tests for node loss during copy/catchup
- Recovery tests after metadata failover or process restart
- Randomized/simulation scenarios combining splits and merges

### `src/sim` Coverage

We should explicitly add simulation coverage in `src/sim` as part of the plan.

Reason:

- merge correctness depends on multi-step reconciliation, routing, and recovery behavior
- split/merge interaction is hard to validate with unit tests alone
- the existing split work already relies on simulation scenarios for liveness and failover coverage

Recommended simulation scenarios:

1. Automatic merge of an under-utilized adjacent pair converges correctly
2. Merge is blocked while either participant is in an active split
3. Split is blocked while either participant is in an active merge
4. Merge copy/catchup survives donor follower restart
5. Merge survives metadata leader failover
6. Merge rollback restores both original shards after injected failure
7. Mixed split+merge workload does not oscillate under cooldown and hysteresis

Likely files:

| File | Change |
|------|--------|
| `src/sim/` | Add merge-focused scenario tests and random/soak coverage similar to split scenarios |
| `src/sim/soak_test.go` | Add merge liveness/failure classification if needed |
| `src/sim/scenario_test.go` or new merge scenario files | Add deterministic merge convergence cases |
| `src/sim/metadata_failover_scenario_test.go` or new merge failover file | Add metadata failover during merge reconciliation |

## Summary

The minimal policy decision is simple:

- Add `MinShardSizeBytes`

The full feature is not:

- Real automatic merges for non-empty under-utilized shards require a real online merge implementation, not just a planner knob

So the plan is:

- land the policy/config surface first
- keep execution empty-only initially
- then build the merge state machine and data movement path for the actual online feature
