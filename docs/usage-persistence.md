# Usage persistence, pricing, and quota

This fork adds `/stats.html` and a persistence adapter for the **existing built-in usage output**. It is attached by the standalone CLI, not a second collector or a replacement for inference, the SDK usage manager, or runtime executors. The original management page and `/management.html#/quota` remain available. Locally counted tokens are **not** provider quota or a billing ledger.

## Choose a deployment deliberately

| Deployment | Usage storage | Database requirement |
| --- | --- | --- |
| Existing file-based/native or [base Compose](../docker-compose.yml) | Instance-specific local journal | None |
| An already active PostgreSQL auth/config store | Borrows that store's actual SQL pool and schema | Its existing database only |
| Explicit [bundled PostgreSQL Compose](../docker-compose.postgres.yml) | Shares the bundled PostgreSQL auth/config store | Bundled database, selected only with `-f docker-compose.postgres.yml` |
| CLIProxyAPIHome | This standalone add-on is not attached | No database added by this feature |

The base, bundled PostgreSQL, and Home/cluster Compose files now default to `ghcr.io/nukwaw/cpa:latest`. `CLI_PROXY_IMAGE` still overrides that choice and `pull_policy: always` remains intact. The base deployment has no required database/password, no new PostgreSQL environment mapping, and no database startup dependency. Native local usage also needs no environment variable. External-database deployments should keep their existing wiring; they must not select the bundled file just to get statistics.

## Published fork images

The [GHCR workflow](../.github/workflows/ghcr-image.yml) publishes native `linux/amd64` and `linux/arm64` builds after a push to `main`, or a manual workflow run on `main`. It checks out the exact triggering commit, with full history/tags for metadata, and never checks out the selected version tag. The version is the highest version-sorted `v*` tag reachable from that commit; a release sorts ahead of its prereleases. For example, a commit after `v8.0.4` builds the new commit with `VERSION=v8.0.4`, not the old tagged tree. No matching tag is an explicit build error: push the relevant version tags to this fork first. At setup, the fork's remote had no tags even though the local checkout had `v8.0.4`; pushing the branch alone does not publish that tag. For the current local release tag, publish it explicitly with `git push origin v8.0.4` before the main build. This does not move the tag to the new commit.

A successful multi-platform build publishes `sha-<full-commit-sha>`, then checks the remote main tip before promoting `latest` and the selected version (for example `v8.0.4`). An old rerun does not cancel a newer commit's run and does not deliberately roll those moving aliases back. SHA tags select source commits, not immutable image bytes: rebuilds can change build time or base images. Use `ghcr.io/nukwaw/cpa@sha256:<manifest-digest>` for immutable image pinning. The existing Dockerfile receives `VERSION`, the full `COMMIT`, and UTC `BUILD_DATE`, and the image has matching OCI labels. Per-run architecture tags prevent the final manifest from mixing two pushes; those `build-*` staging tags remain available for registry cleanup/retention. The workflow builds committed files without refreshing generated catalogs from the network.

Publication uses `GITHUB_TOKEN` with `packages: write`. No Docker Hub secrets or personal publishing token are required. After the first successful run, set the GHCR package visibility to public for anonymous pulls, or authenticate Docker with suitable package-read permission. If an existing package is not linked to this repository, grant the repository Actions access before publishing. Enable Actions for the fork if they are disabled. The inherited Docker Hub workflow runs only in the upstream repository.

For the base deployment, after publication and normal configuration setup:

```sh
docker compose pull cli-proxy-api
docker compose up -d --no-build
```

For bundled PostgreSQL, complete the bootstrap/migration and password prerequisites below, then use `docker compose -p cpa-postgres -f docker-compose.postgres.yml pull` followed by `docker compose -p cpa-postgres -f docker-compose.postgres.yml up -d --no-build`. Keep the same project name to retain volumes. Local builds remain supported; the following examples explicitly select a local image tag rather than retagging a published image.

## Existing collection switch

Enable the existing option in your active configuration; edit the current block rather than adding duplicate keys. For v8:

