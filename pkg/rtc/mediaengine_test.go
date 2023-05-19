package rtc

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/whoyao/protocol/livekit"
)

func TestIsCodecEnabled(t *testing.T) {
	t.Run("empty fmtp requirement should match all", func(t *testing.T) {
		enabledCodecs := []*livekit.Codec{{Mime: "video/h264"}}
		require.True(t, IsCodecEnabled(enabledCodecs, webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, SDPFmtpLine: "special"}))
		require.True(t, IsCodecEnabled(enabledCodecs, webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}))
		require.False(t, IsCodecEnabled(enabledCodecs, webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}))
	})

	t.Run("when fmtp is provided, require match", func(t *testing.T) {
		enabledCodecs := []*livekit.Codec{{Mime: "video/h264", FmtpLine: "special"}}
		require.True(t, IsCodecEnabled(enabledCodecs, webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, SDPFmtpLine: "special"}))
		require.False(t, IsCodecEnabled(enabledCodecs, webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}))
		require.False(t, IsCodecEnabled(enabledCodecs, webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}))
	})
}
