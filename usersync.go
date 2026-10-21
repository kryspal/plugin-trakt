package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
	"github.com/kryspal/plugin-sdk/sdk"
)

const (
	defaultPageLimit = 100
	maxPageLimit     = 1000
	pushBatch        = 100
	// refreshBefore is how long before the access token ends the plugin gets a new one (Trakt tokens live about three months).
	refreshBefore = 24 * time.Hour
	// credentialsVersion is the format version of the stored credentials, so a later format can read an older one.
	credentialsVersion = 1
)

// creds is the plugin-defined content of Credentials.data. The server stores it encrypted and never reads it.
type creds struct {
	V       int    `json:"v"`
	Access  string `json:"access"`
	Refresh string `json:"refresh"`
	Expires int64  `json:"expires"` // Unix seconds when the access token ends; 0 when unknown
}

func parseCreds(c *pluginv1.Credentials) (creds, error) {
	var cr creds
	if err := json.Unmarshal(c.GetData(), &cr); err != nil || cr.Access == "" {
		return creds{}, sdk.Unauthenticated("trakt: the stored credentials are not usable")
	}
	return cr, nil
}

func (c creds) pb() *pluginv1.Credentials {
	c.V = credentialsVersion
	b, _ := json.Marshal(c)
	return &pluginv1.Credentials{Data: b}
}

type tokenReply struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	CreatedAt    int64  `json:"created_at"`
}

func (t tokenReply) creds(now time.Time) creds {
	created := t.CreatedAt
	if created == 0 {
		created = now.Unix()
	}
	var exp int64
	if t.ExpiresIn > 0 {
		exp = created + t.ExpiresIn
	}
	return creds{V: credentialsVersion, Access: t.AccessToken, Refresh: t.RefreshToken, Expires: exp}
}

// ---- Linking (device code flow) ----

// BeginLink asks Trakt for a device code. The device code goes back to the server as the opaque state and returns in PollLink.
func (p *plugin) BeginLink(ctx context.Context, _ *pluginv1.BeginLinkRequest) (*pluginv1.LinkChallenge, error) {
	s, err := p.app()
	if err != nil {
		return nil, err
	}
	r, err := p.api.do(ctx, http.MethodPost, "/oauth/device/code", s.id(), "", nil, map[string]string{"client_id": s.id()})
	if err != nil {
		return nil, err
	}
	if r.status == http.StatusTooManyRequests {
		return nil, r.rateLimited()
	}
	var dc struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURL string `json:"verification_url"`
		ExpiresIn       int32  `json:"expires_in"`
		Interval        int32  `json:"interval"`
	}
	if r.status != http.StatusOK || json.Unmarshal(r.body, &dc) != nil || dc.DeviceCode == "" || dc.UserCode == "" {
		if r.status == http.StatusUnauthorized || r.status == http.StatusForbidden {
			return nil, sdk.NeedsConfiguration("Trakt refused the application client ID")
		}
		return nil, sdk.Unavailable("trakt: device code request failed with " + strconv.Itoa(r.status))
	}
	if dc.Interval <= 0 {
		dc.Interval = 5
	}
	return &pluginv1.LinkChallenge{UserCode: dc.UserCode, VerificationUrl: dc.VerificationURL, ExpiresInSeconds: dc.ExpiresIn,
		IntervalSeconds: dc.Interval, State: []byte(dc.DeviceCode)}, nil
}