```yaml
observability:
  usage:
    usage-statistics-enabled: true
```

For v0:

```yaml
usage-statistics-enabled: true
```

There is **no additional usage enable flag**. The standalone default remains `false`; original v0/v8 parsing, management controls, and hot-reload behavior remain unchanged. Home retains its existing collection behavior outside this standalone attachment.

The original provider checks this gate when it handles a usage record. Turning collection off can suppress a record still waiting upstream; records already admitted to the adapter are not gated again. Disabling collection does not delete saved history or backfill the disabled interval. Historical queries, pricing, and saved/management-refreshed quota remain available subject to management authentication. Usage-derived quota-header updates stop when no records reach the adapter. The selected store still opens with collection off.

## Opt-in bundled PostgreSQL deployment

**This is an auth/config-storage choice, not a usage-only switch.** The [separate Compose file](../docker-compose.postgres.yml) is standalone, not an overlay. Do not combine it with the base file. It starts a private PostgreSQL service and uses the existing `PGSTORE_DSN`, `PGSTORE_SCHEMA`, and `PGSTORE_LOCAL_PATH` interfaces. Usage borrows the active `PostgresStore` connection pool and schema; it opens no second pool and has no separate database settings.

### Bootstrap and migration prerequisites

Understand the existing PGSTORE bootstrap before starting or migrating:

1. The CLI appends `pgstore/` below `PGSTORE_LOCAL_PATH`. The managed configuration is in `pgstore/config/config.yaml`; managed auth files are in `pgstore/auths/`.
2. An existing database config is authoritative and is written into the spool at startup. If the database has no config row, an existing spool config is used. Only when both are absent does bootstrap copy the working directory's [config template](../config.example.yaml). A normal config-path argument or mount alone is **not** the PGSTORE seed.
3. The bundled deployment therefore mounts `CLI_PROXY_CONFIG_PATH` (default `./config.yaml`) **read-only at `/CLIProxyAPI/config.example.yaml`**, the actual bootstrap seed path. The host file must already exist; `create_host_path: false` prevents silently creating a directory for a missing file. This does not overwrite the host configuration. Subsequent seed edits do not replace database configuration; use the existing management configuration workflow against the active store.
4. Existing PGSTORE bootstrap **removes and rebuilds its spool auth directory from database records**. The bundled deployment uses a dedicated `postgres-spool` volume and intentionally does not bind the old local auth directory there. Copying auth files into the spool before startup is not a migration and can lose those copies.
5. For an existing installation, back up its configuration, auth files, and relevant databases first. Plan and verify credential import through the existing authenticated management upload/login flows, or a separately reviewed PGSTORE migration, **before production cutover**. Verify credentials and configuration after a restart. Old host auth/config files remain untouched by this deployment; retain them and a rollback plan. It performs no automatic local-auth or usage-history migration.

The bootstrap behavior is implemented in the existing [server entrypoint](../cmd/server/main.go) and [PostgresStore](../internal/store/postgresstore.go); this deployment does not alter it. Do not delete database config or spool files merely to force reseeding.

### New-installation commands

Run from the repository root. Copy templates only if missing:

```sh
test -e config.yaml || cp config.example.yaml config.yaml
test -e .env || cp .env.example .env
chmod 600 .env
openssl rand -hex 32
```

Before starting:

- Set `POSTGRES_PASSWORD` in the environment file to the generated value. The optional `POSTGRES_USER` and `POSTGRES_DB` default to `cliproxy`. Use URL-safe username/database values and a hexadecimal password because the bundled DSN is constructed from them. These variables are not required by base Compose, native local mode, Home, or an external database.
- Configure real client API keys, desired providers, the existing usage switch, and a strong management secret in the selected seed configuration. A nonempty `MANAGEMENT_PASSWORD` is an alternative, but also enables remote management; review the security section.
- Keep `PGSTORE_LOCAL_PATH` as an absolute container path (default `/var/lib/cliproxy`); the named spool volume is mounted at that path. Do not set it to an existing auth directory or a location that masks application files. `PGSTORE_SCHEMA` defaults to `public` in this example.
- The file deliberately constructs `PGSTORE_DSN` for its bundled `postgres` service. An external shell value does not redirect it. For an external database, use your existing deployment instead, with the bootstrap prerequisites above.
- Resolve host-port conflicts with an existing installation as part of the planned cutover. The example retains the usual proxy/OAuth callback port mappings.

