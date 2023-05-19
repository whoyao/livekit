package rtc

import (
	"context"
	"github.com/pion/webrtc/v3"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/pion/rtcp"
	"github.com/pion/sdp/v3"
	"github.com/pkg/errors"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/proto"

	"github.com/whoyao/livekit/pkg/config"
	"github.com/whoyao/livekit/pkg/routing"
	"github.com/whoyao/livekit/pkg/rtc/supervisor"
	"github.com/whoyao/livekit/pkg/rtc/types"
	"github.com/whoyao/livekit/pkg/sfu"
	"github.com/whoyao/livekit/pkg/sfu/buffer"
	"github.com/whoyao/livekit/pkg/sfu/connectionquality"
	"github.com/whoyao/livekit/pkg/sfu/streamallocator"
	"github.com/whoyao/livekit/pkg/telemetry"
	"github.com/whoyao/livekit/pkg/telemetry/prometheus"
	"github.com/whoyao/mediatransportutil/pkg/twcc"
	"github.com/whoyao/protocol/auth"
	"github.com/whoyao/protocol/livekit"
	"github.com/whoyao/protocol/logger"
	"github.com/whoyao/protocol/utils"
)

const (
	sdBatchSize       = 30
	rttUpdateInterval = 5 * time.Second

	disconnectCleanupDuration = 15 * time.Second
	migrationWaitDuration     = 3 * time.Second
)

type pendingTrackInfo struct {
	trackInfos []*livekit.TrackInfo
	migrated   bool
}

type downTrackState struct {
	transceiver *webrtc.RTPTransceiver
	downTrack   sfu.DownTrackState
}

type participantUpdateInfo struct {
	version   uint32
	state     livekit.ParticipantInfo_State
	updatedAt time.Time
}

type ParticipantParams struct {
	Identity                     livekit.ParticipantIdentity
	Name                         livekit.ParticipantName
	SID                          livekit.ParticipantID
	Config                       *WebRTCConfig
	Sink                         routing.MessageSink
	AudioConfig                  config.AudioConfig
	VideoConfig                  config.VideoConfig
	ProtocolVersion              types.ProtocolVersion
	Telemetry                    telemetry.TelemetryService
	PLIThrottleConfig            config.PLIThrottleConfig
	CongestionControlConfig      config.CongestionControlConfig
	EnabledCodecs                []*livekit.Codec
	Logger                       logger.Logger
	SimTracks                    map[uint32]SimulcastTrackInfo
	Grants                       *auth.ClaimGrants
	InitialVersion               uint32
	ClientConf                   *livekit.ClientConfiguration
	ClientInfo                   ClientInfo
	Region                       string
	Migration                    bool
	AdaptiveStream               bool
	AllowTCPFallback             bool
	TCPFallbackRTTThreshold      int
	AllowUDPUnstableFallback     bool
	TURNSEnabled                 bool
	GetParticipantInfo           func(pID livekit.ParticipantID) *livekit.ParticipantInfo
	ReconnectOnPublicationError  bool
	ReconnectOnSubscriptionError bool
	VersionGenerator             utils.TimedVersionGenerator
	TrackResolver                types.MediaTrackResolver
	DisableDynacast              bool
	SubscriberAllowPause         bool
	SubscriptionLimitAudio       int32
	SubscriptionLimitVideo       int32
	AllowTimestampAdjustment     bool
}

type ParticipantImpl struct {
	params ParticipantParams

	isClosed    atomic.Bool
	state       atomic.Value // livekit.ParticipantInfo_State
	resSinkMu   sync.Mutex
	resSink     routing.MessageSink
	grants      *auth.ClaimGrants
	isPublisher atomic.Bool

	// when first connected
	connectedAt time.Time
	// timer that's set when disconnect is detected on primary PC
	disconnectTimer *time.Timer
	migrationTimer  *time.Timer

	rtcpCh chan []rtcp.Packet

	// hold reference for MediaTrack
	twcc *twcc.Responder

	// client intended to publish, yet to be reconciled
	pendingTracksLock       utils.RWMutex
	pendingTracks           map[string]*pendingTrackInfo
	pendingPublishingTracks map[livekit.TrackID]*pendingTrackInfo
	// migrated in muted tracks are not fired need close at participant close
	mutedTrackNotFired []*MediaTrack

	*TransportManager
	*UpTrackManager
	*SubscriptionManager

	// keeps track of unpublished tracks in order to reuse trackID
	unpublishedTracks []*livekit.TrackInfo

	requireBroadcast bool
	// queued participant updates before join response is sent
	// guarded by updateLock
	queuedUpdates []*livekit.ParticipantInfo
	// cache of recently sent updates, to ensuring ordering by version
	// guarded by updateLock
	updateCache *lru.Cache[livekit.ParticipantID, participantUpdateInfo]
	updateLock  utils.Mutex

	dataChannelStats *telemetry.BytesTrackStats

	rttUpdatedAt time.Time
	lastRTT      uint32

	lock utils.RWMutex
	once sync.Once

	dirty        atomic.Bool
	version      atomic.Uint32
	timedVersion utils.TimedVersion

	// callbacks & handlers
	onTrackPublished     func(types.LocalParticipant, types.MediaTrack)
	onTrackUpdated       func(types.LocalParticipant, types.MediaTrack)
	onTrackUnpublished   func(types.LocalParticipant, types.MediaTrack)
	onStateChange        func(p types.LocalParticipant, oldState livekit.ParticipantInfo_State)
	onMigrateStateChange func(p types.LocalParticipant, migrateState types.MigrateState)
	onParticipantUpdate  func(types.LocalParticipant)
	onDataPacket         func(types.LocalParticipant, *livekit.DataPacket)

	migrateState atomic.Value // types.MigrateState

	onClose            func(types.LocalParticipant)
	onClaimsChanged    func(participant types.LocalParticipant)
	onICEConfigChanged func(participant types.LocalParticipant, iceConfig *livekit.ICEConfig)

	cachedDownTracks map[livekit.TrackID]*downTrackState

	supervisor *supervisor.ParticipantSupervisor

	tracksQuality map[livekit.TrackID]livekit.ConnectionQuality
}

func NewParticipant(params ParticipantParams) (*ParticipantImpl, error) {
	if params.Identity == "" {
		return nil, ErrEmptyIdentity
	}
	if params.SID == "" {
		return nil, ErrEmptyParticipantID
	}
	if params.Grants == nil || params.Grants.Video == nil {
		return nil, ErrMissingGrants
	}
	p := &ParticipantImpl{
		params:                  params,
		rtcpCh:                  make(chan []rtcp.Packet, 100),
		pendingTracks:           make(map[string]*pendingTrackInfo),
		pendingPublishingTracks: make(map[livekit.TrackID]*pendingTrackInfo),
		connectedAt:             time.Now(),
		rttUpdatedAt:            time.Now(),
		cachedDownTracks:        make(map[livekit.TrackID]*downTrackState),
		dataChannelStats: telemetry.NewBytesTrackStats(
			telemetry.BytesTrackIDForParticipantID(telemetry.BytesTrackTypeData, params.SID),
			params.SID,
			params.Telemetry),
		supervisor:    supervisor.NewParticipantSupervisor(supervisor.ParticipantSupervisorParams{Logger: params.Logger}),
		tracksQuality: make(map[livekit.TrackID]livekit.ConnectionQuality),
	}
	p.version.Store(params.InitialVersion)
	p.timedVersion.Update(params.VersionGenerator.New())
	p.migrateState.Store(types.MigrateStateInit)
	p.state.Store(livekit.ParticipantInfo_JOINING)
	p.grants = params.Grants
	p.SetResponseSink(params.Sink)

	p.supervisor.OnPublicationError(p.onPublicationError)

	var err error
	// keep last participants and when updates were sent
	if p.updateCache, err = lru.New[livekit.ParticipantID, participantUpdateInfo](128); err != nil {
		return nil, err
	}

	err = p.setupTransportManager()
	if err != nil {
		return nil, err
	}

	p.setupUpTrackManager()
	p.setupSubscriptionManager()

	return p, nil
}

func (p *ParticipantImpl) GetLogger() logger.Logger {
	return p.params.Logger
}

func (p *ParticipantImpl) GetAdaptiveStream() bool {
	return p.params.AdaptiveStream
}

func (p *ParticipantImpl) GetAllowTimestampAdjustment() bool {
	return p.params.AllowTimestampAdjustment
}

func (p *ParticipantImpl) ID() livekit.ParticipantID {
	return p.params.SID
}

func (p *ParticipantImpl) Identity() livekit.ParticipantIdentity {
	return p.params.Identity
}

func (p *ParticipantImpl) State() livekit.ParticipantInfo_State {
	return p.state.Load().(livekit.ParticipantInfo_State)
}

func (p *ParticipantImpl) ProtocolVersion() types.ProtocolVersion {
	return p.params.ProtocolVersion
}

