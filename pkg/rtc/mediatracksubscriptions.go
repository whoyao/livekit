package rtc

import (
	"errors"
	"sync"

	"github.com/pion/rtcp"
	"github.com/whoyao/webrtc/v3"
	"go.uber.org/atomic"

	"github.com/whoyao/protocol/livekit"
	"github.com/whoyao/protocol/logger"

	"github.com/whoyao/livekit/pkg/rtc/types"
	"github.com/whoyao/livekit/pkg/sfu"
	"github.com/whoyao/livekit/pkg/telemetry"
)

var (
	errAlreadySubscribed = errors.New("already subscribed")
	errNotFound          = errors.New("not found")
)

// MediaTrackSubscriptions manages subscriptions of a media track
type MediaTrackSubscriptions struct {
	params MediaTrackSubscriptionsParams

	subscribedTracksMu sync.RWMutex
	subscribedTracks   map[livekit.ParticipantID]types.SubscribedTrack

	onDownTrackCreated           func(downTrack *sfu.DownTrack)
	onSubscriberMaxQualityChange func(subscriberID livekit.ParticipantID, codec webrtc.RTPCodecCapability, layer int32)
}

type MediaTrackSubscriptionsParams struct {
	MediaTrack types.MediaTrack
	IsRelayed  bool

	ReceiverConfig   ReceiverConfig
	SubscriberConfig DirectionConfig

	Telemetry telemetry.TelemetryService

	Logger logger.Logger
}

func NewMediaTrackSubscriptions(params MediaTrackSubscriptionsParams) *MediaTrackSubscriptions {
	return &MediaTrackSubscriptions{
		params:           params,
		subscribedTracks: make(map[livekit.ParticipantID]types.SubscribedTrack),
	}
}

func (t *MediaTrackSubscriptions) OnDownTrackCreated(f func(downTrack *sfu.DownTrack)) {
	t.onDownTrackCreated = f
}

func (t *MediaTrackSubscriptions) OnSubscriberMaxQualityChange(f func(subscriberID livekit.ParticipantID, codec webrtc.RTPCodecCapability, layer int32)) {
	t.onSubscriberMaxQualityChange = f
}

func (t *MediaTrackSubscriptions) SetMuted(muted bool) {
	// update mute of all subscribed tracks
	for _, st := range t.getAllSubscribedTracks() {
		st.SetPublisherMuted(muted)
	}
}

func (t *MediaTrackSubscriptions) IsSubscriber(subID livekit.ParticipantID) bool {
	t.subscribedTracksMu.RLock()
	defer t.subscribedTracksMu.RUnlock()

	_, ok := t.subscribedTracks[subID]
	return ok
}

