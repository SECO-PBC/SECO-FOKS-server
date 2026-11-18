// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"errors"
	"time"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/jackc/pgx/v5"
)

// Fork-only (docs/api-sessions.md): bearer tokens for the web server's
// /api/v1 HTTPS surface.
//
// A logged-in client mints a token over the authenticated User RPC
// (newApiSession) and sends it as `Authorization: Bearer <token>`. Tokens are
// scoped (proto.ApiScope), short-lived, and stored only as a SHA-256 hash in
// user_api_sessions -- a table separate from user_web_sessions, so an API token
// can never open the admin panel and an admin cookie can never call /api.

const (
	// maxApiSessionsPerScope caps live tokens per user and scope. apiFetch
	// caches a token per scope for its lifetime, so each running client holds
	// one; ten covers a user's devices and restarts. Minting past the cap
	// DELETES the oldest others (never the one being minted), so the table
	// stays bounded however a client misbehaves.
	maxApiSessionsPerScope = 10

	// maxApiMintsPerMinute throttles newApiSession per user and scope. An
	// honest client mints about once a day per scope; a client stuck in a
	// mint-then-401 loop is refused here instead of costing a write each time.
	maxApiMintsPerMinute = 5

	apiSessionGCStartDelay   = 7 * time.Minute
	apiSessionGCInterval     = time.Hour
	apiSessionGCBatch        = 10000
	apiSessionGCBatchTimeout = 60 * time.Second

	// apiSessionGCMargin keeps expired rows a little past etime, so a check
	// that began just before expiry never races its row's delete.
	apiSessionGCMargin = time.Hour
)

// ApiSessionNotFoundError is returned for every reason a bearer token is
// refused (unknown, expired, revoked, wrong scope, user gone), so the HTTP
// layer cannot leak which.
type ApiSessionNotFoundError struct{}

func (ApiSessionNotFoundError) Error() string { return "API session not found" }

// ApiSessionAuth is who a valid bearer token speaks for.
type ApiSessionAuth struct {
	ShortHostID core.ShortHostID
	UID         proto.UID
	Expires     time.Time
}

// NewApiSession mints a bearer token for the user and host in m, good for
// scope until the configured api_session_duration elapses.
func NewApiSession(
	m MetaContext,
	scope proto.ApiScope,
) (
	*proto.ApiSession,
	error,
) {
	if !scope.IsValid() {
		return nil, core.BadArgsError("unknown API scope")
	}
	uid := m.UID()
	if uid.IsZero() {
		return nil, core.AuthError{}
	}
	wcfg, err := m.G().Config().WebConfig(m.Ctx())
	if err != nil {
		return nil, err
	}
	baseURL, err := adminBaseURL(m, wcfg)
	if err != nil {
		return nil, err
	}

	var tok proto.ApiToken
	err = core.RandomFill(tok[:])
	if err != nil {
		return nil, err
	}
	dur, err := wcfg.ApiSessionDuration()
	if err != nil {
		return nil, err
	}
	shid := int(m.ShortHostID())
	uidDB := uid.ExportToDB()
	hash := tok.Hash()

	err = RetryTxUserDB(m, "NewApiSession", func(m MetaContext, tx pgx.Tx) error {
		var recent int
		err := tx.QueryRow(
			m.Ctx(),
			`SELECT COUNT(*) FROM user_api_sessions
			 WHERE short_host_id = $1 AND uid = $2 AND scope = $3
			   AND ctime > NOW() - INTERVAL '1 minute'`,
			shid, uidDB, int(scope),
		).Scan(&recent)
		if err != nil {
			return err
		}
		if recent >= maxApiMintsPerMinute {
			return core.RateLimitError{}
		}
		_, err = tx.Exec(
			m.Ctx(),
			`INSERT INTO user_api_sessions
			   (short_host_id, uid, token_hash, scope, ctime, etime, active)
			 VALUES($1, $2, $3, $4, NOW(), NOW() + $5, true)`,
			shid, uidDB, hash.ExportToDB(), int(scope), dur,
		)
		if err != nil {
			return err
		}
		// Keep the new token plus the newest maxApiSessionsPerScope-1 others.
		// The new one is excluded by hash, not by ctime: NOW() is the
		// transaction's start, so a slow concurrent mint could otherwise
		// look oldest and retire the token it is about to return.
		_, err = tx.Exec(
			m.Ctx(),
			`DELETE FROM user_api_sessions
			 WHERE short_host_id = $1 AND uid = $2 AND scope = $3
			   AND token_hash <> $4
			   AND token_hash NOT IN (
			     SELECT token_hash FROM user_api_sessions
			     WHERE short_host_id = $1 AND uid = $2 AND scope = $3
			       AND token_hash <> $4
			     ORDER BY ctime DESC
			     LIMIT $5)`,
			shid, uidDB, int(scope), hash.ExportToDB(), maxApiSessionsPerScope-1,
		)
		return err
	})
	if err != nil {
		return nil, err
	}

	return &proto.ApiSession{
		Token:    tok,
		BaseURL:  baseURL,
		Lifetime: proto.ExportDurationMilli(dur),
	}, nil
}

