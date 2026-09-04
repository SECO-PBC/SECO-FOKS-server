// Private-channel server-side ACL (fork-only; docs/rt-private-channel-acl.md).
//
// One negative test per row of the §5 path inventory, plus the §8.2
// invariants. These matter more than most tests: a private channel is
// protected by policy, not by keys -- every team member at the channel's read
// role holds the key, and there is no rekey on channel-membership change -- so
// a gap in any one of these paths exposes the channel's entire history,
// silently and retroactively, and nothing else in the suite would go red.
//
// The recurring cast, per §8.1:
//
//	alice -- team owner; creates the private channel and is its ACL owner
//	bob   -- ordinary team member, granted into the channel
//	cleo  -- ordinary team member, NOT in the channel ("Keith" in the spec)
//	dara  -- team admin, NOT in the channel
//	eddie -- granted into the channel, then removed from the team
package lib

import (
	"fmt"
	"testing"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/client/librt"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/lib/team"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/stretchr/testify/require"
)

// privActor bundles one user with the client-side machinery to act as them.
type privActor struct {
	u      *TestUser
	m      librt.MetaContext
	minder *librt.Minder
	rawCli *rem.RealTimeClient
}

// raw returns a RealTime RPC client speaking as this actor, built here rather
// than borrowed from the Minder.
//
// The Minder's wrapped calls resolve a channel against the caller's own
// (correctly filtered) channel list, so for a channel the caller cannot see
// they fail client-side and never reach the server -- which would prove
// nothing about the ACL. These tests want the SERVER's answer. Constructing
// the client here keeps that need in the tests instead of putting a
// filtering-bypass hatch on the production Minder, where a future caller could
// reach for it by accident.
func (a *privActor) raw(t *testing.T) *rem.RealTimeClient {
	if a.rawCli != nil {
		return a.rawCli
	}
	base := a.m.Base()
	cert, err := base.ActiveUserClientCert()
	require.NoError(t, err)
	pr := base.G().ActiveUser().HomeServer()
	require.NotNil(t, pr)
	gcli, err := pr.RPCClient(base, proto.ServerType_RealTime, cert)
	require.NoError(t, err)
	cli := core.NewRealTimeClient(gcli, base)
	a.rawCli = &cli
	return a.rawCli
}

// privScene is a team with a private channel in it, plus the five actors.
type privScene struct {
	tew    *TestEnvWrapper
	tm     *teamObj
	fqt    *proto.FQTeamParsed
	teamID proto.TeamID

	alice, bob, cleo, dara, eddie *privActor

	chid proto.RTChannelID
	name proto.RTChannelName
}

// newPrivActor builds an actor with caching disabled, so that every list and
// every thread read in these tests is answered by the server rather than by a
// client cache warmed before a grant or revoke changed the answer.
func newPrivActor(t *testing.T, tew *TestEnvWrapper, u *TestUser) *privActor {
	m := librt.NewMetaContext(tew.NewClientMetaContextWithEracer(t, u))
	return &privActor{
		u:      u,
		m:      m,
		minder: librt.NewMinderWithCacheSettings(m.G().ActiveUser(), libclient.CacheSettings{}),
	}
}

// setupPrivScene builds the team and, unless `skipChannel`, the private
// channel with alice as its ACL owner and nobody else in it.
//
// The channel takes DefaultRole for both read and write, which makes it
// bottom-tier: privacy is orthogonal to tier (§4.1), and using the bottom tier
// keeps the tier gate out of the way so each test measures the ACL and only
// the ACL. Every actor's team role clears the read role, so anyone who gets in
// can genuinely read -- which is what makes "cleo is denied" meaningful.
func setupPrivScene(t *testing.T, skipChannel bool) *privScene {
	tew := testEnvBeta(t)
	alice := tew.NewTestUser(t)
	bob := tew.NewTestUser(t)
	cleo := tew.NewTestUser(t)
	dara := tew.NewTestUser(t)
	eddie := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)

	tm := tew.makeTeamForOwner(t, alice)
	m := tew.MetaContext()
	tm.makeChanges(t, m, alice,
		[]proto.MemberRole{
			bob.toMemberRole(t, proto.DefaultRole, tm.hepks),
			cleo.toMemberRole(t, proto.DefaultRole, tm.hepks),
			dara.toMemberRole(t, proto.AdminRole, tm.hepks),
			eddie.toMemberRole(t, proto.DefaultRole, tm.hepks),
		}, nil,
	)

	sc := &privScene{
		tew:   tew,
		tm:    tm,
		fqt:   tm.ToFQTeamParsed(t),
		alice: newPrivActor(t, tew, alice),
		bob:   newPrivActor(t, tew, bob),
		cleo:  newPrivActor(t, tew, cleo),
		dara:  newPrivActor(t, tew, dara),
		eddie: newPrivActor(t, tew, eddie),
	}
	teamID, err := tm.id.ToTeamID()
	require.NoError(t, err)
	sc.teamID = teamID

	if skipChannel {
		return sc
	}

	nm, err2 := core.RandomDomain()
	require.NoError(t, err2)
	sc.name = proto.RTChannelName("priv-" + nm)
	chid, err := sc.alice.minder.MakeChannelWithOpts(
		sc.alice.m, sc.teamCfg(), proto.RTAppID_Chat,
		sc.name, "the private channel",
		proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole},
		librt.MakeChannelOpts{Private: true}, nil,
	)
	require.NoError(t, err)
	sc.chid = *chid
	return sc
}

