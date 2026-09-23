package agent

import (
	"context"
	"strings"

	"github.com/foks-proj/go-foks/client/librt"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
)

// rtTeamRoster reads the team's roster for the ACL RPCs below, which resolve
// between usernames and UIDs through it. The roster read is team-mediated
// (the same one the app's member list uses), so it works under closed
// viewership, where a direct user load of another member would not.
func (c *AgentConn) rtTeamRoster(
	ctx context.Context,
	cfg lcl.RTConfig,
) (
	*lcl.TeamRoster,
	error,
) {
	t, err := cfg.Team.GetT()
	if err != nil {
		return nil, err
	}
	if t != lcl.ConfigTeamType_Named {
		return nil, core.BadArgsError("private channels need a named team")
	}
	m, tm, err := c.teamInit(ctx)
	if err != nil {
		return nil, err
	}
	return tm.ListTeamRoster(m, cfg.Team.Named())
}

// rtRosterUID resolves `name@host` to the matching roster member's UID.
// Matching is by username, case-insensitively, the spelling the app's roster
// rows carry; the host part is ignored beyond parsing, because a named
// team's roster is single-host in our deployment and the roster is already
// scoped to the team.
func rtRosterUID(roster *lcl.TeamRoster, fqu proto.FQUserString) (proto.UID, error) {
	var zed proto.UID
	name := string(fqu)
	if i := strings.IndexByte(name, '@'); i >= 0 {
		name = name[:i]
	}
	if name == "" {
		return zed, core.BadArgsError("empty username")
	}
	for i := range roster.Members {
		mem := &roster.Members[i].Mem
		if !strings.EqualFold(string(mem.Name), name) {
			continue
		}
		if !mem.Fqp.Party.IsUser() {
			continue
		}
		return mem.Fqp.Party.UID()
	}
	return zed, core.NotFoundError("user is not on the team roster")
}

// ClientRTChannelGrant adds a team member to a private channel's access
// list. The server enforces who may (a channel owner or a team admin) and
// that the grantee clears the channel's read role.
func (c *AgentConn) ClientRTChannelGrant(
	ctx context.Context,
	arg lcl.ClientRTChannelGrantArg,
) error {
	roster, err := c.rtTeamRoster(ctx, arg.Cfg)
	if err != nil {
		return err
	}
	uid, err := rtRosterUID(roster, arg.FqUser)
	if err != nil {
		return err
	}
	m, minder, err := c.rtInit(ctx, arg.Cfg)
	if err != nil {
		return err
	}
	return minder.GrantChannelMemberIn(
		m, arg.Cfg.Team, arg.Cfg.AppID, arg.Cfg.Channel, uid, false)
}

// ClientRTChannelRevoke removes a team member from a private channel's
// access list. Revoking the caller's own user is Leave, which the server
// allows for any member of the list.
func (c *AgentConn) ClientRTChannelRevoke(
	ctx context.Context,
	arg lcl.ClientRTChannelRevokeArg,
) error {
	roster, err := c.rtTeamRoster(ctx, arg.Cfg)
	if err != nil {
		return err
	}
	uid, err := rtRosterUID(roster, arg.FqUser)
	if err != nil {
		return err
	}
	m, minder, err := c.rtInit(ctx, arg.Cfg)
	if err != nil {
		return err
	}
	return minder.RevokeChannelMemberIn(
		m, arg.Cfg.Team, arg.Cfg.AppID, arg.Cfg.Channel, uid)
}

// ClientRTChannelMembers lists a private channel's access list, with uids
// resolved to usernames via the roster. An entry whose user has left the
// team resolves to an empty name (the server prunes such rows lazily);
// callers drop those.
func (c *AgentConn) ClientRTChannelMembers(
	ctx context.Context,
	arg lcl.RTConfig,
) (
	[]lcl.RTChannelMember,
	error,
) {
	roster, err := c.rtTeamRoster(ctx, arg)
	if err != nil {
		return nil, err
	}
	names := make(map[proto.UID]proto.NameUtf8, len(roster.Members))
	for i := range roster.Members {
		mem := &roster.Members[i].Mem
		if !mem.Fqp.Party.IsUser() {
			continue
		}
		uid, err := mem.Fqp.Party.UID()
		if err != nil {
			continue
		}
		names[uid] = mem.Name
	}
	m, minder, err := c.rtInit(ctx, arg)
	if err != nil {
		return nil, err
	}
	entries, err := minder.ChannelMembersIn(m, arg.Team, arg.AppID, arg.Channel)
	if err != nil {
		return nil, err
	}
	ret := make([]lcl.RTChannelMember, 0, len(entries))
	for _, e := range entries {
		ret = append(ret, lcl.RTChannelMember{
			Uid:           e.Uid,
			Name:          names[e.Uid],
			Owner:         e.Owner,
			GrantedByName: names[e.GrantedBy],
			Ctime:         e.Ctime,
		})
	}
	return ret, nil
}

