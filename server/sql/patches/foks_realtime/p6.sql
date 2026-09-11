/*
 * No-push channels (fork-only; dm-handshake-over-rt).
 *
 * A no-push channel is excluded from the push_outbox fan-out on every send:
 * the inbox-version bump (long-poll wakes, online delivery) is untouched, but
 * no APNs/FCM wake row is queued. The app creates its machine-to-machine
 * control channels (`seco-` name prefix, e.g. the member-DM first-contact
 * handshake) with this flag so a handshake between two members never buzzes
 * every phone in the community with a "new activity" alert behind which
 * nothing visible arrived.
 *
 * A plaintext per-channel column, like `tier` and `private`, because channel
 * NAMES are PTK-encrypted (name_box) -- the send path cannot tell a control
 * channel from conversation any other way. Creation-time only; no flip path.
 */
ALTER TABLE channels ADD COLUMN no_push BOOLEAN NOT NULL DEFAULT false;