// AddSubscriber subscribes sub to current mediaTrack
func (t *MediaTrackSubscriptions) AddSubscriber(sub types.LocalParticipant, wr *WrappedReceiver) (types.SubscribedTrack, error) {
	trackID := t.params.MediaTrack.ID()
	subscriberID := sub.ID()

	// don't subscribe to the same track multiple times
	t.subscribedTracksMu.Lock()
	if _, ok := t.subscribedTracks[subscriberID]; ok {
		t.subscribedTracksMu.Unlock()
		return nil, errAlreadySubscribed
	}
	t.subscribedTracksMu.Unlock()

	var rtcpFeedback []webrtc.RTCPFeedback
	switch t.params.MediaTrack.Kind() {
	case livekit.TrackType_AUDIO:
		rtcpFeedback = t.params.SubscriberConfig.RTCPFeedback.Audio
	case livekit.TrackType_VIDEO:
		rtcpFeedback = t.params.SubscriberConfig.RTCPFeedback.Video
	}
	codecs := wr.Codecs()
	for _, c := range codecs {
		c.RTCPFeedback = rtcpFeedback
	}
	downTrack, err := sfu.NewDownTrack(
		codecs,
		wr,
		sub.GetBufferFactory(),
		subscriberID,
		t.params.ReceiverConfig.PacketBufferSize,
		sub.GetAllowTimestampAdjustment(),
		LoggerWithTrack(sub.GetLogger(), trackID, t.params.IsRelayed),
	)
	if err != nil {
		return nil, err
	}

	if t.onDownTrackCreated != nil {
		t.onDownTrackCreated(downTrack)
	}

	subTrack := NewSubscribedTrack(SubscribedTrackParams{
		PublisherID:       t.params.MediaTrack.PublisherID(),
		PublisherIdentity: t.params.MediaTrack.PublisherIdentity(),
		PublisherVersion:  t.params.MediaTrack.PublisherVersion(),
		Subscriber:        sub,
		MediaTrack:        t.params.MediaTrack,
		DownTrack:         downTrack,
		AdaptiveStream:    sub.GetAdaptiveStream(),
	})

	// Bind callback can happen from replaceTrack, so set it up early
	var reusingTransceiver atomic.Bool
	var dtState sfu.DownTrackState
	downTrack.OnBinding(func() {
		wr.DetermineReceiver(downTrack.Codec())
		if reusingTransceiver.Load() {
			downTrack.SeedState(dtState)
		}
		if err = wr.AddDownTrack(downTrack); err != nil && err != sfu.ErrReceiverClosed {
			sub.GetLogger().Errorw(
				"could not add down track", err,
				"publisher", subTrack.PublisherIdentity(),
				"publisherID", subTrack.PublisherID(),
				"trackID", trackID,
			)
		}

		go subTrack.Bound()

		subTrack.SetPublisherMuted(t.params.MediaTrack.IsMuted())
	})

	downTrack.OnStatsUpdate(func(_ *sfu.DownTrack, stat *livekit.AnalyticsStat) {
		key := telemetry.StatsKeyForTrack(livekit.StreamType_DOWNSTREAM, subscriberID, trackID, t.params.MediaTrack.Source(), t.params.MediaTrack.Kind())
		t.params.Telemetry.TrackStats(key, stat)
	})

	downTrack.OnMaxLayerChanged(func(dt *sfu.DownTrack, layer int32) {
		if t.onSubscriberMaxQualityChange != nil {
			t.onSubscriberMaxQualityChange(subscriberID, dt.Codec(), layer)
		}
	})

	downTrack.OnRttUpdate(func(_ *sfu.DownTrack, rtt uint32) {
		go sub.UpdateMediaRTT(rtt)
	})

	downTrack.AddReceiverReportListener(func(dt *sfu.DownTrack, report *rtcp.ReceiverReport) {
		sub.OnReceiverReport(dt, report)
	})

	var transceiver *webrtc.RTPTransceiver
	var sender *webrtc.RTPSender

	// try cached RTP senders for a chance to replace track
	var existingTransceiver *webrtc.RTPTransceiver
	replacedTrack := false
	existingTransceiver, dtState = sub.GetCachedDownTrack(trackID)
	if existingTransceiver != nil {
		reusingTransceiver.Store(true)
		rtpSender := existingTransceiver.Sender()
		if rtpSender != nil {
			err := rtpSender.ReplaceTrack(downTrack)
			if err == nil {
				sender = rtpSender
				transceiver = existingTransceiver
				replacedTrack = true
			}
		}

		if !replacedTrack {
			// Could not re-use cached transceiver for this track.
			// Stop the transceiver so that it is at least not active.
			// It is not usable once stopped,
			//
			// Adding down track will create a new transceiver (or re-use
			// an inactive existing one). In either case, a renegotiation
			// will happen and that will notify remote of this stopped
			// transceiver
			existingTransceiver.Stop()
		}
	}
	reusingTransceiver.Store(false)

	// if cannot replace, find an unused transceiver or add new one
	if transceiver == nil {
		info := t.params.MediaTrack.ToProto()
		addTrackParams := types.AddTrackParams{
			Stereo: info.Stereo,
			Red:    !info.DisableRed,
		}
		if addTrackParams.Red && (len(codecs) == 1 && codecs[0].MimeType == webrtc.MimeTypeOpus) {
			addTrackParams.Red = false
		}

		sub.VerifySubscribeParticipantInfo(subTrack.PublisherID(), subTrack.PublisherVersion())
		if sub.ProtocolVersion().SupportsTransceiverReuse() {
			//
			// AddTrack will create a new transceiver or re-use an unused one
			// if the attributes match. This prevents SDP from bloating
			// because of dormant transceivers building up.
			//
			sender, transceiver, err = sub.AddTrackToSubscriber(downTrack, addTrackParams)
			if err != nil {
				return nil, err
			}
		} else {
			sender, transceiver, err = sub.AddTransceiverFromTrackToSubscriber(downTrack, addTrackParams)
			if err != nil {
				return nil, err
			}
		}
	}

	// whether re-using or stopping remove transceiver from cache
	// NOTE: safety net, if somehow a cached transceiver is re-used by a different track
	sub.UncacheDownTrack(transceiver)

	// negotiation isn't required if we've replaced track
	subTrack.SetNeedsNegotiation(!replacedTrack)
	subTrack.SetRTPSender(sender)

	sendParameters := sender.GetParameters()
	downTrack.SetRTPHeaderExtensions(sendParameters.HeaderExtensions)

	downTrack.SetTransceiver(transceiver)

	downTrack.OnCloseHandler(func(willBeResumed bool) {
		go t.downTrackClosed(sub, willBeResumed)
	})

	t.subscribedTracksMu.Lock()
	t.subscribedTracks[subscriberID] = subTrack
	t.subscribedTracksMu.Unlock()

	return subTrack, nil
}

