# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Backend service for SDC club management. Go module path is `nycu-sdc/club-manager` — a **bare path**, not a URL, so this module cannot be `go get`-ed. Import internal packages as `nycu-sdc/club-manager/internal/...`. Toolchain pinned to Go 1.26.2.

Endpoints: `GET /api/healthz` (open); Google OAuth login at `GET /api/auth/google/login` + `/callback`, `POST /api/auth/refresh`, `POST /api/auth/logout`; and `GET /api/groups` + `GET /api/groups/{group_key}/members` (JWT-protected), backed by the Google Workspace Admin SDK. A Postgres pool and migration wiring exist but nothing queries the database yet.

## Commands

```bash
make run      # config.yaml + Postgres + build + start (DEBUG=true); Ctrl-C to stop
make build    # -> bin/backend
make test     # go test -cover ./...
make lint     # golangci-lint run ./...
make up/down  # local Postgres only, keeping data
make clean    # tear down containers + volumes, remove bin/
make token    # mint a JWT for testing; TOKEN=$(make -s token)

go test -run TestName ./path/to/pkg    # single test
go vet ./... && gofmt -l .
```

`make run` creates `config.yaml` from `config.example.yaml` when it is missing and never overwrites an existing one. Local dependencies live in `.deploy/local/compose.yaml` (Postgres only — the mailing list cache is in-process, and there is no Redis or LDAP).

`make gen` is a guarded stub: it no-ops while no `queries.sql` exists, and fails with instructions once one appears. Wiring it up means copying summer's `resource/scripts/create_sqlc_full_schema.sh` into `scripts/` and restoring clustron's merge -> `sqlc generate` -> `mockery` pipeline.

Lint runs on golangci-lint defaults; there is deliberately no `.golangci.yml`, matching clustron.

## Shared library: NYCU-SDC/summer

Do **not** hand-roll logging, migrations, middleware, or error responses. They live in `github.com/NYCU-SDC/summer`. Note that directory names and package names differ, so **import aliases are required**:

| Import path | Package | Provides |
|---|---|---|
| `summer/pkg/log` | `logutil` | `ZapDevelopmentConfig`, `ZapProductionConfig`, `WithContext` |
| `summer/pkg/config` | `configutil` | `Merge[T]` — reflection-based non-zero-field overlay |
| `summer/pkg/database` | `databaseutil` | `MigrationUp/Down`, `WrapDBError*`, pg error sentinels |
| `summer/pkg/trace` | `traceutil` | `TraceMiddleware`, `RecoverMiddleware` |
| `summer/pkg/handler` | `handlerutil` | `WriteJSONResponse`, `ParseAndValidateRequestBody`, `ParseUUID` |
| `summer/pkg/middleware` | `middleware` | `Set` chain builder — `NewSet(...).Append(...).HandlerFunc(h)` |
| `summer/pkg/cors` | `corsutil` | `CORSMiddleware` |
| `summer/pkg/problem` | `problem` | RFC 9457 `application/problem+json` responses |

`internal/trace` and `internal/cors` are deliberately thin adapters that wrap the summer functions in a struct, so `main.go` reads uniformly as `NewMiddleware(logger, ...)`.

## Conventions

Mirror `/home/umineko/clustron/clustron-backend` — the org's current production reference, same bare-module style and same Go version. `~/sdc/backend-training` and `~/sdc/eng-training-social-backend` are older and predate the shared library; don't copy from them.

- **stdlib `net/http` only.** No chi/gin/echo. `http.NewServeMux()` with Go 1.22 method patterns (`mux.HandleFunc("GET /api/groups/{group_id}", ...)`), params via `r.PathValue`.
- **Middleware signature is `func(http.HandlerFunc) http.HandlerFunc`**, not `func(http.Handler) http.Handler`. Compose with `middleware.Set`; recover goes first, then trace.
- **CORS wraps the whole mux once** at the entrypoint, never per-route.
- **Routes** live in `main.go` — there is no router package. All under `/api/`, camelCase segments, snake_case wildcards (`{group_id}`).
- **`*zap.Logger` is the first constructor argument** to every service, handler, and middleware. No global or context-stored logger; enrich per-request with `logutil.WithContext(ctx, logger)`.
- **Domain packages are vertical slices** under `internal/<domain>/`: hand-written `handler.go` (HTTP + Request/Response DTOs + a consumer-side `Store` interface), `service.go`, `queries.sql`, `schema.sql`; sqlc-generated `db.go`, `models.go`, `queries.sql.go`.
- **Startup order in `main.go` is load-bearing**: config → validate → logger → `cfgLog.FlushToZap` → migrations → pool → middleware → mux → signal ctx → serve → graceful shutdown. Migrations run *before* the pool is created.

## Config

Four layers, each merged via `configutil.Merge`: **defaults → `config.yaml` → `.env`/env → CLI flags** (later wins). `config.yaml` and `.env` are gitignored; `config.example.yaml` is committed.

The `envconfig:"..."` struct tags are **documentation only** — nothing reads them. Env vars are read by explicit `os.Getenv` calls in `FromEnv`. Adding a field means touching four places: the struct, `Load` defaults, `FromEnv`, and `FromFlags`.

`LogBuffer` exists because config must load before the logger does (`cfg.Debug` selects the logger config). Warnings during load are buffered, then replayed by `cfgLog.FlushToZap(logger)`.

## Google mailing lists

