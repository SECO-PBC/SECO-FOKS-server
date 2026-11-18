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
	"context"
	"encoding/json"
	"errors"
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

type ctxKey struct{}

// AuthFromContext returns the bearer token's user, set by RequireScope.
func AuthFromContext(ctx context.Context) *shared.ApiSessionAuth {
	ret, _ := ctx.Value(ctxKey{}).(*shared.ApiSessionAuth)
	return ret
}

type api struct {
	g       *shared.GlobalContext
	limiter *rateLimiter
}

// InitRouter mounts /api/v1 on mux. It must not sit under the HTML/CSP
// middleware the rest of the web app uses.
func InitRouter(g *shared.GlobalContext, mux chi.Router) {
	a := &api{
		g:       g,
		limiter: newRateLimiter(defaultRatePerSec, defaultBurst, time.Now),
	}
	mux.Route(Prefix, func(r chi.Router) {
		r.Use(a.telemetry, cors, limitBody)
		r.With(a.RequireScope(proto.ApiScope_Ping)).Get("/ping", ping)
		r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
			writeErr(w, http.StatusNotFound, "not_found")
		})
		r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
			writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed")
		})
	})
}

// ping proves the stack end to end: a 204 means the token was good.
func ping(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// RequireScope authenticates the bearer token for scope and rate-limits its
// user. Every refusal is the same 401, whatever the reason.
func (a *api) RequireScope(scope proto.ApiScope) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok, ok := bearerToken(r)
			if !ok {
				unauthorized(w)
				return
			}
			m := shared.NewMetaContext(r.Context(), a.g)
			auth, err := shared.CheckApiSession(m, *tok, scope)
			if errors.As(err, &shared.ApiSessionNotFoundError{}) {
				unauthorized(w)
				return
			}
			if err != nil {
				// No uid or token in the log line: only the failure.
				m.Warnw("api.RequireScope", "err", err)
				writeErr(w, http.StatusInternalServerError, "internal")
				return
			}
			if wait, ok := a.limiter.allow(rateKey{shid: auth.ShortHostID, uid: auth.UID}); !ok {
				secs := max(1, int((wait+time.Second-1)/time.Second))
				w.Header().Set("Retry-After", strconv.Itoa(secs))
				writeErr(w, http.StatusTooManyRequests, "rate_limited")
				return
			}
			ctx := context.WithValue(r.Context(), ctxKey{}, auth)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
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
			route := ""
			if rc := chi.RouteContext(r.Context()); rc != nil {
				route = rc.RoutePattern()
			}
			if r.Method == http.MethodOptions {
				route = Prefix + "/*"
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
