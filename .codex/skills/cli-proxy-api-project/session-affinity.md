# Session Affinity Notes

## Baseline Comparison

The range `c4459c43..c7508b7e` changes 19 files with `+1715/-259`. The stable themes are:

- Add the routing strategy `round-robin-session-affinity`.
- Add OpenAI Responses HTTP session affinity keyed by `previous_response_id` and downstream session headers.
- Add OpenAI Responses WebSocket affinity across reconnects.
- Add pinned-auth soft fallback so a stuck or failed pinned auth can release and rebind to a healthy auth.
- Refactor `internal/runtime/executor/openai_compat_executor.go` around route helpers and support `/responses` with fallback to `/chat/completions` when the upstream does not support the Responses endpoint.
- Add regression tests for non-stream, stream bootstrap, and websocket reconnect and fallback paths.

## Implementation Pattern

1. Enable affinity only when the auth manager reports `round-robin-session-affinity`.
2. Resolve affinity in this order:
- previous response id
- HTTP or WebSocket downstream session key
- normal selector choice
3. When pinning an auth from handler code, pair `WithPinnedAuthID(...)` with `WithPinnedAuthSoftFallback(...)`.
4. Let `sdk/cliproxy/auth/conductor.go` own the release logic:
- keep the pin for success
- release the pin for non-request-invalid failures
- log the rebound auth when a retry selects a different auth
5. Remember affinity after a successful completed response:
- non-stream: extract response id from payload
- SSE stream: extract response id from `response.completed`
- WebSocket stream: extract response id from completed payloads and refresh the session-key mapping

## File Map

- `sdk/api/handlers/handlers.go`: converts context hints into execution metadata.
- `sdk/cliproxy/executor/types.go`: defines `PinnedAuthSoftFallbackMetadataKey`.
- `sdk/cliproxy/auth/conductor.go`: releases pinned auth for soft fallback, retries normal routing, and logs rebinding.
- `sdk/api/handlers/openai/responses_session_affinity.go`: owns cache keys, TTL, cleanup, and session id extraction.
- `sdk/api/handlers/openai/openai_responses_handlers.go`: pins and stores affinity for HTTP and SSE Responses traffic.
- `sdk/api/handlers/openai/openai_responses_websocket.go`: restores and refreshes affinity across reconnects.
- `internal/runtime/executor/openai_compat_executor.go`: routes OpenAI-compatible traffic to `/responses` or `/chat/completions`, preserves Responses SSE metadata, and falls back only when `/responses` is unsupported.

## Rules For Future Changes

- Do not implement auth rebinding in handlers or executors. Handlers publish hints; the auth manager decides fallback.
- Do not store affinity on partial payloads. Store it only after a trustworthy completed response id is available.
- Preserve SSE metadata lines (`event:`, `id:`, `retry:`, comment lines) when proxying Responses streams.
- Keep the unsupported-Responses fallback narrow. Fall back only for endpoint-not-supported cases, not for generic request validation errors.
- When adding a new affinity source, feed it into the existing cache key helpers instead of creating ad hoc maps.

## Tests To Mirror

- `go test -run TestOpenAIResponsesNonStreamSessionAffinityUsesPreviousResponseID ./sdk/api/handlers/openai`
- `go test -run TestOpenAIResponsesNonStreamSessionAffinityFallsBackAndRebinds ./sdk/api/handlers/openai`
- `go test -run TestResponsesWebsocketSessionAffinityReusesCachedAuthAcrossReconnects ./sdk/api/handlers/openai`
- `go test -run TestResponsesWebsocketSessionAffinityFallsBackAndRebinds ./sdk/api/handlers/openai`
- `go test -run TestExecuteStreamWithAuthManager_SoftPinnedAuthFallsBackToNextUpstream ./sdk/api/handlers`