// PollLink asks once whether the member has entered the code. It never waits: the server calls again after the interval.
func (p *plugin) PollLink(ctx context.Context, req *pluginv1.PollLinkRequest) (*pluginv1.LinkResult, error) {
	s, err := p.app()
	if err != nil {
		return nil, err
	}
	body := map[string]string{"code": string(req.GetState()), "client_id": s.id()}
	if sec := s.secret(); sec != "" {
		body["client_secret"] = sec
	}
	r, err := p.api.do(ctx, http.MethodPost, "/oauth/device/token", s.id(), "", nil, body)
	if err != nil {
		return nil, err
	}
	switch r.status {
	case http.StatusOK:
		var tr tokenReply
		if json.Unmarshal(r.body, &tr) != nil || tr.AccessToken == "" {
			return nil, sdk.Unavailable("trakt: bad token response")
		}
		c := tr.creds(time.Now())
		return &pluginv1.LinkResult{Status: pluginv1.LinkResult_STATUS_AUTHORIZED, Credentials: c.pb(), AccountLabel: p.username(ctx, s, c.Access)}, nil
	case http.StatusBadRequest: // not entered yet
		return &pluginv1.LinkResult{Status: pluginv1.LinkResult_STATUS_PENDING}, nil
	case http.StatusTooManyRequests: // polling too fast: ask the server to slow down
		return &pluginv1.LinkResult{Status: pluginv1.LinkResult_STATUS_PENDING, RetryAfterSeconds: int32(r.retryAfter() / time.Second)}, nil
	case http.StatusGone, http.StatusNotFound, http.StatusConflict: // expired, wrong or already used
		return &pluginv1.LinkResult{Status: pluginv1.LinkResult_STATUS_EXPIRED}, nil
	case 418: // the member denied it
		return &pluginv1.LinkResult{Status: pluginv1.LinkResult_STATUS_DENIED}, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, sdk.NeedsConfiguration("Trakt refused the application client ID or secret")
	}
	return nil, sdk.Unavailable("trakt: token request failed with " + strconv.Itoa(r.status))
}

// username reads the account name for display. A failure only leaves the label empty.
func (p *plugin) username(ctx context.Context, s settings, token string) string {
	r, err := p.api.do(ctx, http.MethodGet, "/users/settings", s.id(), token, nil, nil)
	if err != nil || r.status != http.StatusOK {
		return ""
	}
	var us struct {
		User struct {
			Username string `json:"username"`
		} `json:"user"`
	}
	if json.Unmarshal(r.body, &us) != nil {
		return ""
	}
	return us.User.Username
}

// Unlink revokes the access token. It is best effort: every failure still answers success, and the server deletes the credentials anyway.
func (p *plugin) Unlink(ctx context.Context, req *pluginv1.UnlinkRequest) (*pluginv1.UnlinkResponse, error) {
	s := p.settings()
	c, err := parseCreds(req.GetCredentials())
	if err != nil || s.id() == "" {
		return &pluginv1.UnlinkResponse{}, nil
	}
	body := map[string]string{"token": c.Access, "client_id": s.id()}
	if sec := s.secret(); sec != "" {
		body["client_secret"] = sec
	}
	_, _ = p.api.do(ctx, http.MethodPost, "/oauth/revoke", s.id(), "", nil, body)
	return &pluginv1.UnlinkResponse{}, nil
}

// ---- Credentials ----

// fresh returns credentials with an access token that is not about to end, and the credentials to send back to the server when they changed.
// A refresh token Trakt no longer accepts means the member has to link again: UNAUTHENTICATED.
func (p *plugin) fresh(ctx context.Context, s settings, c creds) (creds, *pluginv1.Credentials, error) {
	if c.Expires == 0 || time.Until(time.Unix(c.Expires, 0)) > refreshBefore || c.Refresh == "" {
		return c, nil, nil
	}
	body := map[string]string{"refresh_token": c.Refresh, "client_id": s.id(), "redirect_uri": "urn:ietf:wg:oauth:2.0:oob", "grant_type": "refresh_token"}
	if sec := s.secret(); sec != "" {
		body["client_secret"] = sec
	}
	r, err := p.api.do(ctx, http.MethodPost, "/oauth/token", s.id(), "", nil, body)
	if err != nil {
		if time.Until(time.Unix(c.Expires, 0)) > 0 {
			return c, nil, nil // the old token still works: try with it, refresh next time
		}
		return c, nil, err
	}
	switch r.status {
	case http.StatusOK:
		var tr tokenReply
		if json.Unmarshal(r.body, &tr) != nil || tr.AccessToken == "" {
			return c, nil, sdk.Unavailable("trakt: bad token response")
		}
		n := tr.creds(time.Now())
		return n, n.pb(), nil
	case http.StatusTooManyRequests:
		if time.Until(time.Unix(c.Expires, 0)) > 0 {
			return c, nil, nil
		}
		return c, nil, r.rateLimited()
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		return c, nil, sdk.Unauthenticated("trakt: the refresh token is no longer valid")
	}
	return c, nil, sdk.Unavailable("trakt: token refresh failed with " + strconv.Itoa(r.status))
}

