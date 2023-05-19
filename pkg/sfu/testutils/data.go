package testutils

import (
	"time"

	"github.com/pion/rtp"
	"github.com/whoyao/livekit/pkg/sfu/buffer"
	"github.com/whoyao/webrtc/v3"
)

// -----------------------------------------------------------

type TestExtPacketParams struct {
	SetMarker      bool
	IsKeyFrame     bool
	PayloadType    uint8
	SequenceNumber uint16
	Timestamp      uint32
	SSRC           uint32
	PayloadSize    int
	PaddingSize    byte
	ArrivalTime    time.Time
	VideoLayer     buffer.VideoLayer
}

// -----------------------------------------------------------

func GetTestExtPacket(params *TestExtPacketParams) (*buffer.ExtPacket, error) {
	packet := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Padding:        params.PaddingSize != 0,
			Marker:         params.SetMarker,
			PayloadType:    params.PayloadType,
			SequenceNumber: params.SequenceNumber,
			Timestamp:      params.Timestamp,
			SSRC:           params.SSRC,
		},
		Payload:     make([]byte, params.PayloadSize),
		PaddingSize: params.PaddingSize,
	}

	raw, err := packet.Marshal()
	if err != nil {
		return nil, err
	}

	ep := &buffer.ExtPacket{
		VideoLayer: params.VideoLayer,
		Arrival:    params.ArrivalTime,
		Packet:     &packet,
		KeyFrame:   params.IsKeyFrame,
		RawPacket:  raw,
	}

	return ep, nil
}

// --------------------------------------

func GetTestExtPacketVP8(params *TestExtPacketParams, vp8 *buffer.VP8) (*buffer.ExtPacket, error) {
	ep, err := GetTestExtPacket(params)
	if err != nil {
		return nil, err
	}

	ep.KeyFrame = vp8.IsKeyFrame
	ep.Payload = *vp8
	return ep, nil
}

// --------------------------------------

var TestVP8Codec = webrtc.RTPCodecCapability{
	MimeType:  "video/vp8",
	ClockRate: 90000,
}

var TestOpusCodec = webrtc.RTPCodecCapability{
	MimeType:  "audio/opus",
	ClockRate: 48000,
}

// --------------------------------------
