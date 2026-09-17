/*
 * Delegated push release, part 1 (fork-only; push holds, see p8.sql).
 *
 * A push_outbox row written under a push hold is 'held': the relay never
 * claims it, and only the hold's holder decides it (release -> 'queued', or
 * delete). Alone in its patch because Postgres cannot use a new enum value in
 * the transaction that adds it, and each patch runs in one transaction.
 */
ALTER TYPE push_status ADD VALUE IF NOT EXISTS 'held';
