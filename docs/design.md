# Design notes

## Credentials

`Credentials.data` is JSON `{"v":1,"access","refresh","expires"}`. Before a call that would use a token ending within 24 hours, the plugin refreshes it and returns the new pair as `updated_credentials`; the server writes it before using it. A refresh token Trakt refuses (400, 401, 403) is `UNAUTHENTICATED`: the member has to link again. Network errors and 5xx are `UNAVAILABLE`; a 429 is a per-credential rate limit with the `Retry-After` seconds.

## Application

The client ID and secret come from the build (`-X main.appKey`, `-X main.appSecret`) or from the advanced settings. A secret may be empty (an application registered for PKCE has none); an own client ID never uses the built-in secret.

## Scrobble

`start`, `pause` and `stop` with `progress`. An episode goes by its own IDs when there are any, else by the series IDs with season and episode. A 409 (the same action was just reported), 404 and 422 (Trakt does not know the item) end the event as done: resending changes nothing. An item with no usable number is `INVALID_ARGUMENT`.

## Pull

`GET /sync/history` newest first. The cursor is the next page number (from `X-Pagination-Page-Count`); `since` becomes `start_at`. Trakt lists every play, so an item watched twice comes twice. Trakt reports no unwatched items, so every entry has `watched = true`.

## Push

`POST /sync/history` with up to 100 entries: movies and episodes with their own IDs, other episodes under their series and season. Entries Trakt answers in `not_found`, entries without a usable number and entries that are not watched are returned as `unmatched` by index.

## Requests

One request at a time, at least 1.1 seconds apart (Trakt allows about one write per second). A recognizable User-Agent and the client ID go on every request. Terms followed: only documented methods, no scraping, no extra applications to get around limits.

## Ratings

`GetRatings` (capabilities `ratings.movie` and `ratings.series`, rating source `trakt`) adds Trakt's community rating to an item another plugin matched. Verified against the live API on 2026-10-25.

- With a TMDB number: `GET /search/tmdb/{id}?type=movie|show&extended=full`, one request that returns `rating`, `votes` and the IDs. The hit counts only when its type and TMDB number are the ones asked for and, when the item also has an IMDb number, Trakt's IMDb number agrees. An unknown number is a 200 with an empty array.
- With only an IMDb number: `GET /movies|shows/{imdb}/ratings`. A bare number there is read as a Trakt ID and answers an unrelated title, so only `tt` plus digits is ever sent, never a TMDB number.
- No other numbers, no name search, no `extended=all` (those are other sites' ratings). No votes means no rating. 404 is "nothing to say".
- 429 is a plugin-wide `RateLimited` with Trakt's `Retry-After`; 401 and 403 mean the application is refused (`NeedsConfiguration`); 5xx and network errors are `UNAVAILABLE`.
- In-memory cache, seven days, at most 20000 entries (a missing rating is cached too, a failed call is not). Public lookups are 0.7 seconds apart, about 85 a minute against Trakt's 500 per five minutes for one client ID and address.
- The application is the same one the account sync uses; own client IDs from the advanced settings work for ratings too.