func (p *ParticipantImpl) IsReady() bool {
	state := p.State()

	// when migrating, there is no JoinResponse, state transitions from JOINING -> ACTIVE -> DISCONNECTED
	// so JOINING is considered ready.
	if p.params.Migration {
		return state != livekit.ParticipantInfo_DISCONNECTED
	}

	// when not migrating, there is a JoinResponse, state transitions from JOINING -> JOINED -> ACTIVE -> DISCONNECTED
	return state == livekit.ParticipantInfo_JOINED || state == livekit.ParticipantInfo_ACTIVE
}

func (p *ParticipantImpl) IsDisconnected() bool {
	return p.State() == livekit.ParticipantInfo_DISCONNECTED
}

func (p *ParticipantImpl) IsIdle() bool {
	// check if there are any published tracks that are subscribed
	for _, t := range p.GetPublishedTracks() {
		if t.GetNumSubscribers() > 0 {
			return false
		}
	}

	return !p.SubscriptionManager.HasSubscriptions()
}

func (p *ParticipantImpl) ConnectedAt() time.Time {
	return p.connectedAt
}

func (p *ParticipantImpl) GetClientConfiguration() *livekit.ClientConfiguration {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.params.ClientConf
}

func (p *ParticipantImpl) GetICEConnectionType() types.ICEConnectionType {
	return p.TransportManager.GetICEConnectionType()
}

func (p *ParticipantImpl) GetBufferFactory() *buffer.Factory {
	return p.params.Config.BufferFactory
}

// SetName attaches name to the participant
func (p *ParticipantImpl) SetName(name string) {
	p.lock.Lock()
	if p.grants.Name == name {
		p.lock.Unlock()
		return
	}

	p.grants.Name = name
	p.dirty.Store(true)

	onParticipantUpdate := p.onParticipantUpdate
	onClaimsChanged := p.onClaimsChanged
	p.lock.Unlock()

	if onParticipantUpdate != nil {
		onParticipantUpdate(p)
	}
	if onClaimsChanged != nil {
		onClaimsChanged(p)
	}
}

// SetMetadata attaches metadata to the participant
func (p *ParticipantImpl) SetMetadata(metadata string) {
	p.lock.Lock()
	if p.grants.Metadata == metadata {
		p.lock.Unlock()
		return
	}

	p.grants.Metadata = metadata
	p.requireBroadcast = p.requireBroadcast || metadata != ""
	p.dirty.Store(true)

	onParticipantUpdate := p.onParticipantUpdate
	onClaimsChanged := p.onClaimsChanged
	p.lock.Unlock()

	if onParticipantUpdate != nil {
		onParticipantUpdate(p)
	}
	if onClaimsChanged != nil {
		onClaimsChanged(p)
	}
}

func (p *ParticipantImpl) ClaimGrants() *auth.ClaimGrants {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return p.grants.Clone()
}

func (p *ParticipantImpl) SetPermission(permission *livekit.ParticipantPermission) bool {
	if permission == nil {
		return false
	}
	p.lock.Lock()
	video := p.grants.Video

	if video.MatchesPermission(permission) {
		p.lock.Unlock()
		return false
	}

	video.UpdateFromPermission(permission)
	p.dirty.Store(true)

	canPublish := video.GetCanPublish()
	canSubscribe := video.GetCanSubscribe()
	onParticipantUpdate := p.onParticipantUpdate
	onClaimsChanged := p.onClaimsChanged

	isPublisher := canPublish && p.TransportManager.IsPublisherEstablished()
	p.requireBroadcast = p.requireBroadcast || isPublisher
	p.lock.Unlock()

	// publish permission has been revoked then remove offending tracks
	for _, track := range p.GetPublishedTracks() {
		if !video.GetCanPublishSource(track.Source()) {
			p.RemovePublishedTrack(track, false, false)
			if p.ProtocolVersion().SupportsUnpublish() {
				p.sendTrackUnpublished(track.ID())
			} else {
				// for older clients that don't support unpublish, mute to avoid them sending data
				p.sendTrackMuted(track.ID(), true)
			}
		}
	}

	if canSubscribe {
		// reconcile everything
		p.SubscriptionManager.queueReconcile("")
	} else {
		// revoke all subscriptions
		for _, st := range p.SubscriptionManager.GetSubscribedTracks() {
			st.MediaTrack().RemoveSubscriber(p.ID(), false)
		}
	}

	// update isPublisher attribute
	p.isPublisher.Store(isPublisher)

	if onParticipantUpdate != nil {
		onParticipantUpdate(p)
	}
	if onClaimsChanged != nil {
		onClaimsChanged(p)
	}
	return true
}

func (p *ParticipantImpl) CanSkipBroadcast() bool {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return !p.requireBroadcast
}

func (p *ParticipantImpl) ToProtoWithVersion() (*livekit.ParticipantInfo, utils.TimedVersion) {
	v := p.version.Load()
	piv := p.timedVersion.Load()
	if p.dirty.Swap(false) {
		v = p.version.Inc()
		piv = p.params.VersionGenerator.Next()
		p.timedVersion.Update(&piv)
	}

	p.lock.RLock()
	pi := &livekit.ParticipantInfo{
		Sid:         string(p.params.SID),
		Identity:    string(p.params.Identity),
		Name:        p.grants.Name,
		State:       p.State(),
		JoinedAt:    p.ConnectedAt().Unix(),
		Version:     v,
		Permission:  p.grants.Video.ToPermission(),
		Metadata:    p.grants.Metadata,
		Region:      p.params.Region,
		IsPublisher: p.IsPublisher(),
	}
	p.lock.RUnlock()
	pi.Tracks = p.UpTrackManager.ToProto()

	return pi, piv
}

func (p *ParticipantImpl) ToProto() *livekit.ParticipantInfo {
	pi, _ := p.ToProtoWithVersion()
	return pi
}

// callbacks for clients

func (p *ParticipantImpl) OnTrackPublished(callback func(types.LocalParticipant, types.MediaTrack)) {
	p.lock.Lock()
	p.onTrackPublished = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) OnTrackUnpublished(callback func(types.LocalParticipant, types.MediaTrack)) {
	p.lock.Lock()
	p.onTrackUnpublished = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) OnStateChange(callback func(p types.LocalParticipant, oldState livekit.ParticipantInfo_State)) {
	p.lock.Lock()
	p.onStateChange = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) OnMigrateStateChange(callback func(p types.LocalParticipant, state types.MigrateState)) {
	p.lock.Lock()
	p.onMigrateStateChange = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) getOnMigrateStateChange() func(p types.LocalParticipant, state types.MigrateState) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return p.onMigrateStateChange
}

func (p *ParticipantImpl) OnTrackUpdated(callback func(types.LocalParticipant, types.MediaTrack)) {
	p.lock.Lock()
	p.onTrackUpdated = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) OnParticipantUpdate(callback func(types.LocalParticipant)) {
	p.lock.Lock()
	p.onParticipantUpdate = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) OnDataPacket(callback func(types.LocalParticipant, *livekit.DataPacket)) {
	p.lock.Lock()
	p.onDataPacket = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) OnClose(callback func(types.LocalParticipant)) {
	p.lock.Lock()
	p.onClose = callback
	p.lock.Unlock()
}

func (p *ParticipantImpl) OnClaimsChanged(callback func(types.LocalParticipant)) {
	p.lock.Lock()
	p.onClaimsChanged = callback
	p.lock.Unlock()
}

// HandleOffer an offer from remote participant, used when clients make the initial connection
func (p *ParticipantImpl) HandleOffer(offer webrtc.SessionDescription) {
	p.params.Logger.Debugw("received offer", "transport", livekit.SignalTarget_PUBLISHER)
	shouldPend := false
	if p.MigrateState() == types.MigrateStateInit {
		shouldPend = true
	}

	offer = p.setCodecPreferencesForPublisher(offer)

	p.TransportManager.HandleOffer(offer, shouldPend)
}

// HandleAnswer handles a client answer response, with subscriber PC, server initiates the
// offer and client answers
func (p *ParticipantImpl) HandleAnswer(answer webrtc.SessionDescription) {
	p.params.Logger.Debugw("received answer", "transport", livekit.SignalTarget_SUBSCRIBER)

	/* from server received join request to client answer
	 * 1. server send join response & offer
	 * ... swap candidates
	 * 2. client send answer
	 */
	signalConnCost := time.Since(p.ConnectedAt()).Milliseconds()
	p.TransportManager.UpdateSignalingRTT(uint32(signalConnCost))

	p.TransportManager.HandleAnswer(answer)
}

func (p *ParticipantImpl) onPublisherAnswer(answer webrtc.SessionDescription) error {
	p.params.Logger.Debugw("sending answer", "transport", livekit.SignalTarget_PUBLISHER)
	answer = p.configurePublisherAnswer(answer)
	if err := p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Answer{
			Answer: ToProtoSessionDescription(answer),
		},
	}); err != nil {
		return err
	}

	if p.MigrateState() == types.MigrateStateSync {
		go p.handleMigrateMutedTrack()
	}
	return nil
}

