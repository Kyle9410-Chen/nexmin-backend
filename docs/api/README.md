# Nexmin API

TypeSpec definitions for this service's HTTP API, compiled to OpenAPI 3.1.

Mirrors [`NYCU-SDC/clustron-api`](https://github.com/NYCU-SDC/clustron-api), which is
the org's reference for how API specs are written — TypeSpec sources, never
hand-edited OpenAPI.

## The rule

> **Every endpoint change updates this directory.** Adding a route, renaming a field,
> adding a status code, or changing whether a response may be `null` all mean editing
> the matching `service/*.tsp` in the same change.

CI compiles the spec and checks its formatting, but nothing can check it against the
handlers -- a spec that describes a field the server stopped sending compiles perfectly.
So the rule is still the only thing keeping the spec honest, and a spec that has drifted
is worse than no spec: the frontend builds against it.

## Layout

| Path | Contents |
|---|---|
| `main.tsp` | Service metadata, shared scalars, `ListResponse<T>` |
| `service/error.tsp` | RFC 9457 problem details and one model per status code |
| `service/health.tsp` | `/healthz` |
| `service/auth.tsp` | OAuth login and callback, token refresh, logout |
| `service/user.tsp` | Profiles and the caller's mailing lists |
| `service/group.tsp` | Mailing lists and their members |

`tsp-output/` is generated and gitignored, as is the `publish/` directory the
deployment assembles.

## Published

<https://kyle9410-chen.github.io/nexmin-backend/> -- deployed by
`.github/workflows/api-docs.yml` on every push to `main` that touches `docs/api/**`.
The URL follows the repository owner; nothing here hardcodes it.

| Path | Contents |
|---|---|
| `/` | Swagger UI (`swagger-ui.html`, served as `index.html`) |
| `/openapi.yaml` | The emitted spec -- what the frontend and any codegen should read |
| `/redocly.html` | Redocly's static rendering of the same spec |

Pages must be configured once, by hand: **Settings -> Pages -> Build and deployment ->
Source -> GitHub Actions**. Without it the `deploy` job fails.

## Commands

```bash
make install    # npm install (first time only)
make            # format check + compile
make compile    # -> tsp-output/schema/openapi.yaml
make lint       # Redocly lint over the emitted spec
make preview    # Swagger UI on http://localhost:8090
make mock       # Prism mock server on :4010 (needs Docker)
make clean
```

`npx tsp format "**/*.tsp"` rewrites files in place when the format check fails.

`.github/workflows/api-docs-check.yml` runs the format check and the compile on every
pull request touching `docs/api/**`, which is exactly what `make` does here -- running
it locally first saves a round trip. `make lint` deliberately stays out of CI: its
warnings are catalogued below as ones that should not be fixed.

## Expected lint warnings

`make lint` is clean of errors but reports eight warnings, all of them accurate. Do
not "fix" them by changing the spec — the spec is right and the rule does not apply:

| Rule | Count | Why it stands |
|---|---:|---|
| `operation-4xx-response` | 3 | `/healthz` and the two OAuth redirects genuinely have no 4xx. OAuth failures are 307s carrying `#error=…` in the fragment. |
| `operation-2xx-response` | 2 | The two OAuth operations only ever return 307. |
| `no-unused-components` | 1 | The `Versions` enum comes from `@versioned` and has no referent in the emitted document. |
| `no-server-example.com` | 1 | `http://localhost:8080` is the only server there is; nothing is deployed yet. |
| `info-license` | 1 | The repository has no LICENSE file, and claiming one it does not have would be worse. |

## Differences from clustron-api

- **Error field names are lower-case.** clustron-api's `error.tsp` declares
  `Title`/`Status`/`Type`/`Detail`, but both services serialize through
  `summer/pkg/problem`, whose struct tags are lower-case. This spec matches what the
  server actually sends, and adds the `instance` and `errors` fields clustron-api
  omits.
- **No pagination.** Every list here is bounded by a single mailing list, so
  `ListResponse<T>` is `{items, totalItems}` rather than clustron-api's
  `PaginatedResponse<T>`.
- **No `@service(..., version:)`.** TypeSpec 1.15 moved the version out of
  `ServiceOptions`; it comes from `@versioned` instead.
