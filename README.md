# plugin-trakt

Links each member's own Trakt account: reports what they watch, imports their watched history and sends what was watched on the server. An official [Kryspal Media](https://github.com/kryspal) plugin, built on [plugin-sdk](https://github.com/kryspal/plugin-sdk).

```sh
go build -o trakt .
go vet ./... && go test ./...
```

## What it does

- **Link**: a member links their account in their own settings with the device code flow: the server shows a code, the member enters it on trakt.tv.
- **Scrobble**: start, pause and stop are reported while the member watches. Trakt counts a stop at 80 percent or more as watched.
- **Pull**: the watched history is read in pages. Later pulls read only what was watched after the last one.
- **Push**: items watched on the server are marked as watched on Trakt, with the time they were watched.
- Only items with a TMDB, IMDb or TVDB number are sent or matched. Nothing is ever marked as unwatched.

## Settings

Official builds have the application built in; there is nothing to enter. To use an application you registered yourself on Trakt, enter its client ID (and its client secret, if it has one) under Advanced.

The plugin keeps no state. The server stores each member's credentials, encrypted, and passes them on every call; the plugin never logs them.

Implementation notes: [docs/design.md](docs/design.md).

Licensed under the Apache License 2.0 (see `LICENSE` and `NOTICE`). The Trakt logo in `logo/` is a Trakt brand asset from [app.trakt.tv/branding](https://app.trakt.tv/branding), used unmodified and not covered by that license. Trakt is a trademark of its owner; this plugin is not made or endorsed by Trakt.

Official builds are signed and published through the Kryspal plugin catalog; builds from source are treated as third-party plugins.
