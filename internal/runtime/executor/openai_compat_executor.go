package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/assast/CLIProxyAPI/v6/internal/config"
	"github.com/assast/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/assast/CLIProxyAPI/v6/internal/thinking"
	"github.com/assast/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/assast/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/assast/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/assast/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
)

// OpenAICompatExecutor implements a stateless executor for OpenAI-compatible providers.
// It performs request/response translation and executes against the provider base URL
// using per-auth credentials (API key) and per-auth HTTP transport (proxy) from context.
type OpenAICompatExecutor struct {
	provider string
	cfg      *config.Config
}

type openAICompatUpstreamRoute struct {
	format              sdktranslator.Format
	endpoint            string
	includeStreamUsage  bool
	preserveSSEMetadata bool
}

type openAICompatAttemptResult struct {
	body    []byte
	headers http.Header
	status  int
}

// NewOpenAICompatExecutor creates an executor bound to a provider key (e.g., "openrouter").
func NewOpenAICompatExecutor(provider string, cfg *config.Config) *OpenAICompatExecutor {
	return &OpenAICompatExecutor{provider: provider, cfg: cfg}
}

// Identifier implements cliproxyauth.ProviderExecutor.
func (e *OpenAICompatExecutor) Identifier() string { return e.provider }

// PrepareRequest injects OpenAI-compatible credentials into the outgoing HTTP request.
func (e *OpenAICompatExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	_, apiKey := e.resolveCredentials(auth)
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects OpenAI-compatible credentials into the request and executes it.
func (e *OpenAICompatExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("openai compat executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

func (e *OpenAICompatExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return
	}

	from := opts.SourceFormat
	route := e.resolveUpstreamRoute(opts)
	if route.endpoint == "/responses" {
		if fallback, ok, result, translatedTry, toTry, errTry := e.executeNonStreamAttempt(ctx, auth, req, opts, baseModel, apiKey, baseURL, from, route); errTry != nil {
			return resp, errTry
		} else if ok {
			if fallback {
				route = e.defaultRoute()
			} else {
				reporter.Publish(ctx, helps.ParseOpenAIUsage(result.body))
				reporter.EnsurePublished(ctx)
				var param any
				out := sdktranslator.TranslateNonStream(ctx, toTry, from, req.Model, opts.OriginalRequest, translatedTry, result.body, &param)
				return cliproxyexecutor.Response{Payload: out, Headers: result.headers}, nil
			}
		}
	}

	result, translated, to, err := e.executeNonStreamRoute(ctx, auth, req, opts, baseModel, apiKey, baseURL, from, route)
	if err != nil {
		return resp, err
	}
	reporter.Publish(ctx, helps.ParseOpenAIUsage(result.body))
	// Ensure we at least record the request even if upstream doesn't return usage
	reporter.EnsurePublished(ctx)
	// Translate response back to source format when needed
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, result.body, &param)
	resp = cliproxyexecutor.Response{Payload: out, Headers: result.headers}
	return resp, nil
}

func (e *OpenAICompatExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return nil, err
	}

	from := opts.SourceFormat
	route := e.resolveUpstreamRoute(opts)
	if route.endpoint == "/responses" {
		if fallback, ok, streamResult, translatedTry, toTry, errTry := e.executeStreamAttempt(ctx, auth, req, opts, baseModel, apiKey, baseURL, from, route); errTry != nil {
			return nil, errTry
		} else if ok {
			if fallback {
				route = e.defaultRoute()
			} else {
				return e.forwardOpenAICompatStream(ctx, reporter, req, opts, translatedTry, from, toTry, route, streamResult)
			}
		}
	}

	streamResult, translated, to, err := e.executeStreamRoute(ctx, auth, req, opts, baseModel, apiKey, baseURL, from, route)
	if err != nil {
		return nil, err
	}
	return e.forwardOpenAICompatStream(ctx, reporter, req, opts, translated, from, to, route, streamResult)
}

