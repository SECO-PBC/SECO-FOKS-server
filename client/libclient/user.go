// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package libclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/foks-proj/go-foks/lib/chains"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/lib/merkle"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
)

type UserContext struct {
	sync.RWMutex
	skmm       *SecretKeyMaterialManager
	Info       proto.UserInfo
	PrivKeys   UserPrivateKeys
	Devname    proto.DeviceName
	homeServer *chains.Probe

	teamMinder *TeamMinder

	// If the user is logged in via yubikey, we won't have any secrets coming through
	// the SecretKeyMaterialManager. We'll instead see the YubiID here.
	Yubi core.PrivateSuiter

	treeLocMu sync.Mutex
	treeLocs  map[proto.Seqno]proto.TreeLocation

	userGCli *certBoundClient
	regCli   *rem.RegClient
	mqCli    *rem.MerkleQueryClient

	// Single-flight unlocking keys
	unlockKeysMu sync.Mutex
	lockState    proto.UserLockState

	nagState NagState

	plc *PartyLoaderCache

	apps *Apps

	// The memoized client certificate, while it is built from a saved chain
	// the reg server has not yet confirmed (refreshSavedCert); nil otherwise.
	// Guarded by the UserContext lock.
	savedCert *savedCertPending
	prewarmed bool // connections were pre-warmed for this user once
}

// certBoundClient is a client together with the identity of the certificate
// it presents, so a cache can tell when the certificate in use has moved on.
type certBoundClient struct {
	gcli *core.RpcClient
	cert core.CertChainID
}

// savedCertPending is a client certificate built from a saved chain that the
// reg server has not confirmed yet. It exists only while unconfirmed: once a
// fetch succeeds, or the server refuses the key, UserContext.savedCert drops
// it. Its fields are guarded by the UserContext lock.
type savedCertPending struct {
	cert     *tls.Certificate
	eid      proto.EntityID
	triedAt  time.Time // when the last confirmation started
	inflight bool      // a confirmation is running for this certificate
}

func (u *UserContext) Eq(u2 *UserContext) bool {
	return u.Info.Eq(u2.Info)
}

func (u *UserContext) Apps() *Apps {
	u.Lock()
	defer u.Unlock()
	if u.apps == nil {
		u.apps = NewApps()
	}
	return u.apps
}

func (u *UserContext) PartyLoaderCache() *PartyLoaderCache {
	u.Lock()
	defer u.Unlock()
	if u.plc == nil {
		u.plc = NewPartyLoaderCache(u)
	}
	return u.plc
}

func (u *UserContext) TeamMinder() *TeamMinder {
	u.Lock()
	defer u.Unlock()
	if u.teamMinder == nil {
		u.teamMinder = NewTeamMinder(u)
	}
	return u.teamMinder
}

func (u *UserContext) UID() proto.UID {
	u.RLock()
	defer u.RUnlock()
	return u.Info.Fqu.Uid
}

func (u *UserContext) ClearConnections() {
	u.Lock()
	defer u.Unlock()
	if u.userGCli != nil {
		u.userGCli.gcli.Shutdown()
		u.userGCli = nil
	}
	u.regCli = nil
	u.mqCli = nil
}

func (u *UserContext) UserInfo() proto.UserInfo {
	u.RLock()
	defer u.RUnlock()
	return u.Info
}

func (u *UserContext) RoleKey() (*core.RoleKey, error) {
	rp := u.Role()
	return core.ImportRole(rp)
}

func (u *UserContext) HostID() proto.HostID {
	u.RLock()
	defer u.RUnlock()
	return u.Info.Fqu.HostID
}

func (u *UserContext) HomeServer() *chains.Probe {
	u.RLock()
	defer u.RUnlock()
	return u.homeServer
}

func (u *UserContext) Skmm() *SecretKeyMaterialManager {
	u.RLock()
	defer u.RUnlock()
	return u.skmm
}

func (u *UserContext) ClearSkmm() {
	u.Lock()
	defer u.Unlock()
	u.skmm = nil
}

func (u *UserContext) SkmmGetOrMake() *SecretKeyMaterialManager {
	u.Lock()
	defer u.Unlock()
	if u.skmm != nil {
		return u.skmm
	}
	skmm := NewSecretKeyMaterialManager(u.Info.Fqu, u.Info.Role)
	u.skmm = skmm
	return skmm
}

func (u *UserContext) IsOnYubiKey() bool {
	u.RLock()
	defer u.RUnlock()
	return u.Yubi != nil
}

func (u *UserContext) LoadSkkm(m MetaContext) (*SecretKeyMaterialManager, error) {
	skm := u.SkmmGetOrMake()
	ss := m.G().SecretStore()
	err := skm.Load(m.Ctx(), ss, SecretStoreGetOpts{NoProvisional: true})
	if err != nil {
		return nil, err
	}
	return skm, nil
}

func (u *UserContext) SetSkmm(s *SecretKeyMaterialManager) {
	u.Lock()
	defer u.Unlock()
	u.skmm = s
}

func (u *UserContext) SetHomeServer(p *chains.Probe) {
	u.Lock()
	defer u.Unlock()
	u.homeServer = p
}

func (u *UserContext) AssertUnlocked(ctx context.Context) error {
	u.Lock()
	defer u.Unlock()
	return u.assertUnlockWithMu(ctx)
}

