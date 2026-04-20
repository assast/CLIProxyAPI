package openai

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"time"

	coreauth "github.com/assast/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

const (
	routingStrategyRoundRobinSessionAffinity = "round-robin-session-affinity"
	responsesAffinityEntryTTL                = 6 * time.Hour
	responsesAffinityCleanupPeriod           = 30 * time.Minute
)

type responsesSessionAffinityEntry struct {
	authID string
	expire time.Time
}

type responsesSessionAffinityCache struct {
	mu          sync.RWMutex
	entries     map[string]responsesSessionAffinityEntry
	cleanupOnce sync.Once
}

var defaultResponsesSessionAffinityCache = &responsesSessionAffinityCache{
	entries: make(map[string]responsesSessionAffinityEntry),
}

func responsesSessionAffinityEnabled(manager *coreauth.Manager) bool {
	if manager == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(manager.RoutingStrategy()), routingStrategyRoundRobinSessionAffinity)
}

func responsesAffinityKeyForPreviousResponseID(rawJSON []byte) string {
	return responsesAffinityKeyForResponseID(strings.TrimSpace(gjson.GetBytes(rawJSON, "previous_response_id").String()))
}

func responsesAffinityKeyForResponseID(responseID string) string {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return ""
	}
	return "responses:response:" + responseID
}

func responsesAffinityKeyForWebsocketSession(sessionKey string) string {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return ""
	}
	return "responses:websocket:" + sessionKey
}

func responsesAffinityKeyForHTTPSession(sessionKey string) string {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return ""
	}
	return "responses:http:" + sessionKey
}

// httpResponsesDownstreamSessionKey extracts a stable session identifier from
// HTTP request headers. It checks Claude Code, Codex, and generic session
// headers in priority order.
func httpResponsesDownstreamSessionKey(req *http.Request) string {
	if req == nil {
		return ""
	}
	// Claude Code session
	if v := strings.TrimSpace(req.Header.Get("X-Claude-Code-Session-Id")); v != "" {
		return v
	}
	// Codex turn metadata
	if raw := strings.TrimSpace(req.Header.Get("X-Codex-Turn-Metadata")); raw != "" {
		if sid := strings.TrimSpace(gjson.Get(raw, "session_id").String()); sid != "" {
			return sid
		}
	}
	// Generic session headers
	if v := strings.TrimSpace(req.Header.Get("Session_id")); v != "" {
		return v
	}
	if v := strings.TrimSpace(req.Header.Get("X-Session-Id")); v != "" {
		return v
	}
	return ""
}

func responsesResponseIDFromPayload(rawJSON []byte) string {
	responseID := strings.TrimSpace(gjson.GetBytes(rawJSON, "id").String())
	if responseID == "" {
		return ""
	}
	objectType := strings.TrimSpace(gjson.GetBytes(rawJSON, "object").String())
	if objectType != "" && objectType != "response" {
		return ""
	}
	return responseID
}

func responsesResponseIDFromSSEFrame(frame []byte) string {
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || !gjson.ValidBytes(payload) {
			continue
		}
		if strings.TrimSpace(gjson.GetBytes(payload, "type").String()) != wsEventTypeCompleted {
			continue
		}
		if responseID := strings.TrimSpace(gjson.GetBytes(payload, "response.id").String()); responseID != "" {
			return responseID
		}
	}
	return ""
}

func responsesResponseIDFromWebsocketPayload(payload []byte) string {
	if !gjson.ValidBytes(payload) {
		return ""
	}
	if strings.TrimSpace(gjson.GetBytes(payload, "type").String()) != wsEventTypeCompleted {
		return ""
	}
	return strings.TrimSpace(gjson.GetBytes(payload, "response.id").String())
}

func responsesSessionAffinityRemember(key, authID string) {
	defaultResponsesSessionAffinityCache.set(key, authID)
}

func responsesSessionAffinityResolveAuthID(manager *coreauth.Manager, key string) string {
	authID := defaultResponsesSessionAffinityCache.get(key)
	if authID == "" || manager == nil {
		return authID
	}
	auth, ok := manager.GetByID(authID)
	if !ok || auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
		defaultResponsesSessionAffinityCache.delete(key)
		return ""
	}
	return authID
}

func (c *responsesSessionAffinityCache) set(key, authID string) {
	key = strings.TrimSpace(key)
	authID = strings.TrimSpace(authID)
	if key == "" || authID == "" || c == nil {
		return
	}
	c.cleanupOnce.Do(c.startCleanup)
	now := time.Now()
	c.mu.Lock()
	c.entries[key] = responsesSessionAffinityEntry{
		authID: authID,
		expire: now.Add(responsesAffinityEntryTTL),
	}
	c.mu.Unlock()
}

func (c *responsesSessionAffinityCache) get(key string) string {
	key = strings.TrimSpace(key)
	if key == "" || c == nil {
		return ""
	}
	c.cleanupOnce.Do(c.startCleanup)
	now := time.Now()
	c.mu.RLock()
	entry, ok := c.entries[key]
	valid := ok && entry.authID != "" && entry.expire.After(now)
	c.mu.RUnlock()
	if !valid {
		if ok {
			c.delete(key)
		}
		return ""
	}
	c.mu.Lock()
	entry, ok = c.entries[key]
	if ok && entry.authID != "" && entry.expire.After(now) {
		entry.expire = now.Add(responsesAffinityEntryTTL)
		c.entries[key] = entry
	}
	c.mu.Unlock()
	return entry.authID
}

func (c *responsesSessionAffinityCache) delete(key string) {
	key = strings.TrimSpace(key)
	if key == "" || c == nil {
		return
	}
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

func (c *responsesSessionAffinityCache) startCleanup() {
	go func() {
		ticker := time.NewTicker(responsesAffinityCleanupPeriod)
		defer ticker.Stop()
		for range ticker.C {
			c.purgeExpired()
		}
	}()
}

func (c *responsesSessionAffinityCache) purgeExpired() {
	if c == nil {
		return
	}
	now := time.Now()
	c.mu.Lock()
	for key, entry := range c.entries {
		if entry.authID == "" || !entry.expire.After(now) {
			delete(c.entries, key)
		}
	}
	c.mu.Unlock()
}
