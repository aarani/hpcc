package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// IdP carries the per-tenant discovery info that the OAuth grants
// need. It mirrors the fields scheduler.GetTenantIdP returns —
// copied into this package so auth doesn't depend on the protobuf
// bindings.
type IdP struct {
	TokenURL string
	ClientID string
	Scope    string
}

// TokenResponse is the standard RFC 6749 §5.1 success/error payload.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// PasswordGrant runs an RFC 6749 §4.3 password grant against idp.
// Used only by `hpcc auth login`; the daemon never performs this at
// compile time because it would need the user's plaintext password
// in process memory.
func PasswordGrant(ctx context.Context, idp IdP, username, password, clientSecret string) (TokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "password")
	form.Set("username", username)
	form.Set("password", password)
	if idp.ClientID != "" {
		form.Set("client_id", idp.ClientID)
	}
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}
	if idp.Scope != "" {
		form.Set("scope", idp.Scope)
	}
	return postForm(ctx, idp.TokenURL, form)
}

// RefreshGrant runs an RFC 6749 §6 refresh grant. The IdP may rotate
// the refresh token in the response — callers should persist whatever
// comes back rather than reusing the old one.
func RefreshGrant(ctx context.Context, idp IdP, refreshToken, clientSecret string) (TokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	if idp.ClientID != "" {
		form.Set("client_id", idp.ClientID)
	}
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}
	return postForm(ctx, idp.TokenURL, form)
}

func postForm(ctx context.Context, tokenURL string, form url.Values) (TokenResponse, error) {
	if tokenURL == "" {
		return TokenResponse{}, fmt.Errorf("empty token_url")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return TokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return TokenResponse{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return TokenResponse{}, err
	}
	var parsed TokenResponse
	// IdPs return JSON for both success and RFC-6749 error bodies, so
	// decode first and only fall back to a status-line error if the
	// body isn't JSON-shaped.
	if jsonErr := json.Unmarshal(body, &parsed); jsonErr != nil {
		return TokenResponse{}, fmt.Errorf("token endpoint returned %s: %s", resp.Status, truncate(body, 256))
	}
	if resp.StatusCode != http.StatusOK || parsed.AccessToken == "" {
		if parsed.Error != "" {
			return TokenResponse{}, fmt.Errorf("oauth error %q: %s", parsed.Error, parsed.ErrorDesc)
		}
		return TokenResponse{}, fmt.Errorf("token endpoint returned %s: %s", resp.Status, truncate(body, 256))
	}
	return parsed, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
