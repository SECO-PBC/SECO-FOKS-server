// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package core

import (
	"context"
	"errors"
	"sync"

	"github.com/foks-proj/go-snowpack-rpc/rpc"
)

// ── Optional methods: newer RPCs with a fallback (docs/rtt-budget.md) ──
//
// A caller with a fallback for a server that lacks a newer method names that
// method an OptionalMethod and makes its call under WithOptionalMethod. Once
// the server answers it "method not found", RpcClient.Call2 answers later
// calls to it the same way without a round trip -- for as long as the same
// connection lasts, and no longer: the server binary can only change across a
// re-dial, so a new connection asks again. A method nobody declared optional
// is never short-circuited: its refusal reaches the caller every time, as it
// always has.

// OptionalMethod is a method whose callers can do without it. Only
// OptionalMethodOf makes one, from the generated client's own call, so the
// name is the one Call2 sees by construction.
type OptionalMethod struct {
	name string
}

// Name is the method's name, as RPC stats report it.
func (o OptionalMethod) Name() string { return o.name }

// OptionalMethodOf is the method call makes (one generated-client call
// through cli, which sends nothing), as an OptionalMethod.
func OptionalMethodOf(call func(cli rpc.GenericClient) error) (OptionalMethod, error) {
	pc, err := PlanCall(call)
	if err != nil {
		return OptionalMethod{}, err
	}
	return OptionalMethod{name: pc.Method()}, nil
}

// MustOptionalMethodOf is OptionalMethodOf for package-level declarations;
// it panics if call is not exactly one generated-client call.
func MustOptionalMethodOf(call func(cli rpc.GenericClient) error) OptionalMethod {
	o, err := OptionalMethodOf(call)
	if err != nil {
		panic(err)
	}
	return o
}

type optionalMethodKey struct{}

// WithOptionalMethod returns a context under which a call to o is treated as
// optional by RpcClient.Call2 (see above). Other calls under it are not.
func WithOptionalMethod(ctx context.Context, o OptionalMethod) context.Context {
	return context.WithValue(ctx, optionalMethodKey{}, o.name)
}

func optionalMethodFromContext(ctx context.Context) string {
	s, _ := ctx.Value(optionalMethodKey{}).(string)
	return s
}

// IsMethodNotFound reports whether err is a server's "method not found".
func IsMethodNotFound(err error) bool {
	var mnf rpc.MethodV2NotFoundError
	return errors.As(err, &mnf)
}

// RefusedOver reports whether xp -- a connection of this client's, from
// Connect -- answered o "method not found", so a caller can skip preparing a
// call that would only be refused again. It never dials: the caller holds
// the connection it is about to call over, and a refusal over any other
// connection says nothing about this one.
func (c *RpcClient) RefusedOver(xp rpc.Transporter, o OptionalMethod) bool {
	return c.refused.has(xp, o.name)
}

// refusals is the set of optional methods one connection's server refused.
// A refusal over any other connection starts the set afresh.
type refusals struct {
	mu      sync.Mutex
	xp      rpc.Transporter
	methods map[string]struct{}
}

func (r *refusals) has(xp rpc.Transporter, name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.xp != xp {
		return false
	}
	_, ok := r.methods[name]
	return ok
}

func (r *refusals) note(xp rpc.Transporter, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.xp != xp || r.methods == nil {
		r.xp = xp
		r.methods = make(map[string]struct{})
	}
	r.methods[name] = struct{}{}
}
