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
- A user has at most 10 tokens per scope. Minting past that cap deletes the
  oldest others (never the new one), and a user may mint at most 5 tokens
  per scope per minute.
- The user server garbage-collects rows more than an hour past `etime`
  (`shared.RunApiSessionGC`).
- This is a separate table from `user_web_sessions`. An API token can never
  open the admin panel, and an admin cookie can never call `/api`.

## HTTP layer (`server/web/app/api`)

`/api/v1` is mounted outside the HTML/CSP middleware. Every request runs
through these middlewares, in order:

1. **Telemetry.** Records a span and `foks.http.api.duration`, tagged only with
   the route pattern, a normalized method and the status class. Unknown paths
   are labelled `unmatched` and CORS preflights `preflight`. It never records
   a uid, token or raw path.
2. **CORS** with `Access-Control-Allow-Origin: *`, exposing `Retry-After` and
   `WWW-Authenticate` to scripts. This is safe because auth is a bearer header
   and never a cookie. It is needed because the desktop renderer is a
   `file://` page (Origin `null`).
3. **Body limit** of 64 KiB.
4. **`RequireScope(scope)`** on each route:
   - A token verified in the last 30s (and not yet expired) is served from an
     in-memory cache, without a database lookup. So a deleted or expired
     token can keep working for up to 30s.
   - Otherwise the client IP must be under its budget of *refused* tokens
     (1/s, burst 20) before the database is asked. A flood of made-up tokens
     gets 429s without touching the connection pool. Only refusals spend this
     budget, and the IP is the TCP peer; forwarding headers are ignored.
   - Every refusal is the same 401 (`WWW-Authenticate: Bearer`), whatever the
     reason.
   - A client that hangs up mid-lookup is recorded as 499, not 500.
   - Each user is rate-limited with a token bucket of 2/s and a burst of 30;
     over the limit it gets 429 with `Retry-After`.

Errors are JSON: `{"error": "<code>"}`.

## Known gaps

- Revoking a device does not revoke the API tokens it minted; they live out
  their lifetime (24h by default). Fine for `Ping` and analytics. Fix before
  adding any scope that reads or changes user data.

## Adding a route

1. Add a scope to `ApiScope` in `proto-src/lib/api.snowp` and regenerate.
   Clients mint per scope.
2. Register the route in `api.InitRouter` with `a.RequireScope(scope)`. Read
   the caller with `api.AuthFromContext(ctx)`.
3. Never put the uid, or anything derived from the request body, into
   telemetry attributes.
