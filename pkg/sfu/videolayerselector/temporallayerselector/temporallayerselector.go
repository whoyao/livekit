package temporallayerselector

import "github.com/whoyao/livekit/pkg/sfu/buffer"

type TemporalLayerSelector interface {
	Select(extPkt *buffer.ExtPacket, current int32, target int32) (this int32, next int32)
}
