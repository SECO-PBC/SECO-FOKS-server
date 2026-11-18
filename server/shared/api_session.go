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
	// maxActiveApiSessions caps live tokens per user. Clients cache a token
	// for its lifetime, so a well-behaved one holds about one per scope;
	// minting past the cap deactivates the oldest, which bounds the table
	// against a client stuck in a mint loop.
	maxActiveApiSessions = 10

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
	dur := wcfg.ApiSessionDuration()

	db, err := m.Db(DbTypeUsers)
	if err != nil {
		return nil, err
	}
	defer db.Release()

	tx, err := db.Begin(m.Ctx())
	if err != nil {
		return nil, err
	}
	// A no-op (pgx.ErrTxClosed) once Commit has run.
	defer func() { _ = tx.Rollback(m.Ctx()) }()

	var etime time.Time
	err = tx.QueryRow(
		m.Ctx(),
		`INSERT INTO user_api_sessions
		   (short_host_id, uid, token_hash, scope, ctime, etime, active)
		 VALUES($1, $2, $3, $4, NOW(), NOW() + $5, true)
		 RETURNING etime`,
		int(m.ShortHostID()),
		uid.ExportToDB(),
		tok.Hash(),
		int(scope),
		dur,
	).Scan(&etime)
	if err != nil {
		return nil, err
	}

	// Keep only the newest maxActiveApiSessions live tokens for this user.
	_, err = tx.Exec(
		m.Ctx(),
		`UPDATE user_api_sessions SET active = false
		 WHERE short_host_id = $1 AND uid = $2 AND active = true
		   AND token_hash NOT IN (
		     SELECT token_hash FROM user_api_sessions
		     WHERE short_host_id = $1 AND uid = $2 AND active = true
		     ORDER BY ctime DESC
		     LIMIT $3)`,
		int(m.ShortHostID()),
		uid.ExportToDB(),
		maxActiveApiSessions,
	)
	if err != nil {
		return nil, err
	}
	err = tx.Commit(m.Ctx())
	if err != nil {
		return nil, err
	}

	return &proto.ApiSession{
		Token:   tok,
		Scope:   scope,
		BaseURL: baseURL,
		Expires: proto.ExportTime(etime),
	}, nil
}

// CheckApiSession resolves a bearer token to its user, provided it is active,
// unexpired, minted for scope, and its user is still in use. Any failure to
// match is ApiSessionNotFoundError.
func CheckApiSession(
	m MetaContext,
	tok proto.ApiToken,
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
	err = db.QueryRow(
		m.Ctx(),
		`SELECT s.short_host_id, s.uid
		 FROM user_api_sessions AS s
		 JOIN users AS u USING(short_host_id, uid)
		 JOIN names AS n USING(short_host_id, name_ascii)
		 WHERE s.token_hash = $1 AND s.scope = $2
		   AND s.active = true AND s.etime > NOW()
		   AND n.state = 'in_use'`,
		tok.Hash(),
		int(scope),
	).Scan(&shid, &uidRaw)
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