func (s *privScene) teamCfg() lcl.ConfigTeam { return team.WrapNamedPtr(s.fqt) }

// makePrivateAt creates an extra private channel at a chosen read/write role,
// so tests can cover the case where the channel's read role sits ABOVE the
// role of the person probing it (or above a team admin's).
func (s *privScene) makePrivateAt(
	t *testing.T, by *privActor, role proto.Role,
) (proto.RTChannelID, proto.RTChannelName) {
	nm, err := core.RandomDomain()
	require.NoError(t, err)
	name := proto.RTChannelName("hi-" + nm)
	chid, err := by.minder.MakeChannelWithOpts(
		by.m, s.teamCfg(), proto.RTAppID_Chat, name, "high read role",
		proto.RolePairOpt{Read: &role, Write: &role},
		librt.MakeChannelOpts{Private: true}, nil,
	)
	require.NoError(t, err)
	return *chid, name
}

func (s *privScene) spec() lcl.RTChannelSpecifier {
	return lcl.NewRTChannelSpecifierWithId(s.chid)
}

func (s *privScene) grant(t *testing.T, by *privActor, to *privActor, owner bool) {
	require.NoError(t, by.minder.GrantChannelMember(by.m, s.chid, to.u.uid, owner))
}

func (s *privScene) revoke(t *testing.T, by *privActor, from *privActor) {
	require.NoError(t, by.minder.RevokeChannelMember(by.m, s.chid, from.u.uid))
}

// send posts one message as `by` and returns its seq.
func (s *privScene) send(t *testing.T, by *privActor, body string) proto.RTMsgSeq {
	res, err := by.minder.Send(by.m, s.teamCfg(), proto.RTAppID_Chat, s.spec(), []byte(body))
	require.NoError(t, err)
	return res.Seq
}

// --- direct DB probes -------------------------------------------------------

func (s *privScene) rtdbCount(t *testing.T, q string, args ...any) int {
	m := s.tew.MetaContext()
	db, err := m.Db(shared.DbTypeRealTime)
	require.NoError(t, err)
	defer db.Release()
	var n int
	require.NoError(t, db.QueryRow(m.Ctx(), q, args...).Scan(&n))
	return n
}

// aclUIDs and deliveryUIDs are the two halves of the §4.2 invariant: for a
// private channel the ACL and the delivery rows must name exactly the same set
// of users.
func (s *privScene) rtdbUIDs(t *testing.T, q string) map[string]bool {
	m := s.tew.MetaContext()
	db, err := m.Db(shared.DbTypeRealTime)
	require.NoError(t, err)
	defer db.Release()
	rows, err := db.Query(m.Ctx(), q, m.ShortHostID(), s.chid.Short().Int64())
	require.NoError(t, err)
	defer rows.Close()
	ret := map[string]bool{}
	for rows.Next() {
		var raw []byte
		require.NoError(t, rows.Scan(&raw))
		var uid proto.UID
		require.NoError(t, uid.ImportFromDB(raw))
		ret[uid.EncodeHex()] = true
	}
	require.NoError(t, rows.Err())
	return ret
}

// channelTier reads a channel's stored tier straight from the database, so a
// test can assert which gate it is actually exercising rather than assert it in
// a comment that can drift.
func (s *privScene) channelTier(t *testing.T, id proto.RTChannelID) proto.RTChannelTier {
	m := s.tew.MetaContext()
	db, err := m.Db(shared.DbTypeRealTime)
	require.NoError(t, err)
	defer db.Release()
	var raw string
	var private bool
	require.NoError(t, db.QueryRow(m.Ctx(),
		`SELECT tier, private FROM channels WHERE short_host_id=$1 AND channel_id=$2`,
		m.ShortHostID(), id.Short().Int64()).Scan(&raw, &private))
	require.True(t, private)
	var tier proto.RTChannelTier
	require.NoError(t, tier.ImportFromDB(raw))
	return tier
}

func (s *privScene) aclUIDs(t *testing.T) map[string]bool {
	return s.rtdbUIDs(t,
		`SELECT uid FROM channel_acl WHERE short_host_id=$1 AND channel_id=$2`)
}

func (s *privScene) deliveryUIDs(t *testing.T) map[string]bool {
	return s.rtdbUIDs(t,
		`SELECT uid FROM user_channels WHERE short_host_id=$1 AND channel_id=$2`)
}

func (s *privScene) inboxVersion(t *testing.T, a *privActor) int64 {
	m := s.tew.MetaContext()
	db, err := m.Db(shared.DbTypeRealTime)
	require.NoError(t, err)
	defer db.Release()
	var v int64
	err = db.QueryRow(m.Ctx(),
		`SELECT inbox_version FROM user_inbox
		 WHERE short_host_id=$1 AND uid=$2 AND app_id='chat'`,
		m.ShortHostID(), a.u.uid.ExportToDB()).Scan(&v)
	if err != nil {
		return 0
	}
	return v
}

func (s *privScene) pushRows(t *testing.T, a *privActor) int {
	return s.rtdbCount(t,
		`SELECT count(*) FROM push_outbox WHERE short_host_id=$1 AND channel_id=$2 AND uid=$3`,
		s.tew.MetaContext().ShortHostID(), s.chid.Short().Int64(), a.u.uid.ExportToDB())
}

