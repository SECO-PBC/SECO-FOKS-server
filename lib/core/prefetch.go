// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package core

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/foks-proj/go-snowpack-rpc/rpc"
	"github.com/keybase/go-codec/codec"
)

// A prefetch scope holds server replies fetched ahead of time for one
// operation, so that the operation's own RPCs can be answered without a round
// trip. A batched call (User.sync, say) asks the server for everything the
// operation is about to need in one go; the replies are stored here under the
// exact call each one stands in for; and when the operation's loaders then
// make those calls, RpcClient.Call2 answers them from here. The loaders run
// unchanged and verify the replies exactly as they verify network replies --
// the scope changes where a reply comes from, never what is checked.
//
// Three rules keep this from changing behaviour:
//
//   - An entry is keyed by the client it was fetched over, the method, and
//     the encoded arguments. A call that differs in any of them goes to the
//     network as before. A wrong guess costs a round trip, never correctness.
//   - An entry answers one call, then is gone. A loader that retries (after a
//     merkle race, say) must reach the server, not get the same reply again.
//   - The scope lives in the operation's context and ends with it. Nothing
//     fetched for one operation answers another, however close in time.
type PrefetchScope struct {
	mu      sync.Mutex
	entries map[prefetchKey][]byte
	hits    int
	hitsBy  map[string]int
}

type prefetchKey struct {
	cli    *RpcClient
	method string
	arg    string
}

type prefetchScopeKey struct{}

// WithPrefetchScope returns a context carrying a new, empty prefetch scope.
// If ctx already carries one, it is returned unchanged with that scope, so an
// operation nested in another shares its scope.
func WithPrefetchScope(ctx context.Context) (context.Context, *PrefetchScope) {
	if s := PrefetchScopeFromContext(ctx); s != nil {
		return ctx, s
	}
	s := &PrefetchScope{entries: make(map[prefetchKey][]byte), hitsBy: make(map[string]int)}
	return context.WithValue(ctx, prefetchScopeKey{}, s), s
}

// WithoutPrefetchScope returns ctx with no prefetch scope and no served
// counter: for work an operation leaves running after it returns, which must
// not be answered from that operation's replies.
func WithoutPrefetchScope(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, prefetchScopeKey{}, (*PrefetchScope)(nil))
	return context.WithValue(ctx, prefetchServedKey{}, (*atomic.Int64)(nil))
}

// PrefetchScopeFromContext returns the scope in ctx, or nil.
func PrefetchScopeFromContext(ctx context.Context) *PrefetchScope {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(prefetchScopeKey{}).(*PrefetchScope)
	return s
}

// PlannedCall is one call as a loader will make it: the method's name and its
// argument exactly as RpcClient.Call2 will see them. Only PlanCall makes one,
// by running the generated client, so a reply fetched ahead is keyed under
// the call its loader makes by construction, not by a hand-typed name.
type PlannedCall struct {
	method string
	arg    any
}

// Method is the planned call's method name, as RPC stats report it.
func (p PlannedCall) Method() string { return p.method }

// errCallPlanned stops a planning run after its call is recorded.
var errCallPlanned = InternalError("call planned, not sent")

// planningClient records the one call made through it and sends nothing.
type planningClient struct {
	method string
	arg    any
	calls  int
}

func (c *planningClient) Call2(_ context.Context, m rpc.Methoder, arg, _ any, _ time.Duration, _ rpc.ErrorUnwrapper) error {
	c.calls++
	c.method, c.arg = m.String(), arg
	return errCallPlanned
}

func (c *planningClient) Call(context.Context, rpc.Methoder, any, any, time.Duration) error {
	return NotImplementedError{}
}

func (c *planningClient) CallCompressed(context.Context, rpc.Methoder, any, any, rpc.CompressionType, time.Duration) error {
	return NotImplementedError{}
}

func (c *planningClient) Notify(context.Context, rpc.Methoder, any, time.Duration) error {
	return NotImplementedError{}
}

func (c *planningClient) Transport(context.Context) (rpc.Transporter, error) {
	return nil, NotImplementedError{}
}

// PlanCall runs call -- one generated-client call through cli -- against a
// client that sends nothing, and returns the call it would have made.
func PlanCall(call func(cli rpc.GenericClient) error) (PlannedCall, error) {
	var pc planningClient
	err := call(&pc)
	if pc.calls != 1 || !errors.Is(err, errCallPlanned) {
		return PlannedCall{}, InternalError("PlanCall: expected exactly one call")
	}
	return PlannedCall{method: pc.method, arg: wireData(pc.arg)}, nil
}

