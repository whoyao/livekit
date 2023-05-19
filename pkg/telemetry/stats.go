package telemetry

import (
	"github.com/whoyao/livekit/pkg/telemetry/prometheus"
	"github.com/whoyao/protocol/livekit"
)

type StatsKey struct {
	streamType    livekit.StreamType
	participantID livekit.ParticipantID
	trackID       livekit.TrackID
	trackSource   livekit.TrackSource
	trackType     livekit.TrackType
	track         bool
}

func StatsKeyForTrack(streamType livekit.StreamType, participantID livekit.ParticipantID, trackID livekit.TrackID, trackSource livekit.TrackSource, trackType livekit.TrackType) StatsKey {
	return StatsKey{
		streamType:    streamType,
		participantID: participantID,
		trackID:       trackID,
		trackSource:   trackSource,
		trackType:     trackType,
		track:         true,
	}
}

func StatsKeyForData(streamType livekit.StreamType, participantID livekit.ParticipantID, trackID livekit.TrackID) StatsKey {
	return StatsKey{
		streamType:    streamType,
		participantID: participantID,
		trackID:       trackID,
	}
}

func (t *telemetryService) TrackStats(key StatsKey, stat *livekit.AnalyticsStat) {
	t.enqueue(func() {
		direction := prometheus.Incoming
		if key.streamType == livekit.StreamType_DOWNSTREAM {
			direction = prometheus.Outgoing
		}

		nacks := uint32(0)
		plis := uint32(0)
		firs := uint32(0)
		packets := uint32(0)
		bytes := uint64(0)
		retransmitBytes := uint64(0)
		retransmitPackets := uint32(0)
		for _, stream := range stat.Streams {
			nacks += stream.Nacks
			plis += stream.Plis
			firs += stream.Firs
			packets += stream.PrimaryPackets + stream.PaddingPackets
			bytes += stream.PrimaryBytes + stream.PaddingBytes
			if key.streamType == livekit.StreamType_DOWNSTREAM {
				retransmitPackets += stream.RetransmitPackets
				retransmitBytes += stream.RetransmitBytes
			} else {
				// for upstream, we don't account for these separately for now
				packets += stream.RetransmitPackets
				bytes += stream.RetransmitBytes
			}
			if key.track {
				prometheus.RecordPacketLoss(direction, key.trackSource, key.trackType, stream.PacketsLost, stream.PrimaryPackets+stream.PaddingPackets)
				prometheus.RecordRTT(direction, key.trackSource, key.trackType, stream.Rtt)
				prometheus.RecordJitter(direction, key.trackSource, key.trackType, stream.Jitter)
			}
		}
		prometheus.IncrementRTCP(direction, nacks, plis, firs)
		prometheus.IncrementPackets(direction, uint64(packets), false)
		prometheus.IncrementBytes(direction, bytes, false)
		if retransmitPackets != 0 {
			prometheus.IncrementPackets(direction, uint64(retransmitPackets), true)
		}
		if retransmitBytes != 0 {
			prometheus.IncrementBytes(direction, retransmitBytes, true)
		}

		if worker, ok := t.getWorker(key.participantID); ok {
			worker.OnTrackStat(key.trackID, key.streamType, stat)
		}
	})
}
