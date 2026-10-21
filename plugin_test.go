package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
	"github.com/kryspal/plugin-sdk/sdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fake is a stand-in for the Trakt API: it records requests and answers from a handler.
type fake struct {
	mu   sync.Mutex
	reqs []rec
	h    func(w http.ResponseWriter, r *http.Request, body string)
}

type rec struct{ method, path, query, auth, body string }

func newFake(t *testing.T, h func(w http.ResponseWriter, r *http.Request, body string)) (*plugin, *fake) {
	t.Helper()
	f := &fake{h: h}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, rec{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), string(b)})
		f.mu.Unlock()
		if r.Header.Get("trakt-api-version") != "2" || r.Header.Get("trakt-api-key") != "cid" || !strings.HasPrefix(r.Header.Get("User-Agent"), "Kryspal Media v") {
			t.Errorf("missing common headers on %s", r.URL.Path)
		}
		f.h(w, r, string(b))
	}))
	t.Cleanup(srv.Close)
	p := newPlugin()
	p.api.base, p.api.interval = srv.URL, 0
	p.set = settings{clientID: "cid", clientSecret: "sec"}
	return p, f
}

func (f *fake) last() rec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[len(f.reqs)-1]
}

func jsonReply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

var (
	far  = time.Now().Add(60 * 24 * time.Hour)
	soon = time.Now().Add(time.Hour)
)

func credsFor(access, refresh string, expires time.Time) *pluginv1.Credentials {
	return creds{Access: access, Refresh: refresh, Expires: expires.Unix()}.pb()
}

func code(err error) codes.Code { return status.Code(err) }

func TestBeginLinkReturnsTheDeviceCodeAsState(t *testing.T) {
	p, f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		jsonReply(w, 200, map[string]any{"device_code": "dev1", "user_code": "ABCD1234", "verification_url": "https://trakt.tv/activate", "expires_in": 600, "interval": 5})
	})
	ch, err := p.BeginLink(t.Context(), &pluginv1.BeginLinkRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if ch.UserCode != "ABCD1234" || ch.VerificationUrl != "https://trakt.tv/activate" || ch.IntervalSeconds != 5 || ch.ExpiresInSeconds != 600 || string(ch.State) != "dev1" {
		t.Fatalf("challenge = %+v", ch)
	}
	if got := f.last(); got.path != "/oauth/device/code" || !strings.Contains(got.body, `"client_id":"cid"`) {
		t.Fatalf("request = %+v", got)
	}
}

func TestPollLinkStatuses(t *testing.T) {
	cases := []struct {
		status int
		want   pluginv1.LinkResult_Status
	}{
		{400, pluginv1.LinkResult_STATUS_PENDING}, {429, pluginv1.LinkResult_STATUS_PENDING},
		{404, pluginv1.LinkResult_STATUS_EXPIRED}, {409, pluginv1.LinkResult_STATUS_EXPIRED}, {410, pluginv1.LinkResult_STATUS_EXPIRED},
		{418, pluginv1.LinkResult_STATUS_DENIED},
	}
	for _, c := range cases {
		p, _ := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(c.status)
		})
		res, err := p.PollLink(t.Context(), &pluginv1.PollLinkRequest{State: []byte("dev1")})
		if err != nil || res.Status != c.want {
			t.Errorf("status %d: got %v %v, want %v", c.status, res.GetStatus(), err, c.want)
		}
		if c.status == 429 && res.RetryAfterSeconds != 7 {
			t.Errorf("429 retry_after = %d, want 7", res.RetryAfterSeconds)
		}
	}
}

