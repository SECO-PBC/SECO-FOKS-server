# API sessions: bearer tokens for `/api/v1` (fork-only)

The web server has an authenticated JSON surface at `/api/v1` for app clients.
Its first real consumer will be opt-in client analytics
(`POST /api/v1/client-analytics`). Today the only route is `GET /api/v1/ping`,
which proves the stack end to end.

## Use it only for non-sensitive endpoints

**`/api/v1` is for analytics, health checks and similar data that is
harmless if the wrong person sends or reads it.** Never add a route that
returns user data, changes account or team state, or acts on behalf of the
user. Those belong on the FOKS RPC protocols, where every call is
authenticated by the device key.

The reason is a set of trade-offs we made deliberately, to keep this code
small and lean on a standard library:

- **Tokens can't be revoked.** A token is valid until it expires (1 hour by
  default). Revoking a device, deleting an account or changing a password
  does not end the tokens already minted.
- **Expiry is the only control on a stolen token.** Anyone who copies a
  token (from a log, a proxy, a compromised device) can use it until it
  expires, from anywhere.
- **No per-token state.** The server keeps no record of which tokens exist,
  so it can't list a user's tokens, cap how many there are, or tell a
  replayed token from a fresh request.
- **Tokens are signed, not encrypted.** The payload is readable by anyone
  holding the token: user ID, host, scope and timestamps. Never put anything
  else in it.
- **The user's identity is as of minting.** A token minted before an account
  was deleted keeps naming that account until it expires.
- **Rate limits are per process and in memory.** Behind several web servers,
  each enforces its own share.

If a sensitive endpoint is ever needed, it needs a revocation check (a
per-request lookup, or a denylist), and that is a design change, not a new
route.

## Flow

1. The app calls lcl `Admin.apiSession(scope)`. The agent forwards it to rem
   `User.newApiSession(scope)` on the user's home server, over the
   connection authenticated by the device key.
2. The server returns `lib.ApiSession{token, baseURL, lifetime}`.
   - `token` is a JWT signed with HS256 by one of the host's `api_session`
     challenge HMAC keys. Its claims are `iss=foks`, `aud=foks-api`,
     `sub=<uid>`, `hst=<short host id>`, `scope`, `iat` and `exp`, and its
     header names the key (`kid`).
   - `baseURL` comes from the same logic as admin-panel links
     (`adminBaseURL`), so clients need no config for where the API lives.
   - There is no absolute expiry: a client schedules re-mints from
     `lifetime` and its own clock, so a skewed device clock doesn't matter.
3. The client sends `Authorization: Bearer <token>` to `<baseURL>/api/v1/...`.
   It keeps the token in memory, mints again shortly before it expires, and
   mints again once on a 401.

In Go, `ApiToken.String()` is redacted, so formatting a token or a session
never prints the credential; `ApiToken.Reveal()` is the real value.

## Keys

- Tokens are signed with keys of type `api_session` in `challenge_keys`
  (`foks_users` **p13** adds the enum value). There is no table of tokens.
- A host's first key is created on its first mint, so existing hosts need no
  setup step.
- To rotate, generate a new `api_session` key for the host. New tokens use
  the newest key; tokens signed with older keys stay valid until they
  expire, because each names its key.
- The web server reads each key from the database once and caches it for the
  life of the process (keys never change). Lookups of keys not yet cached
  share a small global budget (1/s, burst 10), since the key ID comes from
  the unverified token.
- Token lifetime is `apps.web.api_session_duration`, 1 hour by default. A
  negative value is a config error.

## HTTP layer (`server/web/app/api`)

`/api/v1` is mounted outside the HTML/CSP middleware. Every request runs
through these middlewares, in order:

1. **Telemetry.** Records a span and `foks.http.api.duration`, tagged only
   with the route pattern, a normalized method and the status class. Unknown
   paths are labelled `unmatched` and CORS preflights `preflight`. It never
   records a uid, token or raw path. A handler that panics is recorded as a
   500.
2. **CORS** with `Access-Control-Allow-Origin: *`, exposing `Retry-After` and
   `WWW-Authenticate` to scripts. This is safe because auth is a bearer header
   and never a cookie. It is needed because the desktop renderer is a
   `file://` page (Origin `null`).
3. **Body limit** of 64 KiB.
4. **`route(scope, handler)`** wraps each route:
   - The token is verified with `golang-jwt/jwt/v5`: HS256 only, issuer,
     audience, scope and expiry (with 30s leeway for clock skew between the
     server that signed it and the one checking it). The key must belong to
     the host the token names.
   - Every refusal is the same 401 (`WWW-Authenticate: Bearer`), whatever the
     reason.
   - A client that hangs up mid-check is recorded as 499, not 500.
   - Each user is rate-limited (`golang.org/x/time/rate`, 2/s, burst 30);
     over the limit it gets 429 with `Retry-After`.
   - Handlers receive the verified caller as a `shared.ApiSessionAuth` value.

Errors are JSON: `{"error": "<code>"}`.

## Adding a route

1. Check it against "Use it only for non-sensitive endpoints" above.
2. Add a scope to `ApiScope` in `proto-src/lib/api.snowp` and regenerate.
   Clients mint per scope.
3. Register it in `api.InitRouter` as `a.route(scope, h)`. Handlers have the
   type `authedHandler`; scope any data access to `auth.ShortHostID` and
   `auth.UID`.
4. Never put the uid, or anything derived from the request body, into
   telemetry attributes.
