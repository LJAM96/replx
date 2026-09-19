main is currently red in GitHub Actions. Security, migrations and frontend jobs pass, but the Go job and generated-spec check fail.

The Go failures are mostly test isolation problems: multiple live packages share the same PostgreSQL database and each attempts to create its own enabled Plex server, violating plex_servers_one_enabled_idx. The retention test also leaves foreign-key-linked playback state behind, causing cleanup failures.

The generated specification is also stale:

REPLX_EDGE_COMPLETE_SPEC.md is stale:
run scripts/build_complete_spec.py

Those are immediate release blockers, although not necessarily production-code defects.

P0: policy can silently weaken when the database misbehaves

This is the most important issue I found.

policy.LoadEffective() calls loadLevel() independently for global, user and device policy.

But loadLevel() returns:

return Policy{}, false

for all of these:

no row
database timeout
database failure
bad JSON
corrupt policy
context timeout

They are indistinguishable.

That means something like:

Global:
4K allowed

Jodie:
max 1080p

could become:

Postgres user-policy query fails
        ↓
user level silently ignored
        ↓
global policy wins
        ↓
4K becomes eligible

This conflicts directly with the core Replx Edge invariant.

LoadEffective should return:

(Policy, Scope, error)

and distinguish:

pgx.ErrNoRows
→ inheritance / no override

database error
→ policy unavailable

invalid JSON
→ policy corrupt

A database failure while resolving an applicable restriction should fail playback closed.

This is more important than almost any new feature.

P0: playback negotiation can bypass the policy engine

HandleDecision() returns false if:

d.RatingKey == ""

or:

token == ""

The proxy interprets false as:

I did not handle this request, proxy it normally.

Therefore an unfamiliar Plex decision request shape can become:

universal/decision
        ↓
Replx fails to parse ratingKey
        ↓
HandleDecision returns false
        ↓
ordinary proxy
        ↓
PMS makes unrestricted decision

That is exactly the class of forward-compatibility situation where playback policy should fail closed rather than disappear.

I would distinguish:

policy engine disabled
→ transparent proxy allowed

known non-policy request
→ transparent proxy allowed

policy-critical decision but parsing failed
→ POLICY_DECISION_UNSUPPORTED / fail closed

Unknown Plex control APIs should remain transparent, but known playback decision endpoints must not become transparent simply because the parser failed.

P0: the part boundary still fails open without session state

This is another important one.

EnforcePart() currently does:

if e == nil || e.Store == nil || sessionID == "" {
    return "", false, ""
}

and does effectively the same thing if the session lookup fails or no active session exists.

Empty result means allow.

So:

client requests media part
        ↓
no session ID
or session missing
or DB read fails
        ↓
allow

The same principle exists at the manifest boundary. Missing session information permits the request.

That undermines the earlier requirement that autoplay or clients which skip another decision cannot bypass source enforcement.

At minimum, for a media part Replx can identify as belonging to a restricted item:

session found
→ validate against selected source

session absent
→ reconstruct policy using user + client + part

cannot reconstruct safely
→ POLICY_DECISION_REQUIRED

A database outage especially should not turn:

Jodie must use 1080p

into:

session lookup error
→ allow requested 4K part

This matters even though the default Cloudflare topology is not a cryptographic hard-enforcement architecture. Requests that do traverse Replx should still obey Replx policy deterministically.

P0: identity caching can apply one device's policy to another device

The new identity layer is conceptually good, but the process cache is keyed only by token fingerprint:

mem map[string]memEntry

and the cached Resolved object contains:

IdentityID
ClientID
AccountID

The first request does:

fingerprint X
Apple TV
→ ClientID A

and caches that entire resolution.

A subsequent request within five minutes with the same fingerprint can come from:

fingerprint X
iPhone

but receives the cached:

ClientID A

without calling recordClient().

That becomes dangerous now that device policy is live:

User token X
Apple TV 4K override
    max 2160p