func TestPollLinkAuthorizedReturnsCredentialsAndName(t *testing.T) {
	p, f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		switch r.URL.Path {
		case "/oauth/device/token":
			jsonReply(w, 200, map[string]any{"access_token": "acc", "refresh_token": "ref", "expires_in": 7776000, "created_at": time.Now().Unix()})
		case "/users/settings":
			jsonReply(w, 200, map[string]any{"user": map[string]any{"username": "someone"}})
		}
	})
	res, err := p.PollLink(t.Context(), &pluginv1.PollLinkRequest{State: []byte("dev1")})
	if err != nil || res.Status != pluginv1.LinkResult_STATUS_AUTHORIZED || res.AccountLabel != "someone" {
		t.Fatalf("result = %+v %v", res, err)
	}
	c, err := parseCreds(res.Credentials)
	if err != nil || c.Access != "acc" || c.Refresh != "ref" || c.Expires == 0 {
		t.Fatalf("credentials = %+v %v", c, err)
	}
	if !strings.Contains(f.reqs[0].body, `"code":"dev1"`) || !strings.Contains(f.reqs[0].body, `"client_secret":"sec"`) {
		t.Fatalf("token request = %s", f.reqs[0].body)
	}
}

func TestPollLinkWithoutASecretOmitsTheField(t *testing.T) {
	p, f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) { w.WriteHeader(400) })
	p.set = settings{clientID: "cid"}
	if _, err := p.PollLink(t.Context(), &pluginv1.PollLinkRequest{State: []byte("d")}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.last().body, "client_secret") {
		t.Fatalf("body = %s", f.last().body)
	}
}

func TestUnlinkRevokesAndNeverFails(t *testing.T) {
	p, f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) { w.WriteHeader(500) })
	if _, err := p.Unlink(t.Context(), &pluginv1.UnlinkRequest{Credentials: credsFor("acc", "ref", far)}); err != nil {
		t.Fatalf("Unlink failed: %v", err)
	}
	if got := f.last(); got.path != "/oauth/revoke" || !strings.Contains(got.body, `"token":"acc"`) {
		t.Fatalf("request = %+v", got)
	}
	if _, err := p.Unlink(t.Context(), &pluginv1.UnlinkRequest{Credentials: &pluginv1.Credentials{Data: []byte("junk")}}); err != nil {
		t.Fatalf("Unlink with junk credentials failed: %v", err)
	}
}

func TestScrobbleBodies(t *testing.T) {
	season, ep := int32(2), int32(5)
	cases := []struct {
		name  string
		media *pluginv1.MediaRef
		want  string
	}{
		{"movie", &pluginv1.MediaRef{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, ProviderIds: map[string]string{"tmdb": "603", "imdb": "tt0133093"}}, `"movie":{"ids":{"imdb":"tt0133093","tmdb":603}}`},
		{"episode own ids", &pluginv1.MediaRef{Type: pluginv1.ItemType_ITEM_TYPE_EPISODE, ProviderIds: map[string]string{"tvdb": "99"}}, `"episode":{"ids":{"tvdb":99}}`},
		{"episode by series", &pluginv1.MediaRef{Type: pluginv1.ItemType_ITEM_TYPE_EPISODE, SeriesIds: map[string]string{"tmdb": "1396"}, SeasonNumber: &season, EpisodeNumber: &ep},
			`"episode":{"season":2,"number":5}`},
	}
	for _, c := range cases {
		p, f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) { w.WriteHeader(201) })
		_, err := p.Scrobble(t.Context(), &pluginv1.ScrobbleRequest{Credentials: credsFor("acc", "ref", far), EventId: "e1",
			Action: pluginv1.ScrobbleRequest_ACTION_STOP, Media: c.media, ProgressPercent: 91.5})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := f.last()
		if got.path != "/scrobble/stop" || got.auth != "Bearer acc" || !strings.Contains(got.body, c.want) || !strings.Contains(got.body, `"progress":91.5`) {
			t.Errorf("%s: request = %+v", c.name, got)
		}
	}
}

func TestScrobbleStatuses(t *testing.T) {
	media := &pluginv1.MediaRef{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, ProviderIds: map[string]string{"tmdb": "1"}}
	cases := []struct {
		status int
		want   codes.Code
	}{{200, codes.OK}, {409, codes.OK}, {404, codes.OK}, {422, codes.OK}, {401, codes.Unauthenticated}, {503, codes.Unavailable}}
	for _, c := range cases {
		p, _ := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) { w.WriteHeader(c.status) })
		_, err := p.Scrobble(t.Context(), &pluginv1.ScrobbleRequest{Credentials: credsFor("acc", "ref", far),
			Action: pluginv1.ScrobbleRequest_ACTION_START, Media: media})
		if code(err) != c.want {
			t.Errorf("status %d: code %v, want %v (%v)", c.status, code(err), c.want, err)
		}
	}
}

