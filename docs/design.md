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