func (u *UserContext) assertUnlockWithMu(ctx context.Context) error {
	switch {
	case u.PrivKeys.GetDevkey() != nil:
		if u.lockState == proto.UserLockState_SSO {
			return core.SSOIdPLockedError{}
		}
		return nil
	case u.skmm != nil:
		return u.skmm.ReadySeed(ctx)
	case u.Info.YubiInfo != nil:
		return core.YubiLockedError{Info: *u.Info.YubiInfo}
	case u.Info.KeyGenus == proto.KeyGenus_BotToken:
		return core.BotTokenLockedError{}
	default:
		return core.InternalError("unhandled locked key scenario")
	}
}

func (u *UserContext) Devkey(ctx context.Context) (core.PrivateSuiter, error) {
	u.Lock()
	defer u.Unlock()
	return u.devkeyLocked(ctx)
}

func (u *UserContext) SetDevkey(d core.PrivateSuiter) {
	u.Lock()
	defer u.Unlock()
	u.PrivKeys.devkey = d
}

func (u *UserContext) devkeyLocked(ctx context.Context) (core.PrivateSuiter, error) {

	u.PrivKeys.Lock()
	defer u.PrivKeys.Unlock()

	if u.PrivKeys.devkey != nil {
		return u.PrivKeys.devkey, nil
	}
	if u.skmm == nil {
		return nil, core.KeyNotFoundError{Which: "devkey"}
	}
	ret, err := u.skmm.DeviceKeyPrivateSuiter(ctx)
	if err != nil {
		return nil, err
	}
	u.PrivKeys.devkey = ret
	return ret, nil
}

func SwitchActiveUser(m MetaContext, ui proto.UserInfo) (*UserContext, error) {

	lui, err := core.ImportLocalUserIndexFromInfo(ui)
	if err != nil {
		return nil, err
	}
	return SwitchActiveUserWithIndex(m, *lui)
}

func SwitchActiveUserWithIndex(m MetaContext, lui core.LocalUserIndex) (*UserContext, error) {
	g := m.G()

	// Lock order:
	//   userMu.Lock
	//     db.Lock
	//     db.Unlock
	//   userMu.Unlock
	//
	g.userMu.Lock()
	defer g.userMu.Unlock()

	u, ok := g.users[lui]
	if !ok {
		return nil, core.UserNotFoundError{}
	}

	active := g.isActiveUserMuLocked(u.InfoCopy())
	if active {
		return nil, core.UserSwitchError("already active")
	}

	uinf := u.Info
	err := StoreCurrentUserToDB(m, &uinf)
	if err != nil {
		return nil, err
	}

	g.curr = u
	return u, nil
}

func SetActiveUser(m MetaContext, u *UserContext) error {
	g := m.G()

	// Lock order:
	//   userMu.Lock
	//     db.Lock
	//     db.Unlock
	//   userMu.Unlock
	//
	g.userMu.Lock()
	defer g.userMu.Unlock()

	uinf := u.Info

	err := StoreCurrentUserToDB(m, &uinf)
	if err != nil {
		return err
	}

	lui, err := core.ImportLocalUserIndexFromInfo(uinf)
	if err != nil {
		return err
	}

	g.users[*lui] = u
	g.curr = u
	return nil
}

func (m MetaContext) clearCurrentUserWithLock() error {
	g := m.G()
	ctx := m.Ctx()
	g.curr = nil

	db, err := g.Db(ctx, DbTypeHard)
	if err != nil {
		return err
	}
	err = db.DeleteGlobalKV(m, core.KVKeyCurrentUser)
	if err != nil {
		return err
	}
	return nil
}

func (m MetaContext) DeleteUserWithLocalUserIndex(
	lui core.LocalUserIndex,
	keyID proto.EntityID,
) error {
	g := m.G()

	g.userMu.Lock()
	defer g.userMu.Unlock()
	au := g.curr
	alui, err := core.ImportLocalUserIndexFromInfo(au.Info)
	if err != nil {
		return err
	}
	u := g.users[lui]
	if u == nil {
		return nil
	}
	if keyID != nil && !u.Info.Key.Eq(keyID) {
		return nil
	}

	delete(g.users, lui)

	if !alui.Eq(lui) {
		return nil
	}

	err = m.clearCurrentUserWithLock()
	if err != nil {
		return err
	}
	return nil
}

func (g *GlobalContext) ActiveUser() *UserContext {
	g.userMu.RLock()
	defer g.userMu.RUnlock()
	return g.curr
}

func (g *GlobalContext) ActiveUserExport() (*proto.UserContext, error) {
	au := g.ActiveUser()
	if au == nil {
		return nil, core.UserNotFoundError{}
	}
	ret, err := au.Export()
	if err != nil {
		return nil, err
	}
	return ret, nil
}

func (g *GlobalContext) isActiveUserMuLocked(i proto.UserInfo) bool {
	if g.curr == nil {
		return false
	}
	return g.curr.Info.Eq(i)
}

func (u *UserContext) InfoCopy() proto.UserInfo {
	u.Lock()
	defer u.Unlock()
	return u.Info
}

func (g *GlobalContext) AgentStatus(ctx context.Context) (*lcl.AgentStatus, error) {
	pid := os.Getpid()

	var ret lcl.AgentStatus
	ret.Pid = int64(pid)

	// In standalone mode the agent runs in-process over an rpc loopback
	// listener and never binds a socket, so the configured socket path names a
	// file that doesn't exist. Report it empty rather than pointing the caller
	// at something it can't connect to.
	if !g.Cfg().Standalone() {
		file, err := g.Cfg().SocketFile()
		if err != nil {
			return nil, err
		}
		ret.Socket = file.String()
	}

	g.userMu.RLock()
	defer g.userMu.RUnlock()
	for _, u := range g.users {

		tmp, err := u.ExportTryUnlock(ctx)
		if err != nil {
			return nil, err
		}

		if g.isActiveUserMuLocked(u.InfoCopy()) {
			tmp.Info.Active = true
		}
		ret.Users = append(ret.Users, *tmp)
	}
	return &ret, nil
}