`internal/googlegroup` reads Google groups and their members via the Admin SDK Directory API (`admin/directory/v1`), authenticating as a **service account using domain-wide delegation** — `google.JWTConfigFromJSON` with `Subject` set to a Workspace admin. This is the org's first use of `google.golang.org/api`; there is no pattern to copy from other repos.

Two scopes are requested: `admin.directory.group.member.readonly` (members) and `admin.directory.group.readonly` (group listing). **Changing that list is not a code-only change** — domain-wide delegation is granted against an exact scope list, not merged, so the Workspace admin console entry must be re-saved with the full set. A stale grant fails every call with `unauthorized_client`, surfacing as a 503 on all Google-backed routes, including ones that worked before.

Group listing uses `Customer("my_customer")`, so all domains in the account are covered.

The service account key travels **base64-encoded in a single env var** (`GOOGLE_SERVICE_ACCOUNT_KEY`) because the org's deploy pipeline injects secrets as scalar strings; clustron does the same for `PRESET_USER`.

Credentials are **optional**. With no key the service starts unconfigured and the endpoint returns 503, so local development never requires a Workspace key. Do not add it to `Config.Validate()`.

Both the group list and each group's member list are cached in-process with a TTL (`google_group.cache_ttl`, default 5m) via the generic `cache[T]`. The group list has no natural key so it uses the constant `allGroupsCacheKey`. Only successes are cached. It is per-replica and cleared on restart — swap for Redis if this is ever scaled out.

## Auth

Login is Google OAuth **gated on mailing list membership**: after the OAuth exchange, `internal/auth` checks the email against `google_group.login_group` via the existing `ListMembers`, requiring `Status == "ACTIVE"` and a Google-verified email. Non-members get `#error=not_a_member` and **no** user row is created. An empty `login_group` refuses everyone — it never falls open.

`internal/jwt` both mints and verifies. Access tokens are stateless HS256 JWTs (15 min); refresh tokens are **opaque row IDs** in `refresh_tokens` (24 h), which is what makes revocation possible. Refresh **rotates**: the presented token is inactivated once its replacement exists.

**Access and state tokens are separated by an `aud` claim** (`club-manager:access` vs `club-manager:state`). Without it, an OAuth state parameter — which travels in URLs, browser history and `Referer` — parses as a valid session token, since both are HS256 over the same secret with a UUID subject. `Parse` and `ParseState` each enforce their audience; anything minting tokens externally (including `scripts/token`) must set `aud`.

Three deliberate departures from clustron: tokens come back in the **URL fragment** (`#accessToken=...`), not the query string, so they stay out of server logs and `Referer`; refresh takes the token in a **POST body**, not a URL path; and there is no `login_info` table or provider map, since there is exactly one provider.

Beware: `main.go` replaces `Secret` with a random UUID when it is left at `DefaultSecret` and `Debug` is false, which invalidates every issued token. Set a real `SECRET` anywhere sessions must survive a restart.

The JWT middleware writes failures through `problemWriter` rather than clustron's `http.Error`, so 401s stay `application/problem+json`.

## Database

`make gen` merges every `internal/*/schema.sql` into `internal/database/full_schema.sql`, writes `sqlc.yaml`, and runs sqlc. All four are **generated and gitignored**, along with `db.go`, `models.go`, `queries.sql.go` — edit the per-package `schema.sql`/`queries.sql` instead. `build`, `test` and `run` depend on `gen`, so a fresh clone works.

Real DDL lives in `internal/database/migrations/`, applied by golang-migrate at startup. **`schema.sql` and the migration deliberately differ in one place**: `refresh_tokens.user_id` omits its `REFERENCES users(id)` in `schema.sql`, because the merge script concatenates files in filesystem order and `internal/jwt` sorts before `internal/user`, so the FK would reference a table sqlc has not parsed yet. sqlc only needs column types; the real constraint is in the migration, where version numbers make ordering explicit.

sqlc regenerates every table's model into every package, so `jwt.User` and `user.User` are identical types in two packages. Convert between them (`jwt.User(localUser)`) rather than duplicating a mapping — the conversion stops compiling if either drifts.

## Gotchas

- **`internal/database/migrations/` is empty and therefore untracked by git.** `main.go` tolerates both an empty and an absent directory: golang-migrate's file source reports either as `fs.ErrNotExist`, which is downgraded to a warning instead of `logger.Fatal`. Once a real migration lands, that branch stops firing — leave it in place until then.
- **No OpenTelemetry SDK is wired up.** `traceutil.TraceMiddleware` runs but no-ops against the global default provider, so logged `trace_id` values are all zeros. Add `initOpenTelemetry` (see clustron's `main.go`) when there's a collector to export to.
- **Login membership is cached** (`cache_ttl`, default 5m), so someone just added to the login group may wait up to five minutes before sign-in works.
- **`members.list` does not recurse.** If `login_group` contains another group as a member, those people are not direct members and cannot log in. The login group needs direct user members.
- **Access tokens cannot be revoked before they expire** — logout only kills refresh tokens. Inherent to stateless JWTs; the 15-minute lifetime is the mitigation.
- **Nested sub-configs must merge through `mergeConfig`, not `configutil.Merge` directly.** `configutil.Merge` compares each top-level field against its zero value, so a nested struct is all-or-nothing: setting only `GOOGLE_IMPERSONATE_SUBJECT` would otherwise discard a `service_account_key` that came from `config.yaml`. `internal/config.mergeConfig` merges nested configs field by field; route any new sub-config through it.