See [the environment example](../.env.example) for the exact settings. Build and start this fork explicitly:

```sh
# Choose your own LOCAL build tag; do not use a third-party published image tag.
export CLI_PROXY_IMAGE=cli-proxy-api:usage-local

docker compose -p cpa-postgres -f docker-compose.postgres.yml config --quiet
docker compose -p cpa-postgres -f docker-compose.postgres.yml pull postgres
docker compose -p cpa-postgres -f docker-compose.postgres.yml build cli-proxy-api
docker compose -p cpa-postgres -f docker-compose.postgres.yml up -d --no-build --pull never
docker compose -p cpa-postgres -f docker-compose.postgres.yml ps
docker compose -p cpa-postgres -f docker-compose.postgres.yml logs --tail=100 cli-proxy-api postgres
```

The explicit database pull above ensures its image is present before `--pull never`; it does not pull or retag the local proxy image.

Alternatively, when deliberately using a prebuilt image of this fork, set `CLI_PROXY_IMAGE` to that image, skip the build, and run `up -d --no-build` with the same file/project arguments. A plain `up` retains `pull_policy: always`; for your locally built image keep `--pull never --no-build` on subsequent starts. Building is always an explicit operator action in the documented flow, not a reinterpretation of the selected image name.

Open `/stats.html` on the proxy (default port `8317`) and authenticate using the **management key**, not a proxy client API key. Configure model prices and generate normal traffic.

PostgreSQL has no published host port, has a named `postgres-data` volume, and is the only service this deployment health-waits for. The spool has its own named `postgres-spool` volume. The chosen project name prefixes both volume names: keep it stable across restarts/upgrades. The store needs permission to create/use its tables and indexes in the selected schema. Failure of the configured PostgreSQL auth/config backend is not a request to switch silently to local history.

## Native and existing external deployments

To build locally instead of pulling the default GHCR image, deliberately choose a local image tag; do not enable PGSTORE or select the bundled file merely to get statistics:

```sh
export CLI_PROXY_IMAGE=cli-proxy-api:usage-local
docker compose -f docker-compose.yml config --quiet
docker compose -f docker-compose.yml build cli-proxy-api
docker compose -f docker-compose.yml up -d --no-build --pull never
```

Existing external deployments can instead retain their selected prebuilt image and custom environment mappings, updating that image to a build containing this feature through their usual process.

### Local journal

With no active `PostgresStore`, the adapter writes `<root>/usage/<instance-id>/usage.jsonl`. The root is the absolute existing `WRITABLE_PATH`, or the resolved `AuthDir` if that variable is absent. The instance ID is the full hexadecimal SHA-256 of the absolute configuration path, not of its contents. Moving that path selects a different instance directory; it does not delete or migrate the previous history. The config path identifies the instance but does **not** choose a writable location beside the configuration. `WRITABLE_PATH` is an existing shared writable-root setting, not a usage-only setting. The original lowercase `writable_path` convention remains supported.

For example, after building a binary of this fork:

```sh
# This is a persistent private directory you provision and back up.
WRITABLE_PATH=/absolute/private/state ./cli-proxy-api --config /absolute/config.yaml
```

Keep the selected state root and instance identity stable. Persist the root in a container/host mount; custom paths inside a checkout need appropriate Git/Docker exclusions. With base Compose, ensure the configured resolved auth directory really maps to a persistent mount; setting a custom auth directory outside the existing mount will not make it persistent automatically.

