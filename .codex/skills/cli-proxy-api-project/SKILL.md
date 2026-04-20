---
name: cli-proxy-api-project
description: Work inside the CLIProxyAPI Go repository, a proxy server that exposes OpenAI, Gemini, Claude, and Codex compatible APIs with OAuth and credential routing. Use when implementing, reviewing, or debugging changes in this repo, especially in `internal/api`, `internal/runtime/executor`, `sdk/api/handlers`, `sdk/cliproxy/auth`, config hot-reload, routing strategy changes, OpenAI Responses handlers, Codex websocket flows, or project-specific validation and build steps.
---

# CLIProxyAPI Project

Start with `AGENTS.md` and keep changes inside the repository's architectural boundaries. Read `project-map.md` first for the repo layout, validation commands, and non-negotiable rules. Read `session-affinity.md` before touching OpenAI Responses, Codex websocket routing, transparent proxy behavior, or `round-robin-session-affinity`.

## Workflow

1. Map the task to the owning layer before editing.
- HTTP surface and execution metadata: `sdk/api/handlers/`
- Auth selection, pinning, retry, and fallback: `sdk/cliproxy/auth/`
- Provider execution and upstream protocol details: `internal/runtime/executor/`
- Core request thinking pipeline: `internal/thinking/`
- Public config and management normalization: `internal/config/`, `sdk/config/`, `internal/api/handlers/management/`

2. Preserve repo-specific invariants.
- Keep `internal/thinking` in canonical-representation-first form: parse suffixes, normalize to canonical `ThinkingConfig`, validate centrally, then translate per provider.
- Keep `internal/runtime/executor/` limited to executors and executor tests. Put helper code under `internal/runtime/executor/helps/`.
- Avoid standalone edits in `internal/translator/`. Only change it as part of a broader feature, or stop if the task only requires translator changes and repository policy is not satisfied.
- Do not add `log.Fatal` or `log.Fatalf`. Return errors and log with logrus.
- After an upstream connection is established, do not add new network timeouts except for the explicit repository exceptions.

3. Follow the local implementation pattern instead of inventing a new one.
- For pinned auth routing, pass hints through handler context and execution metadata, let the auth manager own selection and soft fallback, and keep executors focused on provider behavior.
- For Responses and Codex affinity, pin by response or session key, allow soft fallback, and rebind cache entries after a successful retry onto the new auth.
- For OpenAI-compatible upstreams, prefer route-based execution helpers over large one-off branches.

4. Validate before finishing.
- Run `gofmt -w .` after Go changes.
- Run `go build -o test-output ./cmd/server` after changes. This compile check is required in this repository.
- Run focused tests for touched packages. Run `go test ./...` when the change spans multiple subsystems or routing behavior.

## High-Risk Areas

- `sdk/api/handlers/openai/`: OpenAI Responses HTTP and WebSocket behavior, SSE framing, response and session affinity.
- `sdk/cliproxy/auth/conductor.go`: auth selection, pinned-auth soft fallback, retry loops, model aliasing, and execution session lifecycle.
- `internal/runtime/executor/openai_compat_executor.go`: `/responses` vs `/chat/completions`, unsupported endpoint fallback, SSE passthrough, usage reporting.
- `internal/thinking/`: canonical thinking config path. Do not bypass it with handler-local or executor-local ad hoc logic.

## References

- `project-map.md`: repo layout, coding rules, validation commands, and architecture boundaries.
- `session-affinity.md`: patterns introduced between `c4459c43` and `c7508b7e` for routing strategy, OpenAI Responses affinity, websocket reconnect behavior, and OpenAI-compatible fallback rules.
