package main

import (
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
	"github.com/kryspal/plugin-sdk/sdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func ratingReq(t pluginv1.ItemType, ids map[string]string) *pluginv1.GetRatingsRequest {
	return &pluginv1.GetRatingsRequest{Type: t, ProviderIds: ids}
}

// lookupAnswer is what /search/tmdb/{id} returns for one hit.
func lookupAnswer(kind string, rating float64, votes, tmdb int64, imdb string) []map[string]any {
	title := map[string]any{"rating": rating, "votes": votes, "ids": map[string]any{"trakt": 1, "imdb": imdb, "tmdb": tmdb}}
	return []map[string]any{{"type": kind, "score": 100, kind: title}}
}

func TestRatingByTMDB(t *testing.T) {
	p, f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		jsonReply(w, 200, lookupAnswer("movie", 8.7884, 61261, 603, "tt0133093"))
	})
	resp, err := p.GetRatings(t.Context(), ratingReq(pluginv1.ItemType_ITEM_TYPE_MOVIE, map[string]string{"tmdb": "603", "imdb": "tt0133093"}))
	if err != nil || len(resp.Ratings) != 1 {
		t.Fatalf("resp = %v, err = %v", resp, err)
	}
	if g := resp.Ratings[0]; g.Source != "trakt" || g.Value != 8.7884 || g.Max != 10 || g.Votes != 61261 {
		t.Errorf("rating = %v", g)
	}
	if l := f.last(); l.method != "GET" || l.path != "/search/tmdb/603" || l.query != "extended=full&type=movie" || l.auth != "" {
		t.Errorf("request = %+v", l)
	}
	if len(f.reqs) != 1 {
		t.Errorf("%d requests, want 1", len(f.reqs))
	}
}

func TestRatingSeriesUsesShowType(t *testing.T) {
	p, f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		jsonReply(w, 200, lookupAnswer("show", 9.3, 74041, 1396, "tt0903747"))
	})
	resp, err := p.GetRatings(t.Context(), ratingReq(pluginv1.ItemType_ITEM_TYPE_SERIES, map[string]string{"tmdb": "1396"}))
	if err != nil || len(resp.Ratings) != 1 || resp.Ratings[0].Votes != 74041 {
		t.Fatalf("resp = %v, err = %v", resp, err)
	}
	if l := f.last(); l.query != "extended=full&type=show" {
		t.Errorf("query = %q", l.query)
	}
}

func TestRatingByIMDbOnly(t *testing.T) {
	p, f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		jsonReply(w, 200, map[string]any{"rating": 8.73, "votes": 57023, "distribution": map[string]int{"10": 1}})
	})
	resp, err := p.GetRatings(t.Context(), ratingReq(pluginv1.ItemType_ITEM_TYPE_MOVIE, map[string]string{"imdb": "TT0137523"}))
	if err != nil || len(resp.Ratings) != 1 || resp.Ratings[0].Value != 8.73 {
		t.Fatalf("resp = %v, err = %v", resp, err)
	}
	if l := f.last(); l.path != "/movies/tt0137523/ratings" || l.query != "" {
		t.Errorf("request = %+v", l)
	}
	p2, f2 := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		jsonReply(w, 200, map[string]any{"rating": 9.3, "votes": 10})
	})
	if _, err := p2.GetRatings(t.Context(), ratingReq(pluginv1.ItemType_ITEM_TYPE_SERIES, map[string]string{"imdb": "tt0903747"})); err != nil {
		t.Fatal(err)
	}
	if l := f2.last(); l.path != "/shows/tt0903747/ratings" {
		t.Errorf("path = %q", l.path)
	}
}

// A bare number sent to /ratings is read as a Trakt ID and answers an unrelated title, so a TMDB number never goes there and an IMDb number
// must look like one.
func TestRatingNeverSendsABareNumberToRatings(t *testing.T) {
	p, f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		jsonReply(w, 200, map[string]any{"rating": 7.8, "votes": 915})
	})
	for _, ids := range []map[string]string{
		{"imdb": "603"}, {"imdb": "0133093"}, {"imdb": "tt"}, {"imdb": "tt12x"}, {"tvdb": "81189"}, {}, {"tmdb": "abc"}, {"tmdb": "-1"},
	} {
		resp, err := p.GetRatings(t.Context(), ratingReq(pluginv1.ItemType_ITEM_TYPE_MOVIE, ids))
		if err != nil || len(resp.Ratings) != 0 {
			t.Errorf("%v: resp = %v, err = %v", ids, resp, err)
		}
	}
	if len(f.reqs) != 0 {
		t.Errorf("requests = %+v, want none", f.reqs)
	}
}

