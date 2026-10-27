// Command trakt is the official Trakt plugin for Kryspal Media (capabilities usersync.link, usersync.scrobble, usersync.watched.pull,
// usersync.watched.push, ratings.movie and ratings.series).
//
// Each member links their own Trakt account with the device code flow. The plugin then reports what the member is watching, imports the watched
// history and exports the local watched history. The plugin keeps no state: the server stores the credentials and passes them on every call.
// It also adds Trakt's community rating to movies and series other plugins matched (ratings.*), by their TMDB or IMDb numbers, with a small
// in-memory cache. The application client ID and secret are injected at build time and are not in the source; the settings page has optional fields for your own.
package main

import "github.com/kryspal/plugin-sdk/sdk"

func main() {
	p := newPlugin()
	sdk.Main(sdk.Config{Plugin: p, UserSync: p, RatingProvider: p})
}
