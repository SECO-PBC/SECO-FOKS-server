// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package core

import (
	"net"
	"sync"
)

// connRegistry holds the socket under each connectionMgr's current transport,
// so that a process embedding the client can drop every server connection at
// once -- e.g. when the device reports that its network went away, or when a
// call has stalled on a connection the network silently stopped carrying.
//
// Only the xpLoop goroutine of a connectionMgr sets or clears its entry, so an
// entry always names the socket of that manager's live xp (or is absent).
type connRegistry struct {
	sync.Mutex
	conns map[*connectionMgr]net.Conn
}

var liveConns = connRegistry{conns: make(map[*connectionMgr]net.Conn)}

func (r *connRegistry) set(c *connectionMgr, conn net.Conn) {
	r.Lock()
	defer r.Unlock()
	r.conns[c] = conn
}

func (r *connRegistry) clear(c *connectionMgr) {
	r.Lock()
	defer r.Unlock()
	delete(r.conns, c)
}

func (r *connRegistry) closeAll() {
	r.Lock()
	conns := make([]net.Conn, 0, len(r.conns))
	for _, conn := range r.conns {
		conns = append(conns, conn)
	}
	r.Unlock()
	for _, conn := range conns {
		conn.Close()
	}
}

// CloseAllConnections closes the socket under every open server connection in
// this process. Calls pending on those connections fail with an RPC EOF (a
// transport error), and the next call on each client dials again.
//
// It closes the raw socket rather than calling the transport's Close, which
// waits for the receive loop to finish first -- a wait that never ends when
// the network has stopped delivering packets. Closing the socket makes the
// receive loop's read fail, and the transport tears itself down from there.
func CloseAllConnections() {
	liveConns.closeAll()
}
