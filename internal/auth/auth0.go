// Package auth implements the Auth0 authentication flow used by Maritaca chat.
// Since Auth0's /usernamepassword/login endpoint is protected by anomaly
// detection, we use a headless browser (chromedp) to perform the full OAuth
// Authorization Code with PKCE flow when programmatic login is required.
//
// For storing existing tokens (refresh tokens), we support direct refresh
// via /oauth/token without browser interaction.
package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"

	"github.com/deivid22srk/maritacaproxy/internal/logger"
)

// Config holds Auth0 configuration.
type Config struct {
	Domain     string
	ClientID   string
	Audience   string
	Scope      string
	RedirectURI string
	Connection string
}

// TokenSet represents the result of an OAuth exchange.
type TokenSet struct {
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

// Authenticator performs Auth0 flows.
type Authenticator struct {
	cfg    Config
	client *http.Client
}

// New creates an Authenticator with the given config.
func New(cfg Config) *Authenticator {
	jar, _ := cookiejar.New(nil)
	return &Authenticator{
		cfg: cfg,
		client: &http.Client{
			Timeout: 30 * time.Second,
			Jar:     jar,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				// Don't follow redirects automatically - we handle them explicitly
				return http.ErrUseLastResponse
			},
		},
	}
}

// PKCEPair holds a code_verifier and code_challenge pair.
type PKCEPair struct {
	Verifier  string
	Challenge string
}

// GeneratePKCE creates a new PKCE verifier/challenge pair.
func GeneratePKCE() (*PKCEPair, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(h[:])
	return &PKCEPair{Verifier: verifier, Challenge: challenge}, nil
}

// GenerateState generates a random state value.
func GenerateState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// BuildAuthorizeURL constructs the /authorize URL with PKCE and the optional
// login_ticket for cross-origin auth flow.
func (a *Authenticator) BuildAuthorizeURL(state string, pkce *PKCEPair, loginTicket string) string {
	scope := a.cfg.Scope
	if !strings.Contains(scope, "openid") {
		scope = "openid " + scope
	}
	url := fmt.Sprintf("https://%s/authorize?response_type=code&client_id=%s&redirect_uri=%s&scope=%s&audience=%s&state=%s&code_challenge=%s&code_challenge_method=S256",
		a.cfg.Domain,
		a.cfg.ClientID,
		encode(a.cfg.RedirectURI),
		encode(scope),
		encode(a.cfg.Audience),
		encode(state),
		pkce.Challenge,
	)
	if loginTicket != "" {
		url += "&login_ticket=" + encode(loginTicket)
	}
	return url
}

// SignupAccount creates a new Auth0 user via the /dbconnections/signup endpoint.
// Returns the user_id (or empty string) and whether the email requires verification.
func (a *Authenticator) SignupAccount(email, password string) (userID string, emailVerified bool, err error) {
	body := map[string]interface{}{
		"client_id":  a.cfg.ClientID,
		"email":      email,
		"password":   password,
		"connection": a.cfg.Connection,
	}
	bodyBytes, _ := json.Marshal(body)

	req, _ := http.NewRequest("POST", fmt.Sprintf("https://%s/dbconnections/signup", a.cfg.Domain), bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://chat.maritaca.ai")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/137.0.0.0 Safari/537.36")

	resp, err := a.client.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("signup request failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return "", false, fmt.Errorf("signup failed (%d): %s", resp.StatusCode, string(respBody))
	}

	var data struct {
		ID            string `json:"_id"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := json.Unmarshal(respBody, &data); err != nil {
		return "", false, fmt.Errorf("failed to parse signup response: %w", err)
	}
	logger.Info("[auth] Account created: email=%s verified=%v id=%s", email, data.EmailVerified, data.ID)
	return data.ID, data.EmailVerified, nil
}

// ResendVerification triggers Maritaca's /api/auth/resend-verification endpoint.
func (a *Authenticator) ResendVerification(email string) error {
	body := map[string]interface{}{"email": email}
	bodyBytes, _ := json.Marshal(body)

	req, _ := http.NewRequest("POST", "https://chat.maritaca.ai/api/auth/resend-verification", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/137.0.0.0 Safari/537.36")

	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("resend failed (%d): %s", resp.StatusCode, string(b))
	}
	return nil
}

// ExchangeCode exchanges an authorization code for tokens using PKCE.
func (a *Authenticator) ExchangeCode(code, verifier string) (*TokenSet, error) {
	body := map[string]interface{}{
		"grant_type":    "authorization_code",
		"client_id":     a.cfg.ClientID,
		"code":          code,
		"code_verifier": verifier,
		"redirect_uri":  a.cfg.RedirectURI,
	}
	bodyBytes, _ := json.Marshal(body)

	req, _ := http.NewRequest("POST", fmt.Sprintf("https://%s/oauth/token", a.cfg.Domain), bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("token exchange failed (%d): %s", resp.StatusCode, string(respBody))
	}

	var ts TokenSet
	if err := json.Unmarshal(respBody, &ts); err != nil {
		return nil, fmt.Errorf("failed to parse token response: %w", err)
	}
	return &ts, nil
}

// RefreshToken exchanges a refresh_token for a new access token.
func (a *Authenticator) RefreshToken(refreshToken string) (*TokenSet, error) {
	body := map[string]interface{}{
		"grant_type":    "refresh_token",
		"client_id":     a.cfg.ClientID,
		"refresh_token": refreshToken,
	}
	bodyBytes, _ := json.Marshal(body)

	req, _ := http.NewRequest("POST", fmt.Sprintf("https://%s/oauth/token", a.cfg.Domain), bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("refresh failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("refresh failed (%d): %s", resp.StatusCode, string(respBody))
	}

	var ts TokenSet
	if err := json.Unmarshal(respBody, &ts); err != nil {
		return nil, fmt.Errorf("failed to parse refresh response: %w", err)
	}
	return &ts, nil
}

func encode(s string) string {
	// Simple URL encoding using standard library
	return strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(
		strings.ReplaceAll(strings.ReplaceAll(s, " ", "%20"), "/", "%2F"),
		":", "%3A"), "?", "%3F"), "=", "%3D")
}