func (e *OpenAICompatExecutor) forwardOpenAICompatStream(ctx context.Context, reporter *helps.UsageReporter, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, translated []byte, from, to sdktranslator.Format, route openAICompatUpstreamRoute, streamResult *cliproxyexecutor.StreamResult) (*cliproxyexecutor.StreamResult, error) {
	if streamResult == nil {
		return nil, fmt.Errorf("openai compat executor: stream result is nil")
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		var param any
		for chunk := range streamResult.Chunks {
			if chunk.Err != nil {
				helps.RecordAPIResponseError(ctx, e.cfg, chunk.Err)
				reporter.PublishFailure(ctx)
				out <- chunk
				reporter.EnsurePublished(ctx)
				return
			}
			scanner := bufio.NewScanner(bytes.NewReader(chunk.Payload))
			scanner.Buffer(nil, 52_428_800)
			for scanner.Scan() {
				line := scanner.Bytes()
				helps.AppendAPIResponseChunk(ctx, e.cfg, line)
				if detail, ok := helps.ParseOpenAIStreamUsage(line); ok {
					reporter.Publish(ctx, detail)
				}
				if len(line) == 0 {
					continue
				}
				if route.preserveSSEMetadata && isResponsesSSEMetadataLine(line) {
					out <- cliproxyexecutor.StreamChunk{Payload: bytes.Clone(line)}
					continue
				}
				if !bytes.HasPrefix(line, []byte("data:")) {
					continue
				}
				chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, bytes.Clone(line), &param)
				for i := range chunks {
					out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}
				}
			}
		}
		chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, []byte("data: [DONE]"), &param)
		for i := range chunks {
			out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}
		}
		// Ensure we record the request if no usage chunk was ever seen
		reporter.EnsurePublished(ctx)
	}()
	return &cliproxyexecutor.StreamResult{Headers: streamResult.Headers, Chunks: out}, nil
}

func (e *OpenAICompatExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	from := opts.SourceFormat
	to := sdktranslator.FormatOpenAI
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	modelForCounting := baseModel

	translated, err := thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	enc, err := helps.TokenizerForModel(modelForCounting)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("openai compat executor: tokenizer init failed: %w", err)
	}

	count, err := helps.CountOpenAIChatTokens(enc, translated)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("openai compat executor: token counting failed: %w", err)
	}

	usageJSON := helps.BuildOpenAIUsageJSON(count)
	translatedUsage := sdktranslator.TranslateTokenCount(ctx, to, from, count, usageJSON)
	return cliproxyexecutor.Response{Payload: translatedUsage}, nil
}

// Refresh is a no-op for API-key based compatibility providers.
func (e *OpenAICompatExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("openai compat executor: refresh called")
	_ = ctx
	return auth, nil
}

func (e *OpenAICompatExecutor) resolveCredentials(auth *cliproxyauth.Auth) (baseURL, apiKey string) {
	if auth == nil {
		return "", ""
	}
	if auth.Attributes != nil {
		baseURL = strings.TrimSpace(auth.Attributes["base_url"])
		apiKey = strings.TrimSpace(auth.Attributes["api_key"])
	}
	return
}

func (e *OpenAICompatExecutor) defaultRoute() openAICompatUpstreamRoute {
	return openAICompatUpstreamRoute{
		format:             sdktranslator.FormatOpenAI,
		endpoint:           "/chat/completions",
		includeStreamUsage: true,
	}
}

func (e *OpenAICompatExecutor) resolveUpstreamRoute(opts cliproxyexecutor.Options) openAICompatUpstreamRoute {
	if opts.Alt == "responses/compact" {
		return openAICompatUpstreamRoute{
			format:             sdktranslator.FormatOpenAIResponse,
			endpoint:           "/responses/compact",
			includeStreamUsage: false,
		}
	}
	if opts.SourceFormat == sdktranslator.FormatOpenAIResponse {
		return openAICompatUpstreamRoute{
			format:              sdktranslator.FormatOpenAIResponse,
			endpoint:            "/responses",
			includeStreamUsage:  false,
			preserveSSEMetadata: true,
		}
	}
	return e.defaultRoute()
}