// authed runs one authorized request and maps the statuses every call shares: 401 means the member must link again, 429 a per-credential limit.
func (p *plugin) authed(ctx context.Context, s settings, c creds, method, path string, q url.Values, body any) (reply, error) {
	r, err := p.api.do(ctx, method, path, s.id(), c.Access, q, body)
	if err != nil {
		return r, err
	}
	switch r.status {
	case http.StatusUnauthorized:
		return r, sdk.Unauthenticated("trakt: the access token was refused")
	case http.StatusTooManyRequests:
		return r, r.rateLimited()
	}
	return r, nil
}

// ---- Items ----

// ids are the numbers Trakt can find an item with.
type ids struct {
	IMDb string `json:"imdb,omitempty"`
	TMDB int    `json:"tmdb,omitempty"`
	TVDB int    `json:"tvdb,omitempty"`
}

func (i ids) empty() bool { return i == ids{} }

func (i ids) same(o ids) bool {
	return (i.TMDB != 0 && i.TMDB == o.TMDB) || (i.TVDB != 0 && i.TVDB == o.TVDB) || (i.IMDb != "" && i.IMDb == o.IMDb)
}

func idsOf(m map[string]string) ids {
	var out ids
	if v := strings.TrimSpace(m["imdb"]); strings.HasPrefix(v, "tt") && len(v) > 2 {
		out.IMDb = v
	}
	out.TMDB, _ = strconv.Atoi(strings.TrimSpace(m["tmdb"]))
	out.TVDB, _ = strconv.Atoi(strings.TrimSpace(m["tvdb"]))
	return out
}

func (i ids) toMap() map[string]string {
	m := map[string]string{}
	if i.IMDb != "" {
		m["imdb"] = i.IMDb
	}
	if i.TMDB != 0 {
		m["tmdb"] = strconv.Itoa(i.TMDB)
	}
	if i.TVDB != 0 {
		m["tvdb"] = strconv.Itoa(i.TVDB)
	}
	return m
}

type idObj struct {
	IDs ids `json:"ids"`
}

type epNum struct {
	Season int `json:"season"`
	Number int `json:"number"`
}

// ---- Scrobble ----

// Scrobble reports start, pause or stop. Trakt counts a stop at 80 percent or more as watched. The same action reported twice answers 409, which
// is a success here: the server resends events until it hears one, and Trakt has no field for the time, so only the answer matters.
func (p *plugin) Scrobble(ctx context.Context, req *pluginv1.ScrobbleRequest) (*pluginv1.ScrobbleResponse, error) {
	s, err := p.app()
	if err != nil {
		return nil, err
	}
	var action string
	switch req.GetAction() {
	case pluginv1.ScrobbleRequest_ACTION_START:
		action = "start"
	case pluginv1.ScrobbleRequest_ACTION_PAUSE:
		action = "pause"
	case pluginv1.ScrobbleRequest_ACTION_STOP:
		action = "stop"
	default:
		return nil, sdk.InvalidArgument("trakt: unknown scrobble action")
	}
	body, ok := scrobbleBody(req.GetMedia())
	if !ok {
		return nil, sdk.InvalidArgument("trakt: the item has no TMDB, IMDb or TVDB number")
	}
	prog := req.GetProgressPercent()
	switch {
	case prog < 0:
		prog = 0
	case prog > 100:
		prog = 100
	}
	body["progress"] = prog
	c, err := parseCreds(req.GetCredentials())
	if err != nil {
		return nil, err
	}
	c, upd, err := p.fresh(ctx, s, c)
	if err != nil {
		return nil, err
	}
	r, err := p.authed(ctx, s, c, http.MethodPost, "/scrobble/"+action, nil, body)
	if err != nil {
		return nil, err
	}
	switch r.status {
	case http.StatusOK, http.StatusCreated, http.StatusConflict, http.StatusNotFound, http.StatusUnprocessableEntity:
		// 404 and 422: Trakt does not know the item; resending would not change that, so the event is done.
		return &pluginv1.ScrobbleResponse{UpdatedCredentials: upd}, nil
	}
	return nil, sdk.Unavailable("trakt: scrobble failed with " + strconv.Itoa(r.status))
}

