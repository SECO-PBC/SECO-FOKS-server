// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package chains

import (
	"crypto/tls"
	"sync"
	"time"

	proto "github.com/foks-proj/go-foks/proto/lib"
)

// prewarmUnclaimedAfter is how long a pre-warmed client waits to be taken
// before it is shut down. A cold start takes its clients within a second or
// two; this only bounds the cost of one nobody asks for.
const prewarmUnclaimedAfter = 30 * time.Second

type prewarmKey struct {
	st   proto.ServerType
	cert *tls.Certificate
}

type prewarmClient interface {
	comparable
	Shutdown()
}

// prewarmPool holds clients that started connecting before anyone asked for
// them, one per service and client certificate, each handed out once.
type prewarmPool[C prewarmClient] struct {
	mu      sync.Mutex
	clients map[prewarmKey]C
}

func (p *prewarmPool[C]) has(k prewarmKey) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.clients[k]
	return ok
}

// put holds c under k, unless a client is already held there.
func (p *prewarmPool[C]) put(k prewarmKey, c C) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.clients[k]; ok {
		return false
	}
	if p.clients == nil {
		p.clients = make(map[prewarmKey]C)
	}
	p.clients[k] = c
	return true
}

// take hands over the client held under k, at most once.
func (p *prewarmPool[C]) take(k prewarmKey) (C, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.clients[k]
	if ok {
		delete(p.clients, k)
	}
	return c, ok
}

// expire shuts c down if it is still held under k, that is, if nobody took it.
func (p *prewarmPool[C]) expire(k prewarmKey, c C) {
	p.mu.Lock()
	held, ok := p.clients[k]
	if ok && held == c {
		delete(p.clients, k)
	}
	p.mu.Unlock()
	if ok && held == c {
		c.Shutdown()
	}
}
