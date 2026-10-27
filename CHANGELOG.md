# Changelog

All notable changes to this plugin are listed here. The version in `VERSION` is the single source of the version number; a published version is never changed.

## 1.1.0

- Adds the Trakt community rating (`ratings.movie`, `ratings.series`, rating source `trakt`) to movies and series another plugin matched. Looks up by TMDB number, else by IMDb number; answers nothing when neither is known or Trakt has no votes; a rate limit is reported with Trakt's retry time. In-memory cache of seven days.
- The attribution is now the sentence Trakt's API Use Policy asks for: "Kryspal Media uses Trakt but is not endorsed or certified by Trakt" (the logo and link stay).
- Public lookups are spaced 0.7 seconds apart; requests with a member's token keep the 1.1 second spacing.
- Built with plugin SDK v0.9.0. Needs server 0.9.11 or newer; servers before that stay on 1.0.x.

## 1.0.0

- First release. Links a member's Trakt account with the device code flow, reports watching (start, pause, stop), imports the watched history and sends the local watched history.
- Built with plugin SDK v0.5.0. Needs server 0.9.8 or newer.