func (p *ParticipantImpl) handleMigrateMutedTrack() {
	// muted track won't send rtp packet, so we add mediatrack manually
	var addedTracks []*MediaTrack
	p.pendingTracksLock.Lock()
	for cid, pti := range p.pendingTracks {
		if !pti.migrated {
			continue
		}

		if len(pti.trackInfos) > 1 {
			p.params.Logger.Warnw("too many pending migrated tracks", nil, "count", len(pti.trackInfos), "cid", cid)
		}

		ti := pti.trackInfos[0]
		if ti.Muted && ti.Type == livekit.TrackType_VIDEO {
			mt := p.addMigrateMutedTrack(cid, ti)
			if mt != nil {
				addedTracks = append(addedTracks, mt)
			} else {
				p.params.Logger.Warnw("could not find migrated muted track", nil, "cid", cid)
			}
		}
	}
	p.mutedTrackNotFired = append(p.mutedTrackNotFired, addedTracks...)

	if len(addedTracks) != 0 {
		p.dirty.Store(true)
	}
	p.pendingTracksLock.Unlock()

	// launch callbacks in goroutine since they could block.
	// callbacks handle webhooks as well as db persistence
	go func() {
		for _, t := range addedTracks {
			p.handleTrackPublished(t)
		}
	}()
}

func (p *ParticipantImpl) removeMutedTrackNotFired(mt *MediaTrack) {
	p.pendingTracksLock.Lock()
	for i, t := range p.mutedTrackNotFired {
		if t == mt {
			p.mutedTrackNotFired[i] = p.mutedTrackNotFired[len(p.mutedTrackNotFired)-1]
			p.mutedTrackNotFired = p.mutedTrackNotFired[:len(p.mutedTrackNotFired)-1]
			break
		}
	}
	p.pendingTracksLock.Unlock()
}

// AddTrack is called when client intends to publish track.
// records track details and lets client know it's ok to proceed
func (p *ParticipantImpl) AddTrack(req *livekit.AddTrackRequest) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if !p.grants.Video.GetCanPublishSource(req.Source) {
		p.params.Logger.Warnw("no permission to publish track", nil)
		return
	}

	ti := p.addPendingTrackLocked(req)
	if ti == nil {
		return
	}

	p.sendTrackPublished(req.Cid, ti)
}

func (p *ParticipantImpl) SetMigrateInfo(
	previousOffer, previousAnswer *webrtc.SessionDescription,
	mediaTracks []*livekit.TrackPublishedResponse,
	dataChannels []*livekit.DataChannelInfo,
) {
	p.pendingTracksLock.Lock()
	for _, t := range mediaTracks {
		ti := t.GetTrack()

		p.supervisor.AddPublication(livekit.TrackID(ti.Sid))
		p.supervisor.SetPublicationMute(livekit.TrackID(ti.Sid), ti.Muted)

		p.pendingTracks[t.GetCid()] = &pendingTrackInfo{trackInfos: []*livekit.TrackInfo{ti}, migrated: true}
	}
	p.pendingTracksLock.Unlock()

	p.TransportManager.SetMigrateInfo(previousOffer, previousAnswer, dataChannels)
}

func (p *ParticipantImpl) Start() {
	p.once.Do(func() {
		p.UpTrackManager.Start()
	})
}

func (p *ParticipantImpl) Close(sendLeave bool, reason types.ParticipantCloseReason) error {
	if p.isClosed.Swap(true) {
		// already closed
		return nil
	}

	p.params.Logger.Infow("participant closing", "sendLeave", sendLeave, "reason", reason.String())
	p.clearDisconnectTimer()
	p.clearMigrationTimer()

	// send leave message
	if sendLeave {
		_ = p.writeMessage(&livekit.SignalResponse{
			Message: &livekit.SignalResponse_Leave{
				Leave: &livekit.LeaveRequest{
					Reason: reason.ToDisconnectReason(),
				},
			},
		})
	}

	p.supervisor.Stop()

	p.pendingTracksLock.Lock()
	p.pendingTracks = make(map[string]*pendingTrackInfo)
	closeMutedTrack := p.mutedTrackNotFired
	p.mutedTrackNotFired = p.mutedTrackNotFired[:0]
	p.pendingTracksLock.Unlock()

	for _, t := range closeMutedTrack {
		t.Close(!sendLeave)
	}

	p.UpTrackManager.Close(!sendLeave)

	p.updateState(livekit.ParticipantInfo_DISCONNECTED)

	// ensure this is synchronized
	p.CloseSignalConnection()
	p.lock.RLock()
	onClose := p.onClose
	p.lock.RUnlock()
	if onClose != nil {
		onClose(p)
	}

	// Close peer connections without blocking participant Close. If peer connections are gathering candidates
	// Close will block.
	go func() {
		p.SubscriptionManager.Close(!sendLeave)
		p.TransportManager.Close()
	}()

	p.dataChannelStats.Report()
	return nil
}

func (p *ParticipantImpl) IsClosed() bool {
	return p.isClosed.Load()
}

// Negotiate subscriber SDP with client, if force is true, will cancel pending
// negotiate task and negotiate immediately
func (p *ParticipantImpl) Negotiate(force bool) {
	if p.MigrateState() != types.MigrateStateInit {
		p.TransportManager.NegotiateSubscriber(force)
	}
}

func (p *ParticipantImpl) clearMigrationTimer() {
	p.lock.Lock()
	if p.migrationTimer != nil {
		p.migrationTimer.Stop()
		p.migrationTimer = nil
	}
	p.lock.Unlock()
}

func (p *ParticipantImpl) MaybeStartMigration(force bool, onStart func()) bool {
	allTransportConnected := p.TransportManager.HasSubscriberEverConnected()
	if p.IsPublisher() {
		allTransportConnected = allTransportConnected && p.TransportManager.HasPublisherEverConnected()
	}
	if !force && !allTransportConnected {
		return false
	}

	if onStart != nil {
		onStart()
	}

	p.CloseSignalConnection()

	//
	// On subscriber peer connection, remote side will try ICE on both
	// pre- and post-migration ICE candidates as the migrating out
	// peer connection leaves itself open to enable transition of
	// media with as less disruption as possible.
	//
	// But, sometimes clients could delay the migration because of
	// pinging the incorrect ICE candidates. Give the remote some time
	// to try and succeed. If not, close the subscriber peer connection
	// and help the remote side to narrow down its ICE candidate pool.
	//
	p.clearMigrationTimer()

	p.lock.Lock()
	p.migrationTimer = time.AfterFunc(migrationWaitDuration, func() {
		p.clearMigrationTimer()

		if p.isClosed.Load() || p.IsDisconnected() {
			return
		}
		// TODO: change to debug once we are confident
		p.params.Logger.Infow("closing subscriber peer connection to aid migration")

		//
		// Close all down tracks before closing subscriber peer connection.
		// Closing subscriber peer connection will call `Unbind` on all down tracks.
		// DownTrack close has checks to handle the case of closing before bind.
		// So, an `Unbind` before close would bypass that logic.
		//
		p.SubscriptionManager.Close(true)

		p.TransportManager.SubscriberClose()
	})
	p.lock.Unlock()

	return true
}

func (p *ParticipantImpl) SetMigrateState(s types.MigrateState) {
	preState := p.MigrateState()
	if preState == types.MigrateStateComplete || preState == s {
		return
	}

	p.params.Logger.Debugw("SetMigrateState", "state", s)
	p.migrateState.Store(s)
	p.dirty.Store(true)

	processPendingOffer := false
	if s == types.MigrateStateSync {
		processPendingOffer = true
	}

	if s == types.MigrateStateComplete {
		p.TransportManager.ProcessPendingPublisherDataChannels()
	}

	if processPendingOffer {
		p.TransportManager.ProcessPendingPublisherOffer()
	}

	if onMigrateStateChange := p.getOnMigrateStateChange(); onMigrateStateChange != nil {
		go onMigrateStateChange(p, s)
	}
}

func (p *ParticipantImpl) MigrateState() types.MigrateState {
	return p.migrateState.Load().(types.MigrateState)
}

// ICERestart restarts subscriber ICE connections
func (p *ParticipantImpl) ICERestart(iceConfig *livekit.ICEConfig) {
	p.clearDisconnectTimer()
	p.clearMigrationTimer()

	for _, t := range p.GetPublishedTracks() {
		t.(types.LocalMediaTrack).Restart()
	}

	p.TransportManager.ICERestart(iceConfig)
}

func (p *ParticipantImpl) OnICEConfigChanged(f func(participant types.LocalParticipant, iceConfig *livekit.ICEConfig)) {
	p.lock.Lock()
	p.onICEConfigChanged = f
	p.lock.Unlock()
}

//
// signal connection methods
//

