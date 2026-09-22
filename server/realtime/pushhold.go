package realtime

// Delegated push release (fork-only; push holds, see p7.sql/p8.sql).
//
// A team admin may place a push hold on a (team, app) and becomes its holder.
// While the holder is an active team member, a send into any channel of the
// team that the holder can read writes its push_outbox rows as 'held' instead
// of 'queued'. The relay never claims a held row; only the holder decides it,
// through ReleasePushes (queue, or delete) -- or the hold ends and every row it
// kept back is released unfiltered.
//
// The hold carries nothing but who holds it: the holder brings its own reasons
// for keeping a member's push back, and the server never learns them.

import (
	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/jackc/pgx/v5"
)

const (
	pushStatusQueued = "queued"
	pushStatusHeld   = "held"

	// maxPushHandleLen bounds the opaque handle a notify row may carry. It
	// travels to the push provider as-is, so it stays small.
	maxPushHandleLen = 32
	// maxPushNotifyEntries bounds one notify call: a channel's worth of
	// members.
	maxPushNotifyEntries = 10000
)

// readPushHolder returns the (team, app) hold's holder, or nil for no hold.
func readPushHolder(
	m shared.MetaContext,
	db shared.Querier,
	team proto.TeamID,
	appDB string,
) (
	*proto.UID,
	error,
) {
	var raw []byte
	err := db.QueryRow(
		m.Ctx(),
		`SELECT holder_uid FROM push_holds
		 WHERE short_host_id=$1 AND team_id=$2 AND app_id=$3`,
		m.ShortHostID(),
		team.ExportToDB(),
		appDB,
	).Scan(&raw)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var uid proto.UID
	if err = uid.ImportFromDB(raw); err != nil {
		return nil, err
	}
	return &uid, nil
}

// releaseTeamHeld queues every held row of the team's channels in the app,
// unfiltered: what a hold kept back when the hold ends.
func releaseTeamHeld(m shared.MetaContext, tx pgx.Tx, team proto.TeamID, appDB string) error {
	_, err := tx.Exec(
		m.Ctx(),
		`UPDATE push_outbox SET status='queued', mtime=NOW()
		 WHERE short_host_id=$1 AND status='held'
		   AND channel_id IN (
		     SELECT channel_id FROM channels
		     WHERE short_host_id=$1 AND parent_team_id=$2 AND app_id=$3)`,
		m.ShortHostID(),
		team.ExportToDB(),
		appDB,
	)
	return err
}

// releaseChannelHeld queues every held row of one channel, unfiltered.
func releaseChannelHeld(m shared.MetaContext, tx pgx.Tx, channelID int64) error {
	_, err := tx.Exec(
		m.Ctx(),
		`UPDATE push_outbox SET status='queued', mtime=NOW()
		 WHERE short_host_id=$1 AND channel_id=$2 AND status='held'`,
		m.ShortHostID(),
		channelID,
	)
	return err
}

// pushStatusForSend decides the status of a send's push rows. Runs in the send
// transaction, before the fan-out. A hold whose holder has left the team ends
// here: it is deleted and the team's held rows are released.
func (s *messageSender) pushStatusForSend(m shared.MetaContext) (string, error) {
	appDB, err := s.appID.ExportToDB()
	if err != nil {
		return "", err
	}
	holder, err := readPushHolder(m, s.tx, s.parentTeam, appDB)
	if err != nil || holder == nil {
		return pushStatusQueued, err
	}
	live, err := activeTeamMembers(m, s.userdb, s.parentTeam, []proto.UID{*holder})
	if err != nil {
		return "", err
	}
	role, ok := live[*holder]
	if !ok {
		if err = s.endHold(m, appDB); err != nil {
			return "", err
		}
		return pushStatusQueued, nil
	}
	readable, err := canReadChannel(m, s.tx, s.channelID(), *holder, role, s.readRole, s.private)
	if err != nil {
		return "", err
	}
	if !readable {
		return pushStatusQueued, nil
	}
	return pushStatusHeld, nil
}

func (s *messageSender) endHold(m shared.MetaContext, appDB string) error {
	_, err := s.tx.Exec(
		m.Ctx(),
		`DELETE FROM push_holds WHERE short_host_id=$1 AND team_id=$2 AND app_id=$3`,
		m.ShortHostID(),
		s.parentTeam.ExportToDB(),
		appDB,
	)
	if err != nil {
		return err
	}
	return releaseTeamHeld(m, s.tx, s.parentTeam, appDB)
}

