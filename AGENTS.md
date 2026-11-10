# AGENTS.md

## Current state

FujiPic is implemented as a Go 1.27 single-binary service. There is no Node/JS build step and no external queue or cache.

- `cmd/fujipic` — `serve`, `backup`, `admin create`, `admin reset-password`, `version`
- `internal/` — config, db, store, auth, storage, bucket, secrets, imagefile, upload, cdn, worker, server
- `web/` — embedded `html/template` pages and static CSS/JS
- `migrations/` — embedded SQL migrations
- `deploy/` — Dockerfile, production Compose, Caddyfile
- `docs/CDN.md`, `scripts/verify-cdn.sh` — CDN setup and acceptance

## Verified commands

Run from the repository root with Go 1.27+:

```bash
go build ./...
go test ./...
go test -race ./...
go vet ./...
gofmt -l cmd internal migrations web   # must be empty
```

Make targets:

```bash
make build    # CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/fujipic ./cmd/fujipic
make test
make race
make vet
make fmt
make backup
```

## Local run

Memory storage (no S3, data lost on restart):

```bash
APP_ENV=development \
APP_BASE_URL=http://localhost:8080 \
PUBLIC_BASE_URL=http://localhost:8080 \
DATABASE_PATH=./data/fujipic.db \
TMP_DIR=./data/tmp \
STORAGE_DRIVER=memory \
ALLOW_OPEN_REGISTRATION=true \
BOOTSTRAP_ADMIN_EMAIL=admin@example.com \
BOOTSTRAP_ADMIN_PASSWORD=admin-password-123 \
go run ./cmd/fujipic serve
```

There is no local S3 emulator and no development Compose file. Use `STORAGE_DRIVER=memory` for local UI/API work, or a real AWS S3 / OCI Object Storage test bucket. Docker is not available in every environment; do not claim the production Compose or cloud-bucket path is verified unless it was actually run.

## Conventions

- Keep SQL in `internal/store`; keep HTTP handlers in `internal/server`.
- Use `storage.Store` for object access; never let the CDN or clients reach the bucket directly.
- Resolve image objects through `internal/bucket`; an empty `bucket_id` means the server default bucket. User bucket credentials must stay encrypted at rest.
- New migrations go in `migrations/NNNN_*.sql`; the runner records versions after `0001`.
- API errors use `{"error":{"code","message"}}`; image origin responses must stay cacheable and cookie-free.
- Do not commit `.env`, `data/`, `backup/`, or real credentials.
- Tests use `httptest`, temp SQLite, and the in-memory storage driver. Add a test with each behavior change.