func TestScrobbleRateLimitIsPerCredential(t *testing.T) {
	p, _ := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(429)
	})
	_, err := p.Scrobble(t.Context(), &pluginv1.ScrobbleRequest{Credentials: credsFor("acc", "ref", far), Action: pluginv1.ScrobbleRequest_ACTION_START,
		Media: &pluginv1.MediaRef{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, ProviderIds: map[string]string{"tmdb": "1"}}})
	var rl *sdk.RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != 12*time.Second || !rl.Credential {
		t.Fatalf("err = %#v", err)
	}
}

func TestScrobbleRejectsItemsTraktCannotIdentify(t *testing.T) {
	p, f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) { w.WriteHeader(201) })
	_, err := p.Scrobble(t.Context(), &pluginv1.ScrobbleRequest{Credentials: credsFor("a", "r", far), Action: pluginv1.ScrobbleRequest_ACTION_START,
		Media: &pluginv1.MediaRef{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Title: "no ids", ProviderIds: map[string]string{"imdb": "bogus"}}})
	if code(err) != codes.InvalidArgument || len(f.reqs) != 0 {
		t.Fatalf("code %v, %d requests", code(err), len(f.reqs))
	}
}

func TestExpiringTokenIsRefreshedAndReturned(t *testing.T) {
	p, f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		if r.URL.Path == "/oauth/token" {
			jsonReply(w, 200, map[string]any{"access_token": "new", "refresh_token": "ref2", "expires_in": 7776000, "created_at": time.Now().Unix()})
			return
		}
		w.WriteHeader(201)
	})
	resp, err := p.Scrobble(t.Context(), &pluginv1.ScrobbleRequest{Credentials: credsFor("old", "ref", soon), Action: pluginv1.ScrobbleRequest_ACTION_PAUSE,
		Media: &pluginv1.MediaRef{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, ProviderIds: map[string]string{"tmdb": "1"}}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := parseCreds(resp.UpdatedCredentials)
	if err != nil || c.Access != "new" || c.Refresh != "ref2" {
		t.Fatalf("updated credentials = %+v %v", c, err)
	}
	if got := f.last(); got.auth != "Bearer new" {
		t.Fatalf("the scrobble used %q, want the new token", got.auth)
	}
}

func TestRefusedRefreshTokenNeedsRelink(t *testing.T) {
	p, _ := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) { w.WriteHeader(401) })
	_, err := p.PullWatched(t.Context(), &pluginv1.PullWatchedRequest{Credentials: credsFor("old", "ref", time.Now().Add(-time.Hour))})
	if code(err) != codes.Unauthenticated {
		t.Fatalf("code = %v (%v)", code(err), err)
	}
}