func TestRatingHitMustMatchTheIDs(t *testing.T) {
	for name, tc := range map[string]struct {
		ids    map[string]string
		answer any
	}{
		"other tmdb":      {map[string]string{"tmdb": "603"}, lookupAnswer("movie", 8, 100, 604, "tt0133093")},
		"other imdb":      {map[string]string{"tmdb": "603", "imdb": "tt0000001"}, lookupAnswer("movie", 8, 100, 603, "tt0133093")},
		"wrong type":      {map[string]string{"tmdb": "603"}, lookupAnswer("show", 8, 100, 603, "tt0133093")},
		"empty (unknown)": {map[string]string{"tmdb": "999999999"}, []any{}},
		"no votes":        {map[string]string{"tmdb": "603"}, lookupAnswer("movie", 0, 0, 603, "tt0133093")},
	} {
		p, _ := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) { jsonReply(w, 200, tc.answer) })
		resp, err := p.GetRatings(t.Context(), ratingReq(pluginv1.ItemType_ITEM_TYPE_MOVIE, tc.ids))
		if err != nil || len(resp.Ratings) != 0 {
			t.Errorf("%s: resp = %v, err = %v", name, resp, err)
		}
	}
}

func TestRatingNotFoundIsEmpty(t *testing.T) {
	p, _ := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) { w.WriteHeader(404) })
	resp, err := p.GetRatings(t.Context(), ratingReq(pluginv1.ItemType_ITEM_TYPE_MOVIE, map[string]string{"imdb": "tt0000001"}))
	if err != nil || len(resp.Ratings) != 0 {
		t.Fatalf("resp = %v, err = %v", resp, err)
	}
}

func TestRatingErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		retry  string
		code   codes.Code
	}{
		"rate limit":   {429, "17", codes.ResourceExhausted},
		"unknown app":  {401, "", codes.FailedPrecondition},
		"forbidden":    {403, "", codes.FailedPrecondition},
		"server error": {502, "", codes.Unavailable},
		"other status": {418, "", codes.Unavailable},
	} {
		p, _ := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
			if tc.retry != "" {
				w.Header().Set("Retry-After", tc.retry)
			}
			w.WriteHeader(tc.status)
		})
		_, err := p.GetRatings(t.Context(), ratingReq(pluginv1.ItemType_ITEM_TYPE_MOVIE, map[string]string{"tmdb": "603"}))
		if tc.status == 429 { // the SDK turns this error into RESOURCE_EXHAUSTED with the retry hint
			var rl *sdk.RateLimitError
			if !errors.As(err, &rl) || rl.RetryAfter != 17*time.Second || rl.Credential {
				t.Errorf("rate limit error = %#v", err)
			}
		} else if status.Code(err) != tc.code {
			t.Errorf("%s: err = %v, want %v", name, err, tc.code)
		}
	}
}

func TestRatingRejectsOtherTypes(t *testing.T) {
	p, _ := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) { t.Error("no request expected") })
	_, err := p.GetRatings(t.Context(), ratingReq(pluginv1.ItemType_ITEM_TYPE_EPISODE, map[string]string{"tmdb": "1"}))
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("err = %v", err)
	}
}

func TestRatingCache(t *testing.T) {
	calls := 0
	p, _ := newFake(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		calls++
		jsonReply(w, 200, lookupAnswer("movie", 8.5, 500, 603, "tt0133093"))
	})
	now := time.Unix(1_800_000_000, 0)
	p.ratings.now = func() time.Time { return now }
	ids := map[string]string{"tmdb": "603"}
	get := func() {
		t.Helper()
		if resp, err := p.GetRatings(t.Context(), ratingReq(pluginv1.ItemType_ITEM_TYPE_MOVIE, ids)); err != nil || len(resp.Ratings) != 1 {
			t.Fatalf("resp = %v, err = %v", resp, err)
		}
	}
	get()
	now = now.Add(cacheTTL - time.Hour)
	get()
	if calls != 1 {
		t.Fatalf("%d requests inside the cache time, want 1", calls)
	}
	now = now.Add(2 * time.Hour)
	get()
	if calls != 2 {
		t.Fatalf("%d requests after it expired, want 2", calls)
	}
	// An item without a rating is remembered too, and a failed call is not.
	p.ratings = newRatingCache()
	p.ratings.now = func() time.Time { return now }
	calls = 0
	p.api.base = "http://127.0.0.1:1" // refused
	if _, err := p.GetRatings(t.Context(), ratingReq(pluginv1.ItemType_ITEM_TYPE_MOVIE, ids)); err == nil {
		t.Fatal("want an error")
	}
	if _, ok := p.ratings.get("movie:tmdb:603:imdb:"); ok {
		t.Error("a failed call was cached")
	}
}

func TestRatingCacheIsBounded(t *testing.T) {
	c := newRatingCache()
	for i := 0; i < cacheMax+50; i++ {
		c.put(strconv.Itoa(i), 8, 10)
	}
	if c.order.Len() != cacheMax || len(c.byKey) != cacheMax {
		t.Errorf("size = %d/%d, want %d", c.order.Len(), len(c.byKey), cacheMax)
	}
}

func TestRatingRequestSpacing(t *testing.T) {
	// Public GETs may go faster than the one-write-per-second spacing, but never unpaced.
	if getInterval >= minInterval || getInterval < 600*time.Millisecond {
		t.Errorf("getInterval = %v, minInterval = %v", getInterval, minInterval)
	}
}
