package realtime

// Per-member channel push mute (fork-only; follow-scoped-push).
//
// user_channels.muted has been in the schema since Stage 1a, written false on
// every insert and read only by the inbox. A muted member gets no push_outbox
// row from a send or a notify; nothing else changes. The SECO app keeps it
// equal to "does not follow this open channel" -- the follow itself is E2EE
// team KV the server never sees, so this flag is all it learns.

import (
	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/jackc/pgx/v5"
)

// channelMuter sets the calling user's own mute in one channel and, if it
// changed, bumps their inbox version and stamps the row at it -- the
// readThroughMarker shape, so their other devices see the flag on their next
// sync. Only the caller's row is ever touched.
type channelMuter struct {
	arg    rem.RtSetChannelMutedArg
	uid    proto.UID
	userdb shared.Querier
	tx     pgx.Tx

	app     proto.RTAppID
	appDB   string
	changed bool
}

func (c *channelMuter) channelID() int64 { return c.arg.ChannelID.Short().Int64() }

func (c *channelMuter) run(m shared.MetaContext) error {
	// Read access, like rtReadThrough: a revoked member fails at the ACL
	// rather than learning whether the channel exists.
	ca, err := authorizeChannel(m, c.tx, c.userdb, c.channelID(), accessRead, false)
	if err != nil {
		return err
	}
	c.app = ca.appID
	c.appDB, err = ca.appID.ExportToDB()
	if err != nil {
		return err
	}

	// user_inbox before user_channels: the package-wide lock order (see
	// readThroughMarker.run).
	var lockedVers int64
	err = c.tx.QueryRow(
		m.Ctx(),
		`SELECT inbox_version FROM user_inbox
		 WHERE short_host_id=$1 AND uid=$2 AND app_id=$3
		 FOR UPDATE`,
		m.ShortHostID(),
		c.uid.ExportToDB(),
		c.appDB,
	).Scan(&lockedVers)
	if err != nil && err != pgx.ErrNoRows {
		return err
	}

	tag, err := c.tx.Exec(
		m.Ctx(),
		`UPDATE user_channels
		 SET muted=$4, mtime=NOW()
		 WHERE short_host_id=$1 AND channel_id=$2 AND uid=$3
		   AND muted <> $4`,
		m.ShortHostID(),
		c.channelID(),
		c.uid.ExportToDB(),
		c.arg.Muted,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// Already at this value (a no-op, bumping nothing), or no delivery
		// row at all.
		var one int
		err = c.tx.QueryRow(
			m.Ctx(),
			`SELECT 1 FROM user_channels
			 WHERE short_host_id=$1 AND channel_id=$2 AND uid=$3`,
			m.ShortHostID(),
			c.channelID(),
			c.uid.ExportToDB(),
		).Scan(&one)
		if err == pgx.ErrNoRows {
			return core.RowNotFoundError{}
		}
		return err
	}

	c.changed = true
	var vers int64
	err = c.tx.QueryRow(
		m.Ctx(),
		`INSERT INTO user_inbox (short_host_id, uid, app_id, inbox_version, mtime)
		 VALUES ($1, $2, $3, 1, NOW())
		 ON CONFLICT (short_host_id, uid, app_id)
		 DO UPDATE SET inbox_version = user_inbox.inbox_version + 1, mtime = NOW()
		 RETURNING inbox_version`,
		m.ShortHostID(),
		c.uid.ExportToDB(),
		c.appDB,
	).Scan(&vers)
	if err != nil {
		return err
	}
	_, err = c.tx.Exec(
		m.Ctx(),
		`UPDATE user_channels
		 SET inbox_version=$4
		 WHERE short_host_id=$1 AND channel_id=$2 AND uid=$3`,
		m.ShortHostID(),
		c.channelID(),
		c.uid.ExportToDB(),
		vers,
	)
	return err
}

// SetChannelMuted sets the authenticated user's (m.UID()) push mute in a
// channel.
func SetChannelMuted(m shared.MetaContext, arg rem.RtSetChannelMutedArg) error {
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

	return shared.RetryTx2(m,
		rtdb,
		"realtime.SetChannelMuted",
		func(m shared.MetaContext, tx pgx.Tx) (func(shared.MetaContext), error) {
			c := channelMuter{
				arg:    arg,
				uid:    m.UID(),
				userdb: userdb,
				tx:     tx,
			}
			if err := c.run(m); err != nil {
				return nil, err
			}
			if !c.changed {
				return nil, nil
			}
			// Wake the caller's own parked pollers (their other devices).
			return func(m shared.MetaContext) {
				wakeInboxPollers(m, c.app, []proto.UID{c.uid})
			}, nil
		},
	)
}
