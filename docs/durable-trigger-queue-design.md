# Durable trigger delivery design

## Problem
The scheduler advances its event watermark after `StartTriggeredRun` appends a trigger to an in-memory queue. A process crash can drop the queue while retaining the watermark, permanently losing work.

## Required invariants
1. Persist each trigger before acknowledging it to the scheduler.
2. Commit consumption of the source event and creation of the durable trigger in a single database transaction.
3. Lease runnable trigger rows to workers, with an expiration time for interrupted runs.
4. On restart, retry unacknowledged leases. Establish a stable idempotency key for the source event and target agent.
5. Preserve per-agent concurrency and merge semantics without dropping or duplicating events during merging.
6. Mark a trigger complete only after its conversation has reached a durable terminal state.

## Proposed storage
A trigger-outbox table stores a unique source event key, target agent, trigger payload, state (pending, running, completed), attempt count, lease deadline, and conversation ID. An index over state and lease deadline supports recovery. An event's source identity and agent identity form the deduplication key.

## Required change locations
- `db/triggers.go`: transactional event scan/outbox insert, lease and acknowledgement operations.
- `db/schema.sql`: outbox table, deduplication constraint, pending/lease indexes.
- `server/scheduler.go`: atomically record trigger events before advancing any event watermark.
- `server/conversations.go`: consume leased rows, resume after crashes, acknowledge only after durable completion.
- `server/server.go`: restore pending/expired runs on process startup.

## Verification requirements
- Kill the process immediately after event registration but before conversation startup; the restarted process must run the trigger.
- Kill the process during a trigger conversation; verify retry policy and duplicate-conversation handling.
- Concurrent schedulers must never claim the same lease.
- Verify per-agent serial/parallel execution, merge-by-task and merge-all behavior.
- Confirm database outages do not advance a source watermark or silently discard a trigger.

This design change does not implement the queue. Merge only after the implementation and failure-injection tests land.
