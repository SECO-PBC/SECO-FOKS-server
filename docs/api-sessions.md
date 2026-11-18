# API sessions: bearer tokens for `/api/v1` (fork-only)

The web server has an authenticated JSON surface at `/api/v1` for app clients.
The first real consumer will be opt-in client analytics
(`POST /api/v1/client-analytics`). Today the only route is `GET /api/v1/ping`,
which proves the stack end to end.

FOKS authenticates users with device keys over snowpack RPC, which an HTTP
client cannot do. So a logged-in client **mints a short-lived bearer token over
the RPC connection it already has**, then sends it over HTTPS. This is the shape
the web admin panel already uses (`newWebAdminPanelURL` → `user_web_sessions`),
with three differences: tokens are scoped, hashed and short-lived, and they live
in their own table.

## Flow

1. The app calls lcl `Admin.apiSession(scope)`. The agent forwards it to rem
   `User.newApiSession(scope)` on the user's home server.
2. The server returns `lib.ApiSession{token, scope, baseURL, expires}`.
   `baseURL` comes from the same logic as admin-panel links (`adminBaseURL`),
   so clients need no config for where the API lives.
3. The client sends `Authorization: Bearer foksapi_<base62>` to
   `<baseURL>/api/v1/...`. It keeps the token in memory, mints again shortly
   before `expires`, and mints again once on a 401.

## Storage

- Tokens are stored in `user_api_sessions` (`foks_users` **p13**), keyed by
  `token_hash = SHA-256(token)`. The token itself is never stored.
- `scope` is a `lib.ApiScope`. A token works only on routes that require its
  scope.
- The lifetime is `apps.web.api_session_duration`, 24h by default.
- A user has at most 10 active tokens. Minting past that cap deactivates the
  oldest ones.
- The user server garbage-collects rows more than an hour past `etime`
  (`shared.RunApiSessionGC`).
- This is a separate table from `user_web_sessions`. An API token can never
  open the admin panel, and an admin cookie can never call `/api`.

## HTTP layer (`server/web/app/api`)

`/api/v1` is mounted outside the HTML/CSP middleware. Every request runs
through these middlewares, in order:

1. **Telemetry.** Records a span and `foks.http.api.duration`, tagged only with
   the chi route pattern, a normalized method and the status class. It never
   records a uid, token or raw path, and unmatched paths are tagged
   `unmatched`.
2. **CORS** with `Access-Control-Allow-Origin: *`. This is safe because auth is
   a bearer header and never a cookie. It is needed because the desktop
   renderer is a `file://` page (Origin `null`).
3. **Body limit** of 64 KiB.
4. **`RequireScope(scope)`** on each route. It answers every refusal with the
   same 401 (`WWW-Authenticate: Bearer`), whatever the reason. It also
   rate-limits each user, in memory, with a token bucket of 2/s and a burst of
   30; over the limit it answers 429 with `Retry-After`.

Errors are JSON: `{"error": "<code>"}`.

## Adding a route

1. Add a scope to `ApiScope` in `proto-src/lib/api.snowp` and regenerate.
   Clients mint per scope.
2. Register the route in `api.InitRouter` with `a.RequireScope(scope)`. Read
   the caller with `api.AuthFromContext(ctx)`.
3. Never put the uid, or anything derived from the request body, into
   telemetry attributes.