func TestPullWatchedPagesAndMapsItems(t *testing.T) {
	p, f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		w.Header().Set("X-Pagination-Page-Count", "3")
		jsonReply(w, 200, []map[string]any{
			{"watched_at": "2026-10-01T10:00:00.000Z", "type": "movie", "movie": map[string]any{"title": "A Film", "year": 1999, "ids": map[string]any{"trakt": 1, "imdb": "tt1", "tmdb": 603}}},
			{"watched_at": "2026-10-02T10:00:00.000Z", "type": "episode", "episode": map[string]any{"season": 2, "number": 5, "ids": map[string]any{"tvdb": 99}},
				"show": map[string]any{"title": "A Show", "year": 2008, "ids": map[string]any{"tmdb": 1396}}},
			{"watched_at": "2026-10-03T10:00:00.000Z", "type": "movie", "movie": map[string]any{"title": "No numbers", "ids": map[string]any{"trakt": 5}}},
		})
	})
	resp, err := p.PullWatched(t.Context(), &pluginv1.PullWatchedRequest{Credentials: credsFor("acc", "ref", far), Cursor: "2", Since: "2026-09-30T00:00:00Z", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.last(); got.path != "/sync/history" || !strings.Contains(got.query, "page=2") || !strings.Contains(got.query, "limit=50") || !strings.Contains(got.query, "start_at=2026-09-30T00%3A00%3A00Z") {
		t.Fatalf("request = %+v", got)
	}
	if resp.NextCursor != "3" || len(resp.Entries) != 2 {
		t.Fatalf("next %q, %d entries", resp.NextCursor, len(resp.Entries))
	}
	m := resp.Entries[0].Media
	if m.Type != pluginv1.ItemType_ITEM_TYPE_MOVIE || m.ProviderIds["tmdb"] != "603" || m.ProviderIds["imdb"] != "tt1" || m.Year != 1999 || !resp.Entries[0].Watched {
		t.Fatalf("movie = %+v", resp.Entries[0])
	}
	e := resp.Entries[1].Media
	if e.Type != pluginv1.ItemType_ITEM_TYPE_EPISODE || e.ProviderIds["tvdb"] != "99" || e.SeriesIds["tmdb"] != "1396" || e.GetSeasonNumber() != 2 || e.GetEpisodeNumber() != 5 {
		t.Fatalf("episode = %+v", e)
	}
}

func TestPullWatchedLastPageHasNoCursor(t *testing.T) {
	p, _ := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		w.Header().Set("X-Pagination-Page-Count", "1")
		jsonReply(w, 200, []any{})
	})
	resp, err := p.PullWatched(t.Context(), &pluginv1.PullWatchedRequest{Credentials: credsFor("acc", "ref", far)})
	if err != nil || resp.NextCursor != "" {
		t.Fatalf("%+v %v", resp, err)
	}
	for _, bad := range []*pluginv1.PullWatchedRequest{{Cursor: "x"}, {Cursor: "0"}, {Since: "yesterday"}} {
		bad.Credentials = credsFor("acc", "ref", far)
		if _, err := p.PullWatched(t.Context(), bad); code(err) != codes.InvalidArgument {
			t.Errorf("%+v: code %v, want InvalidArgument", bad, code(err))
		}
	}
}

func TestPushWatchedBuildsTheBodyAndMapsNotFound(t *testing.T) {
	s1, e1, e2 := int32(1), int32(1), int32(2)
	p, f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		jsonReply(w, 201, map[string]any{"added": map[string]any{"movies": 1, "episodes": 2}, "not_found": map[string]any{
			"movies": []any{map[string]any{"ids": map[string]any{"tmdb": 404}}},
			"shows":  []any{map[string]any{"ids": map[string]any{"tmdb": 1396}, "seasons": []any{map[string]any{"number": 1, "episodes": []any{map[string]any{"number": 2}}}}}},
		}})
	})
	entries := []*pluginv1.WatchedEntry{
		{Watched: true, WatchedAt: "2026-10-01T10:00:00Z", Media: &pluginv1.MediaRef{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, ProviderIds: map[string]string{"tmdb": "603"}}},
		{Watched: true, Media: &pluginv1.MediaRef{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, ProviderIds: map[string]string{"tmdb": "404"}}},
		{Watched: true, Media: &pluginv1.MediaRef{Type: pluginv1.ItemType_ITEM_TYPE_EPISODE, SeriesIds: map[string]string{"tmdb": "1396"}, SeasonNumber: &s1, EpisodeNumber: &e1}},
		{Watched: true, Media: &pluginv1.MediaRef{Type: pluginv1.ItemType_ITEM_TYPE_EPISODE, SeriesIds: map[string]string{"tmdb": "1396"}, SeasonNumber: &s1, EpisodeNumber: &e2}},
		{Watched: true, Media: &pluginv1.MediaRef{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Title: "no numbers"}},
		{Watched: false, Media: &pluginv1.MediaRef{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, ProviderIds: map[string]string{"tmdb": "7"}}},
	}
	resp, err := p.PushWatched(t.Context(), &pluginv1.PushWatchedRequest{Credentials: credsFor("acc", "ref", far), Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	var sent struct {
		Movies []struct {
			WatchedAt string `json:"watched_at"`
		} `json:"movies"`
		Shows []struct {
			Seasons []struct {
				Episodes []struct{ Number int } `json:"episodes"`
			} `json:"seasons"`
		} `json:"shows"`
	}
	if err := json.Unmarshal([]byte(f.last().body), &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent.Movies) != 2 || sent.Movies[0].WatchedAt != "2026-10-01T10:00:00Z" || len(sent.Shows) != 1 || len(sent.Shows[0].Seasons) != 1 || len(sent.Shows[0].Seasons[0].Episodes) != 2 {
		t.Fatalf("body = %s", f.last().body)
	}
	want := []int32{1, 3, 4, 5} // the unknown movie, the second episode (not found), the entry without numbers, the unwatched one
	if len(resp.Unmatched) != len(want) {
		t.Fatalf("unmatched = %v, want %v", resp.Unmatched, want)
	}
	for i := range want {
		if resp.Unmatched[i] != want[i] {
			t.Fatalf("unmatched = %v, want %v", resp.Unmatched, want)
		}
	}
}

func TestPushWatchedSendsNothingWhenNoEntryIsUsable(t *testing.T) {
	p, f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) { w.WriteHeader(201) })
	resp, err := p.PushWatched(t.Context(), &pluginv1.PushWatchedRequest{Credentials: credsFor("a", "r", far),
		Entries: []*pluginv1.WatchedEntry{{Watched: true, Media: &pluginv1.MediaRef{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE}}}})
	if err != nil || len(f.reqs) != 0 || len(resp.Unmatched) != 1 {
		t.Fatalf("%+v %v, %d requests", resp, err, len(f.reqs))
	}
	big := make([]*pluginv1.WatchedEntry, 101)
	if _, err := p.PushWatched(t.Context(), &pluginv1.PushWatchedRequest{Credentials: credsFor("a", "r", far), Entries: big}); code(err) != codes.InvalidArgument {
		t.Fatalf("101 entries: code %v", code(err))
	}
}

