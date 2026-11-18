// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// Package api is the web server's authenticated JSON surface, /api/v1
// (fork-only; docs/api-sessions.md).
//
// Clients authenticate with `Authorization: Bearer <token>`, where the token
// was minted over the authenticated User RPC (newApiSession) for one
// proto.ApiScope. Every route requires exactly one scope.
package api

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

const (
	Prefix = "/api/v1"

	// maxBodyBytes bounds every request body. Analytics batches are the
	// largest expected payload and stay well under this.
	maxBodyBytes = 64 << 10
)

// authedHandler is the only shape an /api/v1 handler takes. It receives the
// verified caller as a value, so there is no handler that can run without a
// scope check or find its caller missing.
type authedHandler func(w http.ResponseWriter, r *http.Request, auth shared.ApiSessionAuth)

type api struct {
	g       *shared.GlobalContext
	limiter *rateLimiter[rateKey]    // per user, every authenticated request
	fails   *rateLimiter[clientAddr] // per client IP, refused tokens only
	cache   *authCache
}

// InitRouter mounts /api/v1 on mux. It must not sit under the HTML/CSP
// middleware the rest of the web app uses.
func InitRouter(g *shared.GlobalContext, mux chi.Router) {
	a := &api{
		g:       g,
		limiter: newRateLimiter[rateKey](defaultRatePerSec, defaultBurst, time.Now),
		fails:   newRateLimiter[clientAddr](failRatePerSec, failBurst, time.Now),
		cache:   newAuthCache(time.Now),
	}
	mux.Route(Prefix, func(r chi.Router) {
		r.Use(a.telemetry, cors, limitBody)
		r.Get("/ping", a.route(proto.ApiScope_Ping, ping))
		r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
			writeErr(w, http.StatusNotFound, "not_found")
		})
		r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
			writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed")
		})
	})
}

// ping proves the stack end to end: a 204 means the token was good.
func ping(w http.ResponseWriter, _ *http.Request, _ shared.ApiSessionAuth) {
	w.WriteHeader(http.StatusNoContent)
}

// route authenticates the bearer token for scope, rate-limits its user, and
// only then calls h. Every refusal is the same 401, whatever the reason.
//
// The database is consulted only for a token not verified in the last
// authCacheTTL, and only while the client's IP is under its budget of
// refused tokens -- so neither a flood of made-up tokens nor a runaway client
// over its per-user limit costs a query per request.
func (a *api) route(scope proto.ApiScope, h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		tok, ok := bearerToken(r)
		if !ok {
			a.refuse(w, ip)
			return
		}
		key := authCacheKey{hash: tok.Hash(), scope: scope}
		auth, hit := a.cache.get(key)
		if !hit {
			if wait, ok := a.fails.check(ip); !ok {
				tooMany(w, wait)
				return
			}
			m := shared.NewMetaContext(r.Context(), a.g)
			found, err := shared.CheckApiSession(m, key.hash, scope)
			switch {
			case errors.As(err, &shared.ApiSessionNotFoundError{}):
				a.refuse(w, ip)
				return
			case err != nil && r.Context().Err() != nil:
				// The client hung up mid-lookup: not a server fault.
				w.WriteHeader(statusClientClosed)
				return
			case err != nil:
				// No uid or token in the log line: only the failure.
				m.Warnw("api.route", "err", err)
				writeErr(w, http.StatusInternalServerError, "internal")
				return
			}
			auth = *found
			a.cache.put(key, auth)
		}
		if wait, ok := a.limiter.allow(rateKey{shid: auth.ShortHostID, uid: auth.UID}); !ok {
			tooMany(w, wait)
			return
		}
		h(w, r, auth)
	}
}

// statusClientClosed is nginx's 499: the client went away before the answer.
// Recorded as 4xx, so hang-ups never read as server errors.
const statusClientClosed = 499

func (a *api) refuse(w http.ResponseWriter, ip clientAddr) {
	a.fails.charge(ip)
	unauthorized(w)
}

// clientIP is the TCP peer. Forwarding headers are ignored: the web server
// faces the internet directly, and a client could set them to anything.
func clientIP(r *http.Request) clientAddr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return clientAddr(r.RemoteAddr)
	}
	return clientAddr(host)
}

func tooMany(w http.ResponseWriter, wait time.Duration) {
	secs := max(1, int((wait+time.Second-1)/time.Second))
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeErr(w, http.StatusTooManyRequests, "rate_limited")
}

func bearerToken(r *http.Request) (*proto.ApiToken, bool) {
	h := r.Header.Get("Authorization")
	scheme, val, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return nil, false
	}
	tok, err := proto.ParseApiToken(strings.TrimSpace(val))
	if err != nil {
		return nil, false
	}
	return tok, true
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="foks-api"`)
	writeErr(w, http.StatusUnauthorized, "unauthorized")
}

// cors lets the desktop renderer (a file:// page, Origin "null") and any
// other client call the API. That is safe because auth is a bearer header,
// never a cookie, so a foreign page cannot ride a user's credentials.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		h.Set("Access-Control-Max-Age", "600")
		// Not CORS-safelisted, so a renderer can't read them unless exposed.
		h.Set("Access-Control-Expose-Headers", "Retry-After, WWW-Authenticate")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// telemetry records a span and a latency sample per request, tagged only with
// the route pattern, a normalized method and the status class.
func (a *api) telemetry(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := a.g.Telemetry()
		method := normMethod(r.Method)
		start := time.Now()
		ctx, span := t.StartHTTPAPISpan(r.Context(), method)
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		r = r.WithContext(ctx)
		defer func() {
			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}
			// Fixed labels for the two cases chi would both report as the
			// mount pattern "/api/v1/*": unknown paths and CORS preflights.
			var route string
			switch {
			case r.Method == http.MethodOptions:
				route = "preflight"
			case status == http.StatusNotFound:
				route = "unmatched"
			default:
				if rc := chi.RouteContext(r.Context()); rc != nil {
					route = rc.RoutePattern()
				}
			}
			t.FinishHTTPAPIRequest(ctx, span, method, route, status, time.Since(start))
		}()
		next.ServeHTTP(ww, r)
	})
}

func normMethod(m string) string {
	switch m {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete,
		http.MethodPatch, http.MethodHead, http.MethodOptions:
		return m
	default:
		return "OTHER"
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}
