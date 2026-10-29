# Agent execution plan

Status: batch A implemented and locally checked on 29 October 2026; batch B is next. This is a reusable instruction document for Codex, not a standalone shell scheduler.

## Run instruction

Ask Codex: "Execute the next incomplete batch in docs/agent_execution_plan.md using its assigned models. Inspect the working tree, preserve existing changes, run relevant tests, integrate the results and update the execution record. Continue independent offline work while recording live validation as pending."

Detailed requirements: [bounded cache implementation plan](bounded_cache_implementation_plan.md).

## Model assignments

| Work | Model | Effort | Dependency |
| --- | --- | --- | --- |
| Coordination and integration | Current main-chat model | Current setting | All batches |
| Inventory command and fixture tests | GPT-6 Luna | High | None |
| Cache keys, user isolation and watch state | GPT-6.1 Sol | High | Existing source and fixtures |
| Shared artwork authorization/storage | GPT-6.1 Sol | High | Cache review; explicit authorization contract |
| Collection snapshot synchronization | GPT-6.1 Sol | High | Durable invalidation interface |
| Priority warming | GPT-6.1 Sol | Medium | Artwork and collection interfaces |
| Admin controls, documentation and fixtures | GPT-6 Luna | Medium | Stable implementation interfaces |
| Independent integration review | GPT-6.1 Sol | High | Each implementation batch |
| Live routing/device checks | GPT-6.1 Sol with operator/device access | High | Secure PMS configuration and available clients |

Model settings are passed when spawning each agent. The coordinator cannot change its own current model through these tools. Keep a maximum of three child agents active. Do not spawn further agents without an explicit assigned need. Prefer one focused implementation agent over multiple agents editing the same subsystem.

## Execution rules

- Inspect AGENTS.md and working-tree changes before assignments. Existing documentation changes belong to this conversation and must be preserved.
- Assign exclusive files/packages to each writer. Read-only reviewers may inspect all files.
- Keep dependent changes sequential. Freeze interfaces before allowing downstream implementation.
- Require every agent to report changed files, checks performed, failures and incomplete requirements.
- Use focused tests during implementation, then the existing Go suite and appropriate integration checks during reconciliation. Missing services are recorded as unavailable, never as passes.
- Do not commit, publish or deploy as part of this run unless separately instructed.
- Never put Plex credentials in task prompts, logs or documents. Offline fixtures precede live requests.
- Do not claim client compatibility from unit tests. The live deployment gate remains in force.
- If a model makes no progress after two bounded repair attempts, escalate that task to Sol High with the failure evidence rather than repeat indefinitely.

## Batches

### A: foundation — locally complete

1. Luna High: create a separate read-only inventory command and package, bounded PMS calls and fixture tests.
2. Sol High: implement initial cache/watch-state corrections within cache/proxy packages; retain unresolved Phase 2 requirements explicitly.
3. Luna Medium: read-only artwork/collection interface assessment.
4. Coordinator: integrate, inspect diffs, run checks and record remaining work.

### B: cache authorization and durable invalidation — pending

Sol High completes request-variation handling, durable generation storage and resource authorization contracts. Sol review verifies per-user visibility, revocation and restart races. Resolve these interfaces before shared-artwork implementation.

### C: shared artwork — pending

Sol High implements shared blob/reference records, atomic publication and authorization gates. A subsequent bounded task implements admission reservations, crash recovery and priority eviction. Luna adds admin reporting only after interfaces stabilize. Gate: three authorized users share one blob and unauthorized access remains denied; disk budget survives concurrent writes.

### D: collections — pending

Sol High implements explicit snapshots and ordered membership, then targeted invalidation and optional completion-trigger handling. Luna implements fixtures/documentation. Gate: incomplete scans cannot delete valid memberships, and daily rebuilds publish complete generations.

### E: warming and operations — pending

Sol Medium implements priority jobs and bounded ephemeral user contexts. Luna implements stable admin controls and coverage reporting. Gate: warming respects capacity, permissions, origin request budgets and browsing priority.

### F: qualification — pending

Sol High reviews integrated changes and runs failure/race tests with required services. Real Plex discovery, redirects and physical-client playback remain a separate live gate. Record client/version/protocol evidence for Fire TV, Apple TV, Plex Web and, once identified, Hisense.

## Execution record

- Initial working tree contains the approved planning documents; no runtime edits were present at dispatch.
- Batch A agents dispatched: `inventory`, `cache_correctness`, `collection_assessment`.
- Live access, VPS sizing and client evidence remain unavailable in this chat.
- Batch A completed: bounded inventory command, live home/watch feeds, request-header variation keys, successful watch-write invalidation and foreground/background generation guards.
- Inventory received a separate read-only review; findings were corrected before integration.
- `go test ./...` and `go vet ./...` pass after integration. SQL-backed tests skip without their database configuration; Docker's daemon is unavailable. Race testing requires CGO in this Windows environment and did not run. Real PMS and client checks did not run.
- Header-dependent responses remain cacheable on demand but are not background-warmed because the current warmer cannot reproduce their headers. Durable generations, resource authorization improvements, shared artwork and storage admission are pending batches B/C.
- This completes the bounded first execution batch, not all Phase 0/2 requirements or the overall implementation roadmap. No deployment or commits were performed.

## Initial assessment findings

The collection/artwork assessment is complete. Existing artwork routing is injected through `proxy.Options.Artwork`; the shared store must keep caller authorization separate from blob lookup. The current store uses flat files and an 8 MiB image ceiling. A new sharded store should use additive schema migrations and treat the old cache as disposable rather than importing its authorization assumptions.

Collection synchronization can reuse the worker's injected HTTP client, pagination and PostgreSQL test harness. New ordered membership snapshots must stage then publish atomically; deletion is inferred only from a completed enumeration. Owner collection snapshots remain internal data. Invalidation should be emitted after the database commit, not during a partial sweep.

The assessment confirms that fixtures and local integration work can proceed without server shell access. It does not establish support for actual client transformations, Plex event behavior or Aggregar completion callbacks.

## Inventory command

The first batch adds `go run ./cmd/replx-inventory`. Configure `REPLX_EDGE_PMS_URL`
and `REPLX_EDGE_PMS_TOKEN` securely in the process environment before running it.
The token is never a command-line argument. HTTPS is required except for loopback
fixtures; redirects are not followed. JSON is written to standard output; exit 1
means incomplete counts, and exit 2 means configuration/output failure.

Optional bounds are `REPLX_EDGE_INVENTORY_PAGE_SIZE` (default 200),
`REPLX_EDGE_INVENTORY_WORKERS` (default 4), and
`REPLX_EDGE_INVENTORY_REQUEST_TIMEOUT_SECONDS` (default 15). The command has a
30-minute overall deadline. It prefers PMS total counts and falls back to bounded
page counting. Movie/show/season/episode and collection counts are covered;
representative asset downloads, byte sampling and resumable inventory are still
pending. Do not interpret this command as completion of all Phase 0 deliverables.