func TestConfigureAndDescribe(t *testing.T) {
	p := newPlugin()
	ctx := context.Background()
	cases := []struct {
		name   string
		values map[string]string
		field  string // field expected to carry an error; empty means valid
	}{
		{"own app", map[string]string{keyClientID: "x", keyClientSecret: "y"}, ""},
		{"secret without id", map[string]string{keyClientSecret: "y"}, keyClientID},
		{"http proxy", map[string]string{keyClientID: "x", keyBaseURL: "http://example.com"}, keyBaseURL},
	}
	for _, c := range cases {
		resp, err := p.Configure(ctx, &pluginv1.ConfigureRequest{Values: c.values})
		if err != nil {
			t.Fatal(err)
		}
		if (c.field == "") != (len(resp.FieldErrors) == 0) || (c.field != "" && resp.FieldErrors[c.field] == "") {
			t.Errorf("%s: errors = %v", c.name, resp.FieldErrors)
		}
	}
	m, err := p.Describe(ctx, &pluginv1.DescribeRequest{Language: "zh-CN"})
	if err != nil || m.Id != "trakt" || len(m.Capabilities) != 4 || m.Attribution.GetText() == "" {
		t.Fatalf("manifest = %+v %v", m, err)
	}
}

func TestWithoutAnyApplicationTheCallsNeedConfiguration(t *testing.T) {
	p := newPlugin()
	if _, err := p.BeginLink(t.Context(), &pluginv1.BeginLinkRequest{}); code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v (%v)", code(err), err)
	}
}

func TestAttributionCarriesTheUnmodifiedLogo(t *testing.T) {
	m, err := newPlugin().Describe(t.Context(), &pluginv1.DescribeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	a := m.GetAttribution()
	if a.GetText() != "Powered by Trakt" || a.GetLink() != "https://trakt.tv" || !strings.HasPrefix(a.GetLogoUrl(), "data:image/svg+xml;base64,") {
		t.Fatalf("attribution = %+v", a)
	}
	if !strings.Contains(string(logoSVG), "<svg") || strings.Contains(string(logoSVG), "<script") {
		t.Fatal("the embedded logo is not a plain SVG")
	}
}