func (e *OpenAICompatExecutor) translateForRoute(req cliproxyexecutor.Request, opts cliproxyexecutor.Options, baseModel string, from sdktranslator.Format, route openAICompatUpstreamRoute) ([]byte, []byte, error) {
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalTranslated := sdktranslator.TranslateRequest(from, route.format, baseModel, originalPayloadSource, opts.Stream)
	translated := sdktranslator.TranslateRequest(from, route.format, baseModel, req.Payload, opts.Stream)
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	translated = helps.ApplyPayloadConfigWithRoot(e.cfg, baseModel, route.format.String(), "", translated, originalTranslated, requestedModel)
	if opts.Alt == "responses/compact" {
		if updated, errDelete := sjson.DeleteBytes(translated, "stream"); errDelete == nil {
			translated = updated
		}
	}
	translated, err := thinking.ApplyThinking(translated, req.Model, from.String(), route.format.String(), e.Identifier())
	if err != nil {
		return nil, nil, err
	}
	if opts.Stream && route.includeStreamUsage {
		translated, _ = sjson.SetBytes(translated, "stream_options.include_usage", true)
	}
	return originalTranslated, translated, nil
}

func (e *OpenAICompatExecutor) buildRequest(ctx context.Context, auth *cliproxyauth.Auth, apiKey, baseURL string, route openAICompatUpstreamRoute, translated []byte, stream bool) (*http.Request, string, error) {
	url := strings.TrimSuffix(baseURL, "/") + route.endpoint
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(translated))
	if err != nil {
		return nil, "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpReq.Header.Set("User-Agent", "cli-proxy-openai-compat")
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
		httpReq.Header.Set("Cache-Control", "no-cache")
	}
	return httpReq, url, nil
}

func (e *OpenAICompatExecutor) recordRequest(ctx context.Context, auth *cliproxyauth.Auth, url string, req *http.Request, translated []byte) {
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   req.Header.Clone(),
		Body:      translated,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})
}

func (e *OpenAICompatExecutor) executeNonStreamRoute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, baseModel, apiKey, baseURL string, from sdktranslator.Format, route openAICompatUpstreamRoute) (openAICompatAttemptResult, []byte, sdktranslator.Format, error) {
	_, translated, err := e.translateForRoute(req, opts, baseModel, from, route)
	if err != nil {
		return openAICompatAttemptResult{}, nil, "", err
	}
	httpReq, url, err := e.buildRequest(ctx, auth, apiKey, baseURL, route, translated, false)
	if err != nil {
		return openAICompatAttemptResult{}, nil, "", err
	}
	e.recordRequest(ctx, auth, url, httpReq, translated)
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return openAICompatAttemptResult{}, nil, "", err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("openai compat executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return openAICompatAttemptResult{}, nil, "", err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, body)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), body))
		return openAICompatAttemptResult{
			body:    body,
			headers: httpResp.Header.Clone(),
			status:  httpResp.StatusCode,
		}, translated, route.format, statusErr{code: httpResp.StatusCode, msg: string(body)}
	}
	return openAICompatAttemptResult{
		body:    body,
		headers: httpResp.Header.Clone(),
		status:  httpResp.StatusCode,
	}, translated, route.format, nil
}

func (e *OpenAICompatExecutor) executeNonStreamAttempt(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, baseModel, apiKey, baseURL string, from sdktranslator.Format, route openAICompatUpstreamRoute) (bool, bool, openAICompatAttemptResult, []byte, sdktranslator.Format, error) {
	result, translated, to, err := e.executeNonStreamRoute(ctx, auth, req, opts, baseModel, apiKey, baseURL, from, route)
	if err == nil {
		return false, true, result, translated, to, nil
	}
	if shouldFallbackResponsesToChat(route, result.status, result.body) {
		return true, true, openAICompatAttemptResult{}, nil, "", nil
	}
	return false, true, openAICompatAttemptResult{}, nil, "", err
}

