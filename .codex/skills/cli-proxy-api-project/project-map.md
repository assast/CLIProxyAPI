# Project Map

## Repository Shape

- `cmd/server/`: server entrypoint and startup wiring.
- `internal/api/`: Gin API modules, middleware, and management handlers.
- `internal/runtime/executor/`: provider executors only. Keep helpers in `internal/runtime/executor/helps/`.
- `internal/thinking/`: parse suffixes, normalize to canonical `ThinkingConfig`, validate centrally, then translate per provider.
- `internal/registry/`: model registry and remote updater.
- `internal/store/`: file, Postgres, git, and object-store backed persistence.
- `sdk/api/handlers/`: reusable handler layer shared by the embeddable SDK and the server.
- `sdk/cliproxy/auth/`: auth manager, selectors, retry logic, model aliasing, execution-session lifecycle.

## Non-Negotiable Rules

- Keep changes small and simple.
- Use English comments only.
- Preserve the existing language of user-visible strings in each area.
- Avoid standalone edits in `internal/translator/`. That package is not a free-for-all extension point in this repo.
- Avoid panics in HTTP handlers. Return meaningful status codes and log context.
- Use structured logrus logging and never leak secrets or tokens.
- Avoid `log.Fatal` and `log.Fatalf`.
- After an upstream connection is established, do not introduce new network timeouts except the explicit repository exceptions already documented in `AGENTS.md`.

## Validation

- Run `gofmt -w .` after Go edits.
- Run `go build -o cli-proxy-api ./cmd/server` or `go run ./cmd/server` for local verification when needed.
- Run `go build -o test-output ./cmd/server` after changes. This repository treats that compile check as required.
- Run targeted tests first:
  - `go test ./sdk/api/handlers/...`
  - `go test ./sdk/cliproxy/...`
  - `go test ./internal/runtime/executor/...`
- Run `go test ./...` when changes cross subsystem boundaries.

## Ownership Hints

- Put request metadata and downstream execution hints in `sdk/api/handlers/`.
- Put auth selection, retry, and fallback logic in `sdk/cliproxy/auth/conductor.go`.
- Put provider-specific HTTP route and payload handling in executors such as `internal/runtime/executor/openai_compat_executor.go`.
- Put config schema changes in `internal/config/`, normalize aliases in management handlers, and re-export public SDK types in `sdk/config/` when needed.
