package sfu

import (
	"sync"

	"github.com/whoyao/protocol/livekit"
	"github.com/whoyao/protocol/logger"
	"github.com/whoyao/protocol/utils"
)

type DownTrackSpreaderParams struct {
	Threshold int
	Logger    logger.Logger
}

type DownTrackSpreader struct {
	params DownTrackSpreaderParams

	downTrackMu      sync.RWMutex
	downTracks       map[livekit.ParticipantID]TrackSender
	downTracksShadow []TrackSender
}

func NewDownTrackSpreader(params DownTrackSpreaderParams) *DownTrackSpreader {
	d := &DownTrackSpreader{
		params:     params,
		downTracks: make(map[livekit.ParticipantID]TrackSender),
	}

	return d
}

func (d *DownTrackSpreader) GetDownTracks() []TrackSender {
	d.downTrackMu.RLock()
	defer d.downTrackMu.RUnlock()

	return d.downTracksShadow
}

func (d *DownTrackSpreader) ResetAndGetDownTracks() []TrackSender {
	d.downTrackMu.Lock()
	defer d.downTrackMu.Unlock()

	downTracks := d.downTracksShadow

	d.downTracks = make(map[livekit.ParticipantID]TrackSender)
	d.downTracksShadow = nil

	return downTracks
}

func (d *DownTrackSpreader) Store(ts TrackSender) {
	d.downTrackMu.Lock()
	defer d.downTrackMu.Unlock()

	d.downTracks[ts.SubscriberID()] = ts
	d.shadowDownTracks()
}

func (d *DownTrackSpreader) Free(subscriberID livekit.ParticipantID) {
	d.downTrackMu.Lock()
	defer d.downTrackMu.Unlock()

	delete(d.downTracks, subscriberID)
	d.shadowDownTracks()
}

func (d *DownTrackSpreader) HasDownTrack(subscriberID livekit.ParticipantID) bool {
	d.downTrackMu.RLock()
	defer d.downTrackMu.RUnlock()

	_, ok := d.downTracks[subscriberID]
	return ok
}

func (d *DownTrackSpreader) Broadcast(writer func(TrackSender)) {
	downTracks := d.GetDownTracks()
	threshold := uint64(d.params.Threshold)
	if threshold == 0 {
		threshold = 1000000
	}

	// 100µs is enough to amortize the overhead and provide sufficient load balancing.
	// WriteRTP takes about 50µs on average, so we write to 2 down tracks per loop.
	step := uint64(2)
	utils.ParallelExec(downTracks, threshold, step, func(dt TrackSender) {
		writer(dt)
	})
}

func (d *DownTrackSpreader) DownTrackCount() int {
	d.downTrackMu.RLock()
	defer d.downTrackMu.RUnlock()
	return len(d.downTracksShadow)
}

func (d *DownTrackSpreader) shadowDownTracks() {
	d.downTracksShadow = make([]TrackSender, 0, len(d.downTracks))
	for _, dt := range d.downTracks {
		d.downTracksShadow = append(d.downTracksShadow, dt)
	}
}
