package realtime

import (
	"testing"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/keybase/go-codec/codec"
	"github.com/stretchr/testify/require"
)

// rtSendArgBeforeNoPush is rem.RTSendArg's wire shape before the fork-only
// noPush field (silent-system-notices): what a server that predates it
// decodes a send into.
type rtSendArgBeforeNoPush struct {
	_struct         struct{} `codec:",toarray"` //lint:ignore U1000 msgpack internal field
	Md              *proto.RTMsgMetadataInternal__
	Chid            *proto.RTChannelIDShortInternal__
	Mw              *proto.RTMsgWrapperInternal__
	ExpectedPrevSeq *proto.RTMsgSeqInternal__
}

// Both directions of the mixed deploy: an older server drops the field a new
// client sends (and so pushes as before), and a send from an older client
// decodes here with NoPush false.
func TestRTSendArgNoPushIsCompatible(t *testing.T) {
	arg := rem.RTSendArg{
		Md:              proto.RTMsgMetadata{PrevSeq: 7},
		Chid:            proto.RTChannelIDShort(42),
		ExpectedPrevSeq: 7,
		NoPush:          true,
	}
	b, err := core.EncodeToBytes(&arg)
	require.NoError(t, err)

	var old rtSendArgBeforeNoPush
	require.NoError(t, codec.NewDecoderBytes(b, core.Codec()).Decode(&old))
	require.NotNil(t, old.Chid)
	require.NotNil(t, old.ExpectedPrevSeq)

	var oldBytes []byte
	require.NoError(t, codec.NewEncoderBytes(&oldBytes, core.Codec()).Encode(&old))
	var back rem.RTSendArg
	require.NoError(t, core.DecodeFromBytes(&back, oldBytes))
	require.False(t, back.NoPush)
	require.Equal(t, proto.RTChannelIDShort(42), back.Chid)
	require.Equal(t, proto.RTMsgSeq(7), back.ExpectedPrevSeq)
}

// rtSendArgBeforePushTo is rem.RTSendArg's wire shape before the fork-only
// pushTo field (reaction-push-to-author).
type rtSendArgBeforePushTo struct {
	_struct         struct{} `codec:",toarray"` //lint:ignore U1000 msgpack internal field
	Md              *proto.RTMsgMetadataInternal__
	Chid            *proto.RTChannelIDShortInternal__
	Mw              *proto.RTMsgWrapperInternal__
	ExpectedPrevSeq *proto.RTMsgSeqInternal__
	NoPush          *bool
}

// An older server drops pushTo (and so pushes every recipient, as before),
// and a send from an older client decodes here with PushTo nil.
func TestRTSendArgPushToIsCompatible(t *testing.T) {
	var uid proto.UID
	uid[0] = byte(proto.EntityType_User)
	uid[1] = 7
	arg := rem.RTSendArg{
		Chid:   proto.RTChannelIDShort(42),
		NoPush: true,
		PushTo: &uid,
	}
	b, err := core.EncodeToBytes(&arg)
	require.NoError(t, err)

	var old rtSendArgBeforePushTo
	require.NoError(t, codec.NewDecoderBytes(b, core.Codec()).Decode(&old))
	require.NotNil(t, old.NoPush)
	require.True(t, *old.NoPush)

	var oldBytes []byte
	require.NoError(t, codec.NewEncoderBytes(&oldBytes, core.Codec()).Encode(&old))
	var back rem.RTSendArg
	require.NoError(t, core.DecodeFromBytes(&back, oldBytes))
	require.Nil(t, back.PushTo)
	require.True(t, back.NoPush)

	var round rem.RTSendArg
	require.NoError(t, core.DecodeFromBytes(&round, b))
	require.NotNil(t, round.PushTo)
	require.Equal(t, uid, *round.PushTo)
}
