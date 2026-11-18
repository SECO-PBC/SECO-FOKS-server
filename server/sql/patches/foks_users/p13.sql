-- p13: a challenge-key type for /api/v1 bearer tokens (fork-only;
-- docs/api-sessions.md). The tokens are JWTs signed with these keys; the
-- server keeps no per-token rows. A host's first key is created on its first
-- mint (shared.ensureApiSessionKey).

-- patch-db runs this in a transaction: allowed for ADD VALUE since Postgres 12
-- (the value is usable once the patch commits).
ALTER TYPE challenge_type ADD VALUE IF NOT EXISTS 'api_session';
