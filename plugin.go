package main

import (
	"context"
	_ "embed"
	"encoding/base64"
	"net/url"
	"strings"
	"sync"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
	"github.com/kryspal/plugin-sdk/sdk"
)

const (
	pluginID = "trakt"

	keyClientID     = "client_id"
	keyClientSecret = "client_secret"
	keyBaseURL      = "base_url"

	// attributionText is the sentence Trakt's API Use Policy asks for in an app's About or settings page.
	attributionText = "Kryspal Media uses Trakt but is not endorsed or certified by Trakt"
	attributionLink = "https://trakt.tv"
)

// appKey and appSecret are the application's client ID and secret registered for Kryspal Media. They are not in the source: the release build
// injects them with
//
//	go build -ldflags "-X main.appKey=$TRAKT_CLIENT_ID -X main.appSecret=$TRAKT_CLIENT_SECRET"
//
// A build without them (a fork, a local build) needs the user's own application in the advanced settings. The secret may stay empty: an
// application registered for the PKCE flow has none.
var (
	appKey    string
	appSecret string
)

// logoSVG is the official Trakt "Circle Mark", exactly as downloaded from https://app.trakt.tv/branding (not modified: Trakt's branding rules forbid
// changing, recoloring or redrawing it). It is square because the server shows a source logo in a small square, for example next to the member's
// linked account.
//
//go:embed logo/trakt-circlemark.svg
var logoSVG []byte

var logoDataURI = "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(logoSVG)

// versionFile is the single source of the plugin version: the manifest's version.
//
//go:embed VERSION
var versionFile string

func version() string { return strings.TrimSpace(versionFile) }

// settings are the values saved by Configure.
type settings struct {
	clientID, clientSecret string // the user's own application; empty means the built-in one
}

// id and secret are the application used for requests: the user's own when given, else the built-in one.
func (s settings) id() string {
	if s.clientID != "" {
		return s.clientID
	}
	return appKey
}

func (s settings) secret() string {
	if s.clientID != "" {
		return s.clientSecret // an own client ID goes with its own secret, never with the built-in one
	}
	return appSecret
}

type plugin struct {
	pluginv1.UnimplementedPluginServer
	pluginv1.UnimplementedUserSyncServer
	pluginv1.UnimplementedRatingProviderServer

	mu      sync.Mutex
	set     settings
	api     *api // replaceable in tests
	ratings *ratingCache
}

func newPlugin() *plugin { return &plugin{api: newAPI(), ratings: newRatingCache()} }

// Describe declares the four user sync capabilities and the two rating capabilities. There is no account setting: each member links their own account in the server's interface.
func (p *plugin) Describe(_ context.Context, req *pluginv1.DescribeRequest) (*pluginv1.Manifest, error) {
	t := messages.For(req.GetLanguage())
	return &pluginv1.Manifest{
		Id: pluginID, Name: "Trakt", Description: t.T("desc"), Version: version(), ProtocolVersion: pluginv1.ProtocolVersion,
		Capabilities: []string{
			pluginv1.CapabilityUserSyncLink, pluginv1.CapabilityUserSyncScrobble,
			pluginv1.CapabilityUserSyncWatchedPull, pluginv1.CapabilityUserSyncWatchedPush,
			pluginv1.CapabilityRatingsMovie, pluginv1.CapabilityRatingsSeries,
		},
		RatingSources: []string{ratingSource},
		ConfigGroups:  []*pluginv1.ConfigGroup{{Key: "advanced", Title: t.T("groupAdvanced")}},
		ConfigFields: []*pluginv1.ConfigField{
			{Key: keyClientID, Type: pluginv1.ConfigFieldType_CONFIG_FIELD_TYPE_STRING, Group: "advanced", Required: appKey == "", Label: t.T("idLabel"), Description: t.T("idHelp"),
				Link: "https://trakt.tv/oauth/applications", LinkLabel: t.T("idLink")},
			{Key: keyClientSecret, Type: pluginv1.ConfigFieldType_CONFIG_FIELD_TYPE_SECRET, Group: "advanced", Label: t.T("secretLabel"), Description: t.T("secretHelp")},
			{Key: keyBaseURL, Type: pluginv1.ConfigFieldType_CONFIG_FIELD_TYPE_STRING, Group: "advanced", Label: t.T("baseURLLabel"),
				Description: t.T("baseURLHelp"), Placeholder: defaultBaseURL},
		},
		Attribution:          &pluginv1.Attribution{Text: attributionText, LogoUrl: logoDataURI, Link: attributionLink},
		SuggestedConcurrency: 2,
	}, nil
}

// Configure validates the settings: an application must exist (built in or the user's own), a client secret needs its client ID, and the URL
// must be https.
func (p *plugin) Configure(_ context.Context, req *pluginv1.ConfigureRequest) (*pluginv1.ConfigureResponse, error) {
	t := messages.For(req.GetLanguage())
	v := req.GetValues()
	resp := &pluginv1.ConfigureResponse{FieldErrors: map[string]string{}}
	s := settings{clientID: strings.TrimSpace(v[keyClientID]), clientSecret: strings.TrimSpace(v[keyClientSecret])}
	base := strings.TrimSpace(v[keyBaseURL])
	switch {
	case s.clientSecret != "" && s.clientID == "":
		resp.FieldErrors[keyClientID] = t.T("errSecretNeedsID")
	case s.id() == "":
		resp.FieldErrors[keyClientID] = t.T("errNoApp")
	}
	if base == "" {
		base = defaultBaseURL
	}
	if u, err := url.Parse(base); err != nil || u.Scheme != "https" || u.Host == "" {
		resp.FieldErrors[keyBaseURL] = t.T("errHTTPS")
	}
	if len(resp.FieldErrors) > 0 {
		return resp, nil
	}
	p.mu.Lock()
	p.set = s
	p.mu.Unlock()
	p.api.setBase(strings.TrimRight(base, "/"))
	return resp, nil
}

func (p *plugin) settings() settings {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.set
}

// app returns the application in use, or the needs-configuration error when there is none (a build without a built-in application and no own one).
func (p *plugin) app() (settings, error) {
	s := p.settings()
	if s.id() == "" {
		return s, sdk.NeedsConfiguration("Trakt application is not configured")
	}
	return s, nil
}