// requireHidden asserts the error is exactly the one a non-existent channel
// produces. Anything else -- a permission error, a different message -- tells
// the caller a private channel is there, which is the one thing the ACL is
// meant to prevent.
func requireHidden(t *testing.T, err error, what string) {
	require.Equal(t, core.RowNotFoundError{}, err,
		"%s: a non-member must get the missing-channel error, not a hint that "+
			"the channel exists", what)
}

// --- §8.1: one negative test per inventory row ------------------------------

// Row 1: rtGetThread.
func TestPrivateThreadDeniedToNonMember(t *testing.T) {
	sc := setupPrivScene(t, false)
	sc.grant(t, sc.alice, sc.bob, false)
	sc.send(t, sc.alice, "hello, the few of us")

	q := rem.RTThreadQuery{
		ChannelID: sc.chid,
		Bookends:  []rem.RTThreadRangeBookends{{Start: 1, End: 1}},
	}

	// The two members read it.
	for _, a := range []*privActor{sc.alice, sc.bob} {
		page, err := a.raw(t).RtGetThread(a.m.Ctx(), q)
		require.NoError(t, err)
		require.Len(t, page.RangeMsgs, 1)
		require.Len(t, page.RangeMsgs[0].Lst, 1)
	}

	// A team member who is not in the channel does not, and cannot tell the
	// difference between "private" and "no such channel". Nor can a team admin
	// who has not granted themselves in -- reading is not one of the powers
	// Q1/Q2 give admins; joining visibly is.
	for _, a := range []*privActor{sc.cleo, sc.dara} {
		_, err := a.raw(t).RtGetThread(a.m.Ctx(), q)
		requireHidden(t, err, "rtGetThread")
	}
}

// Row 2: rtGetThreadRecents.
func TestPrivateRecentsDeniedToNonMember(t *testing.T) {
	sc := setupPrivScene(t, false)
	sc.grant(t, sc.alice, sc.bob, false)
	sc.send(t, sc.alice, "recent things")

	arg := rem.RtGetThreadRecentsArg{Ch: sc.chid, Lim: 10}

	lst, err := sc.bob.raw(t).RtGetThreadRecents(sc.bob.m.Ctx(), arg)
	require.NoError(t, err)
	require.Len(t, lst.Lst, 1)

	for _, a := range []*privActor{sc.cleo, sc.dara} {
		_, err := a.raw(t).RtGetThreadRecents(a.m.Ctx(), arg)
		requireHidden(t, err, "rtGetThreadRecents")
	}
}

// Row 3: rtSend sender authorization. cleo's team role clears the channel's
// write role, so only the ACL stands between her and posting into it.
func TestPrivateSendDeniedToNonMember(t *testing.T) {
	sc := setupPrivScene(t, false)
	sc.grant(t, sc.alice, sc.bob, false)

	sc.send(t, sc.alice, "from the owner")
	sc.send(t, sc.bob, "from a member")

	// cleo cannot even name the channel through the wrapped client (it is not
	// in her list), so go at the RPC directly to prove the server refuses.
	_, err := sc.cleo.raw(t).RtSend(sc.cleo.m.Ctx(), rem.RTSendArg{Chid: sc.chid.Short()})
	requireHidden(t, err, "rtSend")

	require.Equal(t, 2, sc.rtdbCount(t,
		`SELECT count(*) FROM messages_enc WHERE short_host_id=$1 AND channel_id=$2`,
		sc.tew.MetaContext().ShortHostID(), sc.chid.Short().Int64()))
}

// Row 4: the send fan-out. Membership is the set of user_channels rows, which
// for a private channel is its ACL -- so a non-member's inbox must not move
// when the channel receives a message.
func TestPrivateFanoutOnlyMembers(t *testing.T) {
	sc := setupPrivScene(t, false)
	sc.grant(t, sc.alice, sc.bob, false)

	before := map[*privActor]int64{}
	for _, a := range []*privActor{sc.alice, sc.bob, sc.cleo, sc.dara} {
		before[a] = sc.inboxVersion(t, a)
	}
	sc.send(t, sc.alice, "tick")

	require.Greater(t, sc.inboxVersion(t, sc.bob), before[sc.bob],
		"a granted member must be delivered to")
	for _, a := range []*privActor{sc.cleo, sc.dara} {
		require.Equal(t, before[a], sc.inboxVersion(t, a),
			"a non-member's inbox version must not move on a send into a private channel")
	}
}

// Row 4/15: the push outbox. Push wakes are content-free, but one arriving at
// all tells a non-member the channel exists and is active.
func TestPrivatePushOutboxOnlyMembers(t *testing.T) {
	sc := setupPrivScene(t, false)
	sc.grant(t, sc.alice, sc.bob, false)
	sc.send(t, sc.alice, "wake the members")

	require.Equal(t, 1, sc.pushRows(t, sc.bob))
	require.Equal(t, 0, sc.pushRows(t, sc.alice), "the sender is never pushed to")
	for _, a := range []*privActor{sc.cleo, sc.dara, sc.eddie} {
		require.Equal(t, 0, sc.pushRows(t, a),
			"a non-member must not receive a push wake for a private channel")
	}
}