// canReadChannel: a member's team role clears the channel's read role, and
// for a private channel they hold an ACL row.
func canReadChannel(
	m shared.MetaContext,
	tx shared.Querier,
	channelID int64,
	uid proto.UID,
	role core.RoleKey,
	readRole proto.Role,
	private bool,
) (bool, error) {
	rr, err := core.ImportRole(readRole)
	if err != nil {
		return false, err
	}
	if role.LessThan(*rr) {
		return false, nil
	}
	if !private {
		return true, nil
	}
	_, found, err := readChannelAclRole(m, tx, channelID, uid)
	return found, err
}

// adminTx runs fn in a realtime transaction after checking the caller is a
// team admin or above.
func adminTx(
	m shared.MetaContext,
	team proto.TeamID,
	appID proto.RTAppID,
	which string,
	fn func(m shared.MetaContext, tx pgx.Tx, appDB string) error,
) error {
	appDB, err := appID.ExportToDB()
	if err != nil {
		return err
	}
	userdb, err := m.Db(shared.DbTypeUsers)
	if err != nil {
		return err
	}
	defer userdb.Release()
	role, err := AuthorizeUserForTeam(m, userdb, team)
	if err != nil {
		return err
	}
	if !role.IsAdminOrAbove() {
		return core.PermissionError("only team admins may place or clear a push hold")
	}
	rtdb, err := m.Db(shared.DbTypeRealTime)
	if err != nil {
		return err
	}
	defer rtdb.Release()
	return shared.RetryTx(m, rtdb, which, func(m shared.MetaContext, tx pgx.Tx) error {
		return fn(m, tx, appDB)
	})
}

// SetPushHold places (or takes over) the team's push hold, with the caller as
// holder.
func SetPushHold(m shared.MetaContext, arg rem.RtSetPushHoldArg) error {
	return adminTx(m, arg.Team, arg.AppID, "realtime.SetPushHold",
		func(m shared.MetaContext, tx pgx.Tx, appDB string) error {
			_, err := tx.Exec(
				m.Ctx(),
				`INSERT INTO push_holds (short_host_id, team_id, app_id, holder_uid, ctime)
				 VALUES ($1, $2, $3, $4, NOW())
				 ON CONFLICT (short_host_id, team_id, app_id)
				 DO UPDATE SET holder_uid=EXCLUDED.holder_uid, ctime=NOW()`,
				m.ShortHostID(),
				arg.Team.ExportToDB(),
				appDB,
				m.UID().ExportToDB(),
			)
			return err
		})
}

// ClearPushHold removes the team's push hold and releases what it held.
func ClearPushHold(m shared.MetaContext, arg rem.RtClearPushHoldArg) error {
	return adminTx(m, arg.Team, arg.AppID, "realtime.ClearPushHold",
		func(m shared.MetaContext, tx pgx.Tx, appDB string) error {
			_, err := tx.Exec(
				m.Ctx(),
				`DELETE FROM push_holds WHERE short_host_id=$1 AND team_id=$2 AND app_id=$3`,
				m.ShortHostID(),
				arg.Team.ExportToDB(),
				appDB,
			)
			if err != nil {
				return err
			}
			return releaseTeamHeld(m, tx, arg.Team, appDB)
		})
}

// holderTx runs fn in a realtime transaction after checking the caller can
// read the channel and holds its team's push hold.
func holderTx(
	m shared.MetaContext,
	chid proto.RTChannelID,
	which string,
	fn func(m shared.MetaContext, tx pgx.Tx, ca *channelAuth, userdb shared.Querier) error,
) error {
	rtdb, err := m.Db(shared.DbTypeRealTime)
	if err != nil {
		return err
	}
	defer rtdb.Release()
	userdb, err := m.Db(shared.DbTypeUsers)
	if err != nil {
		return err
	}
	defer userdb.Release()
	return shared.RetryTx(m, rtdb, which, func(m shared.MetaContext, tx pgx.Tx) error {
		ca, err := authorizeChannel(m, tx, userdb, chid.Short().Int64(), accessRead, false)
		if err != nil {
			return err
		}
		appDB, err := ca.appID.ExportToDB()
		if err != nil {
			return err
		}
		holder, err := readPushHolder(m, tx, ca.team, appDB)
		if err != nil {
			return err
		}
		if holder == nil || !holder.Eq(m.UID()) {
			return core.PermissionError("only the push hold's holder may release or notify")
		}
		return fn(m, tx, ca, userdb)
	})
}