func (u *UserContext) ExportTryUnlock(ctx context.Context) (*proto.UserContext, error) {
	u.Lock()
	defer u.Unlock()

	ret, err := u.exportWithMu()
	if err != nil {
		return nil, err
	}
	lockErr := u.assertUnlockWithMu(ctx)
	ret.LockStatus = core.ErrorToStatus(lockErr)
	return ret, nil
}

func (u *UserContext) Export() (*proto.UserContext, error) {
	u.RLock()
	defer u.RUnlock()
	return u.exportWithMu()
}

func (u *UserContext) exportWithMu() (*proto.UserContext, error) {
	var ret proto.UserContext
	tmp := u.Info
	ret.Info = tmp

	if dk := u.PrivKeys.GetDevkey(); dk != nil {
		ep, err := dk.EntityPublic()
		if err != nil {
			return nil, err
		}
		eid := ep.GetEntityID()
		ret.Key = eid
	}

	for _, puk := range u.PrivKeys.GetPUKs() {
		x, _, err := puk.ExportToSharedKey()
		if err != nil {
			return nil, err
		}
		ret.Puks = append(ret.Puks, *x)
	}
	ret.Devname = u.Devname
	ret.NetworkStatus = core.ErrorToStatus(
		u.homeServer.Connectivity(),
	)

	return &ret, nil
}

func (u *UserContext) ArePUKsUnlocked() bool {
	u.RLock()
	defer u.RUnlock()
	return len(u.PrivKeys.puks) > 0
}

type UserPrivateKeys struct {
	sync.RWMutex
	puks   []core.SharedPrivateSuiter
	pukSet *PUKSet
	devkey core.PrivateSuiter
	subkey core.EntityPrivate // only available if devkey is a yubikey
	cert   *tls.Certificate
}

func (u *UserPrivateKeys) GetPUKs() []core.SharedPrivateSuiter {
	u.RLock()
	defer u.RUnlock()
	return u.puks
}

func (u *UserPrivateKeys) GetPUKSet() *PUKSet {
	u.RLock()
	defer u.RUnlock()
	return u.pukSet
}

func (u *UserPrivateKeys) GetDevkey() core.PrivateSuiter {
	u.RLock()
	defer u.RUnlock()
	return u.devkey
}

func (u *UserPrivateKeys) SetDevkey(d core.PrivateSuiter) {
	u.Lock()
	defer u.Unlock()
	u.devkey = d
}

func (u *UserPrivateKeys) SetSubkey(s core.EntityPrivate) {
	u.Lock()
	defer u.Unlock()
	u.subkey = s
}

func (u *UserPrivateKeys) GetSubkey() core.EntityPrivate {
	u.RLock()
	defer u.RUnlock()
	return u.subkey
}

func (u *UserPrivateKeys) GetCert() *tls.Certificate {
	u.RLock()
	defer u.RUnlock()
	return u.cert
}

func (u *UserPrivateKeys) SetCert(c *tls.Certificate) {
	u.Lock()
	defer u.Unlock()
	u.cert = c
}

// ReplaceCert swaps in next only if old is still the certificate held, so a
// Clear (or another replacement) in between is not undone.
func (u *UserPrivateKeys) ReplaceCert(old, next *tls.Certificate) bool {
	u.Lock()
	defer u.Unlock()
	if u.cert != old {
		return false
	}
	u.cert = next
	return true
}

func (u *UserPrivateKeys) Clear() {
	u.Lock()
	defer u.Unlock()

	u.puks = nil
	u.pukSet = nil
	u.devkey = nil
	u.subkey = nil
	u.cert = nil
}

func (u *UserContext) ClearSecrets() error {
	u.Lock()
	defer u.Unlock()
	u.PrivKeys.Clear()
	if u.skmm != nil {
		u.skmm.ClearSeed()
	}
	return nil
}

func (u *UserContext) loadSubkeyLocked(m MetaContext) (core.EntityPrivate, error) {

	devkey := u.PrivKeys.GetDevkey()
	home := u.homeServer
	subkey := u.PrivKeys.GetSubkey()
	uid := u.Info.Fqu.Uid

	if subkey != nil {
		return subkey, nil
	}
	if devkey == nil {
		return nil, core.InternalError("private key structure isn't initialized")
	}
	if home == nil {
		return nil, core.InternalError("user context isn't initialized with probe")
	}
	if !devkey.HasSubkey() {
		return nil, core.InternalError("device key does not support subkeys")
	}

	key, err := LoadSubkey(m, uid, home, devkey)
	if err != nil {
		return nil, err
	}

	u.PrivKeys.SetSubkey(key)

	return key, nil
}

// ClientCertKeyForTest is the entity ID the client certificate is issued for:
// the key the saved chain is filed under.
func (u *UserContext) ClientCertKeyForTest(m MetaContext) (proto.EntityID, error) {
	u.Lock()
	defer u.Unlock()
	key, err := u.pickBestPrivateKeyForCert(m)
	if err != nil {
		return nil, err
	}
	pub, err := key.EntityPublic()
	if err != nil {
		return nil, err
	}
	return pub.GetEntityID(), nil
}