// Rows 5 and 14: rtListAllChannelsForTeam, including the last-sender join that
// rides on it.
func TestPrivateChannelInvisibleInList(t *testing.T) {
	sc := setupPrivScene(t, false)
	sc.grant(t, sc.alice, sc.bob, false)
	sc.send(t, sc.alice, "activity that must not leak")

	// A public channel in the same team, so "the list is empty" can never be
	// the reason a private channel appears to be hidden.
	_, err := sc.alice.minder.MakeChannel(sc.alice.m, sc.teamCfg(), proto.RTAppID_Chat,
		"town-square", "everyone",
		proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole})
	require.NoError(t, err)

	names := func(a *privActor) map[proto.RTChannelName]bool {
		lst, err := a.minder.ListAllChannelsForTeam(a.m, sc.teamCfg(), proto.RTAppID_Chat)
		require.NoError(t, err)
		ret := map[proto.RTChannelName]bool{}
		for _, ch := range lst.Channels {
			ret[ch.Name] = true
		}
		return ret
	}

	for _, a := range []*privActor{sc.alice, sc.bob} {
		got := names(a)
		require.True(t, got[sc.name], "a member must see the private channel listed")
		require.True(t, got["town-square"])
	}
	got := names(sc.cleo)
	require.False(t, got[sc.name],
		"a team member outside the ACL must not see a private channel in the listing")
	require.True(t, got["town-square"], "public channels are unaffected")

	// The raw wire tells the fuller story: cleo's response does not contain
	// the channel at all, so neither its name box nor its last-message
	// metadata reaches her.
	set, err := sc.cleo.raw(t).RtListAllChannelsForTeam(sc.cleo.m.Ctx(),
		rem.RtListAllChannelsForTeamArg{Team: sc.teamID, AppID: proto.RTAppID_Chat})
	require.NoError(t, err)
	for _, md := range set.Lst {
		require.False(t, md.Id.Eq(sc.chid),
			"the private channel must be absent from a non-member's raw channel set")
	}

	// Q1: a team admin MAY learn that a private channel exists -- private
	// channels are a leadership tool, and a model in which leaders are
	// invisible peers would make the product copy a lie. What they do not get
	// is its activity: no description, no last-message preview, marked
	// unreadable (which is why the wrapped client still hides it from dara's
	// inbox above the wire).
	set, err = sc.dara.raw(t).RtListAllChannelsForTeam(sc.dara.m.Ctx(),
		rem.RtListAllChannelsForTeamArg{Team: sc.teamID, AppID: proto.RTAppID_Chat})
	require.NoError(t, err)
	var seen bool
	for _, md := range set.Lst {
		if !md.Id.Eq(sc.chid) {
			continue
		}
		seen = true
		require.True(t, md.Private)
		require.True(t, md.Unreadable)
		require.Nil(t, md.LastMsg, "an admin outside the ACL must not get activity metadata")
		require.Nil(t, md.DescBox)
	}
	require.True(t, seen, "a team admin may see that a private channel exists (Q1)")
}

// Row 6: rtGetChannel. Unimplemented upstream and here; the point of this test
// is that if someone implements it, they must revisit this file -- and the
// source guard in server/realtime fails the build if they reach `channels`
// outside the chokepoint.
func TestPrivateGetChannelDeniedToNonMember(t *testing.T) {
	sc := setupPrivScene(t, false)
	_, err := sc.cleo.raw(t).RtGetChannel(sc.cleo.m.Ctx(), sc.chid)
	require.Equal(t, core.NotImplementedError{}, err,
		"rtGetChannel is still a stub; when it is implemented it must go "+
			"through authorizeChannel and this test must assert RowNotFound "+
			"for a non-member")
}

// Row 7: rtGetChangedThreads, the path that must not be fooled by a lingering
// user_channels row.
func TestPrivateChangedThreadsAfterRevoke(t *testing.T) {
	sc := setupPrivScene(t, false)
	sc.grant(t, sc.alice, sc.bob, false)
	sc.send(t, sc.alice, "before the revoke")

	hasChannel := func(a *privActor) bool {
		delta, err := a.minder.GetChangedThreads(a.m, proto.RTAppID_Chat, 0, 0)
		require.NoError(t, err)
		for _, ch := range delta.Channels {
			if ch.Md.Id.Eq(sc.chid) {
				return true
			}
		}
		return false
	}

	require.True(t, hasChannel(sc.bob), "a granted member syncs the channel")
	require.False(t, hasChannel(sc.cleo), "a non-member never sees it in a sync")

	sc.revoke(t, sc.alice, sc.bob)
	require.False(t, hasChannel(sc.bob),
		"after a revoke the channel is absent from a full sync, which is how "+
			"the client learns to drop it")

	// Belt and braces: even if a delivery row somehow survived, the sync
	// re-checks the ACL. Put one back by hand and confirm the row alone does
	// not resurrect the channel.
	m := sc.tew.MetaContext()
	db, err := m.Db(shared.DbTypeRealTime)
	require.NoError(t, err)
	_, err = db.Exec(m.Ctx(),
		`INSERT INTO user_channels
		   (short_host_id, channel_id, uid, app_id, inbox_version,
		    last_msg_time, earliest_msg_time, read_through, hidden, muted, ctime, mtime)
		 VALUES ($1, $2, $3, 'chat', 999999, NOW(), NULL, 0, false, false, NOW(), NOW())`,
		m.ShortHostID(), sc.chid.Short().Int64(), sc.bob.u.uid.ExportToDB())
	db.Release()
	require.NoError(t, err)
	require.False(t, hasChannel(sc.bob),
		"a lingering user_channels row must not leak a private channel: "+
			"channel_acl is the authority")
}