func (p *ParticipantImpl) GetAudioLevel() (level float64, active bool) {
	level = 0
	for _, pt := range p.GetPublishedTracks() {
		mediaTrack := pt.(types.LocalMediaTrack)
		if mediaTrack.Source() == livekit.TrackSource_MICROPHONE {
			tl, ta := mediaTrack.GetAudioLevel()
			if ta {
				active = true
				if tl > level {
					level = tl
				}
			}
		}
	}
	return
}

func (p *ParticipantImpl) GetConnectionQuality() *livekit.ConnectionQualityInfo {
	numTracks := 0
	minQuality := livekit.ConnectionQuality_EXCELLENT
	minScore := float32(0.0)
	numUpDrops := 0
	numDownDrops := 0

	availableTracks := make(map[livekit.TrackID]bool)

	for _, pt := range p.GetPublishedTracks() {
		numTracks++

		score, quality := pt.(types.LocalMediaTrack).GetConnectionScoreAndQuality()
		if quality < minQuality {
			// WARNING NOTE: comparing protobuf enums directly
			minQuality = quality
			minScore = score
		} else if quality == minQuality && score < minScore {
			minScore = score
		}

		p.lock.Lock()
		trackID := pt.ID()
		if prevQuality, ok := p.tracksQuality[trackID]; ok {
			// WARNING NOTE: comparing protobuf enums directly
			if prevQuality > quality {
				numUpDrops++
			}
		}
		p.tracksQuality[trackID] = quality
		p.lock.Unlock()

		availableTracks[trackID] = true
	}

	subscribedTracks := p.SubscriptionManager.GetSubscribedTracks()
	for _, subTrack := range subscribedTracks {
		numTracks++

		score, quality := subTrack.DownTrack().GetConnectionScoreAndQuality()
		if quality < minQuality {
			// WARNING NOTE: comparing protobuf enums directly
			minQuality = quality
			minScore = score
		} else if quality == minQuality && score < minScore {
			minScore = score
		}

		p.lock.Lock()
		trackID := subTrack.ID()
		if prevQuality, ok := p.tracksQuality[trackID]; ok {
			// WARNING NOTE: comparing protobuf enums directly
			if prevQuality > quality {
				numDownDrops++
			}
		}
		p.tracksQuality[trackID] = quality
		p.lock.Unlock()

		availableTracks[trackID] = true
	}

	if numTracks == 0 {
		minQuality = livekit.ConnectionQuality_EXCELLENT
		minScore = connectionquality.MaxMOS
	}

	prometheus.RecordQuality(minQuality, minScore, numUpDrops, numDownDrops)

	// remove unavailable tracks from track quality cache
	p.lock.Lock()
	for trackID := range p.tracksQuality {
		if !availableTracks[trackID] {
			delete(p.tracksQuality, trackID)
		}
	}
	p.lock.Unlock()

	return &livekit.ConnectionQualityInfo{
		ParticipantSid: string(p.ID()),
		Quality:        minQuality,
		Score:          minScore,
	}
}

func (p *ParticipantImpl) IsPublisher() bool {
	return p.isPublisher.Load()
}

func (p *ParticipantImpl) CanPublishSource(source livekit.TrackSource) bool {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.grants.Video.GetCanPublishSource(source)
}

func (p *ParticipantImpl) CanSubscribe() bool {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return p.grants.Video.GetCanSubscribe()
}

func (p *ParticipantImpl) CanPublishData() bool {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return p.grants.Video.GetCanPublishData()
}

func (p *ParticipantImpl) Hidden() bool {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return p.grants.Video.Hidden
}

func (p *ParticipantImpl) IsRecorder() bool {
	p.lock.RLock()
	defer p.lock.RUnlock()

	return p.grants.Video.Recorder
}

func (p *ParticipantImpl) VerifySubscribeParticipantInfo(pID livekit.ParticipantID, version uint32) {
	if !p.IsReady() {
		// we have not sent a JoinResponse yet. metadata would be covered in JoinResponse
		return
	}
	if info, ok := p.updateCache.Get(pID); ok && info.version >= version {
		return
	}

	if f := p.params.GetParticipantInfo; f != nil {
		if info := f(pID); info != nil {
			_ = p.SendParticipantUpdate([]*livekit.ParticipantInfo{info})
		}
	}
}

// onTrackSubscribed handles post-processing after a track is subscribed
func (p *ParticipantImpl) onTrackSubscribed(subTrack types.SubscribedTrack) {
	if p.params.ClientInfo.FireTrackByRTPPacket() {
		subTrack.DownTrack().SetActivePaddingOnMuteUpTrack()
	}

	subTrack.AddOnBind(func() {
		if p.TransportManager.HasSubscriberEverConnected() {
			subTrack.DownTrack().SetConnected()
		}
		p.TransportManager.AddSubscribedTrack(subTrack)
	})
}

// onTrackUnsubscribed handles post-processing after a track is unsubscribed
func (p *ParticipantImpl) onTrackUnsubscribed(subTrack types.SubscribedTrack) {
	p.TransportManager.RemoveSubscribedTrack(subTrack)
}

func (p *ParticipantImpl) SubscriptionPermissionUpdate(publisherID livekit.ParticipantID, trackID livekit.TrackID, allowed bool) {
	p.params.Logger.Debugw("sending subscription permission update", "publisherID", publisherID, "trackID", trackID, "allowed", allowed)
	err := p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_SubscriptionPermissionUpdate{
			SubscriptionPermissionUpdate: &livekit.SubscriptionPermissionUpdate{
				ParticipantSid: string(publisherID),
				TrackSid:       string(trackID),
				Allowed:        allowed,
			},
		},
	})
	if err != nil {
		p.params.Logger.Errorw("could not send subscription permission update", err)
	}
}

func (p *ParticipantImpl) UpdateMediaRTT(rtt uint32) {
	now := time.Now()
	p.lock.Lock()
	if now.Sub(p.rttUpdatedAt) < rttUpdateInterval || p.lastRTT == rtt {
		p.lock.Unlock()
		return
	}
	p.rttUpdatedAt = now
	p.lastRTT = rtt
	p.lock.Unlock()
	p.TransportManager.UpdateMediaRTT(rtt)

	for _, pt := range p.GetPublishedTracks() {
		pt.(types.LocalMediaTrack).SetRTT(rtt)
	}
}

func (p *ParticipantImpl) setupTransportManager() error {
	tm, err := NewTransportManager(TransportManagerParams{
		Identity: p.params.Identity,
		SID:      p.params.SID,
		// primary connection does not change, canSubscribe can change if permission was updated
		// after the participant has joined
		SubscriberAsPrimary:      p.ProtocolVersion().SubscriberAsPrimary() && p.CanSubscribe(),
		Config:                   p.params.Config,
		ProtocolVersion:          p.params.ProtocolVersion,
		Telemetry:                p.params.Telemetry,
		CongestionControlConfig:  p.params.CongestionControlConfig,
		EnabledCodecs:            p.params.EnabledCodecs,
		SimTracks:                p.params.SimTracks,
		ClientConf:               p.params.ClientConf,
		ClientInfo:               p.params.ClientInfo,
		Migration:                p.params.Migration,
		AllowTCPFallback:         p.params.AllowTCPFallback,
		TCPFallbackRTTThreshold:  p.params.TCPFallbackRTTThreshold,
		AllowUDPUnstableFallback: p.params.AllowUDPUnstableFallback,
		TURNSEnabled:             p.params.TURNSEnabled,
		Logger:                   p.params.Logger,
	})
	if err != nil {
		return err
	}

	tm.OnICEConfigChanged(func(iceConfig *livekit.ICEConfig) {
		p.lock.Lock()
		onICEConfigChanged := p.onICEConfigChanged

		if p.params.ClientConf == nil {
			p.params.ClientConf = &livekit.ClientConfiguration{}
		}
		if iceConfig.PreferenceSubscriber == livekit.ICECandidateType_ICT_TLS {
			p.params.ClientConf.ForceRelay = livekit.ClientConfigSetting_ENABLED
		} else {
			// UNSET indicates that clients could override RTCConfiguration to forceRelay
			p.params.ClientConf.ForceRelay = livekit.ClientConfigSetting_UNSET
		}
		p.lock.Unlock()

		if onICEConfigChanged != nil {
			onICEConfigChanged(p, iceConfig)
		}
	})

	tm.OnPublisherICECandidate(func(c *webrtc.ICECandidate) error {
		return p.onICECandidate(c, livekit.SignalTarget_PUBLISHER)
	})
	tm.OnPublisherAnswer(p.onPublisherAnswer)
	tm.OnPublisherTrack(p.onMediaTrack)
	tm.OnPublisherInitialConnected(p.onPublisherInitialConnected)

	tm.OnSubscriberOffer(p.onSubscriberOffer)
	tm.OnSubscriberICECandidate(func(c *webrtc.ICECandidate) error {
		return p.onICECandidate(c, livekit.SignalTarget_SUBSCRIBER)
	})
	tm.OnSubscriberInitialConnected(p.onSubscriberInitialConnected)
	tm.OnSubscriberStreamStateChange(p.onStreamStateChange)

	tm.OnPrimaryTransportInitialConnected(p.onPrimaryTransportInitialConnected)
	tm.OnPrimaryTransportFullyEstablished(p.onPrimaryTransportFullyEstablished)
	tm.OnAnyTransportFailed(p.onAnyTransportFailed)
	tm.OnAnyTransportNegotiationFailed(p.onAnyTransportNegotiationFailed)

	tm.OnDataMessage(p.onDataMessage)

	tm.SetSubscriberAllowPause(p.params.SubscriberAllowPause)
	p.TransportManager = tm
	return nil
}