func (c *AgentConn) rtInit(
	ctx context.Context,
	cfg lcl.RTConfig,
) (
	librt.MetaContext,
	*librt.Minder,
	error,
) {
	m := librt.NewMetaContext(c.MetaContext(ctx))
	ret, err := librt.InitReq(m, cfg.Team)
	if err != nil {
		return m, nil, err
	}
	return m, ret, err
}

func (c *AgentConn) ClientRTMakeChannel(
	ctx context.Context,
	arg lcl.ClientRTMakeChannelArg,
) (
	proto.RTChannelID,
	error,
) {
	var zed proto.RTChannelID
	m, minder, err := c.rtInit(ctx, arg.Cfg)
	if err != nil {
		return zed, err
	}
	t, err := arg.Cfg.Channel.GetT()
	if err != nil {
		return zed, err
	}
	if t != lcl.RTChannelSpecifierType_Name {
		return zed, core.BadArgsError("expected a channel name")
	}
	nm := arg.Cfg.Channel.Name().Name
	chid, err := minder.MakeChannelWithOpts(
		m,
		arg.Cfg.Team,
		arg.Cfg.AppID,
		nm,
		arg.Desc,
		arg.Cfg.Roles,
		librt.MakeChannelOpts{NoPush: arg.NoPush, Private: arg.Private},
		nil,
	)
	if err != nil {
		return zed, err
	}
	return *chid, nil
}

func (c *AgentConn) ClientRTListChannelsForTeam(
	ctx context.Context,
	arg lcl.RTConfig,
) (
	lcl.RTChannelSetForTeam,
	error,
) {
	var zed lcl.RTChannelSetForTeam
	m, minder, err := c.rtInit(ctx, arg)
	if err != nil {
		return zed, err
	}
	lst, err := minder.ListAllChannelsForTeam(m, arg.Team, arg.AppID)
	if err != nil {
		return zed, err
	}
	return *lst, nil
}

func (c *AgentConn) ClientRTSend(
	ctx context.Context,
	arg lcl.ClientRTSendArg,
) (
	proto.RTMsgSeq,
	error,
) {
	var zed proto.RTMsgSeq
	m, minder, err := c.rtInit(ctx, arg.Cfg)
	if err != nil {
		return zed, err
	}
	res, err := minder.Send(m, arg.Cfg.Team, arg.Cfg.AppID, arg.Cfg.Channel, arg.Body)
	if err != nil {
		return zed, err
	}
	return res.Seq, nil
}

func (c *AgentConn) ClientRTGetThread(
	ctx context.Context,
	arg lcl.ClientRTGetThreadArg,
) (
	lcl.RTThreadView,
	error,
) {
	var zed lcl.RTThreadView
	m, minder, err := c.rtInit(ctx, arg.Cfg)
	if err != nil {
		return zed, err
	}
	view, err := minder.GetThreadView(m, arg.Cfg.Team, arg.Cfg.AppID, arg.Cfg.Channel, arg.Before, uint(arg.Num))
	if err != nil {
		return zed, err
	}
	return *view, nil
}

func (c *AgentConn) ClientRTInboxView(
	ctx context.Context,
	arg lcl.ClientRTInboxViewArg,
) (
	lcl.RTInboxView,
	error,
) {
	var zed lcl.RTInboxView
	m := librt.NewMetaContext(c.MetaContext(ctx))
	minder, err := librt.InitUserReq(m)
	if err != nil {
		return zed, err
	}
	if !arg.LocalOnly {
		_, err = minder.SyncInbox(m, arg.AppID)
		if err != nil {
			return zed, err
		}
	}
	view, err := minder.LocalInbox(m, arg.AppID)
	if err != nil {
		return zed, err
	}
	return *view, nil
}

// ClientRTUpdateChannel renames a channel and replaces its description; an
// empty desc clears it. Admin-only, enforced by the server.
func (c *AgentConn) ClientRTUpdateChannel(
	ctx context.Context,
	arg lcl.ClientRTUpdateChannelArg,
) error {
	m, minder, err := c.rtInit(ctx, arg.Cfg)
	if err != nil {
		return err
	}
	return minder.UpdateChannel(
		m, arg.Cfg.Team, arg.Cfg.AppID, arg.Cfg.Channel, arg.Name, arg.Desc)
}

// ClientRTSetChannelArchived archives or unarchives a channel. Admin-only,
// enforced by the server.
func (c *AgentConn) ClientRTSetChannelArchived(
	ctx context.Context,
	arg lcl.ClientRTSetChannelArchivedArg,
) error {
	m, minder, err := c.rtInit(ctx, arg.Cfg)
	if err != nil {
		return err
	}
	return minder.SetChannelArchived(
		m, arg.Cfg.Team, arg.Cfg.AppID, arg.Cfg.Channel, arg.Archived)
}

var _ lcl.RealTimeInterface = (*AgentConn)(nil)
