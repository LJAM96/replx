# Replx Edge Full Code Review

Reviewed repository: `LJAM96/replx`

Reviewed branch: `main`

Reviewed commit: `ab15b725beba81a63540c3d6ee28db18dfa38d68`

Review date: 22 September 2026

## Executive assessment

The architecture is substantially better than the earlier review suggested. The separation between playback policy, routing, identity, synchronization, caching, onboarding and administration is generally sensible. The deterministic policy engine is particularly easy to reason about, the database migration design is cautious, the container runtime is appropriately restricted, and the latest playback corrections remove several serious fail open behaviours from the earlier implementation.

I would not currently treat Replx Edge as production ready, however.

The main remaining concern is no longer the playback variant selection code. It is the authorization model around locally generated or locally cached responses.

The most serious issue is `/hubs/search`. Replx Edge maintains an owner wide library index and the search package itself correctly documents that per user library permissions are not known. Despite that, the public proxy can return those owner index results directly to any request containing a nonempty Plex token string. The token does not even have to have been successfully validated. That crosses a fundamental authorization boundary.

There are also important first run administrator security problems, credential forwarding risks caused by ordinary HTTP redirect handling, several cache consistency defects, a broken cache warmer scope model, stale synchronization data, and a Docker admin listener configuration that conflates the host publish address with the address inside the container.

These should be addressed before adding significant additional features.

## HIGH: local search bypasses Plex visibility controls

This is the highest priority issue I found.

The search package explicitly documents the intended security rule:

The owner synchronized index is shared internal data.

Per user library grants are not synchronized.

Local search must fall back to PMS whenever user visibility is uncertain.

That is exactly the correct design requirement.

The proxy does not enforce it.

`serveSearch` only requires a configured search database and a nonempty token fingerprint. It then queries the owner index and constructs the response itself.

It does not require that the token resolved successfully.

It does not require `Resolved.Known`.

It does not verify that the token is currently valid.

It does not verify that the account may access the library containing each result.

It does not ask PMS to authorize the result.

Consequently, an arbitrary nonempty `X-Plex-Token` can potentially obtain locally indexed titles when the index is fresh. A legitimate restricted Plex user can likewise see metadata belonging to libraries that only the owner can see.

The returned information includes rating keys, titles, item types, years and thumbnail references.

### Required change

For Production 1.0 I would disable locally served search results entirely for normal users.

Pass `/hubs/search` through to PMS until Replx has either synchronized reliable per user library grants or has another PMS backed authorization mechanism for candidate results.

Simply requiring `identity.Known` is not enough because identity validity does not prove library visibility.

Tests should explicitly cover an invalid token, a valid owner token, a restricted account and an account without access to a particular library.

## HIGH: established token identities are never revalidated

The identity resolver performs a Plex lookup for a new token fingerprint, but once a fingerprint has an existing identity link in PostgreSQL it is immediately regarded as known.

The database path returns:

`Known: true`

without checking whether the Plex credential is still valid.

It updates `last_seen_at`, but it does not actually update `last_validated_at` through a fresh Plex authentication request.

This is particularly important because Replx now has paths which can return information without contacting PMS.

The browse cache can answer locally.

The artwork cache can answer locally for up to seven days.

Local search can answer entirely from PostgreSQL.

A previously valid token which has subsequently been revoked can therefore continue to operate against locally served content.

There is an additional logic weakness in `resolveCold`. An existing identity relationship is returned before meaningful consideration of `token_status`. An identity link recorded as invalid could therefore still be interpreted as known if it retains its identity association.

### Required change

Separate these concepts:

Identity association

Credential validity

Library authorization

They are not interchangeable.

Persist a validation timestamp and impose a bounded authentication validity period for local serving.

A linked credential which has not been recently validated should not authorize local search or long lived artwork cache responses.

Definitive PMS or plex.tv authentication failures should invalidate the token association and invalidate authorization bearing cache state.

## HIGH: first administrator setup does not require the setup token

`NewMux` correctly wraps most privileged routes with `m.auth(...)`.

`/api/v1/setup` is deliberately registered directly:

`m.mux.HandleFunc("/api/v1/setup", m.handleSetup)`

The handler checks whether `admin_users` is empty and then accepts a username and password.

It never validates the setup token.

This contradicts the documented bootstrap design, which says a random setup token valid for fifteen minutes is issued for initial setup.