func (p *ParticipantImpl) setupUpTrackManager() {
	p.UpTrackManager = NewUpTrackManager(UpTrackManagerParams{
		SID:              p.params.SID,
		Logger:           p.params.Logger,
		VersionGenerator: p.params.VersionGenerator,
	})

	p.UpTrackManager.OnPublishedTrackUpdated(func(track types.MediaTrack) {
		p.lock.RLock()
		onTrackUpdated := p.onTrackUpdated
		p.lock.RUnlock()

		p.dirty.Store(true)
		if onTrackUpdated != nil {
			onTrackUpdated(p, track)
		}
	})

	p.UpTrackManager.OnUpTrackManagerClose(p.onUpTrackManagerClose)
}

func (p *ParticipantImpl) setupSubscriptionManager() {
	p.SubscriptionManager = NewSubscriptionManager(SubscriptionManagerParams{
		Participant:            p,
		Logger:                 p.params.Logger.WithoutSampler(),
		TrackResolver:          p.params.TrackResolver,
		Telemetry:              p.params.Telemetry,
		OnTrackSubscribed:      p.onTrackSubscribed,
		OnTrackUnsubscribed:    p.onTrackUnsubscribed,
		OnSubscriptionError:    p.onSubscriptionError,
		SubscriptionLimitVideo: p.params.SubscriptionLimitVideo,
		SubscriptionLimitAudio: p.params.SubscriptionLimitAudio,
	})
}

func (p *ParticipantImpl) updateState(state livekit.ParticipantInfo_State) {
	oldState := p.State()
	if state == oldState {
		return
	}

	p.params.Logger.Debugw("updating participant state", "state", state.String())
	p.state.Store(state)
	p.dirty.Store(true)

	p.lock.RLock()
	onStateChange := p.onStateChange
	p.lock.RUnlock()
	if onStateChange != nil {
		go func() {
			defer func() {
				if r := Recover(p.GetLogger()); r != nil {
					os.Exit(1)
				}
			}()
			onStateChange(p, oldState)
		}()
	}
}

func (p *ParticipantImpl) setIsPublisher(isPublisher bool) {
	if p.isPublisher.Swap(isPublisher) != isPublisher {
		p.lock.Lock()
		p.requireBroadcast = true
		p.lock.Unlock()

		p.dirty.Store(true)

		// trigger update as well if participant is already fully connected
		if p.State() == livekit.ParticipantInfo_ACTIVE {
			p.lock.RLock()
			onParticipantUpdate := p.onParticipantUpdate
			p.lock.RUnlock()

			if onParticipantUpdate != nil {
				onParticipantUpdate(p)
			}
		}
	}
}

// when the server has an offer for participant
func (p *ParticipantImpl) onSubscriberOffer(offer webrtc.SessionDescription) error {
	p.params.Logger.Debugw("sending offer", "transport", livekit.SignalTarget_SUBSCRIBER)
	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Offer{
			Offer: ToProtoSessionDescription(offer),
		},
	})
}

// when a new remoteTrack is created, creates a Track and adds it to room
func (p *ParticipantImpl) onMediaTrack(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {
	if p.IsDisconnected() {
		return
	}

	publishedTrack, isNewTrack := p.mediaTrackReceived(track, rtpReceiver)
	if publishedTrack == nil {
		p.params.Logger.Warnw("webrtc Track published but can't find MediaTrack", nil,
			"kind", track.Kind().String(),
			"webrtcTrackID", track.ID(),
			"rid", track.RID(),
			"SSRC", track.SSRC(),
			"mime", track.Codec().MimeType,
		)
		return
	}

	if !p.CanPublishSource(publishedTrack.Source()) {
		p.params.Logger.Warnw("no permission to publish mediaTrack", nil,
			"source", publishedTrack.Source(),
		)
		return
	}

	if !p.IsPublisher() {
		p.setIsPublisher(true)
	}

	p.params.Logger.Infow("mediaTrack published",
		"kind", track.Kind().String(),
		"trackID", publishedTrack.ID(),
		"webrtcTrackID", track.ID(),
		"rid", track.RID(),
		"SSRC", track.SSRC(),
		"mime", track.Codec().MimeType,
	)

	p.dirty.Store(true)

	if !isNewTrack && !publishedTrack.HasPendingCodec() && p.IsReady() {
		p.lock.RLock()
		onTrackUpdated := p.onTrackUpdated
		p.lock.RUnlock()
		if onTrackUpdated != nil {
			onTrackUpdated(p, publishedTrack)
		}
	}
}

func (p *ParticipantImpl) onDataMessage(kind livekit.DataPacket_Kind, data []byte) {
	if p.IsDisconnected() || !p.CanPublishData() {
		return
	}

	p.dataChannelStats.AddBytes(uint64(len(data)), false)

	dp := livekit.DataPacket{}
	if err := proto.Unmarshal(data, &dp); err != nil {
		p.params.Logger.Warnw("could not parse data packet", err)
		return
	}

	// trust the channel that it came in as the source of truth
	dp.Kind = kind

	// only forward on user payloads
	switch payload := dp.Value.(type) {
	case *livekit.DataPacket_User:
		p.lock.RLock()
		onDataPacket := p.onDataPacket
		p.lock.RUnlock()
		if onDataPacket != nil {
			payload.User.ParticipantSid = string(p.params.SID)
			onDataPacket(p, &dp)
		}
	default:
		p.params.Logger.Warnw("received unsupported data packet", nil, "payload", payload)
	}

	if !p.IsPublisher() {
		p.setIsPublisher(true)
	}
}

func (p *ParticipantImpl) onICECandidate(c *webrtc.ICECandidate, target livekit.SignalTarget) error {
	if c == nil || p.IsDisconnected() {
		return nil
	}

	if target == livekit.SignalTarget_SUBSCRIBER && p.MigrateState() == types.MigrateStateInit {
		return nil
	}

	return p.sendICECandidate(c, target)
}

func (p *ParticipantImpl) onPublisherInitialConnected() {
	p.supervisor.SetPublisherPeerConnectionConnected(true)
	go p.publisherRTCPWorker()
}

func (p *ParticipantImpl) onSubscriberInitialConnected() {
	go p.subscriberRTCPWorker()

	p.setDowntracksConnected()
}

func (p *ParticipantImpl) onPrimaryTransportInitialConnected() {
	if !p.hasPendingMigratedTrack() && p.MigrateState() == types.MigrateStateSync {
		p.SetMigrateState(types.MigrateStateComplete)
	}
}

func (p *ParticipantImpl) onPrimaryTransportFullyEstablished() {
	p.updateState(livekit.ParticipantInfo_ACTIVE)
}

func (p *ParticipantImpl) clearDisconnectTimer() {
	p.lock.Lock()
	if p.disconnectTimer != nil {
		p.disconnectTimer.Stop()
		p.disconnectTimer = nil
	}
	p.lock.Unlock()
}

func (p *ParticipantImpl) setupDisconnectTimer() {
	p.clearDisconnectTimer()

	p.lock.Lock()
	p.disconnectTimer = time.AfterFunc(disconnectCleanupDuration, func() {
		p.clearDisconnectTimer()

		if p.isClosed.Load() || p.IsDisconnected() {
			return
		}
		p.params.Logger.Infow("closing disconnected participant")
		_ = p.Close(true, types.ParticipantCloseReasonPeerConnectionDisconnected)
	})
	p.lock.Unlock()
}

func (p *ParticipantImpl) onAnyTransportFailed() {
	// clients support resuming of connections when websocket becomes disconnected
	p.CloseSignalConnection()

	// detect when participant has actually left.
	p.setupDisconnectTimer()
}

// subscriberRTCPWorker sends SenderReports periodically when the participant is subscribed to
// other publishedTracks in the room.
func (p *ParticipantImpl) subscriberRTCPWorker() {
	defer func() {
		if r := Recover(p.GetLogger()); r != nil {
			os.Exit(1)
		}
	}()
	for {
		if p.IsDisconnected() {
			return
		}

		subscribedTracks := p.SubscriptionManager.GetSubscribedTracks()

		// send in batches of sdBatchSize
		batchSize := 0
		var pkts []rtcp.Packet
		var sd []rtcp.SourceDescriptionChunk
		for _, subTrack := range subscribedTracks {
			sr := subTrack.DownTrack().CreateSenderReport()
			chunks := subTrack.DownTrack().CreateSourceDescriptionChunks()
			if sr == nil || chunks == nil {
				continue
			}

			pkts = append(pkts, sr)
			sd = append(sd, chunks...)
			batchSize = batchSize + 1 + len(chunks)
			if batchSize >= sdBatchSize {
				if len(sd) != 0 {
					pkts = append(pkts, &rtcp.SourceDescription{Chunks: sd})
				}
				if err := p.TransportManager.WriteSubscriberRTCP(pkts); err != nil {
					if err == io.EOF || err == io.ErrClosedPipe {
						return
					}
					p.params.Logger.Errorw("could not send down track reports", err)
				}

				pkts = pkts[:0]
				sd = sd[:0]
				batchSize = 0
			}
		}

		if len(pkts) != 0 || len(sd) != 0 {
			if len(sd) != 0 {
				pkts = append(pkts, &rtcp.SourceDescription{Chunks: sd})
			}
			if err := p.TransportManager.WriteSubscriberRTCP(pkts); err != nil {
				if err == io.EOF || err == io.ErrClosedPipe {
					return
				}
				p.params.Logger.Errorw("could not send down track reports", err)
			}
		}

		time.Sleep(3 * time.Second)
	}
}

func (p *ParticipantImpl) onStreamStateChange(update *streamallocator.StreamStateUpdate) error {
	if len(update.StreamStates) == 0 {
		return nil
	}

	streamStateUpdate := &livekit.StreamStateUpdate{}
	for _, streamStateInfo := range update.StreamStates {
		state := livekit.StreamState_ACTIVE
		if streamStateInfo.State == streamallocator.StreamStatePaused {
			state = livekit.StreamState_PAUSED
		}
		streamStateUpdate.StreamStates = append(streamStateUpdate.StreamStates, &livekit.StreamStateInfo{
			ParticipantSid: string(streamStateInfo.ParticipantID),
			TrackSid:       string(streamStateInfo.TrackID),
			State:          state,
		})
	}

	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_StreamStateUpdate{
			StreamStateUpdate: streamStateUpdate,
		},
	})
}