func (u *UserContext) pickBestPrivateKeyForCert(m MetaContext) (core.EntityPrivate, error) {
	dk := u.PrivKeys.GetDevkey()
	sk := u.PrivKeys.GetSubkey()
	switch {
	case sk != nil:
		return sk, nil
	case dk != nil && !dk.HasSubkey():
		return dk.CertSigner()
	case u.skmm != nil:
		return u.devkeyLocked(m.Ctx())
	case dk != nil && dk.HasSubkey():
		return u.loadSubkeyLocked(m)
	case u.Info.YubiInfo != nil && dk == nil:
		return nil, core.YubiLockedError{Info: *u.Info.YubiInfo}
	default:
		return nil, core.KeyNotFoundError{}
	}
}

func (g *GlobalContext) ClearSecrets() {
	g.userMu.Lock()
	defer g.userMu.Unlock()
	g.users = make(map[core.LocalUserIndex]*UserContext)
	g.curr = nil
}

func ClearActiveUsers(m MetaContext) error {

	g := m.G()
	ctx := m.Ctx()
	db, err := g.Db(ctx, DbTypeHard)
	if err != nil {
		return err
	}

	// Lock order:
	//   userMu.Lock
	//     dbs.Lock
	//     dbs.Unlock
	//   userMu.Unlock
	//
	g.userMu.Lock()
	defer g.userMu.Unlock()

	err = db.DeleteGlobalKV(m, core.KVKeyCurrentUser)
	if err != nil {
		return err
	}

	g.users = make(map[core.LocalUserIndex]*UserContext)
	g.curr = nil
	return nil
}

func (g *GlobalContext) ActiveUserClientCert(ctx context.Context) (*tls.Certificate, error) {
	g.userMu.RLock()
	curr := g.curr
	g.userMu.RUnlock()
	if curr == nil {
		return nil, core.UserNotFoundError{}
	}
	return curr.ClientCert(NewMetaContext(ctx, g))
}

func (c *UserContext) ClientCert(m MetaContext) (*tls.Certificate, error) {
	c.Lock()
	defer c.Unlock()
	return c.clientCertLocked(m)
}

func (c *UserContext) clientCertLocked(m MetaContext) (*tls.Certificate, error) {

	if cert := c.PrivKeys.GetCert(); cert != nil {
		c.maybeRefreshSavedCertLocked(m, cert)
		return cert, nil
	}

	key, err := c.pickBestPrivateKeyForCert(m)
	if err != nil {
		return nil, err
	}

	pub, err := key.EntityPublic()
	if err != nil {
		return nil, err
	}
	eid := pub.GetEntityID()

	priv, err := key.PrivateKeyForCert()
	if err != nil {
		return nil, err
	}

	if c.homeServer == nil {
		return nil, core.NoDefaultHostError{}
	}
	if c.homeServer.PublicZone() == nil {
		// A home server is configured but discovery never completed -- a
		// connectivity condition, not a configuration one; report it as such
		// so offline-tolerant callers can classify it.
		return nil, core.NewConnectError("host public zone unavailable",
			core.NoDefaultHostError{})
	}

	chain, err := c.fetchClientCertChain(m, eid)
	if err != nil {
		return nil, err
	}

	cert := &tls.Certificate{
		PrivateKey:  priv,
		Certificate: chain.certs,
	}

	switch chain.src {
	case certFromOfflineCache:
		// Not memoized. A cache-served chain may have expired or rotated
		// during the outage; pinning it in memory would keep presenting it
		// after connectivity returns (every handshake failing, reading as
		// "still offline") with no path to the refetch that heals it.
		// Rebuilding from the cached row per use while offline is cheap.
		return cert, nil
	case certFromSaved:
		// Memoized so every client shares it, but unconfirmed until a fetch
		// from the reg server succeeds; until then it is retried
		// (maybeRefreshSavedCertLocked).
		c.savedCert = &savedCertPending{cert: cert, eid: eid}
		c.startSavedCertRefreshLocked(m, c.savedCert)
	case certFromServer:
		c.savedCert = nil
	}
	c.PrivKeys.SetCert(cert)

	// Every user-authenticated service needs this certificate, so start
	// connecting to all of them now, in parallel, rather than one at a time
	// as each is first used. Once per user: a cert rebuilt after a lock and
	// unlock presents the same saved chain, so its clients are kept. Prewarm
	// makes no network call, so it is fine under c's lock.
	if !c.prewarmed {
		c.prewarmed = true
		c.homeServer.Prewarm(m, []proto.ServerType{
			proto.ServerType_User,
			proto.ServerType_KVStore,
			proto.ServerType_RealTime,
		}, cert)
	}
	return cert, nil
}

// certSource says where a client certificate chain came from.
type certSource int

const (
	// certFromServer: issued by the reg server just now.
	certFromServer certSource = iota
	// certFromSaved: saved by an earlier run and still well within its
	// validity; used without asking the reg server first.
	certFromSaved
	// certFromOfflineCache: the saved chain, served because the reg server
	// could not be reached.
	certFromOfflineCache
)

// sourcedCertChain is a client certificate chain and where it came from.
type sourcedCertChain struct {
	certs [][]byte
	src   certSource
}

