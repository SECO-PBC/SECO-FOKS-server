-- p13: bearer tokens for the web server's /api/v1 HTTPS surface (fork-only;
-- docs/api-sessions.md).
--
-- Separate from user_web_sessions on purpose: an API token must never open the
-- admin panel, and an admin session cookie must never call /api. Tokens are
-- stored hashed, so a read of this table yields nothing usable.

CREATE TABLE user_api_sessions (
    short_host_id SMALLINT NOT NULL,
    uid BYTEA NOT NULL,
    token_hash BYTEA NOT NULL,   -- SHA-256 of the bearer token; the token itself is never stored
    scope SMALLINT NOT NULL,     -- proto lib.ApiScope
    ctime TIMESTAMP NOT NULL,
    etime TIMESTAMP NOT NULL,
    active BOOLEAN NOT NULL,
    PRIMARY KEY(token_hash)
);
CREATE INDEX user_api_sessions_uid_idx ON user_api_sessions(short_host_id, uid, scope, ctime);
CREATE INDEX user_api_sessions_etime_idx ON user_api_sessions(etime);
