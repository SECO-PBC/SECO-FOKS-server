-- p12: which accounts are Sages (fork-only; docs/kv-system-store.md).
--
-- A multi-use code flagged daemon_signup makes its signups Sages: the signup
-- transaction inserts into daemon_accounts. Listing is deliberately separate
-- from multiuse_invite_code_users, so an operator can add the existing bots
-- exactly and revoke one.

ALTER TABLE multiuse_invite_codes
    ADD COLUMN daemon_signup BOOLEAN NOT NULL DEFAULT FALSE;

CREATE TABLE daemon_accounts (
    short_host_id SMALLINT NOT NULL,
    uid BYTEA NOT NULL,
    ctime TIMESTAMP NOT NULL,
    PRIMARY KEY(short_host_id, uid)
);
