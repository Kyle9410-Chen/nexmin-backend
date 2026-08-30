# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Backend service for SDC club management. Go module path is `nycu-sdc/nexmin` — a **bare path**, not a URL, so this module cannot be `go get`-ed. Import internal packages as `nycu-sdc/nexmin/internal/...`. Toolchain pinned to Go 1.26.2.

Endpoints: `GET /api/healthz` (open); Google OAuth login at `GET /api/auth/google/login` + `/callback`, `POST /api/auth/refresh`, `POST /api/auth/logout`; user profiles at `GET`/`PATCH /api/users/me` and the caller's own mailing lists at `GET /api/users/me/groups` (any valid JWT), plus the club roster at `GET /api/users`, `POST /api/users` and `DELETE /api/users/{email}`, and one account at `GET /api/users/{user_id}` (all `admin`); and the mailing list routes backed by the Google Workspace Admin SDK: `GET /api/groups` + `GET /api/groups/{group_key}/members` need only a valid JWT, while `POST /api/groups/{group_key}/members`, `PATCH` and `DELETE .../{member_key}` additionally require the `admin` role.

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

`make gen` runs `scripts/create_full_schema.sh` (merge every `internal/*/schema.sql`) and then `sqlc generate`. Adding or changing a query means editing the per-package `queries.sql` and re-running it.

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
- **Every endpoint change updates `docs/api/`.** Adding a route, renaming a field, adding a status code, or changing whether a response may be `null` — all of them mean editing the matching `service/*.tsp` in the same change. The spec is TypeSpec compiled to OpenAPI, never hand-edited YAML, and `tsp-output/` is a generated artifact. CI compiles the spec and checks its formatting, but **nothing checks it against the handlers** — a spec describing a field the server stopped sending compiles perfectly — so the rule is still the only thing keeping the spec honest, and a spec that has drifted is worse than none — the frontend builds against it.
- **`*zap.Logger` is the first constructor argument** to every service, handler, and middleware. No global or context-stored logger; enrich per-request with `logutil.WithContext(ctx, logger)`.
- **Domain packages are vertical slices** under `internal/<domain>/`: hand-written `handler.go` (HTTP + Request/Response DTOs + a consumer-side `Store` interface), `service.go`, `queries.sql`, `schema.sql`; sqlc-generated `db.go`, `models.go`, `queries.sql.go`.
- **Startup order in `main.go` is load-bearing**: config → validate → logger → `cfgLog.FlushToZap` → migrations → pool → middleware → mux → signal ctx → serve → graceful shutdown. Migrations run *before* the pool is created.

## Config

Four layers, each merged via `configutil.Merge`: **defaults → `config.yaml` → `.env`/env → CLI flags** (later wins). `config.yaml` and `.env` are gitignored; `config.example.yaml` is committed.

The `envconfig:"..."` struct tags are **documentation only** — nothing reads them. Env vars are read by explicit `os.Getenv` calls in `FromEnv`. Adding a field means touching four places: the struct, `Load` defaults, `FromEnv`, and `FromFlags`.

`LogBuffer` exists because config must load before the logger does (`cfg.Debug` selects the logger config). Warnings during load are buffered, then replayed by `cfgLog.FlushToZap(logger)`.

## Google mailing lists

`internal/googlegroup` reads and writes Google groups and their members via the Admin SDK Directory API (`admin/directory/v1`), authenticating as a **service account using domain-wide delegation** — `google.JWTConfigFromJSON` with `Subject` set to a Workspace admin. This is the org's first use of `google.golang.org/api`; there is no pattern to copy from other repos.

Two scopes are requested: `admin.directory.group.member` (members — **read-write**, since members are added, re-roled and removed here) and `admin.directory.group.readonly` (group listing, still read-only — nothing creates or edits groups). **Changing that list is not a code-only change** — domain-wide delegation is granted against an exact scope list, not merged, so the Workspace admin console entry must be re-saved with the full set. A stale grant fails every call with `unauthorized_client`, surfacing as a 503 on all Google-backed routes, including ones that worked before and login itself. The impersonated subject also needs privileges to manage groups (Groups Admin or Super Admin); read-only access passing does not mean writes will.

Group listing uses `Customer("my_customer")`, so all domains in the account are covered.