Anyone able to reach the admin listener before the legitimate operator creates the administrator can claim the installation.

The private listener reduces exposure but should not substitute for authentication.

### Required change

Require the setup token for administrator creation.

The creation transaction should consume the setup capability at the same time the administrator is successfully created.

## HIGH: concurrent setup can create multiple administrators

The schema describes Production 1.0 as having one active administrator.

The database does not enforce that invariant.

`username` is unique, but there is no constraint limiting the table to a single account.

The setup handler uses:

`SELECT count(*) FROM admin_users`

followed later by:

`INSERT INTO admin_users ...`

Those operations are not atomic.

Two simultaneous setup requests with different usernames can both observe zero administrators and both insert successfully.

This combines particularly badly with the unauthenticated setup route.

### Required change

Enforce the singleton invariant in PostgreSQL as well as application code.

Administrator setup should run inside one transaction using an advisory lock or another database enforced singleton mechanism.

A count followed by an insert is insufficient.

## HIGH: setup credentials are not actually single use

The documentation describes the setup capability as single use and valid for fifteen minutes.

`setupTokenValid` checks only the token value and elapsed time.

Nothing marks the token consumed.

During that window the setup token can repeatedly authenticate protected endpoints.

More importantly, entering that token into `/admin/login` creates an ordinary browser session.

That session lasts twelve hours.

The result is effectively:

A supposedly fifteen minute bootstrap credential can create a twelve hour privileged session.

Creation of the administrator does not invalidate existing bootstrap sessions.

The token can still be reused until its timer expires.

### Required change

Once the administrator account is created, permanently disable bootstrap authentication for that process.

Invalidate all sessions whose subject is `setup-token`.

If a browser setup session is needed before administrator creation, its expiry must never exceed the bootstrap token expiry.

## HIGH: admin bind validation is incomplete

Configuration validation only rejects the literal string:

`0.0.0.0`

The stated design, however, is that the administrator listener should be reachable only through loopback or an explicitly private route such as Tailscale.

The current check still accepts examples such as:

`::`

an explicitly empty `REPLX_EDGE_ADMIN_BIND`

a public interface address

a hostname resolving to a public interface

An empty bind ultimately forms `:8080`, which is a wildcard listener.

This becomes considerably more important because the first setup endpoint is currently unauthenticated.

### Required change

Parse literal addresses with `net/netip`.

Reject unspecified addresses.

Explicitly define which address classes are permitted.

Do not base the security boundary on one string comparison.

## HIGH: Docker admin binding is internally inconsistent

The Compose deployment uses `REPLX_EDGE_ADMIN_BIND` for two different purposes.

It is used as the host address on Docker's published port.

It is also passed into the container and used by Replx itself as the process listen address.

Those are separate network namespaces and require separate concepts.

With the default `127.0.0.1`, Replx listens only to the container's loopback interface. Docker's port forwarding targets the container's network interface, not that loopback listener.

If the operator instead places a Tailscale host address into `REPLX_EDGE_ADMIN_BIND`, the container normally does not own that host address and the application cannot bind it.

### Required change

Use separate configuration such as:

`REPLX_EDGE_ADMIN_LISTEN`

for the address inside the container.

`REPLX_EDGE_ADMIN_PUBLISH_BIND`

for the Docker host publication.

Within the container, binding to its private container interface can be safe while Docker itself controls which host interface exposes it.

## HIGH: Plex credentials can follow an origin HTTP redirect

Several privileged origin requests are implemented as:

Create request

Set `X-Plex-Token`

Call `http.Client.Do`

This occurs across playback, synchronization, PMS identity checks, delegation and warming.

The ordinary Go HTTP client follows redirects.

Go has special rules preventing selected credential headers such as `Authorization` and `Cookie` from being forwarded to untrusted redirect destinations. `X-Plex-Token` is a custom header and does not gain that protection automatically.

A PMS endpoint that responds with a redirect to another hostname can therefore cause Replx to send that hostname a Plex credential.

The custom reverse proxy also follows origin redirects internally rather than transparently returning the original 3xx response to the Plex client.

### Required change

Build one hardened origin transport and use it everywhere.

For server side Plex API operations, reject cross origin redirects.

If redirects are genuinely required, permit only an exact approved origin tuple and explicitly remove credentials before crossing an authority boundary.

