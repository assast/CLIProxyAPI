// Package codex provides authentication and token management for OpenAI's Codex API.
// It handles the OAuth2 flow, including generating authorization URLs, exchanging
// authorization codes for tokens, and refreshing expired tokens. The package also
// defines data structures for storing and managing Codex authentication credentials.
package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/assast/CLIProxyAPI/v6/internal/config"
	"github.com/assast/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
)

// OAuth configuration constants for OpenAI Codex
const (
	AuthURL     = "https://auth.openai.com/oauth/authorize"
	TokenURL    = "https://auth.openai.com/oauth/token"
	ClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	RedirectURI = "http://localhost:1455/auth/callback"
)

// CodexAuth handles the OpenAI OAuth2 authentication flow.
// It manages the HTTP client and provides methods for generating authorization URLs,
// exchanging authorization codes for tokens, and refreshing access tokens.
type CodexAuth struct {
	httpClient *http.Client
}

// NewCodexAuth creates a new CodexAuth service instance.
// It initializes an HTTP client with proxy settings from the provided configuration.
func NewCodexAuth(cfg *config.Config) *CodexAuth {
	return NewCodexAuthWithProxyURL(cfg, "")
}

// NewCodexAuthWithProxyURL creates a new CodexAuth service instance.
// proxyURL takes precedence over cfg.ProxyURL when non-empty.
func NewCodexAuthWithProxyURL(cfg *config.Config, proxyURL string) *CodexAuth {
	effectiveProxyURL := strings.TrimSpace(proxyURL)
	var sdkCfg config.SDKConfig
	if cfg != nil {
		sdkCfg = cfg.SDKConfig
		if effectiveProxyURL == "" {
			effectiveProxyURL = strings.TrimSpace(cfg.ProxyURL)
		}
	}
	sdkCfg.ProxyURL = effectiveProxyURL
	return &CodexAuth{
		httpClient: util.SetProxy(&sdkCfg, &http.Client{}),
	}
}

// GenerateAuthURL creates the OAuth authorization URL with PKCE (Proof Key for Code Exchange).
// It constructs the URL with the necessary parameters, including the client ID,
// response type, redirect URI, scopes, and PKCE challenge.
func (o *CodexAuth) GenerateAuthURL(state string, pkceCodes *PKCECodes) (string, error) {
	if pkceCodes == nil {
		return "", fmt.Errorf("PKCE codes are required")
	}

	params := url.Values{
		"client_id":                  {ClientID},
		"response_type":              {"code"},
		"redirect_uri":               {RedirectURI},
		"scope":                      {"openid email profile offline_access"},
		"state":                      {state},
		"code_challenge":             {pkceCodes.CodeChallenge},
		"code_challenge_method":      {"S256"},
		"prompt":                     {"login"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
	}

	authURL := fmt.Sprintf("%s?%s", AuthURL, params.Encode())
	return authURL, nil
}

// ExchangeCodeForTokens exchanges an authorization code for access and refresh tokens.
// It performs an HTTP POST request to the OpenAI token endpoint with the provided
// authorization code and PKCE verifier.
func (o *CodexAuth) ExchangeCodeForTokens(ctx context.Context, code string, pkceCodes *PKCECodes) (*CodexAuthBundle, error) {
	return o.ExchangeCodeForTokensWithRedirect(ctx, code, RedirectURI, pkceCodes)
}

// ExchangeCodeForTokensWithRedirect exchanges an authorization code for tokens using
// a caller-provided redirect URI. This supports alternate auth flows such as device
// login while preserving the existing token parsing and storage behavior.
func (o *CodexAuth) ExchangeCodeForTokensWithRedirect(ctx context.Context, code, redirectURI string, pkceCodes *PKCECodes) (*CodexAuthBundle, error) {
	if pkceCodes == nil {
		return nil, fmt.Errorf("PKCE codes are required for token exchange")
	}
	if strings.TrimSpace(redirectURI) == "" {
		return nil, fmt.Errorf("redirect URI is required for token exchange")
	}

	// Prepare token exchange request
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {ClientID},
		"code":          {code},
		"redirect_uri":  {strings.TrimSpace(redirectURI)},
		"code_verifier": {pkceCodes.CodeVerifier},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", TokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to create token request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read token response: %w", err)
	}
	// log.Debugf("Token response: %s", string(body))

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange failed with status %d: %s", resp.StatusCode, string(body))
	}

	// Parse token response
	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
	}

	if err = json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to parse token response: %w", err)
	}

	// Extract account ID from ID token
	claims, err := ParseJWTToken(tokenResp.IDToken)
	if err != nil {
		log.Warnf("Failed to parse ID token: %v", err)
	}

	accountID := ""
	email := ""
	if claims != nil {
		accountID = claims.GetAccountID()
		email = claims.GetUserEmail()
	}

	// Create token data
	tokenData := CodexTokenData{
		IDToken:      tokenResp.IDToken,
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		AccountID:    accountID,
		Email:        email,
		Expire:       time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339),
	}

	// Create auth bundle
	bundle := &CodexAuthBundle{
		TokenData:   tokenData,
		LastRefresh: time.Now().Format(time.RFC3339),
	}

	return bundle, nil
}

// CreateTokenStorage creates a new CodexTokenStorage from a CodexAuthBundle.
// It populates the storage struct with token data, user information, and timestamps.
func (o *CodexAuth) CreateTokenStorage(bundle *CodexAuthBundle) *CodexTokenStorage {
	storage := &CodexTokenStorage{
		IDToken:      bundle.TokenData.IDToken,
		AccessToken:  bundle.TokenData.AccessToken,
		RefreshToken: bundle.TokenData.RefreshToken,
		AccountID:    bundle.TokenData.AccountID,
		LastRefresh:  bundle.LastRefresh,
		Email:        bundle.TokenData.Email,
		Expire:       bundle.TokenData.Expire,
	}

	return storage
}
