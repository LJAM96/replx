# Agent execution plan

Status: batches A–E implemented and locally checked on 29 October 2026. Live PMS and physical-client qualification remains pending. This is a reusable instruction document for Codex, not a standalone shell scheduler.

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

### B: cache authorization and durable invalidation — locally complete

1. Sol High: add `cache_generations` migration and Postgres/memory generation stores. Store only a digest of scopes; keep independent server, scope and class counters. Seed the default global counter and advance cache key schema to v4.
2. Sol High: wire production proxy and warmer keys to Postgres generation reads; bypass on generation read failure. A failed durable bump disables cache serving until an operator/global successful recovery bump.
3. Sol High: add exact-response authorization leases in memory, keyed by caller fingerprint plus exact request cache key. Grant only after a complete successful caller-token PMS response; a lease lasts 30 seconds, does not slide and is revoked on known invalid credentials. Expired or absent proof bypasses local response/artwork reuse.
4. Sol review: verify exact-request scope, restart behavior, SQL atomicity, key privacy, and fail-closed paths.

Gate completed locally: SQL integration tests exercised migrations and a new generation-store instance after restart; current counters remain effective and a later scope bump retires all earlier classes. Real Plex permission revocation and client behavior remain unverified. The 30-second lease is a bounded authorization staleness window, not a library ACL. Unknown/managed-content semantics still need live testing. Race tests remain unavailable without CGO tooling.

### C: shared artwork — locally complete

Sol High implemented shared content-addressed blobs, PostgreSQL transform references, atomic publication, per-caller authorization gates, write reservations, crash recovery, priority eviction and a minimum-free-space guard. Admin storage reporting exposes bounded capacity figures. Gate evidence: proxy and SQL tests verify three callers must each authorize through PMS, their scoped references deduplicate identical bytes to one blob, and anonymous reuse is denied; replacement preserves other references; expired reservations/orphans recover; low-priority data evicts before posters; forced low disk rejects admission while reads remain available; concurrent reservations stay inside budget. Real Plex permission behavior and deployment disk pressure remain pending.

### D: collections — complete locally

Sol High implements explicit snapshots and ordered membership, then targeted invalidation and optional completion-trigger handling. Coordinator integrates admin wiring and documentation. Gate: incomplete scans cannot delete valid memberships, and daily rebuilds publish complete generations.

Collection synchronization stages the complete catalog and ordered PMS child results for one library before a single transaction publishes its metadata, snapshot pointers and deletions. Ordinary and smart collections use PMS-evaluated children. Failed pages, changing totals, duplicate keys and ambiguous empty responses preserve the published generation; an explicit complete empty result can remove all collections or members. Unchanged content retains its published generation. Collection metadata, artwork references, ordering, membership additions/deletions and Plex delete/recreate identifiers participate in the change comparison. A committed change records a durable pending cache invalidation; the callback runs after commit and failures are retried. New catalog placeholders and staged snapshots are internal data and must never be treated as caller authorization or served as published data without a published-generation join.

### E: warming and operations — complete locally

Sol Medium implements priority jobs and bounded ephemeral user contexts. Luna Medium implements stable admin controls and coverage reporting. Gate: warming respects capacity, permissions, origin request budgets and browsing priority.

The scheduler only warms observed browse representations; it does not enumerate every library item, prefetch all artwork, or guess device-specific transforms. Jobs are deduplicated, limited to 512 entries, dispatched serially with request/byte/per-scope budgets, and prioritized across scopes. Foreground traffic cancels active background work. User credentials are held in at most 32 in-memory contexts for a fixed two minutes after successful foreground authorization; jobs contain no credentials, and publication rechecks the job/context before storage. Invalid credentials remove that scope's context and queued entries. Collection browse paths are protected in the queue, library section responses high priority, metadata normal, with low-priority shared artwork eligible for safe eviction only when no higher-priority reference shares its blob.

Authenticated admin operations expose process-local pause/enable and bounded budgets, honest observed-entry coverage, one-collection rebuild, and low-priority artwork eviction. Settings revert to deployment defaults on restart. Full asset enumeration/prefetch, likely-next-page planning, durable warming controls and live PMS/device behavior remain pending qualification or later phases.

### F: qualification — pending

Sol High reviews integrated changes and runs failure/race tests with required services. Real Plex discovery, redirects and physical-client playback remain a separate live gate. Record client/version/protocol evidence for Fire TV, Apple TV, Plex Web and, once identified, Hisense.

## Execution record

- Initial working tree contains the approved planning documents; no runtime edits were present at dispatch.
- Batch A agents dispatched: `inventory`, `cache_correctness`, `collection_assessment`.
- Live access, VPS sizing and client evidence remain unavailable in this chat.
- Batch A completed: bounded inventory command, live home/watch feeds, request-header variation keys, successful watch-write invalidation and foreground/background generation guards.
- Inventory received a separate read-only review; findings were corrected before integration.
- `go test ./...` and `go vet ./...` pass after integration. SQL-backed tests skip without their database configuration; Docker's daemon is unavailable. Race testing requires CGO in this Windows environment and did not run. Real PMS and client checks did not run.
- Header-dependent responses remain cacheable on demand but are not background-warmed because the current warmer cannot reproduce their headers.
- Batch B complete: durable Postgres generations, v4 cache namespace, exact caller/resource leases and proxy integration are in place. `go test ./... -count=1` passed with `REPLX_EDGE_TEST_POSTGRES_URL` pointing to the temporary local PostgreSQL test instance; `go vet ./...`, generated-spec check and `git diff --check` passed.
- Batch C complete: shared content-addressed artwork, durable transform references and reservations, crash recovery, priority eviction, free-space admission guard and admin storage stats are implemented. SQL-backed shared-store tests and proxy/admin tests pass. Production config defaults to a 100 GiB artwork budget and preserves a 30 GiB free-space floor.
- Batch D collection implementation ran in a GPT-6.1 Sol High agent. The PostgreSQL-backed sync tests cover ordered/smart/empty collection publication, unchanged generation reuse, late-page failures preserving the whole library, incomplete catalogs, changing totals, explicit empty deletion, delete/recreate and pending invalidation retry. Coordinator integration adds authenticated completion triggering and a server-wide `collections` generation that preserves unrelated cache classes. `go test ./... -count=1` passes with the temporary PostgreSQL URL configured; `go vet ./...` and `git diff --check` pass. Actual daily Aggregar execution, PMS behavior and client/device behavior remain live validation requirements.
- Batch E implementation used GPT-6.1 Sol Medium for bounded priority warming and GPT-6 Luna Medium for admin operations/documentation. Integration adds foreground cancellation, memory-only expiring user contexts, authorization-aware publication, fair bounded scheduling, request/body budgets, backoff, coverage reporting, targeted collection rebuild, and safe low-priority artwork eviction. `go test ./... -count=1` passed with the temporary PostgreSQL URL configured; `go vet ./...`, generated complete-spec check and `git diff --check` pass. Only observed browse representations are warmed; full library asset prefetch/transform planning is pending. Live PMS, Aggregar and physical-client behavior remain unverified.
- Batch D completes the bounded first execution batch, not all Phase 0/2 requirements or the overall implementation roadmap. No deployment has been performed.

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