func (p *ParticipantImpl) onSubscribedMaxQualityChange(trackID livekit.TrackID, subscribedQualities []*livekit.SubscribedCodec, maxSubscribedQualities []types.SubscribedCodecQuality) error {
	if p.params.DisableDynacast {
		return nil
	}

	if len(subscribedQualities) == 0 {
		return nil
	}

	// normalize the codec name
	for _, subscribedQuality := range subscribedQualities {
		subscribedQuality.Codec = strings.ToLower(strings.TrimLeft(subscribedQuality.Codec, "video/"))
	}

	subscribedQualityUpdate := &livekit.SubscribedQualityUpdate{
		TrackSid:            string(trackID),
		SubscribedQualities: subscribedQualities[0].Qualities, // for compatible with old client
		SubscribedCodecs:    subscribedQualities,
	}

	// send layer info about max subscription changes to telemetry
	track := p.UpTrackManager.GetPublishedTrack(trackID)
	var layerInfo map[livekit.VideoQuality]*livekit.VideoLayer
	if track != nil {
		layers := track.ToProto().Layers
		layerInfo = make(map[livekit.VideoQuality]*livekit.VideoLayer, len(layers))
		for _, layer := range layers {
			layerInfo[layer.Quality] = layer
		}
	}

	for _, maxSubscribedQuality := range maxSubscribedQualities {
		ti := &livekit.TrackInfo{
			Sid:  string(trackID),
			Type: livekit.TrackType_VIDEO,
		}
		if info, ok := layerInfo[maxSubscribedQuality.Quality]; ok {
			ti.Width = info.Width
			ti.Height = info.Height
		}

		p.params.Telemetry.TrackMaxSubscribedVideoQuality(
			context.Background(),
			p.ID(),
			ti,
			maxSubscribedQuality.CodecMime,
			maxSubscribedQuality.Quality,
		)
	}

	p.params.Logger.Infow(
		"sending max subscribed quality",
		"trackID", trackID,
		"qualities", subscribedQualities,
		"max", maxSubscribedQualities,
	)
	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_SubscribedQualityUpdate{
			SubscribedQualityUpdate: subscribedQualityUpdate,
		},
	})
}

func (p *ParticipantImpl) addPendingTrackLocked(req *livekit.AddTrackRequest) *livekit.TrackInfo {
	p.pendingTracksLock.Lock()
	defer p.pendingTracksLock.Unlock()

	if req.Sid != "" {
		track := p.GetPublishedTrack(livekit.TrackID(req.Sid))
		if track == nil {
			p.params.Logger.Infow("could not find existing track for multi-codec simulcast", "trackID", req.Sid)
			return nil
		}

		track.(*MediaTrack).SetPendingCodecSid(req.SimulcastCodecs)
		ti := track.ToProto()
		return ti
	}

	ti := &livekit.TrackInfo{
		Type:       req.Type,
		Name:       req.Name,
		Width:      req.Width,
		Height:     req.Height,
		Muted:      req.Muted,
		DisableDtx: req.DisableDtx,
		Source:     req.Source,
		Layers:     req.Layers,
		DisableRed: req.DisableRed,
		Stereo:     req.Stereo,
		Encryption: req.Encryption,
	}
	p.setStableTrackID(req.Cid, ti)
	for _, codec := range req.SimulcastCodecs {
		mime := codec.Codec
		if req.Type == livekit.TrackType_VIDEO && !strings.HasPrefix(mime, "video/") {
			mime = "video/" + mime
		} else if req.Type == livekit.TrackType_AUDIO && !strings.HasPrefix(mime, "audio/") {
			mime = "audio/" + mime
		}
		if IsCodecEnabled(p.params.EnabledCodecs, webrtc.RTPCodecCapability{MimeType: mime}) {
			ti.Codecs = append(ti.Codecs, &livekit.SimulcastCodecInfo{
				MimeType: mime,
				Cid:      codec.Cid,
			})
		}
	}

	p.params.Telemetry.TrackPublishRequested(context.Background(), p.ID(), p.Identity(), ti)
	p.supervisor.AddPublication(livekit.TrackID(ti.Sid))
	p.supervisor.SetPublicationMute(livekit.TrackID(ti.Sid), ti.Muted)
	if p.getPublishedTrackBySignalCid(req.Cid) != nil || p.getPublishedTrackBySdpCid(req.Cid) != nil || p.pendingTracks[req.Cid] != nil {
		if p.pendingTracks[req.Cid] == nil {
			p.pendingTracks[req.Cid] = &pendingTrackInfo{trackInfos: []*livekit.TrackInfo{ti}}
		} else {
			p.pendingTracks[req.Cid].trackInfos = append(p.pendingTracks[req.Cid].trackInfos, ti)
		}
		p.params.Logger.Infow("pending track queued", "trackID", ti.Sid, "track", ti.String(), "request", req.String())
		return nil
	}

	p.pendingTracks[req.Cid] = &pendingTrackInfo{trackInfos: []*livekit.TrackInfo{ti}}
	p.params.Logger.Infow("pending track added", "trackID", ti.Sid, "track", ti.String(), "request", req.String())
	return ti
}

func (p *ParticipantImpl) sendTrackPublished(cid string, ti *livekit.TrackInfo) {
	p.params.Logger.Debugw("sending track published", "cid", cid, "trackInfo", ti.String())
	_ = p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_TrackPublished{
			TrackPublished: &livekit.TrackPublishedResponse{
				Cid:   cid,
				Track: ti,
			},
		},
	})
}

func (p *ParticipantImpl) SetTrackMuted(trackID livekit.TrackID, muted bool, fromAdmin bool) {
	// when request is coming from admin, send message to current participant
	if fromAdmin {
		p.sendTrackMuted(trackID, muted)
	}

	p.setTrackMuted(trackID, muted)
}

func (p *ParticipantImpl) setTrackMuted(trackID livekit.TrackID, muted bool) {
	p.dirty.Store(true)
	p.supervisor.SetPublicationMute(trackID, muted)

	track := p.UpTrackManager.SetPublishedTrackMuted(trackID, muted)
	var trackInfo *livekit.TrackInfo
	if track != nil {
		trackInfo = track.ToProto()
	}

	isPending := false
	p.pendingTracksLock.RLock()
	for _, pti := range p.pendingTracks {
		for _, ti := range pti.trackInfos {
			if livekit.TrackID(ti.Sid) == trackID {
				ti.Muted = muted
				isPending = true
				trackInfo = ti
			}
		}
	}
	p.pendingTracksLock.RUnlock()

	if trackInfo != nil {
		if muted {
			p.params.Telemetry.TrackMuted(context.Background(), p.ID(), trackInfo)
		} else {
			p.params.Telemetry.TrackUnmuted(context.Background(), p.ID(), trackInfo)
		}
	}

	if !isPending && track == nil {
		p.params.Logger.Warnw("could not locate track", nil, "trackID", trackID)
	}
}