**The API speaks bare group names.** `google_group.domain` (default `sdc.nycu.club`, env `GOOGLE_GROUP_DOMAIN`) is qualified onto any `group_key` without an `@` on the way into Google, and stripped back off group addresses and aliases on the way out — so `GET /api/groups` returns `"email": "general"` and `GET /api/groups/general/members` works. Full addresses still work, `login_group` accepts either spelling, and only an **exact** domain match is stripped, so a group in another domain of the account keeps its full address rather than becoming a key that would not resolve. Set `domain` empty to turn all of this off. **Member** addresses are never rewritten — those are `@gmail.com`, `@nycu.edu.tw` and so on.

## Organization chart

`docs/organization.md` is the reference for how the club's structure maps onto the mailing lists — read it before touching `internal/orgchart` or `internal/membership`.

**The hierarchy lives in Google, as nested group membership** (`committee` contains `administration@`, `branding@`, …; `branding` contains `design@`; `general` contains the technical teams). That is also what decides who actually receives mail, so it is the only copy. `internal/orgchart` holds *display* metadata only — Chinese names, sections, ordering, and which officer role owns which list — in a `go:embed`ed `chart.yaml`. Editing it never changes who receives anything.

Group display names are English, with the `NYCU SDC ` prefix Google carries stripped; the officer role names stay in Chinese, because those are the club's own titles. Both `GET /api/users/me/groups` and `GET /api/groups` are ordered by the chart and carry its names — the latter through `displayName` and `section`, alongside Google's own `name`. The two views differ in one place: the profile view drops sections marked `hidden`, while the account-wide list reports them, since omitting a row there would read as the group having gone missing.

`internal/orgchart` imports nothing from this module, deliberately. `Load()` validates the chart against itself (every sectioned group is named, no group in two sections, no role owning a group twice) and `main.go` treats a failure as `logger.Fatal` — every error it reports is a typo in a committed file. A group the chart does not mention still appears, sorted last under a synthetic `unsectioned` section, so a newly created mailing list is visible rather than silently dropped.

**Membership is reported direct only, on purpose.** The club's lists nest — `consultants` sits inside `committee`, `design` inside `branding` — and this service does not expand that. Measured against the live account, expanding made 52 of the 69 people on the roster look like members of `committee`, purely because they are consultants. That is true of where their mail lands and false of where they sit in the club; this API answers the second question. `membership.Service.expand` therefore does one `groups.list?userKey=` and stops, and the roster uses the reverse index as-is. A test in each path pins this, because the tests that used to cover the walk are gone.

`docs/organization.md` records two things about Google that are worth keeping even though nothing depends on them any more: `groups.list?userKey=` returns direct memberships only, and that parameter also accepts a group's address (answering with that group's direct parents). Both were found by experiment, not documentation.

`internal/membership` is its own slice because it needs `googlegroup`, `orgchart` and `jwt` at once, and `internal/user` may not import `internal/googlegroup`. `internal/user` reaches it through the string-only `MembershipLister` and `RosterWriter` interfaces, the same trick as `RoleResolver`. `RosterWriter` takes a `[]user.GroupRole` — two plain strings — so `membership` imports `user` to implement it, the same direction `googlegroup` already runs; the rule that stays is `internal/user` never importing `internal/googlegroup`.

**`POST /api/users` writes the login group plus whatever `groups` names.** The request is `{email, groups: [{key, role}]}`: the login group is always written and does not have to be named, and everything else goes on top of it, each with its own Google role. Keys and roles are validated against the account's group list *before* the first write, so a typo costs nothing; after that the operation is not atomic and nothing is rolled back. That is safe because **every write is idempotent** — `ErrMemberAlreadyExists` is treated as success, which is also what lets this endpoint add lists to somebody who is already a member — so the recovery for a half-finished request is to send it again. 409 therefore never reaches the caller.

**Granting admin has two spellings, and both are the same act.** `POST /api/users` naming the login group with `role: "MANAGER"`, and `PATCH /api/groups/{login_group}/members/{email}` with the same role, both set a role on the login mailing list, which `auth.localRoleFor` maps onto `admin`. The first is named for adding a member, so the consequence is easy to miss; a test pins it. There is no other way to become an admin. When the login group membership already existed, nothing is written there and `AddToRoster` reports an empty `loginRole` — the handler then reads the live role rather than reporting the one it asked for.