Local storage is single-writer: an OS exclusive lock held through `usage.jsonl.lock` rejects another opener before journal replay/writes. Stop the owner normally before moving/restoring its state. A remaining lock-file name is not itself proof of a live lock; do not delete it to bypass another writer. The supported local capacity is **64 MiB of total journal bytes and 100,000 unique retained events**, whichever would be exceeded first. These are fixed implementation limits, not another setting. Startup checks capacity before loading history or repairing an incomplete tail; an over-limit journal is left untouched and the proxy still starts with unavailable statistics. Runtime capacity rejection preserves existing readable history rather than evicting records. Metadata updates can continue at the event limit only if their appended bytes still fit; the byte limit also applies to pricing/quota updates and deletes. PostgreSQL has no such local limits. Use the existing PostgreSQL backend for larger/shared storage; do not share a journal between processes or rely on network-filesystem locking semantics. These limits bound retained data and cardinality, not exact process RSS; decoding and concurrent queries still need memory headroom.

Optional usage storage initializes independently of inference startup. While journal replay or usage-table initialization is pending, inference and original management remain available and authenticated statistics routes return 503. Successful initialization activates the existing registered routes without modifying the live router; a failure is logged without secrets and leaves statistics unavailable. Stopping the application cancels initialization and prevents a late successful result from attaching an observer. This is separate from failure of the deliberately configured core PGSTORE auth/config backend, whose original startup behavior remains unchanged. Fix permissions, lock ownership, supported capacity, or storage availability and restart; do not assume inference success proves usage persistence is healthy. Requests emitted before persistence attaches are not automatically backfilled.

### Reuse an existing PostgreSQL store

Continue using the existing `PGSTORE_DSN`, `PGSTORE_SCHEMA`, and `PGSTORE_LOCAL_PATH` settings; native lowercase `pgstore_dsn`, `pgstore_schema`, and `pgstore_local_path` forms remain valid. The adapter uses the **actual registered `PostgresStore` SQL handle and schema**, not a new DSN-derived connection pool. Its canonical tables are `usage_events` and `usage_metadata`. Closing the usage adapter does not take ownership of closing the shared pool.

Do not enable PGSTORE solely by adding a usage setting to an otherwise local deployment. Enabling PGSTORE changes core auth/config storage and requires the bootstrap/migration plan above. A correctly configured existing external database requires no bundled PostgreSQL container or health dependency. Use a certificate-verified encrypted connection for an external database, appropriate to its trust/CA setup; `sslmode=disable` in the bundled example is only for that private Compose network.

Native runs load the working directory's environment file. Compose interpolation is not container environment forwarding: [base Compose](../docker-compose.yml) keeps its original mappings, while [bundled Compose](../docker-compose.postgres.yml) explicitly supplies its PGSTORE variables. Existing custom mappings/overrides for external, Git, object-store, or Home deployments remain your deployment's responsibility. There is no automatic cross-store history migration or historical backfill.

## Worker, drop, and shutdown limits

The add-on observes output after the original built-in enqueue. Usage records, normalized management quota observations, and reset barriers share a **bounded in-memory queue of 256 sanitized records, plus at most one in-flight record**. Payloads above 256 KiB and event identity/provider/model/alias/auth-index fields above 1,024 bytes are rejected; invalid canonical records are also rejected. These are fixed admission bounds, not new configuration flags. Deferred work contains normalized fields, not raw provider JSON, credentials, arbitrary headers, or request contexts.

Admission does no storage I/O, never waits for queue space, and never queries the live credential manager: even a manager read lock can wait behind core auth-store I/O. One independent worker inserts accounting and then performs live quota verification, using its own lifecycle context rather than the completed inference request's context. Slow storage or credential-manager operations therefore do not execute in the original usage-dispatch callback; sanitization/admission still have CPU, allocation, and brief bookkeeping-lock overhead. Queue saturation drops work rather than waiting for persistence capacity.

Original management observations use only an atomic, immutable copy of previously obtained identity evidence. An explicit full add-on identity/cache/quota read primes that copy; the normal bridge performs its identity read before quota operations. There is no periodic refresher. If proof has not been primed, optional observation is skipped while the original request still completes. Published proof can be stale and is never mutation authority: the independent worker rechecks the captured generation/revision against the live source before writing. Live checks can still wait in the isolated worker or explicit add-on APIs, but never on either side of the original handler or shared usage callback.