// ReleasePushes decides a channel's held rows up to arg.ThroughSeq: keep
// members stay held, drop pairs are deleted, and of the rest each member's
// highest seq is queued and their lower seqs deleted -- one push per member
// per call. Only held rows are touched, so repeating a call changes nothing.
func ReleasePushes(m shared.MetaContext, arg rem.RtReleasePushesArg) error {
	return holderTx(m, arg.ChannelID, "realtime.ReleasePushes",
		func(m shared.MetaContext, tx pgx.Tx, _ *channelAuth, _ shared.Querier) error {
			chid := arg.ChannelID.Short().Int64()
			if len(arg.Drop) > 0 {
				uids := make([][]byte, len(arg.Drop))
				seqs := make([]int64, len(arg.Drop))
				for i, d := range arg.Drop {
					uids[i] = d.Uid.ExportToDB()
					seqs[i] = d.Seq.Int64()
				}
				_, err := tx.Exec(
					m.Ctx(),
					`DELETE FROM push_outbox p
					 USING unnest($3::bytea[], $4::bigint[]) AS d(uid, seq)
					 WHERE p.short_host_id=$1 AND p.channel_id=$2 AND p.status='held'
					   AND p.uid=d.uid AND p.seq=d.seq`,
					m.ShortHostID(), chid, uids, seqs,
				)
				if err != nil {
					return err
				}
			}
			keep := make([][]byte, len(arg.Keep))
			for i, u := range arg.Keep {
				keep[i] = u.ExportToDB()
			}
			through := arg.ThroughSeq.Int64()
			_, err := tx.Exec(
				m.Ctx(),
				`UPDATE push_outbox p SET status='queued', mtime=NOW()
				 FROM (SELECT uid, MAX(seq) AS seq FROM push_outbox
				       WHERE short_host_id=$1 AND channel_id=$2 AND status='held'
				         AND seq <= $3 AND NOT (uid = ANY($4::bytea[]))
				       GROUP BY uid) top
				 WHERE p.short_host_id=$1 AND p.channel_id=$2 AND p.status='held'
				   AND p.uid=top.uid AND p.seq=top.seq`,
				m.ShortHostID(), chid, through, keep,
			)
			if err != nil {
				return err
			}
			_, err = tx.Exec(
				m.Ctx(),
				`DELETE FROM push_outbox
				 WHERE short_host_id=$1 AND channel_id=$2 AND status='held'
				   AND seq <= $3 AND NOT (uid = ANY($4::bytea[]))`,
				m.ShortHostID(), chid, through, keep,
			)
			return err
		})
}

// NotifyMembers queues one content-free 'system' push per entry whose member
// can read the channel. Others are skipped without error.
// NotifyMembers queues system pushes, which is new activity, so an archived
// channel must not produce one. holderTx authorizes at accessRead, which the
// chokepoint deliberately does not gate on archived_at (a read by explicit id
// still works), so the check belongs here rather than there.
func NotifyMembers(m shared.MetaContext, arg rem.RtNotifyMembersArg) error {
	if len(arg.Entries) > maxPushNotifyEntries {
		return core.BadArgsError("too many push notify entries")
	}
	seen := make(map[proto.UID]bool, len(arg.Entries))
	var entries []rem.RTPushNotify
	for _, e := range arg.Entries {
		if len(e.Handle) > maxPushHandleLen {
			return core.BadArgsError("push handle too long")
		}
		// One push per member per call, however often they are listed.
		if seen[e.Uid] {
			continue
		}
		seen[e.Uid] = true
		entries = append(entries, e)
	}
	arg.Entries = entries
	return holderTx(m, arg.ChannelID, "realtime.NotifyMembers",
		func(m shared.MetaContext, tx pgx.Tx, ca *channelAuth, userdb shared.Querier) error {
			if err := archivedBlocks(ca.archivedAt); err != nil {
				return err
			}
			if len(arg.Entries) == 0 {
				return nil
			}
			chid := arg.ChannelID.Short().Int64()
			uids := make([]proto.UID, len(arg.Entries))
			for i, e := range arg.Entries {
				uids[i] = e.Uid
			}
			live, err := activeTeamMembers(m, userdb, ca.team, uids)
			if err != nil {
				return err
			}
			for _, e := range arg.Entries {
				role, ok := live[e.Uid]
				if !ok {
					continue
				}
				readable, err := canReadChannel(m, tx, chid, e.Uid, role, ca.readRole, ca.private)
				if err != nil {
					return err
				}
				if !readable {
					continue
				}
				var data []byte
				if len(e.Handle) > 0 {
					data = e.Handle
				}
				// Only members with a delivery row: the same set the send
				// fan-out pushes to.
				_, err = tx.Exec(
					m.Ctx(),
					`INSERT INTO push_outbox
					   (short_host_id, uid, channel_id, kind, seq, data, status, ctime, mtime)
					 SELECT uc.short_host_id, uc.uid, uc.channel_id, 'system', NULL, $4, 'queued', NOW(), NOW()
					   FROM user_channels uc
					  WHERE uc.short_host_id=$1 AND uc.channel_id=$2 AND uc.uid=$3`,
					m.ShortHostID(), chid, e.Uid.ExportToDB(), data,
				)
				if err != nil {
					return err
				}
			}
			return nil
		})
}