For transparent proxying, replace `http.Client.Do` with `httputil.ReverseProxy` or a direct `RoundTripper` based implementation so origin redirects remain origin responses rather than being followed by Replx.

This simultaneously removes a security issue and a large amount of custom proxy code.

## MEDIUM: Continue Watching invalidation is unreachable

The code contains an explicit `invalidateOnStateWrite` function for timeline, scrobble and unscrobble operations.

The function is scheduled only inside:

`if o.cacheable && h.cache != nil`

But `Cacheable` permits only `GET` and `HEAD`.

Timeline and scrobble updates therefore cannot enter the branch which installs their invalidation.

The comments and cache statistics claim the invalidation exists, but operationally TTL remains the only invalidation mechanism.

There is a second issue even after moving the call. The invalidator constructs only two exact keys with empty query parameters. Continue Watching variants whose key incorporates query parameters are not removed, and the `default` representation is also omitted.

### Required change

Move state mutation invalidation outside the cacheable request block.

Do not attempt to invalidate a cache family by reconstructing a few expected keys.

A namespace version or generation key per user and cache class would make invalidation reliable and constant time.

## MEDIUM: administrator cache invalidation API does not invalidate anything

The administrative cache invalidation route records an audit event and returns success:

`"invalidated": body.Scope`

It does not actually delete cache entries.

This is worse operationally than not exposing the endpoint because it tells an administrator that invalidation occurred when it did not.

### Required change

Wire the endpoint to a real cache invalidation abstraction.

Until then, return an explicit unsupported response rather than HTTP 200.

## MEDIUM: cache single flight has a race

The proxy tries to prevent cache stampedes by first calling `waitForFlight`.

When that says there is no flight, it later calls `beginFlight`.

Those are separate lock acquisitions.

Two requests can therefore do this simultaneously:

Both check.

Neither sees an active flight.

Both become refreshers.

The feature is consequently best effort rather than single flight.

### Required change

Use an atomic acquire operation or the established `golang.org/x/sync/singleflight` implementation instead of maintaining custom coordination code.

## MEDIUM: the cache warmer cannot match normal owner cache entries

The identity subsystem represents a known Plex account with an `acct:<accountID>` scope.

The proxy then deliberately replaces that with:

`user:<identity UUID>`

through `cache.UserScope`.

Those `user:` values are what become cache snapshot scopes.

The warmer independently calculates its owner scope as:

`acct:<owner account ID>`

It refreshes an entry only when the two scope strings are equal. Otherwise it removes the entry from tracking.

They can never be equal for a normally resolved identity.

The owner warming feature therefore does not do what its comments describe.

### Required change

Define one canonical account scope type and use it throughout identity, proxy, cache and warmer.

Do not translate the same identity between `acct:` and `user:` representations at different layers.

## MEDIUM: cache representation does not implement HTTP Vary semantics

The response key distinguishes JSON, XML and default representation.

It does not include `Accept-Encoding`.

The cache deliberately preserves `Content-Encoding`.

This creates a concrete protocol problem.

A response obtained when the client explicitly requests gzip can be stored as compressed bytes with `Content-Encoding: gzip`.

Another client for the same account can request the same resource without advertising gzip and receive the same cached compressed representation.

The same architectural issue applies to Plex headers which can affect server responses. Account scoped caching shares responses between devices while the key has no general representation of those client capabilities.

The origin `Vary` header is also not used as part of the cache decision.

### Required change

The simplest safe approach is to normalize cacheable origin requests to an uncompressed representation and remove encoding as a variable.

For other Plex specific variations, establish an explicit allowlist of request headers known to affect browse responses and include their normalized values in the key.

Longer term, implement deliberate `Vary` handling rather than inventing a partial HTTP cache model.

## MEDIUM: deleted Plex libraries remain in the synchronized index

`syncSections` creates a `seen` map and its comment claims that sections absent from a full origin sweep are deleted.

The map is populated and then never used.

The surrounding full synchronization code removes stale items within existing sections but does not reconcile missing library records.

An origin library that is removed can therefore persist in Replx.

Because library rows own related indexed objects through foreign keys, deleting the stale library record would naturally remove much of that data.

There is also an interaction with local search freshness. Eventually the stale library cursor becomes old enough that the freshness test can prevent local search altogether.

### Required change

During successful full synchronization, mark libraries with a sweep generation or compare the complete successful section enumeration against the existing rows.

Only delete absent sections after the origin section listing itself completed successfully.