Each admitted snapshot receives one persistence attempt; a failure is counted/logged and the worker moves on. **There is no automatic retry or durable retry spool.** Event insertion and associated quota updates are not one atomic transaction, so a reported write failure can also mean an event persisted but a subsequent quota write did not.

Standalone CLI cleanup detaches the observer and cancels the storage/worker lifecycle before closing it. Pending snapshots can therefore be discarded even during normal CLI shutdown. The internal store's graceful-close API can wait for admitted work when supplied a live context, but that does not upgrade the CLI's shutdown guarantees. Cancellation can bound waiting for a backend; it cannot forcibly interrupt every filesystem operation, and safe cleanup can complete later. The upstream SDK queue is unchanged and is not durably drained by this add-on. Neither admission into memory nor normal shutdown guarantees every execution reaches durable storage.

Monitor authenticated `/v0/management/stats/status`:

- `collection_enabled` and `storage` identify the original collection gate and selected backend.
- `queue_capacity` is 256; `pending_events` includes queued/in-flight work.
- `queue_overflows`, `validation_failures`, `dropped_events`, and `last_drop_at` expose rejected/discarded ingress. Write failures have separate counters rather than necessarily increasing `dropped_events`.
- `write_failures` and `last_write_failure_at` report failed persistence attempts, including possible partial event/quota success.
- `local_capacity`, when present, reports journal bytes/event counts, their supported limits, and capacity rejections without waiting for a filesystem lock. A rejection can occur before the exact byte limit if the next record would not fit.
- `skipped_quota_headers` and `quota_header_scope` distinguish refused quota attribution from lost usage accounting. These are diagnostics, not collection controls.

These are process-local health counters, not a durable audit trail. Original management quota/API-call/reset responses do not wait for optional persistence: sanitized observations and receipt-time reset barriers are admitted nonblockingly. The same drop-new overflow policy applies to resets, with no unbounded priority or retry path; a full queue can leave previously saved quota until another verified observation arrives. Explicit statistics mutations, such as editing prices or writing the display cache, still return their own storage errors. Successful journal writes sync to disk; SQL event insertion commits through the borrowed pool. None of these guarantees makes this exactly-once or billing-grade accounting.

A recorded event represents a **provider execution**, not necessarily one inbound HTTP request. Retries/failover can produce multiple execution events for one client operation. Execution IDs deduplicate repeated delivery of the same execution, not distinct retry executions. Token-counting/non-generation records are excluded. Failures before usage publication, interrupted streams, absent upstream counts, collection-off intervals, and drop/write failures can all reduce recorded coverage. Completed streams are recorded through the existing terminal usage publication rather than per content chunk.

There is no automatic pruning, journal compaction, scheduled backup, or backfill. Local history stops accepting writes that exceed the supported capacity instead of silently deleting records; it does not grow without limit. Local event pagination retains only the bounded page-selection keys rather than copying every matching event, although counting/filtering still scans retained history and deep offsets can select many keys. PostgreSQL history and broad analysis scans still require operator-managed storage and workload planning.

## Security and management access

- All statistics data APIs reuse management authentication: `Authorization: Bearer <management-key>` or `X-Management-Key`. Client API keys do not grant management access. Never put management keys in URLs.
- A nonempty `MANAGEMENT_PASSWORD` enables remote management even when `management.allow-remote` is false. Docker-forwarded traffic may not appear local. Use HTTPS and trusted/firewalled access.
- The example proxy port mappings bind all host interfaces. Restrict them explicitly, for example to `127.0.0.1:8317:8317`, or enforce suitable network controls. No database host port is published, but same-network containers can reach PostgreSQL.
- Disabling control-panel assets or running Home hides standalone statistics assets; turning collection off does not disable historical data APIs. Hiding assets does not revoke management credentials.
- The statistics page uses same-origin credential headers. Credentials stay in memory unless optional tab-only remembering uses `sessionStorage`; they are not placed in URLs or persistent `localStorage`, nor imported from the original management page. Disconnect/forget on shared machines.
- Sanitized usage state excludes raw API keys, provider tokens, auth-file contents, prompts/completions, arbitrary headers/URLs, and upstream error bodies. API keys become stable SHA-256 identifiers. The original provider's own queue/logging behavior is not sanitized or changed by this add-on.
- Hashes, auth indices, and quota lookup filenames remain sensitive correlation data. Protect state, exports, and backups. **A shared PGSTORE database also contains original config/auth secrets**, unlike sanitized usage events alone.
- Rendered Compose configurations and container environments can expose secrets/DSNs. Do not post them publicly. Git/Docker ignores cover the documented repository-root state locations, not arbitrary custom paths.