// Row 9: the late-join fan-in, which backfills delivery rows for channels a
// user's team role can read. A private channel must be excluded outright --
// team role is not a way in.
func TestPrivateNotFannedInOnJoin(t *testing.T) {
	sc := setupPrivScene(t, true)

	// frank joins the team AFTER the private channel exists, which is exactly
	// the case the fan-in was built for.
	nm, err := core.RandomDomain()
	require.NoError(t, err)
	sc.name = proto.RTChannelName("priv-" + nm)
	chid, err := sc.alice.minder.MakeChannelWithOpts(
		sc.alice.m, sc.teamCfg(), proto.RTAppID_Chat, sc.name, "private",
		proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole},
		librt.MakeChannelOpts{Private: true}, nil)
	require.NoError(t, err)
	sc.chid = *chid

	_, err = sc.alice.minder.MakeChannel(sc.alice.m, sc.teamCfg(), proto.RTAppID_Chat,
		"open-house", "public", proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole})
	require.NoError(t, err)

	frank := sc.tew.NewTestUser(t)
	sc.tew.DirectDoubleMerklePokeInTest(t)
	sc.tm.makeChanges(t, sc.tew.MetaContext(), sc.alice.u,
		[]proto.MemberRole{frank.toMemberRole(t, proto.DefaultRole, sc.tm.hepks)}, nil)
	fa := newPrivActor(t, sc.tew, frank)

	// The sync runs the fan-in. frank picks up the public channel and only the
	// public channel.
	delta, err := fa.minder.GetChangedThreads(fa.m, proto.RTAppID_Chat, 0, 0)
	require.NoError(t, err)
	var sawPublic bool
	for _, ch := range delta.Channels {
		require.False(t, ch.Md.Id.Eq(sc.chid),
			"the fan-in must not add a late joiner to a private channel")
		sawPublic = true
	}
	require.True(t, sawPublic, "the fan-in must still backfill public channels")

	require.False(t, sc.deliveryUIDs(t)[frank.uid.EncodeHex()],
		"no delivery row for a late joiner in a private channel")
	require.False(t, sc.aclUIDs(t)[frank.uid.EncodeHex()])
}

// Row 10: rtReadThrough.
func TestPrivateReadThroughAfterRevoke(t *testing.T) {
	sc := setupPrivScene(t, false)
	sc.grant(t, sc.alice, sc.bob, false)
	seq := sc.send(t, sc.alice, "mark me")

	arg := rem.RTReadThroughArg{ChannelID: sc.chid, Seq: seq}
	require.NoError(t, sc.bob.raw(t).RtReadThrough(sc.bob.m.Ctx(), arg))

	// A non-member's mark is refused at the ACL, with the missing-channel
	// error -- not with "you have no membership row", which would confirm the
	// channel is real.
	requireHidden(t, sc.cleo.raw(t).RtReadThrough(sc.cleo.m.Ctx(), arg), "rtReadThrough")

	sc.revoke(t, sc.alice, sc.bob)
	requireHidden(t, sc.bob.raw(t).RtReadThrough(sc.bob.m.Ctx(), arg), "rtReadThrough after revoke")
}

// Row 11a: creation is admins/leaders only (Q2b), enforced server-side rather
// than only in the UI.
func TestPrivateCreateRequiresAdmin(t *testing.T) {
	sc := setupPrivScene(t, true)

	mk := func(a *privActor, nm proto.RTChannelName) error {
		_, err := a.minder.MakeChannelWithOpts(a.m, sc.teamCfg(), proto.RTAppID_Chat,
			nm, "private",
			proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole},
			librt.MakeChannelOpts{Private: true}, nil)
		return err
	}

	err := mk(sc.cleo, "cleo-secret")
	require.True(t, core.IsPermissionError(err),
		"an ordinary member must not be able to create a private channel; got %v", err)

	// The owner and an admin can.
	require.NoError(t, mk(sc.alice, "alice-secret"))
	require.NoError(t, mk(sc.dara, "dara-secret"))

	// And cleo can still make an ordinary channel: the rule is about privacy,
	// not about creation.
	_, err = sc.cleo.minder.MakeChannel(sc.cleo.m, sc.teamCfg(), proto.RTAppID_Chat,
		"cleo-open", "public",
		proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole})
	require.NoError(t, err)
}

// Row 11b: creation fans out to the creator alone. The ordinary path fans out
// to every team member at the read role; doing that for a private channel
// would hand the whole team a delivery row on day one.
func TestPrivateCreateFansOutToCreatorOnly(t *testing.T) {
	sc := setupPrivScene(t, false)

	require.Equal(t,
		map[string]bool{sc.alice.u.uid.EncodeHex(): true},
		sc.deliveryUIDs(t),
		"a fresh private channel is delivered to its creator and nobody else")
	require.Equal(t,
		map[string]bool{sc.alice.u.uid.EncodeHex(): true},
		sc.aclUIDs(t))

	// The creator is seeded as an ACL OWNER, which is what lets them grant
	// without being a team admin afterwards.
	members, err := sc.alice.minder.ChannelMembers(sc.alice.m, sc.chid)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.True(t, members[0].Owner)
	require.Equal(t, sc.alice.u.uid, members[0].Uid)
	require.Equal(t, sc.alice.u.uid, members[0].GrantedBy)
}

