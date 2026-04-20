package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/assast/CLIProxyAPI/v6/internal/registry"
	"github.com/assast/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/assast/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/assast/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/assast/CLIProxyAPI/v6/sdk/config"
)

type orderedResponsesSelector struct {
	mu     sync.Mutex
	order  []string
	cursor int
}

func (s *orderedResponsesSelector) Pick(_ context.Context, _ string, _ string, _ coreexecutor.Options, auths []*coreauth.Auth) (*coreauth.Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(auths) == 0 {
		return nil, errors.New("no auth available")
	}
	for s.cursor < len(s.order) {
		authID := strings.TrimSpace(s.order[s.cursor])
		s.cursor++
		for _, auth := range auths {
			if auth != nil && auth.ID == authID {
				return auth, nil
			}
		}
	}
	for _, auth := range auths {
		if auth != nil {
			return auth, nil
		}
	}
	return nil, errors.New("no auth available")
}

type responsesAffinityExecutor struct {
	mu      sync.Mutex
	authIDs []string
	call    int
}

func (e *responsesAffinityExecutor) Identifier() string { return "test-provider" }

func (e *responsesAffinityExecutor) Execute(_ context.Context, auth *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if auth != nil {
		e.authIDs = append(e.authIDs, auth.ID)
	}
	e.call++
	if e.call == 1 {
		return coreexecutor.Response{Payload: []byte(`{"id":"resp-1","object":"response","status":"completed","output":[]}`)}, nil
	}
	return coreexecutor.Response{Payload: []byte(`{"id":"resp-2","object":"response","status":"completed","output":[]}`)}, nil
}

func (e *responsesAffinityExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}

func (e *responsesAffinityExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *responsesAffinityExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *responsesAffinityExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *responsesAffinityExecutor) AuthIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.authIDs...)
}

func TestOpenAIResponsesNonStreamSessionAffinityUsesPreviousResponseID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	selector := &orderedResponsesSelector{order: []string{"auth-a", "auth-b"}}
	executor := &responsesAffinityExecutor{}
	manager := coreauth.NewManager(nil, selector, nil)
	manager.RegisterExecutor(executor)
	manager.SetConfig(&sdkconfig.Config{
		Routing: sdkconfig.RoutingConfig{Strategy: routingStrategyRoundRobinSessionAffinity},
	})

	authA := &coreauth.Auth{ID: "auth-a", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	authB := &coreauth.Auth{ID: "auth-b", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), authA); err != nil {
		t.Fatalf("Register authA: %v", err)
	}
	if _, err := manager.Register(context.Background(), authB); err != nil {
		t.Fatalf("Register authB: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(authA.ID, authA.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	registry.GetGlobalRegistry().RegisterClient(authB.ID, authB.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(authA.ID)
		registry.GetGlobalRegistry().UnregisterClient(authB.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/responses", h.Responses)

	req1 := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test-model","input":"hello"}`))
	req1.Header.Set("Content-Type", "application/json")
	resp1 := httptest.NewRecorder()
	router.ServeHTTP(resp1, req1)
	if resp1.Code != http.StatusOK {
		t.Fatalf("first status = %d, want %d", resp1.Code, http.StatusOK)
	}

	req2 := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test-model","previous_response_id":"resp-1","input":[]}`))
	req2.Header.Set("Content-Type", "application/json")
	resp2 := httptest.NewRecorder()
	router.ServeHTTP(resp2, req2)
	if resp2.Code != http.StatusOK {
		t.Fatalf("second status = %d, want %d", resp2.Code, http.StatusOK)
	}

	if got := executor.AuthIDs(); len(got) != 2 || got[0] != "auth-a" || got[1] != "auth-a" {
		t.Fatalf("selected auth IDs = %v, want [auth-a auth-a]", got)
	}
}

type responsesAffinityFallbackExecutor struct {
	mu         sync.Mutex
	authIDs    []string
	authACalls int
	successes  int
}

func (e *responsesAffinityFallbackExecutor) Identifier() string { return "test-provider" }

func (e *responsesAffinityFallbackExecutor) Execute(_ context.Context, auth *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	authID := ""
	if auth != nil {
		authID = auth.ID
		e.authIDs = append(e.authIDs, authID)
	}

	if authID == "auth-a" {
		e.authACalls++
		if e.authACalls > 1 {
			return coreexecutor.Response{}, &coreauth.Error{
				Code:       "unauthorized",
				Message:    "unauthorized",
				Retryable:  false,
				HTTPStatus: http.StatusUnauthorized,
			}
		}
	}

	e.successes++
	responseID := fmt.Sprintf("resp-%d", e.successes)
	return coreexecutor.Response{Payload: []byte(fmt.Sprintf(`{"id":"%s","object":"response","status":"completed","output":[]}`, responseID))}, nil
}

func (e *responsesAffinityFallbackExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}

func (e *responsesAffinityFallbackExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *responsesAffinityFallbackExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *responsesAffinityFallbackExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *responsesAffinityFallbackExecutor) AuthIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.authIDs...)
}

func TestOpenAIResponsesNonStreamSessionAffinityFallsBackAndRebinds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	selector := &orderedResponsesSelector{order: []string{"auth-a", "auth-b"}}
	executor := &responsesAffinityFallbackExecutor{}
	manager := coreauth.NewManager(nil, selector, nil)
	manager.RegisterExecutor(executor)
	manager.SetConfig(&sdkconfig.Config{
		Routing: sdkconfig.RoutingConfig{Strategy: routingStrategyRoundRobinSessionAffinity},
	})

	authA := &coreauth.Auth{ID: "auth-a", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	authB := &coreauth.Auth{ID: "auth-b", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), authA); err != nil {
		t.Fatalf("Register authA: %v", err)
	}
	if _, err := manager.Register(context.Background(), authB); err != nil {
		t.Fatalf("Register authB: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(authA.ID, authA.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	registry.GetGlobalRegistry().RegisterClient(authB.ID, authB.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(authA.ID)
		registry.GetGlobalRegistry().UnregisterClient(authB.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/responses", h.Responses)

	req1 := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test-model","input":"hello"}`))
	req1.Header.Set("Content-Type", "application/json")
	resp1 := httptest.NewRecorder()
	router.ServeHTTP(resp1, req1)
	if resp1.Code != http.StatusOK {
		t.Fatalf("first status = %d, want %d", resp1.Code, http.StatusOK)
	}

	req2 := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test-model","previous_response_id":"resp-1","input":[]}`))
	req2.Header.Set("Content-Type", "application/json")
	resp2 := httptest.NewRecorder()
	router.ServeHTTP(resp2, req2)
	if resp2.Code != http.StatusOK {
		t.Fatalf("second status = %d, want %d body=%s", resp2.Code, http.StatusOK, resp2.Body.String())
	}

	req3 := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test-model","previous_response_id":"resp-2","input":[]}`))
	req3.Header.Set("Content-Type", "application/json")
	resp3 := httptest.NewRecorder()
	router.ServeHTTP(resp3, req3)
	if resp3.Code != http.StatusOK {
		t.Fatalf("third status = %d, want %d body=%s", resp3.Code, http.StatusOK, resp3.Body.String())
	}

	if got := executor.AuthIDs(); len(got) != 4 || got[0] != "auth-a" || got[1] != "auth-a" || got[2] != "auth-b" || got[3] != "auth-b" {
		t.Fatalf("selected auth IDs = %v, want [auth-a auth-a auth-b auth-b]", got)
	}
}