## Pricing and provider quota

Token breakdown follows existing canonical accounting: cache reads/writes are input components and reasoning is an output component, not extra totals. Missing/unclassified accounting is not invented. Rates are USD per million tokens. Missing rates remain **unpriced**, not free; explicit manual zero rates are different from missing catalog values. Manual prices override synchronized Models.dev prices. Verify provider/model aliases and all four rate fields.

Rates apply at query time, so changing them can change historical estimates. This does not model all subscription allowances, negotiated discounts, cache lifetimes, regional/long-context/service tiers, image/audio/tool charges, or taxes. Catalog synchronization requires outbound access; it does not upload usage or management keys to the catalog.

Provider quota is independent of local usage. Saved state requires a current server-derived credential generation, not just a filename or auth index. **The core usage producer remains unchanged:** automatic Codex header observations are intentionally skipped because its existing payload lacks the account selector needed to distinguish same-token account changes. Usage accounting still persists. Only verified token-scoped Claude header observations are eligible for automatic attribution; scoped/unknown header evidence is refused rather than guessed. Generic rate-limit headers are not inferred as account balances. Management observations use bounded capture and nonblocking admission; optional database/filesystem work does not delay the original handler response.

For the pinned management bundle, manual-refresh source checks cover the actual **Codex account selector**, **Antigravity project resolver result**, and **xAI unpaid user-selector header**. Fixed-token/index paths have narrower checks. The authenticated identity response can supply selector digests; the bridge keeps them in private sidecars, never in cache PUTs, quota UI state, or diagnostics. Wrappers preserve original requests and results and make no additional provider calls. This is not universal proof for every provider: **Meta's literal DCA bearer, read from downloaded file content, is not covered by the current selector-proof contract.** Its original manual UI still works, but that download-to-request path must not be described as source-proof complete. See the [adapter's precise coverage](../internal/usagepersist/web/README.md#quota-fidelity).

Header observations use the existing request timestamp plus latency heuristic, not an independently measured upstream capture time. Long streams, merged/reused headers, and late WebSocket frames limit freshness. Stored timestamps/reset estimates are operational hints, not independently timestamped billing evidence. A passed reset timestamp does not prove renewed allowance; refresh through the original management flow.

A verified reset observation invalidates saved state for its matching credential generation when its queued write succeeds. Missing identity proof, overflow, or write failure can leave older history; failed or unrecognized responses do not prove a completed provider reset. Receipt-time cutoffs reject older observations according to the available evidence. The bridge fences old operations and clears affected display state; it does not initiate provider resets.

The canonical browser assets are under `/stats-assets/`; the original quota page bridge exposes `window.CPAQuotaPersistence.status` and the `cpa-quota-persistence-status` diagnostic event. Inspect `X-CPA-Quota-Persistence` (`enabled` or `unsupported`) on management HTML to diagnose attachment; an absent header can mean the adapter is inactive or the response passed through. The adapter recognizes known bundle shapes and does not rewrite the downloaded source file. Only a management-document GET uses a cloned request with cache revalidation headers removed, so an old cached pre-bridge document cannot remain active through a 304. The original request object is restored; inference requests, HEAD, range handling, and other preconditions are not rewritten. Unknown/encoded/oversized/partial documents and unsupported response paths pass through rather than being forced into an injected page.

The original quota page has **no new historical-freshness badge**. Restored success can be old and is not proof of live remaining allowance. Hydration fills absent/idle entries; it does not replace loading, error, or live-success states. Replacement/deletion invalidates old-generation eligibility even when filename, index, email, or copied file timestamps are unchanged. Unknown/generationless state is not assigned to a new account.

The separate runtime revision fences operations, not persistent account identity. Core `Generation` can advance on ordinary execution results, so overlapping operations may conservatively lose persistence eligibility; revisions are not guaranteed stable during benign activity. Revision-only changes preserve settled same-generation success/error/idle values and their freshness sidecars while fencing old operations; obsolete loading states are not restored. A late stale A upload returning 409 cannot clear replacement B's display or discard its newer valid upload. Failed observations are never re-stamped for a newer binding. Stable-generation history can survive a restart, but old operation revisions cannot be reused. The [adapter documentation](../internal/usagepersist/web/README.md#quota-fidelity) describes these boundaries.

## Statistics API and integration boundary

All data routes are under `/v0/management/stats` and reuse the existing management authentication:

| Method and suffix | Purpose |
| --- | --- |
| `GET /overview`, `GET /analysis` | Aggregates and time series |
| `GET /events`, `GET /live` | Recorded/recent completed executions |
| `GET /filters` | Available filter values |
| `GET /status` | Collection gate, backend, process-local ingestion health |
| `GET /pricing`, `PUT /pricing` | Read rates/save a manual override |
| `DELETE /pricing?model=...` | Remove a manual override |
| `POST /pricing/sync` | Synchronize catalog rates |
| `GET /quota` | Saved normalized quota snapshots filtered by current credential generation |
| `GET /quota/identities` | Current secret-free credential-generation and operation bindings |
| `GET /quota/cache`, `PUT /quota/cache` | Read/upsert sanitized original UI display state with mandatory identity preconditions |

Manual prices require `model` plus finite, nonnegative, non-null `input_per_million`, `output_per_million`, `cache_read_per_million`, and `cache_write_per_million`; explicit zero is allowed. There is no separate read-only statistics role. Filters use `from`/`to` (RFC3339, inclusive/exclusive), `provider`, `model`, `auth_index`, `key_id`, and `status` (`success`/`failed`). Missing dates mean the last 24 hours; analysis buckets are UTC `hour`/`day`, and event pagination uses `limit` (1–500) and `offset`.

Feature code lives under `internal/usagepersist`, with a passive built-in-output observer, API adapter, and standalone CLI attachment. It does not replace/dequeue the original output, register a competing SDK usage collector, change the existing enable gate, or add inference/executor hooks. Automatic attachment is CLI-only; embedding the SDK does not automatically enable this store/page. External SDK users cannot assume Go `internal` packages are a stable public add-on API.

## Backups and upgrades

Both named volumes survive ordinary restarts/recreation and `down` when the same project/file selection is used. **`docker compose ... down -v` deletes named volumes**, including the bundled auth/config database, usage history, prices, and quota state. It is not an upgrade step.

Back up the bundled database without publishing a database port, preferably to a private directory outside the checkout:

```sh
umask 077
docker compose -p cpa-postgres -f docker-compose.postgres.yml exec -T postgres sh -c \
  'pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc' \
  > /absolute/private/backups/cliproxy.dump
```

Check exit status and test restoration into an isolated database before relying on the backup. Back up the seed configuration, relevant spool/local files, environment secrets, and any old auth directory separately. Preserve credential confidentiality throughout.

For local usage, stop its writer normally and copy the entire instance directory; do not edit a live journal or remove a live lock to force access. Incomplete final journal writes may be repaired at startup, while corrupt complete records fail instead of silently dropping retained history. Keep the backup before attempting recovery.

PostgreSQL initialization settings apply to an empty data directory. Editing the password environment value does not rotate an existing database role's password; coordinate SQL credential changes and application settings. The example PostgreSQL image is major-version pinned to `17-alpine`, not digest pinned. Plan upgrades/security updates deliberately, and never switch major versions against a populated data directory without a migration/dump-restore plan.

Before upgrading, back up and explicitly rebuild/select this fork's image, restart with the same file/project/state paths, and verify a known persisted event and price. There is no automatic cross-backend migration, history-reset API, or provider-billing reset from deleting local usage. Do not delete or rename user data as a package/deployment cleanup operation.

## Validation and operational checks

### Recorded validation — September 29, 2026

The tested worktree passed the full Go suite and required server build, integrated race tests, 58 Node regressions, real PostgreSQL/schema tests, and tests using the actual compiled management fixture. These are results for that snapshot, not guarantees for later upstream bundles or deployments.

- **Browser:** the real React Codex quota page saved/reloaded server-backed state, rejected old state after a new-token replacement, refused persistence of a stale same-token account selector, and recovered through a fresh operation. Identity/source-proof responses and cache APIs were real; provider lookup bodies were fixtures. Other providers' full browser paths were not exercised.
- **Statistics:** three synthetic rows produced three executions, 4,500 tokens, and USD 0.0114 after the real manual form saved all four rates. Desktop and mobile layouts passed. Those rows test UI/query/pricing integration, not provider accounting capture.
- **Executable wire checks:** legacy and v8 configurations passed fixture JSON, SSE, and downstream Responses WebSocket comparisons, plus a real SQL-lock test in which inference and original usage-queue delivery continued before storage was unlocked. The baseline had the adapter attached with collection off; this was **not** an unpatched-upstream versus patched-binary comparison. No request mutation was found in the inspected paths or exercised comparisons, but native provider WebSocket transports, every request shape, retries, and interrupted streams were not covered by that smoke test.

No live-provider inference or Docker/container/bootstrap validation was performed. A preliminary original Antigravity metadata lookup occurred before wire-test isolation; later fixtures blocked the updater. Do not infer zero external network activity for the entire session. The browser and wire tests used isolated credentials, databases, and local fixtures; they do not establish hardware durability, all-platform runtime behavior, or billing accuracy.

### Repeat locally

Run the checks appropriate to your deployment; the Docker commands below are instructions, not recorded Docker validation:

```sh
go test ./...
go test -race ./internal/usagepersist/... ./internal/redisqueue/...
go build -o test-output ./cmd/server && rm test-output
node --test internal/usagepersist/web/web_test.cjs

docker compose -f docker-compose.yml config --quiet
docker compose -p cpa-postgres -f docker-compose.postgres.yml config --quiet
```

The bundled config validation requires its database password and intended seed path. If Docker CLI is unavailable, YAML parsing/static inspection can catch syntax/structure errors but **cannot validate Compose interpolation, pull/build policy, volume mounting, container health, or runtime bootstrap**. Do not claim those checks from a YAML parse alone.

Real-database tests require an isolated PostgreSQL database and explicit opt-in; never use production credentials. The [PostgreSQL test](../internal/usagepersist/postgres_test.go) creates/drops its own test schema with `CASCADE`, so the role needs suitable permission. `PGSTORE_TEST_DSN` is only a test input, not a new runtime setting:

```sh
PGSTORE_TEST_DSN='<isolated PostgreSQL test database DSN>' \
  go test -race -run '^TestPostgres' ./internal/usagepersist
```

A management-bundle fixture test is also available:

```sh
CPA_MANAGEMENT_FIXTURE=/absolute/path/to/compiled/index.html \
  go test -run '^TestActualManagementBuild$' ./internal/usagepersist/web
```

For deployment smoke tests, verify the intended seed/DB config and auth inventory **after restart**, generate a known execution with collection on, wait for visibility, restart, and check both the event and a saved price. Toggle collection off/on while historical/pricing/management-quota operations remain available. Confirm unauthorized data access fails, the original quota UI still refreshes/resets correctly, and the database has no published host port. Test backup restoration separately. A visible stored event proves that event persisted, not that the upstream shutdown tail or every retry was captured.