// fetchClientCertChain gets the cert chain the reg server issued for the
// given key, caching it in the soft DB (it is derived state: the reg server
// reissues it on demand, so losing the row costs nothing while online).
//
// A saved chain with at least certChainMinRemaining of validity left is used
// without asking the reg server: client certificates run ten years, and every
// server re-checks the device key on each connection (authConnection), so a
// revoked device fails at the handshake whatever certificate it presents.
// That spares a cold start a reg connection, its vhost select and the call:
// four round trips. Offline, the cached chain is served whatever its
// remaining validity -- an expired chain simply fails the TLS handshake,
// which reads as offline.
func (c *UserContext) fetchClientCertChain(
	m MetaContext,
	eid proto.EntityID,
) (
	*sourcedCertChain,
	error,
) {
	// Callers hold c's lock (clientCertLocked); touch fields directly rather
	// than via locking accessors.
	uid := c.Info.Fqu.Uid

	if saved := usableSavedCertChain(m, uid, eid); saved != nil {
		return &sourcedCertChain{certs: saved, src: certFromSaved}, nil
	}

	certChain, err := c.fetchClientCertChainFromServer(m, uid, eid)
	if err == nil {
		return &sourcedCertChain{certs: certChain, src: certFromServer}, nil
	}
	if !core.IsTransportError(err) {
		return nil, err
	}
	var cached lcl.UserCertChain
	_, derr := m.DbGet(&cached, DbTypeSoft, &uid, lcl.DataType_UserCertChain, eid)
	if derr != nil || len(cached.Certs) == 0 {
		return nil, err
	}
	m.Warnw("fetchClientCertChain", "err", err,
		"note", "server unreachable; using cached cert chain")
	return &sourcedCertChain{certs: cached.Certs, src: certFromOfflineCache}, nil
}

// certChainMinRemaining is how long a saved chain must still be valid for to
// be used without asking the reg server first.
const certChainMinRemaining = 24 * time.Hour

// savedCertRetry is how long after a failed confirmation of a saved chain the
// next use of the certificate tries again.
const savedCertRetry = 30 * time.Second

// usableSavedCertChain is the chain saved for this key, if its leaf parses and
// is valid for at least certChainMinRemaining more.
func usableSavedCertChain(m MetaContext, uid proto.UID, eid proto.EntityID) [][]byte {
	var saved lcl.UserCertChain
	_, err := m.DbGet(&saved, DbTypeSoft, &uid, lcl.DataType_UserCertChain, eid)
	if err != nil || len(saved.Certs) == 0 {
		return nil
	}
	if !certChainValidFor(saved.Certs, m.G().Now(), certChainMinRemaining) {
		return nil
	}
	return saved.Certs
}

// certChainValidFor reports that a chain's leaf parses and is still valid d
// from now. NotBefore is not checked: the reg server stamps it with its own
// clock, and a device clock running behind would otherwise reject a chain
// the servers accept.
func certChainValidFor(chain [][]byte, now time.Time, d time.Duration) bool {
	if len(chain) == 0 {
		return false
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return false
	}
	return now.Add(d).Before(leaf.NotAfter)
}

// maybeRefreshSavedCertLocked retries confirming a certificate built from a
// saved chain, when it is the one in use, unconfirmed, no confirmation is
// running, and the last one started at least savedCertRetry ago (offline,
// say). Caller holds c's lock.
func (c *UserContext) maybeRefreshSavedCertLocked(m MetaContext, cert *tls.Certificate) {
	p := c.savedCert
	if p == nil || p.cert != cert || p.inflight ||
		m.G().Now().Sub(p.triedAt) < savedCertRetry {
		return
	}
	c.startSavedCertRefreshLocked(m, p)
}

func (c *UserContext) startSavedCertRefreshLocked(m MetaContext, p *savedCertPending) {
	p.inflight = true
	p.triedAt = m.G().Now()
	go c.refreshSavedCert(m.G(), c.Info.Fqu.Uid, p)
}

// savedCertOutcome is how a confirmation of a saved chain ended.
type savedCertOutcome int

const (
	// savedCertUnreachable: no answer from the reg server (offline, say);
	// the certificate stays unconfirmed and is retried.
	savedCertUnreachable savedCertOutcome = iota
	// savedCertConfirmed: the reg server issued a chain for the key.
	savedCertConfirmed
	// savedCertRefused: the reg server refused the key (a revoked device).
	savedCertRefused
)