// CheckApiSession resolves a token (by its hash) to its user, provided it is
// active, unexpired, minted for scope, and its user is still in use. Any
// failure to match is ApiSessionNotFoundError.
func CheckApiSession(
	m MetaContext,
	hash proto.ApiTokenHash,
	scope proto.ApiScope,
) (
	*ApiSessionAuth,
	error,
) {
	db, err := m.Db(DbTypeUsers)
	if err != nil {
		return nil, err
	}
	defer db.Release()
	var shid int
	var uidRaw []byte
	var etime time.Time
	err = db.QueryRow(
		m.Ctx(),
		`SELECT s.short_host_id, s.uid, s.etime
		 FROM user_api_sessions AS s
		 JOIN users AS u USING(short_host_id, uid)
		 JOIN names AS n USING(short_host_id, name_ascii)
		 WHERE s.token_hash = $1 AND s.scope = $2
		   AND s.active = true AND s.etime > NOW()
		   AND n.state = 'in_use'`,
		hash.ExportToDB(),
		int(scope),
	).Scan(&shid, &uidRaw, &etime)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ApiSessionNotFoundError{}
	}
	if err != nil {
		return nil, err
	}
	var uid proto.UID
	err = uid.ImportFromDB(uidRaw)
	if err != nil {
		return nil, err
	}
	return &ApiSessionAuth{
		ShortHostID: core.ShortHostID(shid),
		UID:         uid,
		Expires:     etime,
	}, nil
}

// DeleteExpiredApiSessions deletes up to limit API sessions whose etime is
// more than grace in the past, reporting how many it deleted. The cutoff is
// computed in SQL from NOW(), the same clock that wrote etime, so neither a
// skewed process clock nor a non-UTC process time zone (pgx sends a TIMESTAMP
// parameter's wall clock) can shift it.
func DeleteExpiredApiSessions(m MetaContext, grace time.Duration, limit int) (int, error) {
	db, err := m.Db(DbTypeUsers)
	if err != nil {
		return 0, err
	}
	defer db.Release()
	tag, err := db.Exec(m.Ctx(),
		`DELETE FROM user_api_sessions
		 WHERE token_hash IN (
		   SELECT token_hash FROM user_api_sessions
		   WHERE etime < NOW() - $1::interval
		   LIMIT $2)`,
		grace,
		limit,
	)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func gcExpiredApiSessions(m MetaContext) (int, error) {
	total := 0
	for {
		mb, cancel := m.WithContextTimeout(apiSessionGCBatchTimeout)
		n, err := DeleteExpiredApiSessions(mb, apiSessionGCMargin, apiSessionGCBatch)
		cancel()
		total += n
		if err != nil {
			return total, err
		}
		if n < apiSessionGCBatch {
			return total, nil
		}
	}
}

// RunApiSessionGC deletes expired API sessions apiSessionGCStartDelay after
// startup and then every apiSessionGCInterval, until m's context ends. Like
// RunVOBearerTokenGC, a failed round is retried on the next tick and running
// it in several processes is harmless.
func RunApiSessionGC(m MetaContext) {
	wait := apiSessionGCStartDelay
	for {
		select {
		case <-time.After(wait):
		case <-m.Ctx().Done():
			return
		}
		wait = apiSessionGCInterval
		n, err := gcExpiredApiSessions(m)
		if err != nil {
			m.Warnw("RunApiSessionGC", "deleted", n, "err", err)
		} else if n > 0 {
			m.Infow("RunApiSessionGC", "deleted", n)
		}
	}
}
