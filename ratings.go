package main

import (
	"container/list"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
	"github.com/kryspal/plugin-sdk/sdk"
)

// Ratings: Trakt's community rating for movies and series another plugin has matched. Public endpoints, so only the client ID is sent (no member
// account, no OAuth). Verified against the live API on 2026-10-25 (research/0234b):
//
//	GET /search/tmdb/{id}?type=movie|show&extended=full   one request: [{type, score, movie|show:{rating, votes, ids:{trakt, imdb, tmdb}}}]; an
//	                                                      unknown ID is 200 with an empty array, not 404
//	GET /movies|shows/{imdb id}/ratings                   {rating, votes, distribution}
//
// A bare number in /ratings is read as a Trakt ID, so a TMDB number must never be sent there: it answers with an unrelated title. /ratings is
// therefore called only with an IMDb ID ("tt" and digits). Never extended=all: those are other sites' ratings, and the source here is Trakt's own.
const (
	ratingSource = "trakt"

	// cacheTTL is how long an answer is reused. The host asks again after a metadata refresh and in a weekly task; a week keeps a library's
	// requests low without letting ratings go stale for long.
	cacheTTL = 7 * 24 * time.Hour
	// cacheMax bounds the cache so a very large library cannot grow the process without limit; the oldest entries go first.
	cacheMax = 20000
)

var imdbID = regexp.MustCompile(`^tt[0-9]{1,12}$`)

type ratingEntry struct {
	key     string
	rating  float64
	votes   int64
	fetched time.Time
}

// ratingCache is a bounded in-memory cache of answers, found or not (an item without a rating is remembered too).
type ratingCache struct {
	mu    sync.Mutex
	order *list.List // front is the newest
	byKey map[string]*list.Element
	now   func() time.Time
}

func newRatingCache() *ratingCache {
	return &ratingCache{order: list.New(), byKey: map[string]*list.Element{}, now: time.Now}
}

func (c *ratingCache) get(key string) (ratingEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.byKey[key]
	if !ok {
		return ratingEntry{}, false
	}
	e := el.Value.(ratingEntry)
	if c.now().Sub(e.fetched) > cacheTTL {
		c.order.Remove(el)
		delete(c.byKey, key)
		return ratingEntry{}, false
	}
	return e, true
}

func (c *ratingCache) put(key string, rating float64, votes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.byKey[key]; ok {
		c.order.Remove(el)
	}
	c.byKey[key] = c.order.PushFront(ratingEntry{key: key, rating: rating, votes: votes, fetched: c.now()})
	for c.order.Len() > cacheMax {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.byKey, oldest.Value.(ratingEntry).key)
	}
}

// GetRatings looks the item up by its TMDB number, else by its IMDb number, and returns Trakt's rating. It never searches by name. An item with
// no usable number, one Trakt does not know and one without votes get an empty list.
func (p *plugin) GetRatings(ctx context.Context, req *pluginv1.GetRatingsRequest) (*pluginv1.GetRatingsResponse, error) {
	var kind string // Trakt's name for the type
	switch req.GetType() {
	case pluginv1.ItemType_ITEM_TYPE_MOVIE:
		kind = "movie"
	case pluginv1.ItemType_ITEM_TYPE_SERIES:
		kind = "show"
	default:
		return nil, sdk.InvalidArgument("trakt: ratings are for movies and series")
	}
	ids := req.GetProviderIds()
	tmdb, imdb := strings.TrimSpace(ids["tmdb"]), strings.ToLower(strings.TrimSpace(ids["imdb"]))
	if _, err := strconv.ParseUint(tmdb, 10, 64); err != nil {
		tmdb = ""
	}
	if !imdbID.MatchString(imdb) {
		imdb = ""
	}
	if tmdb == "" && imdb == "" {
		return &pluginv1.GetRatingsResponse{}, nil
	}
	key := kind + ":tmdb:" + tmdb + ":imdb:" + imdb
	if e, ok := p.ratings.get(key); ok {
		return ratingResponse(e.rating, e.votes), nil
	}
	s := p.settings()
	var rating float64
	var votes int64
	var err error
	if tmdb != "" {
		rating, votes, err = p.lookupRating(ctx, s, kind, tmdb, imdb)
	} else {
		rating, votes, err = p.imdbRating(ctx, s, kind, imdb)
	}
	if err != nil {
		return nil, err
	}
	p.ratings.put(key, rating, votes)
	return ratingResponse(rating, votes), nil
}