func (p *ParticipantImpl) mediaTrackReceived(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) (*MediaTrack, bool) {
	p.pendingTracksLock.Lock()
	newTrack := false

	p.params.Logger.Debugw(
		"media track received",
		"kind", track.Kind().String(),
		"trackID", track.ID(),
		"rid", track.RID(),
		"SSRC", track.SSRC(),
		"mime", track.Codec().MimeType,
	)
	mid := p.TransportManager.GetPublisherMid(rtpReceiver)
	if mid == "" {
		p.params.Logger.Warnw("could not get mid for track", nil, "trackID", track.ID())
		return nil, false
	}

	// use existing media track to handle simulcast
	mt, ok := p.getPublishedTrackBySdpCid(track.ID()).(*MediaTrack)
	if !ok {
		signalCid, ti := p.getPendingTrack(track.ID(), ToProtoTrackKind(track.Kind()))
		if ti == nil {
			p.pendingTracksLock.Unlock()
			return nil, false
		}

		ti.MimeType = track.Codec().MimeType
		mt = p.addMediaTrack(signalCid, track.ID(), ti)
		newTrack = true
		p.dirty.Store(true)
	}

	ssrc := uint32(track.SSRC())
	if p.twcc == nil {
		p.twcc = twcc.NewTransportWideCCResponder(ssrc)
		p.twcc.OnFeedback(func(pkt rtcp.RawPacket) {
			p.postRtcp([]rtcp.Packet{&pkt})
		})
	}
	p.pendingTracksLock.Unlock()

	if mt.AddReceiver(rtpReceiver, track, p.twcc, mid) {
		p.removeMutedTrackNotFired(mt)
		if newTrack {
			go p.handleTrackPublished(mt)
		}
	}

	return mt, newTrack
}

func (p *ParticipantImpl) addMigrateMutedTrack(cid string, ti *livekit.TrackInfo) *MediaTrack {
	p.params.Logger.Debugw("add migrate muted track", "cid", cid, "track", ti.String())
	rtpReceiver := p.TransportManager.GetPublisherRTPReceiver(ti.Mid)
	if rtpReceiver == nil {
		p.params.Logger.Errorw("could not find receiver for migrated track", nil, "track", ti.Sid)
		return nil
	}

	mt := p.addMediaTrack(cid, cid, ti)

	potentialCodecs := make([]webrtc.RTPCodecParameters, 0, len(ti.Codecs))
	parameters := rtpReceiver.GetParameters()
	for _, c := range ti.Codecs {
		for _, nc := range parameters.Codecs {
			if strings.EqualFold(nc.MimeType, c.MimeType) {
				potentialCodecs = append(potentialCodecs, nc)
				break
			}
		}
	}
	mt.SetPotentialCodecs(potentialCodecs, parameters.HeaderExtensions)

	for _, codec := range ti.Codecs {
		for ssrc, info := range p.params.SimTracks {
			if info.Mid == codec.Mid {
				mt.MediaTrackReceiver.SetLayerSsrc(codec.MimeType, info.Rid, ssrc)
			}
		}
	}
	mt.SetSimulcast(ti.Simulcast)
	mt.SetMuted(true)

	return mt
}

func (p *ParticipantImpl) addMediaTrack(signalCid string, sdpCid string, ti *livekit.TrackInfo) *MediaTrack {
	mt := NewMediaTrack(MediaTrackParams{
		TrackInfo:           proto.Clone(ti).(*livekit.TrackInfo),
		SignalCid:           signalCid,
		SdpCid:              sdpCid,
		ParticipantID:       p.params.SID,
		ParticipantIdentity: p.params.Identity,
		ParticipantVersion:  p.version.Load(),
		RTCPChan:            p.rtcpCh,
		BufferFactory:       p.params.Config.BufferFactory,
		ReceiverConfig:      p.params.Config.Receiver,
		AudioConfig:         p.params.AudioConfig,
		VideoConfig:         p.params.VideoConfig,
		Telemetry:           p.params.Telemetry,
		Logger:              LoggerWithTrack(p.params.Logger, livekit.TrackID(ti.Sid), false),
		SubscriberConfig:    p.params.Config.Subscriber,
		PLIThrottleConfig:   p.params.PLIThrottleConfig,
		SimTracks:           p.params.SimTracks,
	})

	mt.OnSubscribedMaxQualityChange(p.onSubscribedMaxQualityChange)

	// add to published and clean up pending
	p.supervisor.SetPublishedTrack(livekit.TrackID(ti.Sid), mt)
	p.UpTrackManager.AddPublishedTrack(mt)

	pti := p.pendingTracks[signalCid]
	if pti != nil {
		if p.pendingPublishingTracks[livekit.TrackID(ti.Sid)] != nil {
			p.params.Logger.Infow("unexpected pending publish track", "trackID", ti.Sid)
		}
		p.pendingPublishingTracks[livekit.TrackID(ti.Sid)] = &pendingTrackInfo{
			trackInfos: []*livekit.TrackInfo{pti.trackInfos[0]},
			migrated:   pti.migrated,
		}
	}

	p.pendingTracks[signalCid].trackInfos = p.pendingTracks[signalCid].trackInfos[1:]
	if len(p.pendingTracks[signalCid].trackInfos) == 0 {
		delete(p.pendingTracks, signalCid)
	}

	trackID := livekit.TrackID(ti.Sid)
	mt.AddOnClose(func() {
		p.supervisor.ClearPublishedTrack(trackID, mt)

		// not logged when closing
		p.params.Telemetry.TrackUnpublished(
			context.Background(),
			p.ID(),
			p.Identity(),
			mt.ToProto(),
			!p.IsClosed(),
		)

		// re-use Track sid
		p.pendingTracksLock.Lock()
		if pti := p.pendingTracks[signalCid]; pti != nil {
			p.sendTrackPublished(signalCid, pti.trackInfos[0])
		} else {
			p.unpublishedTracks = append(p.unpublishedTracks, ti)
		}
		p.pendingTracksLock.Unlock()

		p.dirty.Store(true)

		if !p.IsClosed() {
			// unpublished events aren't necessary when participant is closed
			p.params.Logger.Infow("unpublished track", "trackID", ti.Sid, "trackInfo", ti)
			p.lock.RLock()
			onTrackUnpublished := p.onTrackUnpublished
			p.lock.RUnlock()
			if onTrackUnpublished != nil {
				onTrackUnpublished(p, mt)
			}
		}
	})

	return mt
}

func (p *ParticipantImpl) handleTrackPublished(track types.MediaTrack) {
	p.lock.RLock()
	onTrackPublished := p.onTrackPublished
	p.lock.RUnlock()
	if onTrackPublished != nil {
		onTrackPublished(p, track)
	}

	// send webhook after callbacks are complete, persistence and state handling happens
	// in `onTrackPublished` cb
	p.params.Telemetry.TrackPublished(
		context.Background(),
		p.ID(),
		p.Identity(),
		track.ToProto(),
	)

	p.pendingTracksLock.Lock()
	delete(p.pendingPublishingTracks, track.ID())
	p.pendingTracksLock.Unlock()

	if !p.hasPendingMigratedTrack() {
		p.SetMigrateState(types.MigrateStateComplete)
	}
}

func (p *ParticipantImpl) hasPendingMigratedTrack() bool {
	p.pendingTracksLock.RLock()
	defer p.pendingTracksLock.RUnlock()

	for _, t := range p.pendingTracks {
		if t.migrated {
			return true
		}
	}

	for _, t := range p.pendingPublishingTracks {
		if t.migrated {
			return true
		}
	}

	return false
}

func (p *ParticipantImpl) onUpTrackManagerClose() {
	p.postRtcp(nil)
}

func (p *ParticipantImpl) getPendingTrack(clientId string, kind livekit.TrackType) (string, *livekit.TrackInfo) {
	signalCid := clientId
	pendingInfo := p.pendingTracks[clientId]
	if pendingInfo == nil {
	track_loop:
		for cid, pti := range p.pendingTracks {

			ti := pti.trackInfos[0]
			for _, c := range ti.Codecs {
				if c.Cid == clientId {
					pendingInfo = pti
					signalCid = cid
					break track_loop
				}
			}
		}

		if pendingInfo == nil {
			//
			// If no match on client id, find first one matching type
			// as MediaStreamTrack can change client id when transceiver
			// is added to peer connection.
			//
			for cid, pti := range p.pendingTracks {
				ti := pti.trackInfos[0]
				if ti.Type == kind {
					pendingInfo = pti
					signalCid = cid
					break
				}
			}
		}
	}

	// if still not found, we are done
	if pendingInfo == nil {
		p.params.Logger.Errorw("track info not published prior to track", nil, "clientId", clientId)
		return signalCid, nil
	}

	return signalCid, pendingInfo.trackInfos[0]
}