// RemoveSubscriber removes participant from subscription
// stop all forwarders to the client
func (t *MediaTrackSubscriptions) RemoveSubscriber(subscriberID livekit.ParticipantID, willBeResumed bool) error {
	subTrack := t.getSubscribedTrack(subscriberID)
	if subTrack == nil {
		return errNotFound
	}

	t.params.Logger.Debugw("removing subscriber", "subscriberID", subscriberID, "willBeResumed", willBeResumed)
	t.closeSubscribedTrack(subTrack, willBeResumed)
	return nil
}

func (t *MediaTrackSubscriptions) closeSubscribedTrack(subTrack types.SubscribedTrack, willBeResumed bool) {
	dt := subTrack.DownTrack()
	if dt == nil {
		return
	}

	dt.CloseWithFlush(!willBeResumed)

	if willBeResumed {
		tr := dt.GetTransceiver()
		if tr != nil {
			sub := subTrack.Subscriber()
			sub.CacheDownTrack(subTrack.ID(), tr, dt.GetState())
		}
	}
}

func (t *MediaTrackSubscriptions) ResyncAllSubscribers() {
	t.params.Logger.Debugw("resyncing all subscribers")

	for _, subTrack := range t.getAllSubscribedTracks() {
		subTrack.DownTrack().Resync()
	}
}

func (t *MediaTrackSubscriptions) GetAllSubscribers() []livekit.ParticipantID {
	t.subscribedTracksMu.RLock()
	defer t.subscribedTracksMu.RUnlock()

	subs := make([]livekit.ParticipantID, 0, len(t.subscribedTracks))
	for id := range t.subscribedTracks {
		subs = append(subs, id)
	}
	return subs
}

func (t *MediaTrackSubscriptions) GetAllSubscribersForMime(mime string) []livekit.ParticipantID {
	t.subscribedTracksMu.RLock()
	defer t.subscribedTracksMu.RUnlock()

	subs := make([]livekit.ParticipantID, 0, len(t.subscribedTracks))
	for id, subTrack := range t.subscribedTracks {
		if subTrack.DownTrack().Codec().MimeType != mime {
			continue
		}

		subs = append(subs, id)
	}
	return subs
}

func (t *MediaTrackSubscriptions) GetNumSubscribers() int {
	t.subscribedTracksMu.RLock()
	defer t.subscribedTracksMu.RUnlock()

	return len(t.subscribedTracks)
}

func (t *MediaTrackSubscriptions) UpdateVideoLayers() {
	for _, st := range t.getAllSubscribedTracks() {
		st.UpdateVideoLayer()
	}
}

func (t *MediaTrackSubscriptions) getSubscribedTrack(subscriberID livekit.ParticipantID) types.SubscribedTrack {
	t.subscribedTracksMu.RLock()
	defer t.subscribedTracksMu.RUnlock()

	return t.subscribedTracks[subscriberID]
}

func (t *MediaTrackSubscriptions) getAllSubscribedTracks() []types.SubscribedTrack {
	t.subscribedTracksMu.RLock()
	defer t.subscribedTracksMu.RUnlock()

	return t.getAllSubscribedTracksLocked()
}

func (t *MediaTrackSubscriptions) getAllSubscribedTracksLocked() []types.SubscribedTrack {
	subTracks := make([]types.SubscribedTrack, 0, len(t.subscribedTracks))
	for _, subTrack := range t.subscribedTracks {
		subTracks = append(subTracks, subTrack)
	}
	return subTracks
}

func (t *MediaTrackSubscriptions) DebugInfo() []map[string]interface{} {
	subscribedTrackInfo := make([]map[string]interface{}, 0)
	for _, val := range t.getAllSubscribedTracks() {
		if st, ok := val.(*SubscribedTrack); ok {
			dt := st.DownTrack().DebugInfo()
			dt["PubMuted"] = st.pubMuted.Load()
			dt["SubMuted"] = st.subMuted.Load()
			subscribedTrackInfo = append(subscribedTrackInfo, dt)
		}
	}

	return subscribedTrackInfo
}

func (t *MediaTrackSubscriptions) downTrackClosed(
	sub types.LocalParticipant,
	willBeResumed bool,
) {
	subscriberID := sub.ID()
	t.subscribedTracksMu.Lock()
	subTrack := t.subscribedTracks[subscriberID]
	delete(t.subscribedTracks, subscriberID)
	t.subscribedTracksMu.Unlock()

	if subTrack != nil {
		subTrack.Close(willBeResumed)
	}
}
