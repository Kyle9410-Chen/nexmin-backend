# The sqlc-generated files (db.go, models.go, queries.sql.go) and sqlc.yaml are
# gitignored -- `make gen` produces them -- so the build has to generate them itself
# rather than copying them in. That is why the builder installs sqlc.
FROM golang:1.26.2 AS builder

WORKDIR /src

# Installed before the source is copied so editing Go files does not reinstall sqlc.
# Keep the version in step with .github/workflows/*.yml (SQLC_VERSION).
RUN go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# The two steps `make gen` runs. Called directly: the image has no reason to carry
# make, and the Makefile target mixes in colour codes and local-only conveniences.
RUN ./scripts/create_full_schema.sh && sqlc generate

# CGO_ENABLED=0 because distroless/static carries no libc.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/backend cmd/backend/main.go

# :nonroot runs as uid 65532 and ships ca-certificates, which the Google Directory
# API calls need. It has no shell -- see the comment on healthchecks in .deploy/.
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=builder /out/backend /app/backend

# golang-migrate reads migrations off the filesystem rather than an embedded FS, so
# they have to travel in the image. Point MIGRATION_SOURCE at file:///app/migrations.
COPY internal/database/migrations /app/migrations

EXPOSE 8080

ENTRYPOINT ["/app/backend"]