// refreshSavedCert asks the reg server for the chain after a saved one was
// used. It never holds c's lock across the network.
//
//   - Issued, and the saved chain is still good (replaceSavedCert false):
//     nothing changes, not even the saved row, so the chain presented stays
//     the same across lock/unlock and restarts and no client is rebuilt.
//   - Issued, and the saved chain stopped being good enough (CA changed,
//     close to expiry): the fresh chain is saved and swapped in, only if the
//     saved certificate is still the one held (so a UserLock in between is
//     not undone). Clients notice by CertChainID and rebuild.
//   - Refused (AuthError): the saved row is dropped and the certificate
//     un-memoized, so the next use asks the reg server and its caller gets
//     the refusal, as it did before saved chains were used.
//   - No answer: retried on a later use (maybeRefreshSavedCertLocked).
func (c *UserContext) refreshSavedCert(g *GlobalContext, uid proto.UID, p *savedCertPending) {
	m, cancel := NewMetaContextBackground(g).WithContextTimeout(time.Minute)
	defer cancel()
	outcome := savedCertUnreachable
	defer func() {
		c.Lock()
		defer c.Unlock()
		p.inflight = false
		if outcome != savedCertUnreachable && c.savedCert == p {
			c.savedCert = nil
		}
	}()

	cli, err := c.regClientUnlocked(m)
	if err != nil {
		m.Warnw("refreshSavedCert", "stage", "regClient", "err", err)
		return
	}
	fresh, err := cli.GetClientCertChain(m.Ctx(), rem.GetClientCertChainArg{Uid: uid, Key: p.eid})
	if core.IsAuthError(err) {
		outcome = savedCertRefused
		m.Warnw("refreshSavedCert", "stage", "fetch", "err", err, "note", "key refused; dropping saved chain")
		derr := m.DbDelete(DbTypeSoft, &uid, lcl.DataType_UserCertChain, p.eid)
		if derr != nil {
			m.Warnw("refreshSavedCert", "stage", "delete", "err", derr)
		}
		c.PrivKeys.ReplaceCert(p.cert, nil)
		return
	}
	if err != nil {
		m.Warnw("refreshSavedCert", "stage", "fetch", "err", err)
		return
	}
	now := m.G().Now()
	if !certChainValidFor(fresh, now, 0) {
		m.Warnw("refreshSavedCert", "stage", "check", "err", "reg server returned an unusable chain")
		return
	}
	outcome = savedCertConfirmed
	if !replaceSavedCert(p.cert.Certificate, fresh, now) {
		return
	}
	store := lcl.UserCertChain{Certs: fresh}
	err = m.DbPut(DbTypeSoft, PutArg{Scope: &uid, Typ: lcl.DataType_UserCertChain, Key: p.eid, Val: &store})
	if err != nil {
		m.Warnw("refreshSavedCert", "stage", "store", "err", err)
	}
	c.PrivKeys.ReplaceCert(p.cert, &tls.Certificate{PrivateKey: p.cert.PrivateKey, Certificate: fresh})
}

// SavedCertUnconfirmedForTest reports that the certificate in use was built
// from a saved chain the reg server has not confirmed yet.
func (c *UserContext) SavedCertUnconfirmedForTest() bool {
	c.Lock()
	defer c.Unlock()
	return c.savedCert != nil
}

// replaceSavedCert says whether a certificate built from the saved chain
// should give way to the freshly fetched one. The reg server issues a new
// leaf every time (new serial and start time), so the chains always differ;
// the saved one stops working only if the issuing CA changed or it is close
// to expiry.
func replaceSavedCert(saved, fresh [][]byte, now time.Time) bool {
	return !sameCertIssuer(saved, fresh) || !certChainValidFor(saved, now, certChainMinRemaining)
}

// sameCertIssuer reports that two chains' leaves were issued by the same CA.
func sameCertIssuer(a, b [][]byte) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	la, err := x509.ParseCertificate(a[0])
	if err != nil {
		return false
	}
	lb, err := x509.ParseCertificate(b[0])
	if err != nil {
		return false
	}
	return bytes.Equal(la.AuthorityKeyId, lb.AuthorityKeyId) && bytes.Equal(la.RawIssuer, lb.RawIssuer)
}

// fetchClientCertChainFromServer asks the reg server for the chain and caches
// what it returns. A failed cache write is logged, not returned: the chain in
// hand is still good, and the next fetch will try the write again.
func (c *UserContext) fetchClientCertChainFromServer(
	m MetaContext,
	uid proto.UID,
	eid proto.EntityID,
) ([][]byte, error) {
	cli, err := c.regClientLocked(m)
	if err != nil {
		return nil, err
	}
	certChain, err := cli.GetClientCertChain(m.Ctx(), rem.GetClientCertChainArg{
		Uid: uid,
		Key: eid,
	})
	if err != nil {
		return nil, err
	}
	store := lcl.UserCertChain{Certs: certChain}
	serr := m.DbPut(DbTypeSoft, PutArg{
		Scope: &uid,
		Typ:   lcl.DataType_UserCertChain,
		Key:   eid,
		Val:   &store,
	})
	if serr != nil {
		m.Warnw("fetchClientCertChain", "stage", "store", "err", serr)
	}
	return certChain, nil
}

func (c *UserContext) UserGCli(m MetaContext) (*core.RpcClient, error) {
	c.Lock()
	defer c.Unlock()

	// Keep the client unless the memoized certificate has changed under it
	// (refreshSavedCert). A replaced client is retired: shut down once the
	// calls still in flight on it are done.
	if c.userGCli != nil {
		cur := c.PrivKeys.GetCert()
		if cur == nil || core.ClientCertID(cur) == c.userGCli.cert {
			return c.userGCli.gcli, nil
		}
	}

	cert, err := c.clientCertLocked(m)
	if err != nil {
		return nil, err
	}

	gcli, err := c.homeServer.RPCClient(m, proto.ServerType_User, cert)
	if err != nil {
		return nil, err
	}
	if c.userGCli != nil {
		c.userGCli.gcli.Retire()
	}
	c.userGCli = &certBoundClient{gcli: gcli, cert: core.ClientCertID(cert)}
	return gcli, nil
}

func (c *UserContext) UserClient(m MetaContext) (*rem.UserClient, error) {
	gcli, err := c.UserGCli(m)
	if err != nil {
		return nil, err
	}
	ret := core.NewUserClient(gcli, m)
	return &ret, nil
}

func (c *UserContext) TeamAdminClient(m MetaContext) (*rem.TeamAdminClient, error) {
	gcli, err := c.UserGCli(m)
	if err != nil {
		return nil, err
	}
	ret := &rem.TeamAdminClient{Cli: gcli, ErrorUnwrapper: core.StatusToError}
	return ret, nil
}