// setStableTrackID either generates a new TrackID or reuses a previously used one
// for
func (p *ParticipantImpl) setStableTrackID(cid string, info *livekit.TrackInfo) {
	var trackID string
	// if already pending, use the same SID
	// should not happen as this means multiple `AddTrack` requests have been called, but check anyway
	if pti := p.pendingTracks[cid]; pti != nil {
		trackID = pti.trackInfos[0].Sid
	}

	// check against published tracks as re-publish could be happening
	if trackID == "" {
		if pt := p.getPublishedTrackBySignalCid(cid); pt != nil {
			ti := pt.ToProto()
			if ti.Type == info.Type && ti.Source == info.Source && ti.Name == info.Name {
				trackID = ti.Sid
			}
		}
	}

	if trackID == "" {
		// check a previously published matching track
		for i, ti := range p.unpublishedTracks {
			if ti.Type == info.Type && ti.Source == info.Source && ti.Name == info.Name {
				trackID = ti.Sid
				if i < len(p.unpublishedTracks)-1 {
					p.unpublishedTracks = append(p.unpublishedTracks[:i], p.unpublishedTracks[i+1:]...)
				} else {
					p.unpublishedTracks = p.unpublishedTracks[:i]
				}
				break
			}
		}
	}

	// otherwise generate
	if trackID == "" {
		trackPrefix := utils.TrackPrefix
		if info.Type == livekit.TrackType_VIDEO {
			trackPrefix += "V"
		} else if info.Type == livekit.TrackType_AUDIO {
			trackPrefix += "A"
		}
		switch info.Source {
		case livekit.TrackSource_CAMERA:
			trackPrefix += "C"
		case livekit.TrackSource_MICROPHONE:
			trackPrefix += "M"
		case livekit.TrackSource_SCREEN_SHARE:
			trackPrefix += "S"
		case livekit.TrackSource_SCREEN_SHARE_AUDIO:
			trackPrefix += "s"
		}
		trackID = utils.NewGuid(trackPrefix)
	}
	info.Sid = trackID
}

func (p *ParticipantImpl) getPublishedTrackBySignalCid(clientId string) types.MediaTrack {
	for _, publishedTrack := range p.GetPublishedTracks() {
		if publishedTrack.(types.LocalMediaTrack).SignalCid() == clientId {
			return publishedTrack
		}
	}

	return nil
}

func (p *ParticipantImpl) getPublishedTrackBySdpCid(clientId string) types.MediaTrack {
	for _, publishedTrack := range p.GetPublishedTracks() {
		if publishedTrack.(types.LocalMediaTrack).HasSdpCid(clientId) {
			p.params.Logger.Debugw("found track by sdp cid", "sdpCid", clientId, "trackID", publishedTrack.ID())
			return publishedTrack
		}
	}

	return nil
}

func (p *ParticipantImpl) publisherRTCPWorker() {
	defer func() {
		if r := Recover(p.GetLogger()); r != nil {
			os.Exit(1)
		}
	}()

	// read from rtcpChan
	for pkts := range p.rtcpCh {
		if pkts == nil {
			p.params.Logger.Debugw("exiting publisher RTCP worker")
			return
		}

		if err := p.TransportManager.WritePublisherRTCP(pkts); err != nil {
			p.params.Logger.Errorw("could not write RTCP to participant", err)
		}
	}
}

func (p *ParticipantImpl) DebugInfo() map[string]interface{} {
	info := map[string]interface{}{
		"ID":    p.params.SID,
		"State": p.State().String(),
	}

	pendingTrackInfo := make(map[string]interface{})
	p.pendingTracksLock.RLock()
	for clientID, pti := range p.pendingTracks {
		var trackInfos []string
		for _, ti := range pti.trackInfos {
			trackInfos = append(trackInfos, ti.String())
		}

		pendingTrackInfo[clientID] = map[string]interface{}{
			"TrackInfos": trackInfos,
			"Migrated":   pti.migrated,
		}
	}
	p.pendingTracksLock.RUnlock()
	info["PendingTracks"] = pendingTrackInfo

	info["UpTrackManager"] = p.UpTrackManager.DebugInfo()

	return info
}

func (p *ParticipantImpl) postRtcp(pkts []rtcp.Packet) {
	select {
	case p.rtcpCh <- pkts:
	default:
		p.params.Logger.Warnw("rtcp channel full", nil)
	}
}

func (p *ParticipantImpl) setDowntracksConnected() {
	for _, t := range p.SubscriptionManager.GetSubscribedTracks() {
		if dt := t.DownTrack(); dt != nil {
			dt.SetConnected()
		}
	}
}

func (p *ParticipantImpl) CacheDownTrack(trackID livekit.TrackID, rtpTransceiver *webrtc.RTPTransceiver, downTrack sfu.DownTrackState) {
	p.lock.Lock()
	if existing := p.cachedDownTracks[trackID]; existing != nil && existing.transceiver != rtpTransceiver {
		p.params.Logger.Infow("cached transceiver changed", "trackID", trackID)
	}
	p.cachedDownTracks[trackID] = &downTrackState{transceiver: rtpTransceiver, downTrack: downTrack}
	p.lock.Unlock()
}

func (p *ParticipantImpl) UncacheDownTrack(rtpTransceiver *webrtc.RTPTransceiver) {
	p.lock.Lock()
	for trackID, dts := range p.cachedDownTracks {
		if dts.transceiver == rtpTransceiver {
			delete(p.cachedDownTracks, trackID)
			break
		}
	}
	p.lock.Unlock()
}

func (p *ParticipantImpl) GetCachedDownTrack(trackID livekit.TrackID) (*webrtc.RTPTransceiver, sfu.DownTrackState) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	dts := p.cachedDownTracks[trackID]
	if dts != nil {
		return dts.transceiver, dts.downTrack
	}

	return nil, sfu.DownTrackState{}
}

func (p *ParticipantImpl) IssueFullReconnect(reason types.ParticipantCloseReason) {
	_ = p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Leave{
			Leave: &livekit.LeaveRequest{
				CanReconnect: true,
				Reason:       reason.ToDisconnectReason(),
			},
		},
	})
	p.CloseSignalConnection()

	// on a full reconnect, no need to supervise this participant anymore
	p.supervisor.Stop()
}

func (p *ParticipantImpl) onPublicationError(trackID livekit.TrackID) {
	if p.params.ReconnectOnPublicationError {
		p.params.Logger.Infow("issuing full reconnect on publication error", "trackID", trackID)
		p.IssueFullReconnect(types.ParticipantCloseReasonPublicationError)
	}
}

func (p *ParticipantImpl) onSubscriptionError(trackID livekit.TrackID) {
	if p.params.ReconnectOnSubscriptionError {
		p.params.Logger.Infow("issuing full reconnect on subscription error", "trackID", trackID)
		p.IssueFullReconnect(types.ParticipantCloseReasonPublicationError)
	}
}

func (p *ParticipantImpl) onAnyTransportNegotiationFailed() {
	if p.TransportManager.SinceLastSignal() < negotiationFailedTimeout {
		p.params.Logger.Infow("negotiation failed, starting full reconnect")
	}
	p.IssueFullReconnect(types.ParticipantCloseReasonNegotiateFailed)
}

func (p *ParticipantImpl) UpdateSubscribedQuality(nodeID livekit.NodeID, trackID livekit.TrackID, maxQualities []types.SubscribedCodecQuality) error {
	track := p.GetPublishedTrack(trackID)
	if track == nil {
		p.params.Logger.Warnw("could not find track", nil, "trackID", trackID)
		return errors.New("could not find published track")
	}

	track.(types.LocalMediaTrack).NotifySubscriberNodeMaxQuality(nodeID, maxQualities)
	return nil
}

func (p *ParticipantImpl) UpdateMediaLoss(nodeID livekit.NodeID, trackID livekit.TrackID, fractionalLoss uint32) error {
	track := p.GetPublishedTrack(trackID)
	if track == nil {
		p.params.Logger.Warnw("could not find track", nil, "trackID", trackID)
		return errors.New("could not find published track")
	}

	track.(types.LocalMediaTrack).NotifySubscriberNodeMediaLoss(nodeID, uint8(fractionalLoss))
	return nil
}

func codecsFromMediaDescription(m *sdp.MediaDescription) (out []sdp.Codec, err error) {
	s := &sdp.SessionDescription{
		MediaDescriptions: []*sdp.MediaDescription{m},
	}

	for _, payloadStr := range m.MediaName.Formats {
		payloadType, err := strconv.ParseUint(payloadStr, 10, 8)
		if err != nil {
			return nil, err
		}

		codec, err := s.GetCodecForPayloadType(uint8(payloadType))
		if err != nil {
			if payloadType == 0 {
				continue
			}
			return nil, err
		}

		out = append(out, codec)
	}

	return out, nil
}