**`DELETE /api/users/{email}` removes every list, not just the login group.** Taking someone off `login_group` alone ends their access while leaving them on the lists that actually carry the club's mail. It reads their lists through `expand`, **not** `GroupKeysForEmail` — that one drops the sections the chart marks hidden, and `info` still delivers — and removes the login group **last**, so a failure part-way through leaves them on the roster where they are still visible and the request can be repeated. Removals are idempotent: Google answers an unknown member key with a 400 reading `Missing required field: memberKey` rather than a 404, so both sentinels count as success and deleting someone already gone is a 204.

It is addressed by email while `GET /api/users/{user_id}` takes a UUID. That asymmetry is deliberate: a roster entry for someone with no local row has no UUID, and those are exactly the people a roster edit touches. Nothing stops an admin removing themselves — the equivalent group route has no such guard either, and one that existed only here would be trivially bypassed. The local `users` row is never deleted, so a profile survives being removed and re-added.

**The `users` table is an extension of the login group, not the source of identity.** The mailing list decides who exists; the local row holds only what this service owns — profile, UUID, and the cached role. `GET /api/users` therefore reads `login_group` and fills in local profiles where they exist (`profile: null` for a member this service knows nothing else about), rather than listing rows and looking up their lists. Same principle as the Auth section's "authority comes from the mailing list", applied to the roster. It also means that endpoint has nothing to degrade to when Google is down: no mailing list, no roster, so it answers 503.

The roster is built by **inverting** the member lists — one `members.list` per group rather than one `groups.list?userKey=` per person — because the club grows in people, not in mailing lists, and those calls share the member cache with `GET /api/groups/{group_key}/members`. `membership.Service.RosterGroupKeys` and `GroupKeysForEmail` must agree; they share `visibleKeys` so they cannot drift.

**Officer roles cannot be derived from Google.** All six office holders are `MANAGER` in every department group, so membership role only answers "is this an officer", never which portfolio. `chart.yaml` therefore declares role → owned groups by hand, and the API reports `leadership` as a boolean rather than naming a position.

A bare name and a group's immutable ID are indistinguishable (both are letters and digits with no `@`), so `withGroupKey` tries the qualified key first and retries once with the key exactly as given when Google says not found. That is what keeps immutable IDs working; it costs a second call only on a path nothing routine takes, and a 404 means nothing was written, so retrying a write is safe. Member operations report an unknown group as `ErrMemberNotFound`, not `ErrGroupNotFound` — `isNotFound` covers both, and narrowing it would silently break ID addressing on the write routes.

The service account key travels **base64-encoded in a single env var** (`GOOGLE_SERVICE_ACCOUNT_KEY`) because the org's deploy pipeline injects secrets as scalar strings; clustron does the same for `PRESET_USER`.

Credentials are **optional**. With no key the service starts unconfigured and the endpoint returns 503, so local development never requires a Workspace key. Do not add it to `Config.Validate()`.

Both the group list and each group's member list are cached in-process with a TTL (`google_group.cache_ttl`, default 5m) via the generic `cache[T]`. The group list has no natural key so it uses the constant `allGroupsCacheKey`. Only successes are cached. It is per-replica and cleared on restart — swap for Redis if this is ever scaled out.

Every successful write drops **both** caches wholesale (`invalidateCaches`), not just the affected key: a group can be addressed by email or by immutable ID, so the key a write sees need not match the key an earlier read cached under. The group cache goes too because `DirectMembersCount` moves with each change.

Roles are `MEMBER` / `MANAGER` / `OWNER`, validated by `NormalizeRole` rather than a `oneof` struct tag, so the legal set is defined once beside the constants and comparison stays case-insensitive. Empty means `MEMBER`, matching Google's own default for `members.insert`.

## Startup profile sync

`internal/directory` reads the club's **Google Form response sheet** once, at startup, and creates a `users` row for every login group member who does not have one. It exists because Google cannot supply names: `members.list` returns only addresses and roles, and the Directory API knows display names only for identities inside the Workspace account — most of the club signs up with `@nycu.edu.tw` or `@gmail.com`. Without this, `GET /api/users` is a column of bare emails until each person signs in for the first time.