same token X
Bedroom 1080p client

The second client can potentially inherit the first client's client_instances UUID and therefore its device-level policy.

The fix is to separate account identity caching from client resolution.

Cache:

fingerprint
→ account/identity

but calculate or record:

(client identifier, identity)
→ clientInstanceID

on every request or cache it independently by:

fingerprint + X-Plex-Client-Identifier
P0/P1: transient plex.tv failures are treated as invalid credentials

The identity resolver calls plex.tv on an unknown fingerprint.

If GetUser() returns any error:

if err != nil || id == 0 {
    r.markInvalid(...)
    return fallback
}

But GetUser() can fail because of:

401 invalid token
403 invalid authorization

or

DNS failure
timeout
plex.tv 500
plex.tv 503
temporary TLS/network problem

The resolver doesn't distinguish them, even though the Plex client layer already has a typed StatusError.

It then negative-caches that token for an hour.

The practical effect is:

plex.tv temporarily unavailable
        ↓
valid user's token marked invalid
        ↓
identity falls back to unresolved
        ↓
user/device policy disappears
        ↓
global policy only

Only definitive authentication responses such as the appropriate 401/403 should get the one-hour negative cache.

Network and server failures should return an identity resolution degraded result and should not destroy previously established policy identity.

For known token links, I would actually continue using the established identity unless PMS itself rejects the user request.

P0: shared artwork cache can leak restricted artwork across users

This is the strongest isolation issue I found.

Artwork is explicitly keyed without the Plex token:

path + transformation parameters

and shared across all users.

The proxy only checks:

o.fingerprint != ""

before serving the shared cached artwork.

So the first user can do:

Luke requests poster X
Luke is authorised
origin returns poster
Replx caches poster globally

Then another authenticated but restricted user could request that same transcode URL:

Jodie requests poster X
        ↓
cache hit
        ↓
poster returned without PMS authorization check

The comment says:

authorization to reference required

but there is no authorization check on a cache hit.

An authenticated token is not equivalent to authorization for a specific library item.

For Production 1.0 I would make artwork keys account scoped just like metadata:

acct:<id>:artwork:<hash>

You can revisit shared binary deduplication later using a two-layer design:

per-user authorization key
        ↓
shared content-addressed blob

That preserves disk deduplication without bypassing authorization.

P1: response caching can store a truncated origin response

copyBody() uses:

n, _ := io.CopyN(...)

and deliberately ignores the error.

If the origin terminates unexpectedly below the 2 MiB cap, Replx can cache whatever bytes arrived.

For example:

Content-Length 400 KB

origin disconnects at 210 KB

could result in a 210 KB malformed XML/JSON response being persisted and served repeatedly until TTL expiry.

You should only cache when the complete response body was successfully consumed.

For bounded cache bodies, a safer structure is:

limited := io.LimitReader(resp.Body, MaxEntryBytes+1)
body, err := io.ReadAll(limited)

if err != nil {
    do not cache
}

if len(body) > MaxEntryBytes {
    stream/skip cache
}

Although because you're already streaming to the client, you'd probably want a tee that tracks copy errors and only commits after a clean EOF.

P1: cached responses lose most origin headers

The cache entry stores only:

Status
ContentType
Body

On cache hits, Replx reconstructs only:

Content-Type
X-Replx-Edge-Request-ID
X-Replx-Edge-Cache

Any relevant Plex or HTTP response headers disappear.

Potential examples include:

ETag
Last-Modified
Cache-Control
Content-Language
Content-Encoding
Plex-specific response metadata

Not every one needs preserving, but it should be a deliberate allowlist rather than silently dropping everything.

I would add:

Headers http.Header

to the cache entry with an explicit safe-header allowlist.

Never blindly persist Set-Cookie or authentication-related headers.

P1: identity resolver contains a race

The resolver protects mem with:

mu sync.Mutex

but the same object also contains:

server string
srvAt time.Time

