// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package engine

import (
	"context"

	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/foks-proj/go-foks/server/shared"
)

// Social invites (docs/social_signup_spec.md), inviter side. All calls but
// Reply authorize on the caller being the row's inviter; Reply is made by
// the invitee and carries the write key instead.

func (c *UserClientConn) Create(ctx context.Context, arg rem.CreateArg) error {
	m := shared.NewMetaContextConn(ctx, c)
	return shared.InsertSocialInvite(m, arg, m.UID())
}

func (c *UserClientConn) List(ctx context.Context) ([]rem.SocialInviteRow, error) {
	m := shared.NewMetaContextConn(ctx, c)
	return shared.ListSocialInvites(m, m.UID())
}

func (c *UserClientConn) Reply(ctx context.Context, arg rem.ReplyArg) error {
	m := shared.NewMetaContextConn(ctx, c)
	inviter, err := shared.ReplySocialInvite(m, arg, m.UID())
	if err != nil {
		return err
	}
	// FORK-ONLY (invite-review-push): wake the inviter.
	queueInvitationPush(m, "SocialInvite.Reply", inviter, shared.InviteReplyPushMarker)
	return nil
}

// AskAgain and Close (FORK-ONLY, invitee-acceptance-push) wake the invitee
// when the inviter asked for a different proof or accepted them. A decline
// or a cancel wakes no one.
func (c *UserClientConn) AskAgain(ctx context.Context, arg rem.AskAgainArg) error {
	m := shared.NewMetaContextConn(ctx, c)
	invitee, err := shared.AskAgainSocialInvite(m, arg, m.UID())
	if err != nil {
		return err
	}
	queueInvitationPush(m, "SocialInvite.AskAgain", invitee, shared.InviteAskPushMarker)
	return nil
}

func (c *UserClientConn) Close(ctx context.Context, arg rem.CloseArg) error {
	m := shared.NewMetaContextConn(ctx, c)
	invitee, err := shared.CloseSocialInvite(m, arg, m.UID())
	if err != nil {
		return err
	}
	queueInvitationPush(m, "SocialInvite.Close", invitee, shared.InviteAcceptedPushMarker)
	return nil
}

// queueInvitationPush wakes uid, if any. Best-effort: the change is stored,
// and failing the call now would only make the client retry into a no-op,
// which queues nothing.
func queueInvitationPush(m shared.MetaContext, op string, uid *proto.UID, marker string) {
	if uid == nil {
		return
	}
	if err := shared.QueueInvitationPush(m, *uid, marker); err != nil {
		m.Warnw(op, "stage", "queue invitation push", "marker", marker, "err", err)
	}
}

// NewInviteCode mints a standard single-use code against the caller's
// basket, capped by the caller's outstanding unused codes. Richer allocation
// policy (budgets, per-diem, admin exceptions) is host economics and
// deliberately not built here; see "Left open" in the spec.
func (c *UserClientConn) NewInviteCode(ctx context.Context) (rem.InviteCode, error) {
	m := shared.NewMetaContextConn(ctx, c)
	code, err := shared.NewUserInviteCode(m, m.UID())
	if err != nil {
		return rem.InviteCode{}, err
	}
	return *code, nil
}

var _ rem.SocialInviteInterface = (*UserClientConn)(nil)