// scrobbleBody builds the movie or episode part of a scrobble: an episode with its own IDs goes by them, else by the series IDs and season and episode.
func scrobbleBody(m *pluginv1.MediaRef) (map[string]any, bool) {
	own := idsOf(m.GetProviderIds())
	switch m.GetType() {
	case pluginv1.ItemType_ITEM_TYPE_MOVIE:
		if own.empty() {
			return nil, false
		}
		return map[string]any{"movie": idObj{own}}, true
	case pluginv1.ItemType_ITEM_TYPE_EPISODE:
		if !own.empty() {
			return map[string]any{"episode": idObj{own}}, true
		}
		series := idsOf(m.GetSeriesIds())
		if series.empty() || m.SeasonNumber == nil || m.EpisodeNumber == nil {
			return nil, false
		}
		return map[string]any{"show": idObj{series}, "episode": epNum{int(m.GetSeasonNumber()), int(m.GetEpisodeNumber())}}, true
	}
	return nil, false
}

// ---- Pull ----

type historyItem struct {
	WatchedAt string `json:"watched_at"`
	Type      string `json:"type"`
	Movie     *struct {
		Title string `json:"title"`
		Year  int    `json:"year"`
		IDs   ids    `json:"ids"`
	} `json:"movie"`
	Episode *struct {
		Season int    `json:"season"`
		Number int    `json:"number"`
		Title  string `json:"title"`
		IDs    ids    `json:"ids"`
	} `json:"episode"`
	Show *struct {
		Title string `json:"title"`
		Year  int    `json:"year"`
		IDs   ids    `json:"ids"`
	} `json:"show"`
}