// Put stores res as the reply to call over cli. res is the wire form the
// call receives: the Export() of its result, without any rpc.DataWrap header.
func (s *PrefetchScope) Put(cli *RpcClient, call PlannedCall, res any) error {
	if s == nil || cli == nil || call.method == "" {
		return nil
	}
	ab, err := encodeAny(call.arg)
	if err != nil {
		return err
	}
	rb, err := encodeAny(res)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[prefetchKey{cli: cli, method: call.method, arg: string(ab)}] = rb
	return nil
}

// Hits is how many calls the scope has answered.
func (s *PrefetchScope) Hits() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

// Pending is how many stored replies no call has used (yet).
func (s *PrefetchScope) Pending() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

type prefetchServedKey struct{}

// WithPrefetchServedCount returns a context that counts the calls made under
// it that a prefetch scope answered. A chain loader uses it to tell a reply
// fetched ahead from one fetched just now: only the former may predate state
// stored since (BaseChainLoader.runMany).
func WithPrefetchServedCount(ctx context.Context) (context.Context, *atomic.Int64) {
	n := new(atomic.Int64)
	return context.WithValue(ctx, prefetchServedKey{}, n), n
}

func countPrefetchServed(ctx context.Context) {
	if n, _ := ctx.Value(prefetchServedKey{}).(*atomic.Int64); n != nil {
		n.Add(1)
	}
}

// HitsByMethod is how many calls of each method the scope answered.
func (s *PrefetchScope) HitsByMethod() map[string]int {
	ret := make(map[string]int)
	if s == nil {
		return ret
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.hitsBy {
		ret[k] = v
	}
	return ret
}

// PendingMethods lists the methods of stored replies no call has used, for
// diagnosing a batch planned differently from the calls that followed it.
func (s *PrefetchScope) PendingMethods() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ret := make([]string, 0, len(s.entries))
	for k := range s.entries {
		ret = append(ret, k.method)
	}
	sort.Strings(ret)
	return ret
}

// serve answers method(arg) over cli from the scope, if it can, decoding the
// stored reply into res, and reports whether it did. A stored reply that
// won't decode into res answers nothing: the call goes to the network.
func (s *PrefetchScope) serve(cli *RpcClient, method string, arg any, res any) bool {
	if s == nil {
		return false
	}
	ab, err := encodeAny(wireData(arg))
	if err != nil {
		// Not a shape we can key on; let the network have it.
		return false
	}
	k := prefetchKey{cli: cli, method: method, arg: string(ab)}
	s.mu.Lock()
	rb, ok := s.entries[k]
	delete(s.entries, k)
	s.mu.Unlock()
	if !ok {
		return false
	}
	if decodeInto(rb, res) != nil {
		// Leave nothing half-decoded for the network reply to land on.
		if v := reflect.ValueOf(res); v.Kind() == reflect.Pointer && !v.IsNil() {
			v.Elem().SetZero()
		}
		return false
	}
	s.mu.Lock()
	s.hits++
	s.hitsBy[method]++
	s.mu.Unlock()
	return true
}

// isDataWrap reports whether v is an rpc.DataWrap[H, D] (a generic struct
// with Header and Data fields), which carries a protocol header around the
// payload. The header is transport framing, not part of what was asked for.
func isDataWrap(v reflect.Value) bool {
	if v.Kind() != reflect.Struct {
		return false
	}
	t := v.Type()
	return strings.HasPrefix(t.Name(), "DataWrap[") &&
		v.FieldByName("Header").IsValid() && v.FieldByName("Data").IsValid()
}

// wireData strips an rpc.DataWrap header from a call's argument, leaving the
// payload that identifies the call.
func wireData(arg any) any {
	v := reflect.ValueOf(arg)
	for v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
	}
	if isDataWrap(v) {
		return v.FieldByName("Data").Interface()
	}
	return arg
}

// decodeInto decodes a stored payload into res. If res is an rpc.DataWrap,
// the payload goes into its Data and the header is the one this client's own
// protocol version would have received.
func decodeInto(b []byte, res any) error {
	v := reflect.ValueOf(res)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return InternalError("prefetch: result must be a non-nil pointer")
	}
	e := v.Elem()
	target := res
	if isDataWrap(e) {
		h := e.FieldByName("Header")
		ph := MakeProtoHeader()
		if h.Type() == reflect.TypeOf(ph) && h.CanSet() {
			h.Set(reflect.ValueOf(ph))
		}
		target = e.FieldByName("Data").Addr().Interface()
	}
	return codec.NewDecoderBytes(b, Codec()).Decode(target)
}

func encodeAny(v any) ([]byte, error) {
	var b []byte
	err := codec.NewEncoderBytes(&b, Codec()).Encode(v)
	if err != nil {
		return nil, err
	}
	return b, nil
}
