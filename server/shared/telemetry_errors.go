// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"sync"
	"time"
)

// otelErrorWarnEvery is how often a persistent OTel error (typically an
// export failing because the agent is down) is logged at Warn.
const otelErrorWarnEvery = time.Minute

// otelErrorHandler logs OTel SDK errors without flooding the log. When the
// agent is unreachable every export fails -- every metric interval and
// trace batch, in every server process -- so it logs the first error at
// Warn, then at most one Warn per interval with a count of the errors it
// suppressed in between. Suppressed errors still log at Debug.
type otelErrorHandler struct {
	warn  func(msg string, kv ...interface{})
	debug func(msg string, kv ...interface{})
	now   func() time.Time
	every time.Duration

	mu         sync.Mutex
	lastWarn   time.Time
	suppressed int
}

func newOtelErrorHandler(g *GlobalContext) *otelErrorHandler {
	return &otelErrorHandler{
		warn:  func(msg string, kv ...interface{}) { g.Log().Warnw(msg, kv...) },
		debug: func(msg string, kv ...interface{}) { g.Log().Debugw(msg, kv...) },
		now:   time.Now,
		every: otelErrorWarnEvery,
	}
}

func (h *otelErrorHandler) Handle(err error) {
	h.mu.Lock()
	now := h.now()
	if !h.lastWarn.IsZero() && now.Sub(h.lastWarn) < h.every {
		h.suppressed++
		h.mu.Unlock()
		h.debug("otel", "err", err)
		return
	}
	n := h.suppressed
	h.lastWarn, h.suppressed = now, 0
	h.mu.Unlock()
	h.warn("otel", "err", err, "suppressed", n)
}
