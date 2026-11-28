// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package chains

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"reflect"
	"sync"
	"time"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/lib/merkle"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
)

type MetaContext interface {
	Infow(msg string, keysAndValues ...interface{})
	Warnw(msg string, keysAndValues ...interface{})
	Errorw(msg string, keysAndValues ...interface{})
	Ctx() context.Context
	WithLogTagI(tag string) MetaContext
	// WithContextI is this MetaContext over ctx. DiscoveryMgr uses it to
	// run work that outlives the call that started it (revalidate).
	WithContextI(ctx context.Context) MetaContext
	WarnwWithContext(ctx context.Context, msg string, keysAndValues ...interface{})
}

type DiscoveryInterface interface {
	MakeRpcClient(m MetaContext, addr proto.TCPAddr, rootCAs *x509.CertPool, cliCert *tls.Certificate, opts *core.RpcClientOpts) (*core.RpcClient, error)
	LoadHostchainFromDB(m MetaContext, hostID proto.HostID) (*core.Hostchain, error)
	// Store/LoadPublicZoneFromDB cache the host's signed public zone after it
	// has been verified (CheckZoneSig), so a cold start with no network can
	// still name the host's service addresses. Verified-on-write: only a zone
	// that passed verification is ever stored.
	StorePublicZoneToDB(m MetaContext, hostID proto.HostID, pz *proto.PublicZone) error
	LoadPublicZoneFromDB(m MetaContext, hostID proto.HostID) (*proto.PublicZone, error)
	MakeMerkleAgent(m MetaContext, hostID proto.HostID, addr proto.TCPAddr, rootCAs *x509.CertPool) (*merkle.Agent, error)
	StoreHostchainToDB(m MetaContext, hc *core.Hostchain) error
	CheckPin(m MetaContext, hn proto.Hostname, hid proto.HostID) error
	ProbeRootCAs(m MetaContext) (*x509.CertPool, []string, error)
	HostsBeacon(m MetaContext) (proto.TCPAddr, error)
	DefProbePort() proto.Port
}

type DiscoveryMgr struct {
	cacheLock   sync.RWMutex
	probes      map[proto.HostID]*Probe
	hostAddrMap map[proto.TCPAddr]proto.HostID
	resolvMap   map[proto.HostID]proto.TCPAddr

	di DiscoveryInterface

	cliLock sync.Mutex
	cli     *core.RpcClient

	// bg bounds the background work the manager starts (revalidate):
	// Shutdown cancels it.
	bg     context.Context
	bgStop context.CancelFunc
}

func NewDiscoveryMgr(di DiscoveryInterface) *DiscoveryMgr {
	bg, bgStop := context.WithCancel(context.Background())
	return &DiscoveryMgr{
		bg:          bg,
		bgStop:      bgStop,
		probes:      make(map[proto.HostID]*Probe),
		hostAddrMap: make(map[proto.TCPAddr]proto.HostID),
		resolvMap:   make(map[proto.HostID]proto.TCPAddr),
		di:          di,
	}
}

type ResolveOpts struct {
	Timeout time.Duration
}

type ResolveRes struct {
	Addr  proto.TCPAddr
	Probe *Probe
}

type ProbeArg struct {
	HostID  proto.HostID
	Addr    proto.TCPAddr
	Timeout time.Duration
	DefPort proto.Port
	Fresh   bool
	RootCAs *x509.CertPool

	prev          *core.Hostchain // previous hostchain, preloaded
	checkHostname bool            // if we did a beacon resolve, we need to check hostchain against probe zone
}

func (d ProbeArg) check() error {
	if d.HostID.IsZero() && d.Addr.IsZero() {
		return core.InternalError("bad arguement to Probe, no lookup mechanism given")
	}
	return nil
}

func (d *DiscoveryMgr) checkCacheForProbe(arg ProbeArg) *Probe {

	d.cacheLock.RLock()
	defer func() {
		d.cacheLock.RUnlock()
	}()
	if arg.Fresh {
		return nil
	}
	hid := arg.HostID
	if hid.IsZero() {
		hid = d.hostAddrMap[arg.Addr]
	}
	if hid.IsZero() {
		return nil
	}
	ret, ok := d.probes[arg.HostID]
	if ok {
		return ret
	}
	return nil
}

