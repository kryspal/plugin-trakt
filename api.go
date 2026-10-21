package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/kryspal/plugin-sdk/sdk"
)

// The Trakt API (https://trakt.docs.apiary.io, developer.trakt.tv):
//
//	every request          trakt-api-version: 2, trakt-api-key: <client ID>, Content-Type and Accept: application/json, a recognizable User-Agent
//	POST /oauth/device/code     {client_id} → {device_code, user_code, verification_url, expires_in, interval}
//	POST /oauth/device/token    {code, client_id[, client_secret]} → 200 tokens; 400 pending; 404 wrong code; 409 already used; 410 expired; 418 denied; 429 slow down
//	POST /oauth/token           refresh_token grant → 200 tokens; 401/400 the refresh token is no longer valid
//	POST /oauth/revoke          {token, client_id[, client_secret]}
//	GET  /users/settings        Bearer → {user:{username}}
//	POST /scrobble/{start,pause,stop}   Bearer; 409 means the same action was just reported
//	GET  /sync/history          Bearer; page, limit, start_at; X-Pagination-Page-Count
//	POST /sync/history          Bearer; {movies, episodes, shows} with watched_at → {added, not_found}
//
// Terms (developer.trakt.tv, "Create an App"): follow the branding requirements, use only documented methods and never scrape the site, do not
// create extra applications to get around limits, cache and keep requests low. Writes are limited to about one per second; the plugin sends
// one request at a time, at least minInterval apart.

const (
	defaultBaseURL   = "https://api.trakt.tv"
	maxResponseBytes = 8 << 20
	requestTimeout   = 30 * time.Second
	minInterval      = 1100 * time.Millisecond
	defaultRetry     = 30 * time.Second
)

// api is the HTTP client: one request at a time, at least interval apart.
type api struct {
	http     *http.Client
	base     string
	interval time.Duration
	mu       sync.Mutex
	last     time.Time
}

func newAPI() *api {
	return &api{http: &http.Client{Timeout: requestTimeout}, base: defaultBaseURL, interval: minInterval}
}

type reply struct {
	status int
	header http.Header
	body   []byte
}

func (a *api) setBase(base string) {
	a.mu.Lock()
	a.base = base
	a.mu.Unlock()
}

func (a *api) wait(ctx context.Context) error {
	if d := time.Until(a.last.Add(a.interval)); d > 0 {
		t := time.NewTimer(d)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		}
	}
	a.last = time.Now()
	return nil
}

// do sends one request with the common headers. Connection errors and 5xx are mapped to UNAVAILABLE here; every other status, 429 and 401
// included, is returned for the caller, because the device flow and the scrobble call read them differently.
func (a *api) do(ctx context.Context, method, path, clientID, token string, query url.Values, body any) (reply, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.wait(ctx); err != nil {
		return reply{}, err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return reply{}, sdk.InvalidArgument("trakt: bad request")
		}
		rd = bytes.NewReader(b)
	}
	u := a.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return reply{}, sdk.Unavailable("trakt: bad request")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("trakt-api-version", "2")
	req.Header.Set("trakt-api-key", clientID)
	req.Header.Set("User-Agent", userAgent())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return reply{}, transportError(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return reply{}, sdk.Unavailable("trakt: " + err.Error())
	}
	if resp.StatusCode >= 500 {
		return reply{}, sdk.Unavailable("trakt: " + resp.Status)
	}
	return reply{status: resp.StatusCode, header: resp.Header, body: raw}, nil
}

// userAgent is the recognizable agent string: the application name and version.
func userAgent() string { return "Kryspal Media v" + version() }

// retryAfter reads the Retry-After header of a 429.
func (r reply) retryAfter() time.Duration {
	if s, err := strconv.Atoi(r.header.Get("Retry-After")); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	return defaultRetry
}

// rateLimited is the error for a 429.
func (r reply) rateLimited() error {
	return sdk.RateLimitedCredential(r.retryAfter(), "trakt: rate limit reached")
}

func transportError(ctx context.Context, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err // url.Error carries the full URL; keep only the cause
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return sdk.Unavailable("trakt: " + err.Error())
}