**It is a source of names at row-creation time and nothing else.** Existing rows are never touched, nothing is deleted, nothing is written back to the sheet, and it does not run again until the next restart. `SeedProfile` is a single `INSERT ... ON CONFLICT (email) DO NOTHING`, so "already there" is success rather than a conflict and the whole sync is idempotent — that is what makes restarting safe and what keeps it from fighting `PATCH /api/users/me`. Whoever creates the row first wins: someone who signs in before the first sync keeps their OAuth display name for good.

**Only direct members of `login_group` are seeded.** The sheet is a form response log, not the roster — it holds applicants who were never added and people who have since left — so `SyncOnce` reads the mailing list first and drops anything that does not match, case-insensitively. That read happens **before the first write**, so a Google failure means nothing is seeded rather than the whole sheet being dumped into the roster. Both sides are flat: `members.list` does not recurse and the sheet has no nesting, so no group expansion is involved.

**The column mapping is declared, not detected.** A Forms response sheet's layout is decided by the form — column A is the timestamp, column B the collected address, then the answers in question order — so `email_column` / `name_column` / `nickname_column` have no defaults and are required once `spreadsheet_id` is set. Header names are Chinese and get reworded, which is why they are not used for detection; instead the header row is **logged at startup** next to the values read, so a mis-set column is visible immediately rather than as a roster full of the wrong field. Rows are addressed from column A so an index is its own column number, and the Sheets API truncates trailing empty cells, so short rows are the easiest way to panic here — `cell` guards every read.

**It authenticates as the service account itself — no `Subject`, no domain-wide delegation.** The sheet is shared with the service account's own address instead. This is deliberate: a delegation grant is matched against an exact scope list rather than merged, so adding `spreadsheets.readonly` to it would mean re-saving the console entry with every scope spelled out, and getting that wrong fails *every* Google-backed call including login. Reading one spreadsheet is not worth that risk, especially on the startup path.

Failure is never fatal. An absent sheet, key or login group leaves the service unconfigured and `SyncOnce` a no-op; a malformed column letter is a hard error, matching how `googlegroup` treats an unparseable `cache_ttl`. `main.go` warns on either and carries on — same posture as Google credentials everywhere else, so local development needs none of this. The sync runs inline with a 30s timeout rather than in a goroutine, so the port opens with the data already in place.

Side effect worth knowing: reading `login_group` warms `googlegroup`'s member cache, so the first roster request after startup does not wait on Google.

## API spec

`docs/api/` holds TypeSpec sources that compile to `tsp-output/schema/openapi.yaml` (OpenAPI 3.1). `make api` from the repo root, or `make compile` / `make preview` / `make lint` inside `docs/api`. It is deliberately **not** a prerequisite of `build`/`test`/`lint`: the Go workflow has to keep working on a machine with no node installed. The Go service still has no `/openapi.yaml` route — nothing serves the spec at runtime.

