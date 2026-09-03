# Telemetry ingest

A small self-hosted service that accepts batched, anonymous usage events from
the [Git Graph Libre](https://github.com/PlohnenSoftware/git-graph-libre)
VS Code extension and writes them to Postgres. Two routes plus a courtesy
redirect, one table, no framework, no dashboard.

The question this exists to answer: **which features do people actually use, so
the maintainer knows what to improve.** Everything else is out of scope.

**This service stores nothing that could identify or deanonymize a user.** What
lands in Postgres is exactly the payload the extension documents under
[Telemetry](https://github.com/PlohnenSoftware/git-graph-libre#telemetry) and
nothing more — no geolocation, no request headers, no user agent,
no cookie, no account, no email, and no access log of any kind. The ingest never
enriches a row, never derives a new field, and never reads anything but the JSON
body it was posted. There is no identifier here that maps to a person and no
second field to pivot through, so the data cannot be joined against another
dataset to work back to one. See
[What is stored, and what cannot be](#what-is-stored-and-what-cannot-be).

This README is deliberately standalone: the service is a separate deployable
that shares nothing with the extension's build or toolchain, and the folder is
written so it can live in (or be extracted to) its own repository.

## How the data arrives

```
VS Code extension (Git Graph Libre)
  └─ env.createTelemetryLogger(sender)   ← consent gate + PII scrubbing (VS Code)
       └─ EventQueue (batch 25 / flush 30s)
            └─ POST https://<this-service>/v1/events
                 └─ Go ingest (net/http)
                      └─ Postgres  ← read directly with DataGrip
```

The extension never calls this service from feature code. Everything goes
through VS Code's `env.createTelemetryLogger()`, which gates on the user's
global telemetry consent and scrubs paths, file URIs, and usernames before
anything leaves the editor. The client also enforces its own exclusions — no
file names or paths, no workspace or repository names, no remote URLs, no
branch or tag names, no commit hashes or messages, no author identities, no
environment variables, no credentials, no installed-extension list. That side
is documented in the extension repository (`telemetry.json`, inspectable via
`code --telemetry`).

This ingest is the only receiver of that data, and it stores a strict subset of
it: the columns in [`schema.sql`](schema.sql) plus one server-side receipt
timestamp. Nothing is forwarded anywhere else — no third-party analytics, no
hosted product, no CDN beacon, no export.

Two containers — this ingest plus Postgres — are the whole backend.


## Event model

Two event types only. Not one bespoke event per feature — one `feature` event
with an id string means adding a feature costs zero telemetry work and every
question is a single `GROUP BY`.

| Event | When | Carries |
| --- | --- | --- |
| `activate` | once per session | environment + which settings the user changed from defaults (that they changed, never what to) |
| `feature` | per invocation | `{ "feature": <command id>, "ok": <boolean> }` |

`createTelemetryLogger` injects a fixed set of `common.*` properties into every
event. The ingest splits the known ones into real columns
(`mapevent.go`); anything else lands in the `props jsonb` column.

| Injected key | Column | Why it is useful |
| --- | --- | --- |
| `common.vscodemachineid` | `machine_id` | Reach denominator. Anonymized per install by VS Code. |
| `common.vscodesessionid` | `session_id` | Group events from one window. |
| `common.extname` | `ext_name` | Sanity check that events are ours. |
| `common.extversion` | `ext_version` | Error rate per release; adoption curve. |
| `common.vscodeversion` | `vscode_version` | Which VS Code versions can be dropped. |
| `common.product` | `product` | Code / Insiders / VSCodium / Cursor. |
| `common.os` | `os` | QA matrix. |
| `common.nodearch` | `node_arch` | QA matrix. |
| `common.platformversion` | `platform_version` | QA matrix. |
| `common.uikind` | `ui_kind` | Desktop vs web. |
| `common.remotename` | `remote_name` | `wsl` / `ssh-remote` / `dev-container` — the extension advertises devcontainer support; this validates it. |
| `common.isnewappinstall` | `is_new_install` | Separates first-run behavior from steady state. |

## API

```
POST /v1/events
content-type: application/json

{ "events": [ { "name": "feature", "ts": 1756100000000,
                "data": { "feature": "pushTag", "ok": true, "common.extversion": "1.3.0" } } ] }
```

| Status | Meaning |
| --- | --- |
| `204` | Accepted |
| `400` | Malformed body, unknown event name, or malformed feature id |
| `413` | Body over 64 KB |
| `503` | Database unreachable |

```
GET /healthz  ->  200 {"ok":true,"database":true}   |   503 when Postgres is down
```

Every other `GET` — `/`, `/favicon.ico`, even `GET /v1/events` — answers
`302` to <https://github.com/PlohnenSoftware/git-graph-libre>. There is no UI
here, so a browser pointed at the ingest lands on the project instead of on
`404 page not found`. Non-`GET` methods still get `405`.

`/healthz` is also the container's own health check. The runtime image is
distroless — no shell, no curl — so the probe is the binary re-invoked as
`telemetry-ingest healthcheck`, which GETs `http://127.0.0.1:$PORT/healthz` and
exits non-zero on anything but `200`. It lives in the `HEALTHCHECK` stanza of
the Dockerfile; do not add a `healthcheck:` key for `ingest` to the compose
file, because an override there suppresses it and Coolify goes back to
reporting "Healthcheck: not configured".

### Validation

All cheap, all in `validate.go`:

- body must be an object with an `events` array of `1..100` entries
- request body capped at 64 KB, enforced before parsing
- `name` must be `activate` or `feature`
- for `feature`, `data.feature` must match `^[a-z][a-zA-Z0-9._-]{0,63}$`
- `data` values must be primitives; nested objects are dropped, not stored
- at most 64 properties per event, 512 characters per string value

A per-feature-name whitelist was considered and deferred: it would mean keeping
a second list in sync across two deployables. The charset and length limits
plus the two-event-type whitelist are enough to make junk cheap to reject.
Revisit if the endpoint is ever actually abused.

## Storage

One table, defined in [`schema.sql`](schema.sql) — the source of truth for
columns and indexes. The schema is applied idempotently on every boot, so a
redeploy is a no-op rather than a migration step anyone has to remember.

### What is stored, and what cannot be

**The stored rows are the extension's documented
[Telemetry](https://github.com/PlohnenSoftware/git-graph-libre#telemetry)
payload, minus whatever the validator rejects, plus a `received_at` timestamp
the server sets itself.** That is the complete list. Every column exists in
`schema.sql`; there is no shadow table, no side channel, and no field this
service invents about whoever sent the request.

| Never stored | Why it cannot show up later |
| --- | --- |
| **Geolocation** — country, region, city, ASN | Would require an IP, which is never read. Avoiding a hosted product's automatic IP geolocation was a main reason to write this ingest at all. |
| **Request headers, user agent, cookies, referrer** | Nothing outside the JSON body is parsed or persisted. There is no session and no cookie; the endpoint is unauthenticated by design, so there is not even a token to tie requests together. |
| **Access log or request log** | There is none. `slog` emits startup, shutdown, and insert failures only; the single request-path line is an error string plus a row count — never a body, an address, or a property value. |
| **Names, emails, accounts, licenses** | Excluded on the client, and there is no column here any of them could land in. Nothing in this system has a user account to begin with. |
| **File, path, workspace, repo, branch, tag, commit, or author data** | Scrubbed by VS Code, excluded by the extension's `telemetry.json`, and absent from this schema. Nested objects in `data` are dropped rather than stored, so a structure carrying them cannot sneak through. |
| **Anything joinable to another dataset** | No account id, no install token, no license key, no cross-service correlation id. A leaked dump of this table joins to nothing. |

The two identifier-shaped columns are the closest thing to a pseudonym in the
schema, and neither is a person:

- **`machine_id`** is VS Code's own `common.vscodemachineid` — an opaque value
  VS Code anonymizes per install, shares across all extensions, and resets on an
  OS reinstall. It is not derived from a hardware serial, a hostname, a MAC
  address, or a user name. It exists here purely as the denominator in
  `count(distinct machine_id)`.
- **`session_id`** groups the events of one editor window and is meaningless
  once that window closes.

Neither can be reversed into a name, an email, a host on a network, or a GitHub
account, and — with no IP, no headers, and no log — this database offers no
second field to pivot through. That absence is the point: dropping IPs is what
makes the rest of the schema safe to keep.

One honest caveat about where the guarantee is enforced: the ingest can only
decline to store what it is sent, and unknown properties fall through to the
`props jsonb` column. The client's `telemetry.json` is therefore the real
enforcement point for *what is collected*, and any new property has to be
argued for there first. What this service guarantees is the other half — that it
adds nothing of its own, and that a row can never grow an identifying field
between the request arriving and the insert.

## Reading the data

Postgres is not on the `coolify` network and publishes only to the VPS's own
loopback, as `127.0.0.1:55432`. Nothing from the internet can reach it: 80/443
are open on the host firewall and Docker bypasses UFW, so a wildcard publish
would put the database online, but a loopback-bound one only answers traffic
that already arrived on the host — which is what an SSH tunnel delivers. The
non-default host port keeps it clear of the other Coolify apps on the same box.

Reach it from DataGrip over an SSH tunnel. Configure the data source once and
the tunnel is saved with it; there is nothing to set up per session:

| DataGrip field | Value |
| --- | --- |
| Host | `127.0.0.1` — resolved on the VPS, because the tunnel is on |
| Port | `55432` |
| Database / User | `telemetry` / `telemetry` |
| Password | Coolify → this app → Environment Variables → `SERVICE_PASSWORD_POSTGRES` |
| SSH/SSL tab | *Use SSH tunnel* → the VPS host, its user, and your key |

The equivalent by hand, if you would rather tunnel outside the IDE:

```bash
ssh -N -L 55432:127.0.0.1:55432 user@your-vps
```

For a quick look without any of that, `docker exec -it $(docker ps -qf
name=postgres-git-graph-libre-telemetry) psql -U telemetry -d telemetry` on the
VPS.

### The query that answers the question

```sql
select feature,
       count(distinct machine_id)           as installs,
       count(*)                             as uses,
       round(100.0 * avg((not ok)::int), 1) as fail_pct
from events
where event = 'feature'
  and received_at > now() - interval '28 days'
group by feature
order by installs desc;
```

Rank work by **`installs`**, not `uses`. A feature used 200 times by 3 power
users and one used 200 times by 150 people demand opposite decisions, and raw
counts cannot tell them apart. `fail_pct` finds features that are broken rather
than merely unused.

### Two caveats that matter when reading any of this

- **These numbers are a biased lower bound, not a user count.** Telemetry is
  off for everyone who disabled it, everyone under enterprise policy, and
  effectively all VSCodium users. Good for ratios and trends within the
  consenting population; absolute counts do not belong in a README or a report.
- **`machine_id` is per machine, not per person.** Laptop plus devcontainer
  plus Codespace is three ids, and an OS reinstall resets it, so retention
  curves read pessimistically.

## Configuration

| Variable | Required | Default | Notes |
| --- | --- | --- | --- |
| `DATABASE_URL` | yes | — | Postgres connection string. Startup fails loudly without it. |
| `PORT` | no | `8080` | Startup fails on an invalid value rather than binding somewhere unexpected. |

## CI Image Builds (GitHub Actions → GHCR)

The ingest image is **built and tested in GitHub Actions and pushed to GHCR**,
not built on the Coolify server. See
[`.github/workflows/docker-publish.yml`](.github/workflows/docker-publish.yml),
which uses the
[Docker Build and Coolify Deploy](https://github.com/marketplace/actions/docker-build-and-coolify-deploy)
action.

| Image | From | Used by |
| --- | --- | --- |
| `ghcr.io/plohnensoftware/git-graph-libre-telemetry` | `./Dockerfile` | `ingest` |

Tags pushed from `main`: `main`, `latest`, `sha-<full-commit-sha>`.
`docker-compose.yml` tracks `main` and sets `pull_policy: always`, so a redeploy
always re-fetches rather than reusing the cached image.

A `test` job (`go vet ./...`, `go test ./...` — hermetic, no database) gates the
publish, because Coolify may pull a published tag at any time.

This repo publishes a single image, so the Coolify redeploy trigger rides on
the publish step itself rather than in a job of its own (the split job exists
for image matrices, where a webhook per leg would deploy a half-published set).
It needs a repository **variable** `COOLIFY_WEBHOOK_URL` (this app's Deploy
Webhook URL, with `force=true`) and a repository **secret** `COOLIFY_TOKEN` (a
Coolify API key with deploy permission). Both must be repo-level: on the GitHub
Free plan, org-level Actions secrets are unavailable to private repositories
and resolve to an empty string with no error. The step skips itself when either
is missing, so a push before configuration still publishes the image.

To roll back, point the image env var at an immutable tag and redeploy:

```
INGEST_IMAGE=ghcr.io/plohnensoftware/git-graph-libre-telemetry:sha-<commit>
```

Build locally without CI (the compose file has no `build:` stanza by design):

```bash
docker build -t telemetry-ingest:local .
INGEST_IMAGE=telemetry-ingest:local docker compose up ingest
```

Note the compose file joins the external `coolify` network, so a full `up`
only works on the Coolify host; locally, bring up Postgres and `go run .` as
described under [Local development](#local-development).

The host's Docker daemon needs GHCR credentials once — a **classic** PAT
scoped to `read:packages` (ghcr.io rejects fine-grained tokens), installed as
root:

```bash
docker login ghcr.io -u <github-username>
```

## Deploying on Coolify

1. Point the app at this directory's `docker-compose.yml` (Build Pack: Docker
   Compose).
2. Set the Domain **with the scheme** — `https://tel.example.com`, not a bare
   hostname. A bare hostname makes Coolify emit broken Traefik labels (empty
   `Host()`, the domain treated as a path).
3. Leave the `coolify` network membership and the
   `traefik.docker.network=coolify` label alone. Both are required; without
   them the route returns 504 after the next unrelated deploy, because
   Coolify's Traefik has no default docker network and eventually resolves the
   container's unreachable private-network IP.
4. Coolify generates and persists `SERVICE_PASSWORD_POSTGRES` itself.
5. For CI deploys: copy the app's Deploy Webhook URL (Webhooks page) into the
   `COOLIFY_WEBHOOK_URL` repository variable, and put a Coolify API key with
   deploy permission into the `COOLIFY_TOKEN` repository secret.

If a route starts 504ing, `docker restart coolify-proxy` forces Traefik to
re-resolve endpoints.

## Local development

```bash
docker compose up -d postgres-git-graph-libre-telemetry
export $(grep -v '^#' .env.example | xargs)
go run .
go test ./...
```

The test suite needs no database: the pure modules are tested directly and the
handlers take a `store` interface that a fake satisfies.

## Code map

| File | Role |
| --- | --- |
| `main.go` | Entrypoint, config, graceful shutdown, `healthcheck` probe |
| `app.go` | `net/http` handlers, size cap, `/healthz`, project redirect |
| `validate.go` | Pure: shape + charset validation |
| `mapevent.go` | Pure: `common.*` → column mapping |
| `db.go` | pgx pool, schema-on-boot, batch insert |
| `schema.sql` | Table + indexes (source of truth) |
| `*_test.go` | stdlib `testing`, no config, no database |
| `.github/workflows/docker-publish.yml` | CI: test, build, push to GHCR, Coolify redeploy |