// Row 12: grant/revoke/members authority.
func TestPrivateGrantRequiresOwnerOrAdmin(t *testing.T) {
	sc := setupPrivScene(t, false)
	sc.grant(t, sc.alice, sc.bob, false)

	// A plain channel member cannot grant: they are in the channel, so they
	// get a permission error rather than the missing-channel error -- there is
	// nothing left to hide from them.
	err := sc.bob.minder.GrantChannelMember(sc.bob.m, sc.chid, sc.cleo.u.uid, false)
	require.True(t, core.IsPermissionError(err), "got %v", err)

	// A team member outside the channel gets the missing-channel error
	// instead: telling her she lacks permission would confirm the channel.
	err = sc.cleo.minder.GrantChannelMember(sc.cleo.m, sc.chid, sc.cleo.u.uid, false)
	requireHidden(t, err, "rtChannelGrant by a non-member")

	// A team admin may grant themselves in (Q2) -- and every member sees who
	// let them in, which is what makes leaders transparent peers rather than
	// invisible ones (§6.1).
	require.NoError(t, sc.dara.minder.GrantChannelMember(sc.dara.m, sc.chid, sc.dara.u.uid, false))
	members, err := sc.bob.minder.ChannelMembers(sc.bob.m, sc.chid)
	require.NoError(t, err)
	byUID := map[proto.UID]rem.RTChannelAclEntry{}
	for _, e := range members {
		byUID[e.Uid] = e
	}
	require.Len(t, byUID, 3)
	require.Equal(t, sc.dara.u.uid, byUID[sc.dara.u.uid].GrantedBy,
		"a self-grant is recorded as such, and members can see it")
	require.Equal(t, sc.alice.u.uid, byUID[sc.bob.u.uid].GrantedBy)

	// The ACL is not readable by someone outside it.
	_, err = sc.cleo.minder.ChannelMembers(sc.cleo.m, sc.chid)
	requireHidden(t, err, "rtChannelMembers by a non-member")

	// An owner may promote a member to owner, after which they can grant.
	sc.grant(t, sc.alice, sc.bob, true)
	require.NoError(t, sc.bob.minder.GrantChannelMember(sc.bob.m, sc.chid, sc.cleo.u.uid, false))

	// A grantee must be a member of the parent team who can read the channel;
	// granting an outsider would put the ACL and the delivery set out of step
	// and hand a row to someone who could not decrypt anything anyway.
	outsider := sc.tew.NewTestUser(t)
	sc.tew.DirectDoubleMerklePokeInTest(t)
	err = sc.alice.minder.GrantChannelMember(sc.alice.m, sc.chid, outsider.uid, false)
	require.True(t, core.IsPermissionError(err), "got %v", err)
}

// TestPrivateGrantVisibleToWarmCache is a regression test for the one thing
// every actor above is structurally blind to: they run with caching disabled,
// so every channel listing they make is a full sync.
//
// A real client is not. It sends its cached channel-set version as `last`, the
// server short-circuits to an empty list when that matches, and the listing
// filters on `updated_at_set_vers > last`. A grant that touched only the
// grantee's inbox version would therefore never reach a warm client's channel
// list -- and librt resolves channel names against exactly that list, so the
// grantee could not send to or read the channel by name at all.
func TestPrivateGrantVisibleToWarmCache(t *testing.T) {
	sc := setupPrivScene(t, false)

	// bob with a REAL cache, warmed before the grant. The public channel gives
	// the warm-up something to find, so a later empty list is about the grant
	// and not about an empty team.
	_, err := sc.alice.minder.MakeChannel(sc.alice.m, sc.teamCfg(), proto.RTAppID_Chat,
		"lobby", "public", proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole})
	require.NoError(t, err)

	warm := librt.NewMinderWithCacheSettings(sc.bob.m.G().ActiveUser(),
		libclient.CacheSettings{UseMem: true, UseDisk: true})
	lst, err := warm.ListAllChannelsForTeam(sc.bob.m, sc.teamCfg(), proto.RTAppID_Chat)
	require.NoError(t, err)
	for _, ch := range lst.Channels {
		require.NotEqual(t, sc.name, ch.Name, "precondition: bob is not in the channel yet")
	}

	sc.grant(t, sc.alice, sc.bob, false)

	lst, err = warm.ListAllChannelsForTeam(sc.bob.m, sc.teamCfg(), proto.RTAppID_Chat)
	require.NoError(t, err)
	var found bool
	for _, ch := range lst.Channels {
		if ch.Name == sc.name {
			found = true
		}
	}
	require.True(t, found,
		"a grant must reach a client whose channel-set cache is already warm, "+
			"or the grantee can never address the channel by name")

	// And the end-to-end consequence: bob can now send and read by name.
	_, err = warm.Send(sc.bob.m, sc.teamCfg(), proto.RTAppID_Chat, sc.spec(), []byte("hello"))
	require.NoError(t, err)
	msgs, _, err := warm.GetThreadBookended(sc.bob.m, sc.teamCfg(), proto.RTAppID_Chat,
		makeChannelSpecifier(sc.name), 1, 1)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, "hello", string(msgs[0].Body))
}

// --- §8.2: invariants -------------------------------------------------------