## MEDIUM: the custom Valkey implementation creates unnecessary contention

`internal/valkey` implements RESP manually over a single TCP connection guarded by one mutex.

All GET, SET and DEL operations are therefore serialized.

A slow cache operation blocks every other cache operation behind it.

The comment says the connection performs a reconnect once on transport failure. The implementation actually drops the failed connection and immediately returns the error. It does not retry that operation.

At the cache abstraction layer, the request context is ignored for production Valkey operations.

Socket deadlines eventually bound the request, but caller cancellation cannot directly stop the operation.

### Required change

I would remove this client.

Use a maintained Go Valkey or Redis client with connection pooling, context cancellation and tested RESP handling.

There is little product value in owning a Redis protocol implementation.

## MEDIUM: policy rejection provenance can be incorrect

Policies correctly merge field by field.

`LoadEffective`, however, returns one overall scope string representing the most specific policy row encountered.

`Evaluate` uses that one scope for every rejection.

Example:

Global denies HDR.

Device policy configures only maximum audio channels.

Effective policy correctly continues to deny HDR.

The HDR rejection is labelled `DEVICE_HDR_DENIED`, despite the restriction coming from the global policy.

The enforcement decision is still correct, but its explanation is not.

For a policy product, inaccurate provenance will make investigation and support significantly harder.

### Required change

Preserve provenance per effective field during merging.

The merged structure should carry both value and winning source for each configurable field.

## MEDIUM: accepted policy fields do not alter runtime behaviour

`preferDirectPlay` is accepted by the policy JSON schema, validated and merged.

I found no execution path where its value alters candidate ranking.

`routingMode` is similarly accepted and persisted into the session, but values such as `origin_preferred` occur only in validation and documentation rather than actual transport selection.

This means an administrator can successfully save policy settings which produce no corresponding runtime behaviour.

### Required change

Either implement them or mark them unavailable in the Production 1.0 API.

A policy field should not be accepted as a functioning setting until its effect is testable.

## MEDIUM: unsuccessful negotiation can create playback session state

The playback engine forwards the decision request to PMS and parses the returned body.

The forwarding layer does not require an expected successful status before proceeding.

A syntactically valid JSON response without explicit mode flags can still be interpreted, with mode determination falling through to transcode.

Session state can therefore be persisted for a negotiation which PMS itself rejected.

This does not appear to reopen the old arbitrary part vulnerability because the selected part is still bound by the policy layer, but it can produce phantom active sessions and misleading diagnostics.

### Required change

Require an expected PMS success status.

Require the expected decision response structure.

Only create an active session after both checks succeed.

## MEDIUM: administrator login has no effective rate limiting

There is a reusable `checkRate` implementation in the admin package.

The password login handler does not use it.

The private listener limits the attack surface but does not remove it, particularly on shared Tailscale networks or after configuration mistakes.

### Required change

Rate limit by a combination of source and username.

Keep authentication failure behaviour indistinguishable for unknown and existing usernames.

## MEDIUM: Argon2 password records are not genuinely self describing

Passwords are stored in a PHC style string containing:

Argon2 version

memory cost

time cost

parallelism

salt

hash

That is good.

Verification ignores most of it.

`verifyPassword` decodes the salt and hash but recomputes with the current compile time constants instead of parsing the parameters contained in the stored password record.

Changing Argon2 parameters in a future version will therefore make existing valid administrator passwords stop working.

### Required change

Parse and validate the stored parameters.

Verify with those values.

After successful login, transparently rehash when the stored parameters are older than the current desired parameters.

## MEDIUM: several admin database handlers can return silent partial results

The core search and synchronization query loops correctly inspect `rows.Err()` after iteration.

Several administrator handlers do not.

For example, the users path breaks out of iteration when `rows.Scan` fails and continues with the data accumulated so far.

Settings retrieval follows a similar pattern.

An operator can therefore receive an apparently successful but incomplete result.

### Required change

A failed scan must fail the request.

Every rows loop should end with an explicit `rows.Err()` check before serializing a successful response.

## MEDIUM: logging redaction fails open for malformed URLs

`RedactURLString` parses the supplied string and returns the original value when parsing fails.

If malformed input contains a token looking query component, malformed input is precisely the case where conservative redaction is most important.

The logger also does not perform final field level redaction itself. It depends on callers placing only sanitized data into `Entry.Fields`.

### Required change