func (c *UserContext) TeamMemberClient(m MetaContext) (*rem.TeamMemberClient, error) {
	gcli, err := c.UserGCli(m)
	if err != nil {
		return nil, err
	}
	ret := &rem.TeamMemberClient{Cli: gcli, ErrorUnwrapper: core.StatusToError}
	return ret, nil
}

func (c *UserContext) regClientLocked(m MetaContext) (*rem.RegClient, error) {

	if c.regCli != nil {
		return c.regCli, nil
	}

	gcli, err := c.homeServer.RegGCli(m)
	if err != nil {
		return nil, err
	}

	tmp := core.NewRegClient(gcli, m.G())
	ret := &tmp
	c.regCli = ret
	return ret, nil
}

// regClientUnlocked is RegClient for a caller that must not hold c's lock
// across the network: it makes the client (which may refresh the probe)
// without the lock.
func (c *UserContext) regClientUnlocked(m MetaContext) (*rem.RegClient, error) {
	c.Lock()
	if c.regCli != nil {
		cli := c.regCli
		c.Unlock()
		return cli, nil
	}
	hs := c.homeServer
	c.Unlock()
	if hs == nil {
		return nil, core.NoDefaultHostError{}
	}
	gcli, err := hs.RegGCli(m)
	if err != nil {
		return nil, err
	}
	tmp := core.NewRegClient(gcli, m.G())
	c.Lock()
	defer c.Unlock()
	if c.regCli != nil {
		gcli.Shutdown()
		return c.regCli, nil
	}
	c.regCli = &tmp
	return c.regCli, nil
}

func (c *UserContext) RegClient(m MetaContext) (*rem.RegClient, error) {
	c.Lock()
	defer c.Unlock()
	return c.regClientLocked(m)
}

type YubiLoader func(
	ctx context.Context,
	i proto.YubiKeyInfoHybrid,
	r proto.Role,
	h proto.HostID,
) (
	core.PrivateSuiter,
	error,
)

func (u *UserContext) YubiUnlock(m MetaContext) error {
	if u == nil {
		return core.UserNotFoundError{}
	}
	u.Lock()
	defer u.Unlock()
	return u.yubiUnlockUserLocked(m)
}

func (u *UserContext) yubiUnlockUserLocked(m MetaContext) error {
	if u.PrivKeys.GetDevkey() != nil {
		return nil
	}
	if u.Info.YubiInfo == nil {
		return core.KeyNotFoundError{}
	}
	yd := m.G().YubiDispatch()
	yk, err := yd.LoadHybrid(m.Ctx(), *u.Info.YubiInfo, u.Info.Role, u.Info.Fqu.HostID)
	if err != nil {
		return err
	}
	u.PrivKeys.SetDevkey(yk)
	u.Yubi = yk

	return nil
}

func (u *UserContext) Role() proto.Role {
	u.Lock()
	defer u.Unlock()
	return u.Info.Role
}

func (u *UserContext) FQU() proto.FQUser {
	u.Lock()
	defer u.Unlock()
	return u.Info.Fqu
}

func (u *UserPrivateKeys) SetPUKs(ps *PUKSet) {
	u.Lock()
	defer u.Unlock()
	u.pukSet = ps
	u.puks = ps.All()
}

func (u *UserContext) RefreshPUKs(m MetaContext) (*PUKSet, error) {
	pm := NewPUKMinder(u)
	pukset, err := pm.GetPUKSetForRole(m, u.Role())
	if err != nil {
		return nil, err
	}
	u.Lock()
	defer u.Unlock()

	u.PrivKeys.SetPUKs(pukset)

	return pukset, nil
}

func (u *UserContext) GetSharedKeyManager(m MetaContext) (SharedKeyManager, error) {
	return NewPUKMinder(u), nil
}

func (u *UserContext) PopulateWithDevkey(m MetaContext) error {
	user, err := LoadMe(m, u)

	if core.IsTransportError(err) {
		// Cold start with no network: fall back to the last verified
		// sigchain snapshot, so a device that has completed an online load
		// before can unlock and work offline (cached reads, durably queued
		// writes). A device with no snapshot keeps the original error --
		// there is nothing verified to serve. See LoadMeFromCache for the
		// trust reasoning and docs/rt_offline.md ("cold-start bootstrap").
		cached, cerr := LoadMeFromCache(m, u)
		if cerr != nil {
			m.Warnw("PopulateWithDevkey", "stage", "cacheFallback", "err", cerr)
			return err
		}
		m.Warnw("PopulateWithDevkey", "stage", "offline",
			"note", "using cached sigchain snapshot; server unreachable", "err", err)
		user = cached
		err = nil
	}
	if err != nil {
		return err
	}

	pm := NewPUKMinder(u).SetUser(user)
	pukset, err := pm.GetPUKSetForRole(m, u.Role())
	if err != nil {
		return err
	}

	u.Lock()
	defer u.Unlock()

	u.Info.Username = user.prot.Username.B
	u.Info.HostAddr = user.hostAddr

	u.PrivKeys.SetPUKs(pukset)

	uinf := u.Info
	err = StoreUserToDB(m, &uinf)
	if err != nil {
		return err
	}

	return nil
}