func loadPrevChain(m MetaContext, di DiscoveryInterface, id proto.HostID) (*core.Hostchain, error) {
	ch, err := di.LoadHostchainFromDB(m, id)
	if err != nil && errors.Is(err, core.RowNotFoundError{}) {
		// No prior chain, this is OK
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ch, nil
}

func (d *DiscoveryMgr) resolveHostAddr(m MetaContext, arg *ProbeArg) error {
	if !arg.Addr.IsZero() {
		return nil
	}
	addrp, prev, err := d.ResolveHostAddr(m, arg.HostID)
	if err != nil {
		return err
	}
	arg.Addr = *addrp
	arg.prev = prev
	arg.checkHostname = true
	return nil
}

func (d *DiscoveryMgr) ResolveHostAddr(m MetaContext, hostID proto.HostID) (*proto.TCPAddr, *core.Hostchain, error) {

	prev, err := loadPrevChain(m, d.di, hostID)
	if err != nil {
		return nil, nil, err
	}

	if prev != nil && !prev.Addr().IsZero() {
		tmp := prev.Addr()
		return &tmp, prev, nil
	}

	d.cacheLock.RLock()
	addr := d.resolvMap[hostID]
	d.cacheLock.RUnlock()

	if !addr.IsZero() {
		return &addr, nil, nil
	}

	addrp, err := d.resolveViaBeacon(m, hostID)
	if err != nil {
		return nil, nil, err
	}

	d.cacheLock.Lock()
	d.resolvMap[hostID] = *addrp
	d.cacheLock.Unlock()

	return addrp, nil, nil
}

// Probe returns (non-nil, nil) on success. It mostly returns (nil, non-nil)
// on error. But in the case of a ConnectinoError, it will return (non-nil, non-nil)
// so that the caller can try again later.
func (d *DiscoveryMgr) Probe(m MetaContext, arg ProbeArg) (*Probe, error) {

	pr, cached, err := d.makeProbe(m, arg)
	if err != nil {
		return nil, err
	}
	if cached {
		return pr, nil
	}
	return d.runProbe(m, pr)
}

// ProbeCachedOK is Probe for a host this device has verified before: it is
// used at once, from the identity (hostchain and public zone) the last full
// probe stored, while a full probe re-verifies it in the background
// (revalidate). Until that finishes, the client trusts what it verified last
// time; a host whose identity has changed or no longer verifies is re-probed
// before any new connection is made to it. A host with no stored identity,
// or a manager that has been shut down (so cannot re-verify), gets an
// ordinary probe.
func (d *DiscoveryMgr) ProbeCachedOK(m MetaContext, hostID proto.HostID, timeout time.Duration) (*Probe, error) {
	if hostID.IsZero() {
		return nil, core.InternalError("ProbeCachedOK needs a host ID")
	}
	pr, cached, err := d.makeProbe(m, ProbeArg{HostID: hostID, Timeout: timeout})
	if err != nil {
		return nil, err
	}
	if cached {
		return pr, nil
	}
	if d.bg.Err() == nil && pr.hydrateFromCache(m) {
		d.cacheProbe(pr)
		go d.revalidate(m, pr)
		return pr, nil
	}
	return d.runProbe(m, pr)
}

func (d *DiscoveryMgr) runProbe(m MetaContext, pr *Probe) (*Probe, error) {
	err := pr.Run(m)

	// If we failed to connect, then we can still return the dead
	// probe, and we can try to rejeuvenate it later.
	if core.IsTransportError(err) {
		return pr, err
	}

	if err != nil {
		return nil, err
	}

	d.cacheProbe(pr)
	return pr, nil
}

// Shutdown stops the manager's background work (revalidate). Probes it
// already handed out keep working.
func (d *DiscoveryMgr) Shutdown() {
	d.bgStop()
}

// revalidateTimeout bounds a background probe whose probe RPC has no
// timeout of its own; otherwise it gets twice that timeout, the merkle steps
// after the probe RPC being bounded by nothing else.
const revalidateTimeout = time.Minute

// revalidate runs a full probe of pr's host -- every check a probe makes --
// on a probe of its own, after pr was put to use from its cached identity
// (ProbeCachedOK). If the host proves the same identity, pr carries on.
// Otherwise pr is marked failed: the next connection made through it
// re-probes (Probe.refresh) and so picks up the host's new identity, or
// fails if it no longer verifies. A transport failure leaves pr as an
// offline start does: serving the cached identity, re-probing on next use.
func (d *DiscoveryMgr) revalidate(m MetaContext, pr *Probe) {
	// It outlives the start that kicked it off, up to a bound, and stops on
	// Shutdown.
	to := revalidateTimeout
	if pr.timeout > 0 {
		to = 2 * pr.timeout
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(m.Ctx()), to)
	defer cancel()
	defer context.AfterFunc(d.bg, cancel)()
	m = m.WithContextI(ctx).WithLogTagI("probe")

	fresh := &Probe{
		di:      d.di,
		addr:    pr.addr,
		timeout: pr.timeout,
		defport: pr.defport,
		rootCAs: pr.rootCAs,
		hostID:  pr.hostID,
	}
	err := fresh.runVerified(m)

	// Counted on pr, which the next refresh probes through, so a revalidate
	// that met a bad certificate counts toward the interception warning.
	if pr.noteCertOutcome(err) {
		m.Warnw("probe", "stage", "revalidate", "err", err,
			"note", "cannot verify server identity; serving cached identity",
			"certFails", pr.certFailCount())
	}

	same := err == nil && sameIdentity(pr, fresh)
	// The merkle agent fresh connected is for the same host, zone and CAs
	// as pr's when the identity matches: pr takes it rather than connect
	// its own later. Otherwise nobody will use it.
	if ma := fresh.takeMerkleAgent(); ma != nil && !(same && pr.adoptMerkleAgent(ma)) {
		ma.ShutdownClient()
	}

	switch {
	case same:
		m.Infow("probe", "stage", "revalidated")
	case err == nil:
		m.Warnw("probe", "stage", "revalidate", "note", "host identity changed since cached; re-probing")
		pr.setLastFail(errIdentityChanged)
	default:
		m.Warnw("probe", "stage", "revalidate", "err", err)
		pr.setLastFail(err)
	}
}

// errIdentityChanged marks a probe whose cached identity is no longer the
// host's, so its next use re-probes.
var errIdentityChanged = core.InternalError("host identity changed since it was cached")

// sameIdentity reports whether two probes of one host hold the same verified
// identity: hostchain tail and public zone. The tail's hash commits to the
// whole chain, so it covers the keys and CAs derived from it.
func sameIdentity(a, b *Probe) bool {
	if a.Chain() == nil || b.Chain() == nil || a.PublicZone() == nil || b.PublicZone() == nil {
		return false
	}
	return reflect.DeepEqual(a.Chain().Tail(), b.Chain().Tail()) &&
		reflect.DeepEqual(*a.PublicZone(), *b.PublicZone())
}

func (d *DiscoveryMgr) cacheProbe(pr *Probe) {
	d.cacheLock.Lock()
	defer d.cacheLock.Unlock()
	hostID := pr.Chain().HostID()
	addr := pr.CanonicalAddr()
	d.probes[hostID] = pr
	d.hostAddrMap[addr] = hostID
	d.resolvMap[hostID] = addr
}

func (d *DiscoveryMgr) MakeProbe(m MetaContext, arg ProbeArg) (*Probe, error) {
	ret, _, err := d.makeProbe(m, arg)
	return ret, err
}

func (d *DiscoveryMgr) makeProbe(m MetaContext, arg ProbeArg) (*Probe, bool, error) {

	if err := arg.check(); err != nil {
		return nil, false, err
	}

	if ret := d.checkCacheForProbe(arg); ret != nil {
		return ret, true, nil
	}

	err := d.resolveHostAddr(m, &arg)
	if err != nil {
		return nil, false, err
	}

	pr := &Probe{
		di:      d.di,
		addr:    arg.Addr,
		prev:    arg.prev,
		timeout: arg.Timeout,
		defport: arg.DefPort,
		rootCAs: arg.RootCAs,
	}
	if !arg.HostID.IsZero() {
		pr.hostID = &arg.HostID
	}
	return pr, false, nil
}

func (d *DiscoveryMgr) getBeaconCli(m MetaContext) (*rem.BeaconClient, error) {
	gcli, err := d.getBeaconGCli(m)
	if err != nil {
		return nil, err
	}
	ret := core.NewBeaconClient(gcli, m)
	return &ret, nil
}

func (d *DiscoveryMgr) getBeaconGCli(m MetaContext) (*core.RpcClient, error) {

	d.cliLock.Lock()
	cli := d.cli
	d.cliLock.Unlock()

	if cli != nil {
		return cli, nil
	}

	addr, err := d.di.HostsBeacon(m)
	if err != nil {
		return nil, err
	}
	if addr == "" {
		return nil, core.NoDefaultHostError{}
	}
	rootCAs, _, err := d.di.ProbeRootCAs(m)
	if err != nil {
		return nil, err
	}
	ret, err := d.di.MakeRpcClient(m, addr, rootCAs, nil, nil)
	if err != nil {
		return nil, err
	}

	d.cliLock.Lock()
	d.cli = cli
	d.cliLock.Unlock()

	return ret, nil
}

func (d *DiscoveryMgr) resolveViaBeacon(m MetaContext, i proto.HostID) (*proto.TCPAddr, error) {
	cli, err := d.getBeaconCli(m)
	if err != nil {
		return nil, err
	}
	res, err := cli.BeaconLookup(m.Ctx(), i)
	if err != nil {
		return nil, err
	}
	return &res, nil
}
