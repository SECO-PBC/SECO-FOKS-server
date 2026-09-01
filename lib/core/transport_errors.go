// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package core

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
)

// IsTransportError classifies an RPC failure as transport-level (the request
// may never have reached the server, or the reply was lost) versus semantic
// (the server received and refused it). Callers that queue work for retry --
// notably the realtime outbox drain (docs/rt_offline.md, D7) -- retry only on
// transport errors.
//
// The default is deliberately semantic: an unrecognized error fails fast and
// surfaces to the caller rather than being retried forever. The table below is
// built from the error paths of the client transport stack (net dialing, TLS,
// the snowpack-RPC framing layer) rather than string matching.
func IsTransportError(err error) bool {
	if err == nil {
		return false
	}

	// Client-side connection establishment failed; the request never left.
	var connErr ConnectError
	if errors.As(err, &connErr) {
		return true
	}
	var agentErr AgentConnectError
	if errors.As(err, &agentErr) {
		return true
	}

	// The RPC layer's own signals for a dropped connection or an unanswered
	// call.
	var eofErr RPCEOFError
	if errors.As(err, &eofErr) {
		return true
	}
	var timeoutErr TimeoutError
	if errors.As(err, &timeoutErr) {
		return true
	}

	// Wire-level failures: dial errors, resets, timeouts, TLS handshake
	// failures (which surface as net.OpError), and torn connections.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	for _, errno := range []syscall.Errno{
		syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ECONNABORTED,
		syscall.EPIPE, syscall.EHOSTUNREACH, syscall.EHOSTDOWN,
		syscall.ENETDOWN, syscall.ENETUNREACH, syscall.ETIMEDOUT,
	} {
		if errors.Is(err, errno) {
			return true
		}
	}

	// Wire-decoded statuses (a server that answered) and everything else are
	// semantic.
	return false
}