Malformed URLs should log a placeholder or at least everything after the first query delimiter should be removed.

Centralize final secret filtering at the logging sink.

Using `log/slog` for the structured logging machinery with a Replx redaction handler would reduce custom implementation while retaining the existing JSON format.

## LOW: expired administrator sessions accumulate

The session store removes an expired session only when that exact ID is subsequently looked up.

A session created and never used again remains in memory indefinitely until process restart.

In normal administration volumes this is unlikely to create operational pressure, but cleanup should be deterministic.

A bounded session store or occasional sweep is sufficient.

## LOW: logout is permitted through GET

`handleLogout` accepts both GET and POST.

A cross site navigation can therefore terminate an administrator session.

This is not credential compromise, but logout is state mutation and should be POST only with the normal CSRF protection.

## LOW: mutable settings use a deny heuristic instead of an allowlist

The settings endpoint rejects keys when their names look like tokens, secrets or onboarding values.

It otherwise permits arbitrary keys to be written.

This is fragile and makes the API contract undefined.

### Required change

Define supported runtime settings explicitly.

For every setting define:

Type

range

default

whether restart is required

whether it is secret

The handler should reject unknown settings.

## LOW: secret entropy calculation is misleading

`checkSecretEntropy` computes the empirical Shannon entropy of the characters contained in the supplied secret.

That is not a reliable measurement of how difficult a human chosen secret is to guess.

A sufficiently complicated looking deterministic phrase can pass.

### Required change

Make secret generation part of deployment.

Prefer a fixed requirement such as 32 randomly generated bytes represented as hex or Base64 rather than attempting to estimate entropy from a supplied string.

## LOW: database TLS silently defaults to disabled

When a complete `REPLX_EDGE_POSTGRES_URL` is not supplied, `DatabaseURL` explicitly sets:

`sslmode=disable`

That is understandable for the local Docker Compose network but is a surprising default for an externally hosted PostgreSQL database.

Expose the SSL mode explicitly or construct a secure connection when the database host is not the local deployment service.

## LOW: artwork cache file writes can race

The artwork cache uses predictable temporary paths formed by appending `.tmp` to the final cache key.

Two simultaneous requests for the same uncached artwork can therefore operate on the same temporary files.

Metadata and body are also renamed independently, leaving a small window where one exists without the other.

Use `os.CreateTemp` in the destination filesystem followed by rename.

Since these files contain user accessible media, restrictive file and directory permissions would also be preferable to `0644` and `0755`.

## LOW: client IP attribution trusts inbound forwarding history

The proxy appends its parsed remote address to an existing client supplied `X-Forwarded-For`.

The comment correctly says it is not used for authentication.

It can still reduce evidential value in logs because the leftmost portion can be supplied by the client unless Replx knows the immediate upstream overwrites it.

Use a defined trusted proxy model.

Only consume forwarding headers from known Cloudflare or internal proxy peers.

Use `net.SplitHostPort` or `net/netip` rather than splitting the remote address at the final colon.

## INCOMPLETE: frontend CI is currently cosmetic

The frontend GitHub Actions job is green.

The frontend itself is still a placeholder.

`lint` prints a success message.

`typecheck` prints a success message.

`test` prints a success message.

`build` copies `index.html` into `dist`.

The HTML itself explicitly describes the interface as Alpha scaffolding.

This is fine during Alpha development, but the existence of a required green `frontend` check should not be interpreted as meaningful frontend validation.

Once the real interface lands, replace all placeholder jobs immediately with actual compiler, lint and component or integration tests.

## INCOMPLETE: media fallback gateway remains a placeholder

The optional media gateway is explicitly documented as health only.

It does not implement session lookup, capability verification, policy enforcement or streaming.

This is documented rather than hidden, so I do not regard it as a defect.

I would nevertheless prevent operators from enabling `media_fallback` as if it were functional until the gateway implementation exists.

Currently the policy API accepts `media_fallback` despite the feature being incomplete.

## INCOMPLETE: logs API is a stub

`GET /api/v1/logs` always returns an empty collection with a message instructing the administrator to inspect Docker stderr instead.

Again, this is not inherently wrong for Alpha.

It should not be exposed as a finished administration capability.

## BUILD AND SUPPLY CHAIN

The runtime container is reasonably well hardened.

The final image is distroless.

The Go application is built with `CGO_ENABLED=0`.

The runtime uses the nonroot distroless user.