// TestPrivateAclEqualsUserChannels is the invariant every other path leans on:
// for a private channel, "may read" and "is delivered to" are the same set.
// The moment they diverge, either a member stops receiving messages or a
// non-member starts.
func TestPrivateAclEqualsUserChannels(t *testing.T) {
	sc := setupPrivScene(t, false)
	same := func(stage string) {
		require.Equal(t, sc.aclUIDs(t), sc.deliveryUIDs(t),
			"channel_acl and user_channels must name the same users (%s)", stage)
	}
	same("after create")

	sc.grant(t, sc.alice, sc.bob, false)
	sc.grant(t, sc.alice, sc.cleo, false)
	sc.grant(t, sc.alice, sc.eddie, false)
	same("after grants")

	sc.revoke(t, sc.alice, sc.cleo)
	same("after a revoke")

	// Team-leave cascade (§6.3): removing eddie from the team leaves his rows
	// behind -- there is no un-fanout -- until the next send prunes them. The
	// outer gate already denies him every read; this is about delivery.
	sc.tm.makeChanges(t, sc.tew.MetaContext(), sc.alice.u,
		[]proto.MemberRole{
			sc.eddie.u.toMemberRole(t, proto.NewRoleDefault(proto.RoleType_NONE), nil),
		}, nil)
	require.True(t, sc.deliveryUIDs(t)[sc.eddie.u.uid.EncodeHex()],
		"precondition: a team leave does not itself remove the delivery row")

	before := sc.inboxVersion(t, sc.eddie)
	sc.send(t, sc.alice, "the message that prunes")

	require.False(t, sc.aclUIDs(t)[sc.eddie.u.uid.EncodeHex()],
		"an ex-team-member is pruned from the ACL by the next send")
	require.False(t, sc.deliveryUIDs(t)[sc.eddie.u.uid.EncodeHex()])
	same("after the team-leave prune")
	require.Equal(t, 0, sc.pushRows(t, sc.eddie),
		"an ex-team-member gets no push wake from the send that prunes them")
	// The prune bumps his inbox once (so his client drops the channel); what
	// it must not do is deliver the message to him.
	require.GreaterOrEqual(t, sc.inboxVersion(t, sc.eddie), before)

	// Re-admitting him to the team does NOT put him back in the channel, BECAUSE
	// the send above already pruned him: the rows are gone and a re-grant has to
	// be explicit. (The order matters -- see the test below for what happens
	// when no send intervenes.)
	sc.tm.makeChanges(t, sc.tew.MetaContext(), sc.alice.u,
		[]proto.MemberRole{sc.eddie.u.toMemberRole(t, proto.DefaultRole, sc.tm.hepks)}, nil)
	_, err := sc.eddie.minder.GetChangedThreads(sc.eddie.m, proto.RTAppID_Chat, 0, 0)
	require.NoError(t, err)
	require.False(t, sc.aclUIDs(t)[sc.eddie.u.uid.EncodeHex()],
		"re-joining the team after a prune must not silently re-join the channel")
	same("after re-admission to the team")
}

// TestPrivateReadmissionBeforeAnySendKeepsMembership pins the LIMIT of the
// team-leave cascade, so the guarantee in §6.3 is the one the code actually
// gives rather than the one it would be nice to have.
//
// Pruning happens at send time and nothing else removes the rows, so a member
// removed from the team and re-admitted before the channel's next message was
// never pruned and is still a member. That is a real gap -- closing it needs an
// eager cross-database cascade on team removal, which §7.2 rejects -- but it is
// bounded: while they are out, every read is denied against the current roster,
// so membership survives without any access surviving with it.
func TestPrivateReadmissionBeforeAnySendKeepsMembership(t *testing.T) {
	sc := setupPrivScene(t, false)
	sc.grant(t, sc.alice, sc.eddie, false)
	require.True(t, sc.aclUIDs(t)[sc.eddie.u.uid.EncodeHex()])

	// Out of the team, with no message sent in between.
	sc.tm.makeChanges(t, sc.tew.MetaContext(), sc.alice.u,
		[]proto.MemberRole{
			sc.eddie.u.toMemberRole(t, proto.NewRoleDefault(proto.RoleType_NONE), nil),
		}, nil)

	// While out, he reads nothing -- the outer gate is what carries the
	// guarantee here, not the ACL.
	_, err := sc.eddie.raw(t).RtGetThreadRecents(sc.eddie.m.Ctx(),
		rem.RtGetThreadRecentsArg{Ch: sc.chid, Lim: 10})
	require.Error(t, err, "an ex-team-member must read nothing, ACL row or not")

	// Back in the team, still with no send having occurred.
	sc.tm.makeChanges(t, sc.tew.MetaContext(), sc.alice.u,
		[]proto.MemberRole{sc.eddie.u.toMemberRole(t, proto.DefaultRole, sc.tm.hepks)}, nil)

	require.True(t, sc.aclUIDs(t)[sc.eddie.u.uid.EncodeHex()],
		"documented v1 behaviour: with no send to prune them, the ACL row "+
			"survives a team round trip and membership comes back without a "+
			"re-grant (§6.3). If this ever starts failing, the cascade became "+
			"eager -- update §6.3, which currently promises only the post-prune "+
			"guarantee.")
	require.Equal(t, sc.aclUIDs(t), sc.deliveryUIDs(t))
}

// TestPrivateNoInboxBumpForNonMember: a non-member's inbox version and push
// rows must be untouched across a run of sends into a private channel. An
// inbox bump carries no content, but a stream of them is a readable signal
// that a channel exists and who is busy in it.
func TestPrivateNoInboxBumpForNonMember(t *testing.T) {
	sc := setupPrivScene(t, false)
	sc.grant(t, sc.alice, sc.bob, false)

	before := sc.inboxVersion(t, sc.cleo)
	for i := range 5 {
		sc.send(t, sc.alice, fmt.Sprintf("msg-%d", i))
	}
	require.Equal(t, before, sc.inboxVersion(t, sc.cleo))
	require.Equal(t, 0, sc.pushRows(t, sc.cleo))
	require.Equal(t, 5, sc.pushRows(t, sc.bob))
}