func ratingResponse(rating float64, votes int64) *pluginv1.GetRatingsResponse {
	if !(rating > 0) || votes <= 0 {
		return &pluginv1.GetRatingsResponse{}
	}
	return &pluginv1.GetRatingsResponse{Ratings: []*pluginv1.Rating{{Source: ratingSource, Value: rating, Max: 10, Votes: votes}}}
}

type traktIDs struct {
	IMDb string `json:"imdb"`
	TMDB int64  `json:"tmdb"`
}

type traktTitle struct {
	Rating float64  `json:"rating"`
	Votes  int64    `json:"votes"`
	IDs    traktIDs `json:"ids"`
}

type lookupHit struct {
	Type  string     `json:"type"`
	Movie traktTitle `json:"movie"`
	Show  traktTitle `json:"show"`
}

// lookupRating asks Trakt for the title with this TMDB number: one request returns its rating and votes. A hit counts only when its TMDB number is
// the one asked for and, when the item also has an IMDb number, Trakt's IMDb number agrees; otherwise the item's numbers contradict each other and
// the plugin answers nothing rather than guess.
func (p *plugin) lookupRating(ctx context.Context, s settings, kind, tmdb, imdb string) (float64, int64, error) {
	q := url.Values{"type": {kind}, "extended": {"full"}}
	r, err := p.api.do(ctx, http.MethodGet, "/search/tmdb/"+tmdb, s.id(), "", q, nil)
	if err != nil {
		return 0, 0, err
	}
	if err := ratingStatus(r); err != nil || r.status == http.StatusNotFound {
		return 0, 0, err
	}
	var hits []lookupHit
	if json.Unmarshal(r.body, &hits) != nil {
		return 0, 0, sdk.Unavailable("trakt: unexpected answer")
	}
	want, _ := strconv.ParseInt(tmdb, 10, 64)
	for _, h := range hits {
		if h.Type != kind {
			continue
		}
		t := h.Movie
		if kind == "show" {
			t = h.Show
		}
		if t.IDs.TMDB != want || (imdb != "" && t.IDs.IMDb != "" && !strings.EqualFold(t.IDs.IMDb, imdb)) {
			continue
		}
		return t.Rating, t.Votes, nil
	}
	return 0, 0, nil
}

// imdbRating reads the rating by IMDb number, for items that have no TMDB number.
func (p *plugin) imdbRating(ctx context.Context, s settings, kind, imdb string) (float64, int64, error) {
	r, err := p.api.do(ctx, http.MethodGet, "/"+kind+"s/"+imdb+"/ratings", s.id(), "", nil, nil)
	if err != nil {
		return 0, 0, err
	}
	if err := ratingStatus(r); err != nil || r.status == http.StatusNotFound {
		return 0, 0, err
	}
	var t traktTitle
	if json.Unmarshal(r.body, &t) != nil {
		return 0, 0, sdk.Unavailable("trakt: unexpected answer")
	}
	return t.Rating, t.Votes, nil
}

// ratingStatus maps a status the rating calls cannot use. 404 is "nothing to say" and is left to the caller. The rate limit is for the plugin's
// own client ID and address, so it pauses the plugin's rating calls (the host keeps applying metadata). Trakt answers 401 and 403 when the
// application is unknown or was deleted: that is a setup problem, not a missing rating.
func ratingStatus(r reply) error {
	switch r.status {
	case http.StatusOK, http.StatusNotFound:
		return nil
	case http.StatusTooManyRequests:
		return sdk.RateLimited(r.retryAfter(), "trakt: rate limit reached")
	case http.StatusUnauthorized, http.StatusForbidden:
		return sdk.NeedsConfiguration("trakt: the application was refused; check the client ID")
	}
	return sdk.Unavailable("trakt: HTTP " + strconv.Itoa(r.status))
}
