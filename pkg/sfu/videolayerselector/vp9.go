package videolayerselector

import (
	"github.com/pion/rtp/codecs"
	"github.com/whoyao/livekit/pkg/sfu/buffer"
	"github.com/whoyao/protocol/logger"
)

type VP9 struct {
	*Base
}

func NewVP9(logger logger.Logger) *VP9 {
	return &VP9{
		Base: NewBase(logger),
	}
}

func NewVP9FromNull(vls VideoLayerSelector) *VP9 {
	return &VP9{
		Base: vls.(*Null).Base,
	}
}

func (v *VP9) IsOvershootOkay() bool {
	return false
}

func (v *VP9) Select(extPkt *buffer.ExtPacket, _layer int32) (result VideoLayerSelectorResult) {
	vp9, ok := extPkt.Payload.(codecs.VP9Packet)
	if !ok {
		return
	}

	currentLayer := v.currentLayer
	if v.currentLayer != v.targetLayer {
		updatedLayer := v.currentLayer

		if !v.currentLayer.IsValid() {
			if !extPkt.KeyFrame {
				return
			}

			updatedLayer = extPkt.VideoLayer
		} else {
			if v.currentLayer.Temporal != v.targetLayer.Temporal {
				if v.currentLayer.Temporal < v.targetLayer.Temporal {
					// temporal scale up
					if extPkt.VideoLayer.Temporal > v.currentLayer.Temporal && extPkt.VideoLayer.Temporal <= v.targetLayer.Temporal && vp9.U && vp9.B {
						currentLayer.Temporal = extPkt.VideoLayer.Temporal
						updatedLayer.Temporal = extPkt.VideoLayer.Temporal
					}
				} else {
					// temporal scale down
					if vp9.E {
						updatedLayer.Temporal = v.targetLayer.Temporal
					}
				}
			}

			if v.currentLayer.Spatial != v.targetLayer.Spatial {
				if v.currentLayer.Spatial < v.targetLayer.Spatial {
					// spatial scale up
					if extPkt.VideoLayer.Spatial > v.currentLayer.Spatial && extPkt.VideoLayer.Spatial <= v.targetLayer.Spatial && !vp9.P && vp9.B {
						currentLayer.Spatial = extPkt.VideoLayer.Spatial
						updatedLayer.Spatial = extPkt.VideoLayer.Spatial
					}
				} else {
					// spatial scale down
					if vp9.E {
						updatedLayer.Spatial = v.targetLayer.Spatial
					}
				}
			}
		}

		if updatedLayer != v.currentLayer {
			if !v.currentLayer.IsValid() && updatedLayer.IsValid() {
				result.IsResuming = true
			}

			if v.currentLayer.Spatial != v.requestSpatial && updatedLayer.Spatial == v.requestSpatial {
				result.IsSwitchingToRequestSpatial = true
			}

			if v.currentLayer.Spatial != v.maxLayer.Spatial && updatedLayer.Spatial == v.maxLayer.Spatial {
				result.IsSwitchingToMaxSpatial = true
				v.logger.Infow(
					"reached max layer",
					"current", v.currentLayer,
					"target", v.targetLayer,
					"max", v.maxLayer,
					"layer", extPkt.VideoLayer.Spatial,
					"req", v.requestSpatial,
					"maxSeen", v.maxSeenLayer,
					"feed", extPkt.Packet.SSRC,
				)
			}

			v.currentLayer = updatedLayer
		}
	}

	result.RTPMarker = extPkt.Packet.Marker
	if vp9.E && extPkt.VideoLayer.Spatial == currentLayer.Spatial && (vp9.P || v.targetLayer.Spatial <= v.currentLayer.Spatial) {
		result.RTPMarker = true
	}
	result.IsSelected = !extPkt.VideoLayer.GreaterThan(currentLayer)
	result.IsRelevant = true
	return
}