// TestPrivateSameErrorAsMissingChannel: the error a non-member gets for a
// private channel must be indistinguishable from the error for a channel id
// that was never issued. Otherwise the ACL hides the contents but advertises
// the container.
func TestPrivateSameErrorAsMissingChannel(t *testing.T) {
	sc := setupPrivScene(t, false)

	missing, err := proto.NewRTChannelID()
	require.NoError(t, err)

	q := func(id proto.RTChannelID) rem.RTThreadQuery {
		return rem.RTThreadQuery{
			ChannelID: id,
			Bookends:  []rem.RTThreadRangeBookends{{Start: 1, End: 1}},
		}
	}
	_, errPrivate := sc.cleo.raw(t).RtGetThread(sc.cleo.m.Ctx(), q(sc.chid))
	_, errMissingThread := sc.cleo.raw(t).RtGetThread(sc.cleo.m.Ctx(), q(*missing))
	require.Error(t, errPrivate)
	require.Equal(t, errMissingThread, errPrivate,
		"a private channel must be indistinguishable from one that does not exist")
	errMissing := errMissingThread

	errPrivate = sc.cleo.raw(t).RtReadThrough(sc.cleo.m.Ctx(),
		rem.RTReadThroughArg{ChannelID: sc.chid, Seq: 1})
	errMissing = sc.cleo.raw(t).RtReadThrough(sc.cleo.m.Ctx(),
		rem.RTReadThroughArg{ChannelID: *missing, Seq: 1})
	require.Error(t, errPrivate)
	require.Equal(t, errMissing, errPrivate)

	// The case above is the easy one: cleo's team role clears the channel's
	// read role, so only the ACL stands between her and it. The dangerous case
	// is a private channel whose read role sits ABOVE her -- there, a role gate
	// running before the ACL check would answer PermissionError, and
	// "permission denied" is itself a disclosure: it says a channel exists at
	// this id, which is the one thing a private channel must not admit.
	highID, _ := sc.makePrivateAt(t, sc.alice, proto.OwnerRole)

	// One channel covers both gates that could answer ahead of the ACL check.
	// A read role of Owner puts it above cleo (the role gate), and the tier is
	// DERIVED from the read role -- anything admin-or-above lands in the Admin
	// tier -- so it is admin-tier too, and cleo is below that (the tier gate).
	// Asserted rather than asserted-in-prose: if channel creation ever stops
	// deriving the tier this way, this test should say so instead of quietly
	// covering one gate while claiming two.
	require.Equal(t, proto.RTChannelTier_Admin, sc.channelTier(t, highID),
		"expected an admin-tier channel, so this case exercises the tier gate "+
			"and the role gate together")

	_, errPrivate = sc.cleo.raw(t).RtGetThread(sc.cleo.m.Ctx(), q(highID))
	require.Equal(t, errMissingThread, errPrivate,
		"a private channel whose read role and tier are both above the caller "+
			"must still be indistinguishable from one that does not exist")

	// The same channel through the recents path: what is new here is the RPC,
	// not the channel. Both thread reads share getThreadGeneric, so this pins
	// the second entry point to the same answer.
	_, errPrivate = sc.cleo.raw(t).RtGetThreadRecents(sc.cleo.m.Ctx(),
		rem.RtGetThreadRecentsArg{Ch: highID, Lim: 10})
	_, errMissingRecents := sc.cleo.raw(t).RtGetThreadRecents(sc.cleo.m.Ctx(),
		rem.RtGetThreadRecentsArg{Ch: *missing, Lim: 10})
	require.Error(t, errPrivate)
	require.Equal(t, errMissingRecents, errPrivate)
}

// TestPrivateAdminManagesAboveOwnReadRole covers the authority §6.1 gives team
// admins over private channels they cannot themselves read.
//
// A private channel whose read role is Owner is exactly the kind most likely
// to need moderating, and an admin sits below its read role. Gating management
// on readability would silently withdraw the documented power precisely there.
func TestPrivateAdminManagesAboveOwnReadRole(t *testing.T) {
	sc := setupPrivScene(t, true)

	// alice (owner) makes a private channel readable only at Owner, and puts
	// nobody else in it. dara is a team admin: below the read role.
	highID, _ := sc.makePrivateAt(t, sc.alice, proto.OwnerRole)

	// dara can see who is in it and can revoke, without being able to read it.
	members, err := sc.dara.minder.ChannelMembers(sc.dara.m, highID)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.Equal(t, sc.alice.u.uid, members[0].Uid)

	require.NoError(t, sc.dara.minder.RevokeChannelMember(sc.dara.m, highID, sc.alice.u.uid),
		"a team admin must be able to moderate a private channel whose read "+
			"role is above their own")

	// But management is not a way in: dara still cannot grant a reader who
	// does not clear the read role -- including herself.
	err = sc.dara.minder.GrantChannelMember(sc.dara.m, highID, sc.dara.u.uid, false)
	require.True(t, core.IsPermissionError(err),
		"management must not let an admin smuggle in a reader below the read role; got %v", err)

	// And she still cannot read it.
	_, err = sc.dara.raw(t).RtGetThreadRecents(sc.dara.m.Ctx(),
		rem.RtGetThreadRecentsArg{Ch: highID, Lim: 10})
	require.Error(t, err, "managing a channel is not reading it")
}