func (e *OpenAICompatExecutor) executeStreamRoute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, baseModel, apiKey, baseURL string, from sdktranslator.Format, route openAICompatUpstreamRoute) (*cliproxyexecutor.StreamResult, []byte, sdktranslator.Format, error) {
	_, translated, err := e.translateForRoute(req, opts, baseModel, from, route)
	if err != nil {
		return nil, nil, "", err
	}
	httpReq, url, err := e.buildRequest(ctx, auth, apiKey, baseURL, route, translated, true)
	if err != nil {
		return nil, nil, "", err
	}
	e.recordRequest(ctx, auth, url, httpReq, translated)
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, nil, "", err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("openai compat executor: close response body error: %v", errClose)
		}
		return &cliproxyexecutor.StreamResult{
			Headers: httpResp.Header.Clone(),
			Chunks:  nil,
		}, translated, route.format, statusErr{code: httpResp.StatusCode, msg: string(b)}
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("openai compat executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800)
		for scanner.Scan() {
			out <- cliproxyexecutor.StreamChunk{Payload: bytes.Clone(scanner.Bytes())}
		}
		if errScan := scanner.Err(); errScan != nil {
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, translated, route.format, nil
}

func (e *OpenAICompatExecutor) executeStreamAttempt(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, baseModel, apiKey, baseURL string, from sdktranslator.Format, route openAICompatUpstreamRoute) (bool, bool, *cliproxyexecutor.StreamResult, []byte, sdktranslator.Format, error) {
	streamResult, translated, to, err := e.executeStreamRoute(ctx, auth, req, opts, baseModel, apiKey, baseURL, from, route)
	if err == nil {
		return false, true, streamResult, translated, to, nil
	}
	status := 0
	body := []byte(nil)
	if se, ok := err.(statusErr); ok {
		status = se.code
		body = []byte(se.msg)
	}
	if shouldFallbackResponsesToChat(route, status, body) {
		return true, true, nil, nil, "", nil
	}
	return false, true, nil, nil, "", err
}

func shouldFallbackResponsesToChat(route openAICompatUpstreamRoute, status int, body []byte) bool {
	if route.endpoint != "/responses" {
		return false
	}
	switch status {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return true
	}
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	return looksLikeUnsupportedResponsesEndpoint(body)
}

func looksLikeUnsupportedResponsesEndpoint(body []byte) bool {
	lower := strings.ToLower(strings.TrimSpace(string(body)))
	if lower == "" {
		return false
	}
	endpointHints := []string{
		"/responses",
		"responses endpoint",
		"unknown url",
		"unknown path",
		"unknown route",
		"unsupported endpoint",
		"unsupported route",
		"not found",
		"method not allowed",
	}
	match := false
	for _, hint := range endpointHints {
		if strings.Contains(lower, hint) {
			match = true
			break
		}
	}
	if !match {
		return false
	}
	return strings.Contains(lower, "response") || strings.Contains(lower, "endpoint") || strings.Contains(lower, "path") || strings.Contains(lower, "route") || strings.Contains(lower, "url")
}

func isResponsesSSEMetadataLine(line []byte) bool {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return false
	}
	return bytes.HasPrefix(trimmed, []byte("event:")) ||
		bytes.HasPrefix(trimmed, []byte("id:")) ||
		bytes.HasPrefix(trimmed, []byte("retry:")) ||
		bytes.HasPrefix(trimmed, []byte(":"))
}

func (e *OpenAICompatExecutor) overrideModel(payload []byte, model string) []byte {
	if len(payload) == 0 || model == "" {
		return payload
	}
	payload, _ = sjson.SetBytes(payload, "model", model)
	return payload
}

type statusErr struct {
	code       int
	msg        string
	retryAfter *time.Duration
}

func (e statusErr) Error() string {
	if e.msg != "" {
		return e.msg
	}
	return fmt.Sprintf("status %d", e.code)
}
func (e statusErr) StatusCode() int            { return e.code }
func (e statusErr) RetryAfter() *time.Duration { return e.retryAfter }