func (k *UserPrivateKeys) LatestPuk() core.SharedPrivateSuiter {
	k.RLock()
	defer k.RUnlock()
	n := len(k.puks)
	if n == 0 {
		return nil
	}
	return k.puks[n-1]
}

func (m MetaContext) DbPutUserInfo(u *proto.UserInfo, isActive bool) error {
	var ltx LocalDbTx
	err := ltx.PutUser(*u, isActive)
	if err != nil {
		return err
	}
	return m.DbPutTx(DbTypeHard, ltx.Arg())
}

func (u *UserContext) PUKs() []core.SharedPrivateSuiter {
	u.RLock()
	defer u.RUnlock()
	return u.PrivKeys.puks
}

func (u *UserContext) GetUnlockedSKMWK(m MetaContext) (*lcl.UnlockedSKMWK, error) {
	ucli, err := u.UserClient(m)
	if err != nil {
		return nil, err
	}
	fqu := u.FQU()
	puks := u.PUKs()
	tmp, err := GetUnlockedSKMWK(m.Ctx(), ucli, fqu, puks)
	if err != nil {
		return nil, err
	}
	return tmp, nil
}

func (u *UserContext) GetKexPPE(m MetaContext) (*lcl.KexPPE, error) {

	_, err := u.RefreshPUKs(m)
	if err != nil {
		return nil, err
	}

	tmp, err := u.GetUnlockedSKMWK(m)
	if errors.Is(err, core.PassphraseNotFoundError{}) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &lcl.KexPPE{
		Skwk:  core.Last(tmp.Lst),
		PpGen: tmp.ExpectedGen,
		Salt:  tmp.Salt,
		Sv:    tmp.Sv,
	}, nil
}

func (u *UserContext) GetSelfViewToken(m MetaContext) (*proto.PermissionToken, error) {

	u.RLock()
	idx := u.Info.ToLocalUserIndex()
	u.RUnlock()

	ui, err := LoadUserFromDB(m, idx)
	if err == nil && ui.ViewToken != nil && ui.ViewToken.IsSelf {
		return &ui.ViewToken.Token, nil
	}

	skmm, err := u.LoadSkkm(m)
	if err != nil {
		return nil, err
	}
	return skmm.SelfViewToken(), nil
}

func (u *UserContext) GrantRemoteViewPermissionTo(
	m MetaContext,
	viewer proto.FQParty,
) (
	*proto.PermissionToken,
	error,
) {
	cli, err := u.UserClient(m)
	if err != nil {
		return nil, err
	}
	tok, err := cli.GrantRemoteViewPermissionForUser(m.Ctx(),
		rem.GrantRemoteViewPermissionPayload{
			Viewee: u.FQU().Uid.ToPartyID(),
			Viewer: viewer,
			Tm:     proto.Now(),
		},
	)
	if err != nil {
		return nil, err
	}
	return &tok, nil
}

func (u *UserContext) Refresh(m MetaContext, _ *TeamMinder) (CryptoPartier, error) {
	_, err := u.RefreshPUKs(m)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (u *UserContext) KeyRefresher(m MetaContext) (SharedKeySequence, error) {
	keys, err := u.RefreshPUKs(m)
	if err != nil {
		return nil, err
	}
	return keys, nil
}

var _ CryptoPartier = (*UserContext)(nil)

func (u *UserContext) FQParty() proto.FQParty {
	u.RLock()
	defer u.RUnlock()
	return u.Info.Fqu.FQParty()
}

func (u *UserContext) CurrentAdminKey(m MetaContext) (core.SharedPrivateSuiter, error) {
	puks, err := u.RefreshPUKs(m)
	if err != nil {
		return nil, err
	}
	return puks.Current(), nil
}

func (u *UserContext) PrivateKeyAt(m MetaContext, gen proto.Generation) (core.SharedPrivateSuiter, error) {
	puks, err := u.RefreshPUKs(m)
	if err != nil {
		return nil, err
	}
	return puks.At(gen), nil
}

func (u *UserContext) SrcRole() proto.Role { return proto.OwnerRole }

func (g *GlobalContext) TeamMinder() (*TeamMinder, error) {
	au := g.ActiveUser()
	if au == nil {
		return nil, core.NoActiveUserError{}
	}
	return au.TeamMinder(), nil
}

func (m MetaContext) TeamMinder() (*TeamMinder, error) {
	return m.G().TeamMinder()
}

func (g *GlobalContext) FindUser(u proto.FQUserParsed) (*UserContext, error) {
	g.userMu.RLock()
	defer g.userMu.RUnlock()
	var best *UserContext
	for _, v := range g.users {
		eq, err := v.Info.EqFQUserParsed(u, core.NormalizeName)
		if err != nil {
			return nil, err
		}
		if !eq {
			continue
		}
		if best == nil {
			best = v
		} else {
			gt, err := v.Info.Role.GreaterThan(best.Info.Role)
			if err != nil {
				return nil, err
			}
			if gt {
				best = v
			}
		}
	}
	if best == nil {
		return nil, core.UserNotFoundError{}
	}
	return best, nil
}

func (u *UserContext) MerkleAgent(m MetaContext) (*merkle.Agent, error) {
	hs := u.HomeServer()
	if hs == nil {
		return nil, core.NoDefaultHostError{}
	}
	return hs.MerkleAgent(m)
}

func (u *UserContext) MerkleAgentWithLock(m MetaContext) (*merkle.Agent, error) {
	hs := u.homeServer
	if hs == nil {
		return nil, core.NoDefaultHostError{}
	}
	return hs.MerkleAgent(m)
}