Those are good choices.

Reproducibility can be improved.

Base images are referenced through mutable tags instead of immutable digests.

The frontend image uses `npm install`.

There is no frontend lockfile in the repository.

The CI security step also invokes `govulncheck@latest`, which means the exact analysis tool can change between runs. The GitHub Actions dependencies use ordinary release references rather than immutable commit identities.

For a security sensitive infrastructure service I would progressively make builds reproducible:

Commit dependency lock files.

Use `npm ci`.

Pin builder and runtime images by digest.

Pin security tooling versions.

Consider immutable Action SHAs for higher assurance CI.

## CUSTOM CODE THAT SHOULD PROBABLY BE REMOVED

### Reverse proxy

The proxy manually recreates header filtering, forwarding address handling, origin requests, response copying, redirect handling and streaming.

The standard library already provides `net/http/httputil.ReverseProxy`.

Replx has legitimate custom requirements around classification, cache policy, playback policy and media routing, but those can sit around the standard proxy rather than replacing its HTTP transport mechanics.

This would directly reduce the redirect credential problem and remove a substantial amount of protocol code.

### Valkey protocol

The custom RESP implementation provides very little strategic value and introduces connection serialization, context limitations and retry bugs.

Replace it with a maintained implementation.

### Single flight

The custom flight map has a race between observation and acquisition.

Use the existing Go single flight implementation.

### Address validation

Use `net/netip` rather than string comparisons.

### Password record parsing

The hashing primitive is fine.

The storage format parser should either be fully implemented or delegated to a well tested PHC implementation rather than maintaining a half parsed PHC format.

## CODE DUPLICATION

The previous review identified duplicate token extraction and part path parsing.

Those have been improved.

`spike.ExtractToken` now delegates to the canonical trace parser rather than implementing its own token precedence.

The spike part parser likewise delegates to `playback.PartIDFromPath`.

That is the right direction.

The largest remaining duplication is the origin HTTP client model.

Playback, PMS discovery, synchronization, delegation, warming, artwork and the proxy each build or use HTTP clients independently.

Every one of those pieces must independently get timeout, redirect and credential forwarding rules correct.

They already have not.

Create one `origin` package containing the security policy for trusted PMS communication.

Anything sending a PMS credential should go through it.

There is also repeated database list and pagination code throughout the admin package. A small internal helper could consistently enforce context timeouts, scan failures and `rows.Err()` without introducing a generic repository abstraction.

## CRYPTOGRAPHY

The cryptographic primitives are sensible.

Secrets at rest use AES GCM with fresh random nonces.

Fingerprints use HMAC SHA256.

Encryption purposes are separated.

I did not find a direct cryptographic vulnerability in this layer.

The one thing I would simplify is key derivation.

`DeriveKey` hashes the root secret together with a purpose string manually.

This is not obviously exploitable given a strong root secret and explicit domain separator, but HKDF SHA256 is the established primitive for deriving purpose specific keys and removes the need to maintain a custom construction.

If changing this, version encrypted data so key derivation changes can be migrated safely.

## DATABASE AND MIGRATIONS

The core database infrastructure is one of the healthier parts of the repository.

Connection pool construction and migrations are separated.

Migrations execute transactionally.

An advisory lock prevents competing instances from applying migrations simultaneously.

Migration completion is surfaced independently for readiness.

The principal database problems I found are therefore data model invariants around administrator singleton setup and application reconciliation around deleted Plex sections, rather than the migration engine itself.

## PLAYBACK POLICY

The policy engine is straightforward and deterministic.

Variant eligibility is separated from ranking.

Source restrictions are evaluated before output bandwidth constraints.

The code fails when no permitted variant exists rather than reopening forbidden sources.

Transcode denial occurs after selection.

Stable error codes are used for diagnostics.

Those are all good architectural decisions.

The remaining policy work is mostly:

Accurate per field provenance.

Making every accepted setting operational.

Requiring a valid successful PMS decision before committing active session state.

Continuing to run the same boundary tests across both ingress modes.

## STATUS OF THE PREVIOUS SEVEN PLAYBACK FINDINGS

The existing `codereview.md` contains seven concrete playback defects from the earlier code.

Current `main` is specifically the merge titled `fix: review round six (playback boundary correctness, findings 1-7)`.

I rechecked those affected areas rather than copying the old report.

The current code now has persistent selected Plex media and part identifiers.