// PullWatched reads one page of the watch history, newest first. The cursor is the next page number; since becomes Trakt's start_at, so a later
// pull reads only what was watched after the last one. Trakt lists every play, so an item watched twice comes as two entries. Trakt does not
// report an item as unwatched, so every entry is a watched one.
func (p *plugin) PullWatched(ctx context.Context, req *pluginv1.PullWatchedRequest) (*pluginv1.PullWatchedResponse, error) {
	s, err := p.app()
	if err != nil {
		return nil, err
	}
	c, err := parseCreds(req.GetCredentials())
	if err != nil {
		return nil, err
	}
	page := 1
	if cur := req.GetCursor(); cur != "" {
		if page, err = strconv.Atoi(cur); err != nil || page < 1 {
			return nil, sdk.InvalidArgument("trakt: bad cursor")
		}
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = defaultPageLimit
	}
	if limit > maxPageLimit {
		limit = maxPageLimit
	}
	q := url.Values{"page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(limit)}}
	if since := req.GetSince(); since != "" {
		if _, err := time.Parse(time.RFC3339, since); err != nil {
			return nil, sdk.InvalidArgument("trakt: bad since")
		}
		q.Set("start_at", since)
	}
	c, upd, err := p.fresh(ctx, s, c)
	if err != nil {
		return nil, err
	}
	r, err := p.authed(ctx, s, c, http.MethodGet, "/sync/history", q, nil)
	if err != nil {
		return nil, err
	}
	if r.status != http.StatusOK {
		return nil, sdk.Unavailable("trakt: history failed with " + strconv.Itoa(r.status))
	}
	var items []historyItem
	if json.Unmarshal(r.body, &items) != nil {
		return nil, sdk.Unavailable("trakt: bad history response")
	}
	resp := &pluginv1.PullWatchedResponse{UpdatedCredentials: upd}
	for _, it := range items {
		if e, ok := it.entry(); ok {
			resp.Entries = append(resp.Entries, e)
		}
	}
	if pages, _ := strconv.Atoi(r.header.Get("X-Pagination-Page-Count")); page < pages {
		resp.NextCursor = strconv.Itoa(page + 1)
	}
	return resp, nil
}

func (it historyItem) entry() (*pluginv1.WatchedEntry, bool) {
	e := &pluginv1.WatchedEntry{Watched: true, WatchedAt: it.WatchedAt, PlayCount: 1}
	switch {
	case it.Type == "movie" && it.Movie != nil && !it.Movie.IDs.empty():
		e.Media = &pluginv1.MediaRef{Type: pluginv1.ItemType_ITEM_TYPE_MOVIE, Title: it.Movie.Title, Year: int32(it.Movie.Year), ProviderIds: it.Movie.IDs.toMap()}
	case it.Type == "episode" && it.Episode != nil && it.Show != nil && (!it.Episode.IDs.empty() || !it.Show.IDs.empty()):
		season, number := int32(it.Episode.Season), int32(it.Episode.Number)
		e.Media = &pluginv1.MediaRef{Type: pluginv1.ItemType_ITEM_TYPE_EPISODE, Title: it.Show.Title, Year: int32(it.Show.Year),
			ProviderIds: it.Episode.IDs.toMap(), SeriesIds: it.Show.IDs.toMap(), SeasonNumber: &season, EpisodeNumber: &number}
	default:
		return nil, false
	}
	return e, true
}

// ---- Push ----

type pushMovie struct {
	WatchedAt string `json:"watched_at,omitempty"`
	IDs       ids    `json:"ids"`
}

type pushEpisode struct {
	Number    int    `json:"number"`
	WatchedAt string `json:"watched_at,omitempty"`
}

type pushSeason struct {
	Number   int           `json:"number"`
	Episodes []pushEpisode `json:"episodes"`
}

type pushShow struct {
	IDs     ids          `json:"ids"`
	Seasons []pushSeason `json:"seasons"`
}

type pushBody struct {
	Movies   []pushMovie `json:"movies,omitempty"`
	Episodes []pushMovie `json:"episodes,omitempty"`
	Shows    []pushShow  `json:"shows,omitempty"`
}

// pushKey is where a request entry came from, so the not_found answer can be given back as indexes into the request.
type pushKey struct {
	index      int
	kind       string // movie, episode (own IDs) or show
	ids        ids
	season, ep int
}

// PushWatched marks items as watched on Trakt, with the time they were watched. Entries without a usable number, and entries that are
// not watched, come back as unmatched, as do those Trakt could not find. A batch is at most 100 entries.
func (p *plugin) PushWatched(ctx context.Context, req *pluginv1.PushWatchedRequest) (*pluginv1.PushWatchedResponse, error) {
	s, err := p.app()
	if err != nil {
		return nil, err
	}
	entries := req.GetEntries()
	if len(entries) > pushBatch {
		return nil, sdk.InvalidArgument("trakt: at most 100 entries per batch")
	}
	c, err := parseCreds(req.GetCredentials())
	if err != nil {
		return nil, err
	}
	var body pushBody
	var keys []pushKey
	unmatched := map[int32]bool{}
	for i, e := range entries {
		m := e.GetMedia()
		own := idsOf(m.GetProviderIds())
		series := idsOf(m.GetSeriesIds())
		at := e.GetWatchedAt()
		switch {
		case !e.GetWatched():
			unmatched[int32(i)] = true
		case m.GetType() == pluginv1.ItemType_ITEM_TYPE_MOVIE && !own.empty():
			body.Movies = append(body.Movies, pushMovie{WatchedAt: at, IDs: own})
			keys = append(keys, pushKey{index: i, kind: "movie", ids: own})
		case m.GetType() == pluginv1.ItemType_ITEM_TYPE_EPISODE && !own.empty():
			body.Episodes = append(body.Episodes, pushMovie{WatchedAt: at, IDs: own})
			keys = append(keys, pushKey{index: i, kind: "episode", ids: own})
		case m.GetType() == pluginv1.ItemType_ITEM_TYPE_EPISODE && !series.empty() && m.SeasonNumber != nil && m.EpisodeNumber != nil:
			body.addShowEpisode(series, int(m.GetSeasonNumber()), pushEpisode{Number: int(m.GetEpisodeNumber()), WatchedAt: at})
			keys = append(keys, pushKey{index: i, kind: "show", ids: series, season: int(m.GetSeasonNumber()), ep: int(m.GetEpisodeNumber())})
		default:
			unmatched[int32(i)] = true
		}
	}
	resp := &pluginv1.PushWatchedResponse{}
	if len(keys) > 0 {
		c, upd, err := p.fresh(ctx, s, c)
		if err != nil {
			return nil, err
		}
		resp.UpdatedCredentials = upd
		r, err := p.authed(ctx, s, c, http.MethodPost, "/sync/history", nil, body)
		if err != nil {
			return nil, err
		}
		if r.status != http.StatusCreated && r.status != http.StatusOK {
			return nil, sdk.Unavailable("trakt: history update failed with " + strconv.Itoa(r.status))
		}
		var out struct {
			NotFound struct {
				Movies   []idObj `json:"movies"`
				Episodes []idObj `json:"episodes"`
				Shows    []struct {
					IDs     ids `json:"ids"`
					Seasons []struct {
						Number   int `json:"number"`
						Episodes []struct {
							Number int `json:"number"`
						} `json:"episodes"`
					} `json:"seasons"`
				} `json:"shows"`
			} `json:"not_found"`
		}
		if json.Unmarshal(r.body, &out) != nil {
			return nil, sdk.Unavailable("trakt: bad history response")
		}
		for _, k := range keys {
			switch k.kind {
			case "movie":
				for _, m := range out.NotFound.Movies {
					if m.IDs.same(k.ids) {
						unmatched[int32(k.index)] = true
					}
				}
			case "episode":
				for _, m := range out.NotFound.Episodes {
					if m.IDs.same(k.ids) {
						unmatched[int32(k.index)] = true
					}
				}
			case "show":
				for _, sh := range out.NotFound.Shows {
					if !sh.IDs.same(k.ids) {
						continue
					}
					for _, se := range sh.Seasons {
						for _, ep := range se.Episodes {
							if se.Number == k.season && ep.Number == k.ep {
								unmatched[int32(k.index)] = true
							}
						}
					}
				}
			}
		}
	}
	for i := range unmatched {
		resp.Unmatched = append(resp.Unmatched, i)
	}
	sort.Slice(resp.Unmatched, func(a, b int) bool { return resp.Unmatched[a] < resp.Unmatched[b] })
	return resp, nil
}

// addShowEpisode puts an episode under its series and season, creating them as needed (one series object for all its episodes in the batch).
func (b *pushBody) addShowEpisode(series ids, season int, ep pushEpisode) {
	for si := range b.Shows {
		if b.Shows[si].IDs != series {
			continue
		}
		for qi := range b.Shows[si].Seasons {
			if b.Shows[si].Seasons[qi].Number == season {
				b.Shows[si].Seasons[qi].Episodes = append(b.Shows[si].Seasons[qi].Episodes, ep)
				return
			}
		}
		b.Shows[si].Seasons = append(b.Shows[si].Seasons, pushSeason{Number: season, Episodes: []pushEpisode{ep}})
		return
	}
	b.Shows = append(b.Shows, pushShow{IDs: series, Seasons: []pushSeason{{Number: season, Episodes: []pushEpisode{ep}}}})
}