and enabledServer() reads/writes them without that mutex.

Resolve() is naturally called concurrently by HTTP handlers.

Two cold identity lookups can therefore concurrently access:

r.server
r.srvAt

This should show under a targeted race test.

Either protect those fields under the existing mutex or use a separate lock/atomic cache.

P1: retention swallows database failures

PurgeOnce() has:

exec := func(...) int64 {
    if err := ...; err != nil {
        return -1
    }
}

and then always:

return out, nil

Therefore:

DELETE fails
→ count becomes -1
→ error is lost
→ purgeAndLog logs successful "purged"

Retention errors are exactly the type you need visible before a database fills up.

Return the first error.

Something like:

func exec(...) (int64, error)

and abort or aggregate failures.

P1: local-search freshness checks only one section

The freshness condition is:

SELECT EXISTS(
  SELECT 1
  FROM sync_cursors
  WHERE server_id=$1
    AND sync_type='section'
    AND status='complete'
    AND last_completed_at > ...
)

That means:

Movies fresh
TV stale for 3 days

still yields:

index fresh = true

The freshness gate needs to ensure every enabled/indexed section expected for that server has a recent completed cursor, not merely one.

The search package itself correctly acknowledges that it must not be an authorization decision, which is good.

P1: the current live test strategy is not isolated

This is what has made main red.

Several packages connect to the same database and each tries to create:

enabled = true

Plex servers.

Because the Production 1.0 schema correctly permits only one enabled server, unrelated package tests interfere.

Don't weaken the production constraint.

Instead give each live test:

transaction + rollback

where possible, or:

unique schema/database

per package/test.

For tests which must commit across multiple connections, create a unique PostgreSQL database/schema per test namespace.

The retention cleanup FK errors in the same CI log are another sign that package tests are assuming global DB ownership they do not actually have.

Documentation drift is back

The CI check you deliberately added is doing its job.

The authoritative aggregate file currently does not match the component documentation.

Run the generator and include the generated change in the same commit whenever docs change.

I would add a local pre-commit helper such as:

python3 scripts/build_complete_spec.py
git diff --exit-code REPLX_EDGE_COMPLETE_SPEC.md

so this fails before GitHub.

Things that are strong

There is a lot I would keep.

The codebase is now separated into sensible domains rather than becoming one giant proxy package. The current tree includes dedicated packages for identity, policy, playback, synchronization, cache, artwork, diagnostics, retention and routing.

The playback engine's architecture is good in principle:

load policy
→ load variants
→ evaluate
→ rewrite mediaIndex
→ ask PMS
→ validate PMS response
→ persist session

and critically it refuses to proceed when it cannot persist the session used by the media boundary.

PMS disagreement is explicitly inspected rather than blindly trusting the origin's selected media index. That's exactly the sort of defensive design Replx Edge needs.

The user token storage stance also remains strong: the new identity layer stores a fingerprint-to-account link and does not persist replayable user tokens.

The Cloudflare invariant remains intact at the proxy level. Unknown media semantics do not silently turn into video delivery over the Cloudflare control endpoint.

And the security workflow is currently green, including govulncheck, so the earlier vulnerable x/text issue is resolved.

What I would do next

I would not add another major feature to 0.3.x yet.

The next commit should be a concentrated 0.3.1 correctness hardening pass:

Make policy DB errors explicit and fail playback closed.
Split identity/account caching from client-instance resolution.
Only negative-cache definite authentication failures.
Fail closed on unparseable universal playback decisions.
Re-evaluate policy at the media boundary when session state is absent instead of allowing.
User-scope artwork authorization.
Never cache incomplete origin bodies and preserve a safe response-header set.
Synchronise the resolver's server cache.
Propagate retention errors.
Fix search freshness to require all relevant sections.
Isolate PostgreSQL live tests and get go test -race ./... green.
Regenerate REPLX_EDGE_COMPLETE_SPEC.md.