The indexed variant path binds its SQL argument.

Raw part enforcement is invoked in direct mode.

Manifest checks are tied more tightly to the negotiated title and media selection.

Direct origin construction rejects authority bearing paths and verifies that the resolved destination remains on the configured origin.

Live metadata normalization was consolidated.

Transcode restrictions are applied at the media boundary.

The duplicated token and part ID parsing called out by the earlier review has also been centralized.

I therefore do not consider those seven appropriate to report again as current unresolved defects.

## TESTING AND CI

The current HEAD is green in GitHub Actions.

The repository has six check runs on this commit and the visible checks report successful completion. The independent Go job is also successful.

The Go CI is considerably more meaningful than the frontend job because it exercises compilation, tests and race detection according to the workflow.

There are real unit and live database tests around important packages, including policy, synchronization, search and playback.

The missing tests line up quite closely with the new defects found in this review.

There should be explicit regression tests for:

Unauthorized local search with an arbitrary token.

Restricted user search against an owner only library.

Previously valid then revoked tokens hitting local caches.

Administrator setup without the bootstrap token.

Concurrent administrator setup.

Bootstrap token reuse after administrator creation.

Cross host redirects carrying `X-Plex-Token`.

Docker administrator listener reachability.

Timeline mutation invalidating Continue Watching.

Cache invalidation API actually deleting entries.

Concurrent cache misses resulting in exactly one origin request.

Owner cache warming with a resolved identity.

Different `Accept-Encoding` requests sharing a cache key.

Full synchronization after removing an origin library.

PMS error responses never creating playback sessions.

Policy rejection provenance from mixed global, user and device settings.

Each of those should fail against the current implementation and become a permanent regression test as its fix lands.

## RECOMMENDED REMEDIATION ORDER

### Security boundary phase

Disable local search serving until user visibility can be proven.

Require and consume the setup token during first administrator creation.

Make administrator creation atomic and database enforced.

Invalidate bootstrap sessions after setup.

Fix admin listener address validation and separate Docker listen versus publish addresses.

Centralize Plex HTTP transport and prevent credential forwarding across redirect authorities.

Introduce credential validity freshness for local responses.

### Cache correctness phase

Move watch state invalidation outside cache eligibility.

Replace guessed key deletion with namespace invalidation.

Make the admin invalidation endpoint real.

Replace the custom single flight mechanism.

Unify account scope identifiers.

Fix HTTP representation variation.

Replace the custom Valkey client.

### Data and policy phase

Reconcile deleted libraries during full synchronization.

Track policy provenance per field.

Either implement or reject `preferDirectPlay` and `routingMode`.

Validate successful PMS decision semantics before storing playback sessions.

Make administrator query failures explicit rather than returning partial data.

### Hardening phase

Parse stored Argon2 parameters correctly.

Add login throttling.

Use POST plus CSRF for logout.

Use an explicit settings allowlist.

Improve log redaction.

Use generated random secret material rather than entropy estimation.

Make PostgreSQL TLS behaviour explicit.

Harden artwork cache atomic writes.

Normalize trusted proxy and client IP handling.

### Simplification phase

Adopt `httputil.ReverseProxy`.

Adopt a maintained Valkey client.

Adopt standard single flight coordination.

Use `net/netip`.

Consider `log/slog` behind the Replx logging abstraction.

Pin dependencies and build inputs more tightly.

## FINAL ASSESSMENT

The current code is substantially more mature than the previous playback review indicates.

The playback policy architecture is now reasonably strong and most of the earlier fail open boundary problems appear to have been addressed.

The largest architectural weakness has moved to authorization around data Replx serves without PMS participating in the request.

That distinction should become a core design rule:

PMS remains the authorization authority.

The owner synchronized database is an index, not an authorization database.

A historical token to account mapping is identity information, not proof that the credential remains valid.

An account identity does not prove access to every library owned by the server administrator.

A cache entry does not remove the need for an authorization model.

Enforcing those distinctions would eliminate the most serious remaining issues.

The other major recommendation is simplification. Replx currently owns more HTTP and cache infrastructure than the product actually needs to own. Standard reverse proxying, a mature Valkey client, standard single flight coordination and standard network address parsing would remove code while improving correctness.

The project already has a useful package structure and an increasingly valuable test suite. The next development cycle should concentrate on these security and consistency boundaries before adding further routing, frontend or media gateway capability.