`.github/workflows/api-docs.yml` publishes it to GitHub Pages instead (<https://kyle9410-chen.github.io/nexmin-backend/>): Swagger UI at `/`, the raw `openapi.yaml`, and a Redocly rendering at `/redocly.html`. It runs on pushes to `main` **filtered on `docs/api/**`**, so a Go-only change does not redeploy, and uses the official `actions/deploy-pages` (as `NYCU-SDC.github.io` does) rather than clustron-api's `peaceiris` + `gh-pages` branch — which means the repo's Pages source must be set to "GitHub Actions" by hand, once. `api-docs-check.yml` runs the same format check and compile on pull requests touching that path. `make lint` stays out of both: its warnings are catalogued in `docs/api/README.md` as ones the spec is right to trigger. The published `swagger-ui.html` keeps a **relative** `./openapi.yaml`, unlike clustron-api's hardcoded URL, so the same file serves `make preview` and Pages and survives the repo moving to another owner.

This mirrors `NYCU-SDC/clustron-api`, with three deliberate departures:

- **Error field names are lower-case** (`title`/`status`/`type`/`detail`, plus `instance` and `errors`). clustron-api's `error.tsp` capitalizes them, but both services serialize through `summer/pkg/problem`, whose struct tags are lower-case — that spec does not match what its server sends.
- **No pagination.** Every list is bounded by one mailing list, so `ListResponse<T>` is `{items, totalItems}` rather than clustron-api's `PaginatedResponse<T>`.
- **No `version` in `@service`.** TypeSpec 1.15 dropped it from `ServiceOptions`; the version comes from `@versioned` instead, so clustron-api's `main.tsp` no longer compiles on a current toolchain.

3.1 rather than 3.0 because this API uses `null` to carry meaning in four places — `profile`, `groups`, `via`, `ownerRole` — and only 3.1 expresses that without 3.0's non-standard `nullable` keyword.

## Auth

Login is Google OAuth **gated on mailing list membership**: after the OAuth exchange, `internal/auth` checks the email against `google_group.login_group` via the existing `ListMembers`, requiring a Google-verified email and a usable membership status. **A usable status is `ACTIVE` *or empty*:** the Directory API only fills in `status` for identities inside the Workspace account, so every member from an outside domain (`@nycu.edu.tw`, `@gmail.com` — most of the club) reports `""`. Only explicitly named states like `SUSPENDED` are refused; tightening this to `== "ACTIVE"` locks out everyone but `@sdc.nycu.club`. Non-members get `#error=not_a_member` and **no** user row is created. An empty `login_group` refuses everyone — it never falls open.

**Authority comes from the mailing list, never from local config.** There is no admin whitelist to maintain: `internal/auth.localRoleFor` maps a caller's role in `login_group` onto this service's role — `OWNER` and `MANAGER` become `admin`, everyone else `member` — and `RoleFor` reads it in the same single pass that answers the login gate. The result is written to `users.role` on every sign-in (promotions *and* demotions) and carried in the access token's `role` claim, which `jwt.Middleware.RequireRole` checks. Granting someone admin is entirely a Google Groups operation: no config edit, no SQL, no restart.

The mapping itself lives in `internal/auth.RoleResolver` (`rolesync.go`), which is the single implementation of "what may this member do here" — the sign-in gate (`RoleFor`) and the user read paths (`LocalRoles`) share it so they cannot disagree.

**What the user endpoints report and what the middleware permits are two different clocks.** `GET /api/users`, `/api/users/me` and `/api/users/{user_id}` recompute each role from the login group on every request and write back any drift, so `users.role` is a cache and the response is current to within the member cache TTL. But `RequireRole` still reads the `role` claim minted into the access token, and **`POST /api/auth/refresh` re-reads the `users` row and never re-reads the mailing list**. So a demoted admin sees `"role": "member"` immediately while keeping actual admin access for up to one access-token lifetime (15 min). Closing that gap would mean re-checking the mailing list on the refresh path, which would make `internal/jwt` depend on `internal/googlegroup`.

**`users.name` is not synced from Google.** It is seeded once when the row is created — from the OAuth display name at first sign-in, or from the sign-up sheet if the startup sync got there first (see *Startup profile sync*) — and belongs to the user from then on — they edit it, along with `nickname` and `department`, through `PATCH /api/users/me`. `FindOrCreateByEmail` deliberately touches only `role` on the existing-user path; do not "fix" it to refresh the name. Role is deliberately not PATCHable: it comes from the mailing list or nowhere.

`internal/jwt` both mints and verifies. Access tokens are stateless HS256 JWTs (15 min); refresh tokens are **opaque row IDs** in `refresh_tokens` (24 h), which is what makes revocation possible. Refresh **rotates**: the presented token is inactivated once its replacement exists.

**Access and state tokens are separated by an `aud` claim** (`nexmin:access` vs `nexmin:state`). Without it, an OAuth state parameter — which travels in URLs, browser history and `Referer` — parses as a valid session token, since both are HS256 over the same secret with a UUID subject. `Parse` and `ParseState` each enforce their audience; anything minting tokens externally (including `scripts/token`) must set `aud`.

Three deliberate departures from clustron: tokens come back in the **URL fragment** (`#accessToken=...`), not the query string, so they stay out of server logs and `Referer`; refresh takes the token in a **POST body**, not a URL path; and there is no `login_info` table or provider map, since there is exactly one provider.

Beware: `main.go` replaces `Secret` with a random UUID when it is left at `DefaultSecret` and `Debug` is false, which invalidates every issued token. Set a real `SECRET` anywhere sessions must survive a restart.

The JWT middleware writes failures through `problemWriter` rather than clustron's `http.Error`, so 401s stay `application/problem+json`.

## Database

`make gen` merges every `internal/*/schema.sql` into `internal/database/full_schema.sql`, writes `sqlc.yaml`, and runs sqlc. All four are **generated and gitignored**, along with `db.go`, `models.go`, `queries.sql.go` — edit the per-package `schema.sql`/`queries.sql` instead. `build`, `test` and `run` depend on `gen`, so a fresh clone works.

Real DDL lives in `internal/database/migrations/`, applied by golang-migrate at startup. **`schema.sql` and the migration deliberately differ in one place**: `refresh_tokens.user_id` omits its `REFERENCES users(id)` in `schema.sql`, because the merge script concatenates files in filesystem order and `internal/jwt` sorts before `internal/user`, so the FK would reference a table sqlc has not parsed yet. sqlc only needs column types; the real constraint is in the migration, where version numbers make ordering explicit.

sqlc regenerates every table's model into every package, so `jwt.User` and `user.User` are identical types in two packages. Convert between them (`jwt.User(localUser)`) rather than duplicating a mapping — the conversion stops compiling if either drifts.

## Deployment

**Nothing deploys automatically.** CI builds and pushes an image; putting it on a machine is a manual `docker compose -f .deploy/<env>/compose.yaml up -d`. There is no n8n webhook, no traefik, and no server this repo knows about.

The `Dockerfile` is **multi-stage and self-contained**, deliberately unlike clustron-backend's, which only `COPY`s a `bin/backend` its CI built beforehand. The sqlc output is gitignored, so the builder installs sqlc and runs `create_full_schema.sh` + `sqlc generate` itself — which is what lets `docker build .` work on any machine, and drops the image from ~1.1GB (`FROM golang`) to ~30MB (`distroless/static-debian12:nonroot`, uid 65532, ca-certificates included). **The sqlc version is pinned in two places** — the `go install` line and `SQLC_VERSION` in the workflows — and they have to move together.

`internal/database/migrations/` is copied into the image because golang-migrate reads migrations off the filesystem, not an embedded FS; the compose files point `MIGRATION_SOURCE` at `file:///app/migrations`.

A container has no `config.yaml` and no `.env`, so **every setting arrives as an environment variable** and the two "failed to load config" warnings at startup are expected. `HOST=0.0.0.0` is required — the default `localhost` binds to loopback inside the container, where nothing can reach it. `SECRET` and `BASE_URL` are written `${VAR:?...}` so compose refuses to start without them: an unset secret means `main.go` swaps in a random UUID and invalidates every token on each restart, and `BASE_URL` has to match the OAuth redirect URI exactly. Google credentials stay optional, as everywhere else.

**The backend has no healthcheck**: distroless carries no shell, so any `test:` would report unhealthy forever. `postgres` has one (`up --wait` waits on it); check the backend from outside with `curl /api/healthz`. `.deploy/dev` and `.deploy/stage` differ only in project name, published port (8081/8082 — 8080 belongs to `make run`), image tag and `DEBUG`, and unlike `.deploy/local` both keep a named volume for Postgres — mounted at `/var/lib/postgresql`, **not** `/var/lib/postgresql/data`, which `postgres:18` refuses to start against. Each directory carries a committed `.env.example`; the `.env` beside it is gitignored and is what makes `docker compose logs`/`down` work without re-supplying the required variables on every command.

`main.yml` (push to `main`, `:dev`) and `stage.yml` (tags `v*`, `:stage`) run Lint → Test → Build; `pull-request.yml` runs the first two. All three skip `docs/api/**` and `**.md`, which `api-docs*.yml` covers. **`make gen` runs before lint and test** — nothing compiles until it has. The Build job is gated on `vars.DOCKER_IMAGE_ENABLED == 'true'` so the pipeline stays green until the registry credentials exist. **That variable has to be repository-scoped while the credentials sit on the `docker-hub` environment**: a job-level `if` is evaluated before the environment is resolved, so an environment-scoped variable reads as empty and skips the job every time — which is exactly what it looks like when the switch is "on". The image lives in a personal namespace (`umineko9410/nexmin-backend`, not `nycusdc/`) because this repo does, and moving it means setting `vars.DOCKER_IMAGE`, not editing the workflows.

## Gotchas

- **`main.go` tolerates an empty or absent `internal/database/migrations/`**: golang-migrate's file source reports either as `fs.ErrNotExist`, which is downgraded to a warning instead of `logger.Fatal`. `000001_init` now exists so that branch no longer fires in practice; leave it in place for fresh checkouts.
- **No OpenTelemetry SDK is wired up.** `traceutil.TraceMiddleware` runs but no-ops against the global default provider, so logged `trace_id` values are all zeros. Add `initOpenTelemetry` (see clustron's `main.go`) when there's a collector to export to.
- **The cache hands out copies, and callers must not sort what a store gives them.** `cache.get` and `cache.set` both `slices.Clone`, because `GET /api/groups` sorts the group list into organizational order and used to do it in place, on the cache's own array — while the roster was still reading it. `membership.directIndex` paired a group with its member list by position across ~34 Google round trips, so one concurrent sort re-attributed everyone to the wrong mailing list, and only on a cold cache: the fan-out is seconds long there, and the first sort after the cache fills is the only one that actually moves anything. `directIndex` now carries the key alongside the members, and `ListGroupsHandler` sorts the response slice it built itself.
- **Invalidation has to survive reads that are already in flight.** `cache.clear` bumps a generation and `cache.set` drops any result whose fetch began earlier, because otherwise a read that started before a write finishes after it and writes the pre-write list back over the invalidation — where it is then served for a full TTL, which reads to the user as the change they just made being undone. `invalidateCaches` also swaps `memberFlight` for a fresh `singleflight.Group`: singleflight hands every joiner the in-flight call's result, so a read arriving after the write would otherwise be served the list it was waiting on.
- **Nothing is cached for 5 seconds after a write** (`writeSuppression` in `internal/googlegroup/cache.go`). The Directory API is eventually consistent, so a read issued right after a successful write can still answer with the pre-write list — and that read carries the current generation, so the generation check does not catch it. The window costs a handful of uncached reads per write and removes the whole class of rollback symptoms.
- **The sheet sync only runs at startup.** Someone added to the sign-up sheet does not get a profile until the next restart — there is no endpoint to trigger it and no schedule. `SyncOnce` is written to be re-runnable, so exposing it later is a handler and a route, nothing more. Their row also appears on its own the moment they sign in.
- **Login membership is cached** (`cache_ttl`, default 5m), so someone added to the login group **in the Google admin console** may wait up to five minutes before sign-in works. Adding them through this service's own `POST .../members` clears the cache, so that path takes effect immediately.
- **Role changes lag in the middleware, not in the response.** The user endpoints report the live mailing-list role, but a demoted admin keeps their `admin` *claim* — and therefore real access — until their access token expires or they sign in again. See the Auth section.
- **`GET /api/users/me` degrades, `GET /api/users/me/groups` does not.** The compact `groups` list on the profile is null (not empty) when Google is unreachable, so the profile page still loads; the dedicated endpoint is entirely Google-derived and returns 503. Same split as the role fallback.
- **`internal/user` must never import `internal/googlegroup`.** The edge runs the other way: `googlegroup` imports `user` to attach profiles to mailing list members. Anything in `user` that needs to know about group roles takes a consumer-side interface in plain strings (`user.RoleResolver`) and is wired to `auth.RoleResolver` in `main.go`.
- **The Google mailing list sentinels live in `internal/apperr`**, not `internal/googlegroup`; `internal/googlegroup/errors.go` only aliases them. `internal/errors.go` has to map them without importing `googlegroup`, because `internal/jwt` imports `internal` for the context key and would otherwise close a cycle through `googlegroup → user → jwt`. `errors.Is` matches either name — they are the same values.
- **`members.list` does not recurse.** If `login_group` contains another group as a member, those people are not direct members and cannot log in. The login group needs direct user members.
- **Access tokens cannot be revoked before they expire** — logout only kills refresh tokens. Inherent to stateless JWTs; the 15-minute lifetime is the mitigation.
- **Nested sub-configs must merge through `mergeConfig`, not `configutil.Merge` directly.** `configutil.Merge` compares each top-level field against its zero value, so a nested struct is all-or-nothing: setting only `GOOGLE_IMPERSONATE_SUBJECT` would otherwise discard a `service_account_key` that came from `config.yaml`. `internal/config.mergeConfig` merges nested configs field by field; route any new sub-config through it.
