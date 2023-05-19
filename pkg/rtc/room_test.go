package rtc

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/whoyao/livekit/version"
	"github.com/whoyao/protocol/livekit"
	"github.com/whoyao/protocol/logger"
	"github.com/whoyao/protocol/webhook"

	"github.com/whoyao/livekit/pkg/config"
	"github.com/whoyao/livekit/pkg/rtc/types"
	"github.com/whoyao/livekit/pkg/rtc/types/typesfakes"
	"github.com/whoyao/livekit/pkg/sfu/audio"
	"github.com/whoyao/livekit/pkg/telemetry"
	"github.com/whoyao/livekit/pkg/telemetry/telemetryfakes"
	"github.com/whoyao/livekit/pkg/testutils"
)

const (
	numParticipants     = 3
	defaultDelay        = 10 * time.Millisecond
	audioUpdateInterval = 25
)

func init() {
	config.InitLoggerFromConfig(config.LoggingConfig{
		Config: logger.Config{Level: "debug"},
	})
	// allow immediate closure in testing
	RoomDepartureGrace = 1
	roomUpdateInterval = defaultDelay
}

var iceServersForRoom = []*livekit.ICEServer{{Urls: []string{"stun:stun.l.google.com:19302"}}}

func TestJoinedState(t *testing.T) {
	t.Run("new room should return joinedAt 0", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 0})
		require.Equal(t, int64(0), rm.FirstJoinedAt())
		require.Equal(t, int64(0), rm.LastLeftAt())
	})

	t.Run("should be current time when a participant joins", func(t *testing.T) {
		s := time.Now().Unix()
		rm := newRoomWithParticipants(t, testRoomOpts{num: 1})
		require.LessOrEqual(t, s, rm.FirstJoinedAt())
		require.Equal(t, int64(0), rm.LastLeftAt())
	})

	t.Run("should be set when a participant leaves", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 1})
		p0 := rm.GetParticipants()[0]
		s := time.Now().Unix()
		rm.RemoveParticipant(p0.Identity(), p0.ID(), types.ParticipantCloseReasonClientRequestLeave)
		require.LessOrEqual(t, s, rm.LastLeftAt())
	})

	t.Run("LastLeftAt should be set when there are still participants in the room", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 2})
		p0 := rm.GetParticipants()[0]
		rm.RemoveParticipant(p0.Identity(), p0.ID(), types.ParticipantCloseReasonClientRequestLeave)
		require.Greater(t, rm.LastLeftAt(), int64(0))
	})
}

func TestRoomJoin(t *testing.T) {
	t.Run("joining returns existing participant data", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: numParticipants})
		pNew := newMockParticipant("new", types.CurrentProtocol, false, false)

		_ = rm.Join(pNew, nil, nil, iceServersForRoom)

		// expect new participant to get a JoinReply
		res := pNew.SendJoinResponseArgsForCall(0)
		require.Equal(t, livekit.RoomID(res.Room.Sid), rm.ID())
		require.Len(t, res.OtherParticipants, numParticipants)
		require.Len(t, rm.GetParticipants(), numParticipants+1)
		require.NotEmpty(t, res.IceServers)
	})

	t.Run("subscribe to existing channels upon join", func(t *testing.T) {
		numExisting := 3
		rm := newRoomWithParticipants(t, testRoomOpts{num: numExisting})
		p := newMockParticipant("new", types.CurrentProtocol, false, false)

		err := rm.Join(p, nil, &ParticipantOptions{AutoSubscribe: true}, iceServersForRoom)
		require.NoError(t, err)

		stateChangeCB := p.OnStateChangeArgsForCall(0)
		require.NotNil(t, stateChangeCB)
		p.StateReturns(livekit.ParticipantInfo_ACTIVE)
		stateChangeCB(p, livekit.ParticipantInfo_JOINED)

		// it should become a subscriber when connectivity changes
		numTracks := 0
		for _, op := range rm.GetParticipants() {
			if p == op {
				continue
			}

			numTracks += len(op.GetPublishedTracks())
		}
		require.Equal(t, numTracks, p.SubscribeToTrackCallCount())
	})

	t.Run("participant state change is broadcasted to others", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: numParticipants})
		var changedParticipant types.Participant
		rm.OnParticipantChanged(func(participant types.LocalParticipant) {
			changedParticipant = participant
		})
		participants := rm.GetParticipants()
		p := participants[0].(*typesfakes.FakeLocalParticipant)
		disconnectedParticipant := participants[1].(*typesfakes.FakeLocalParticipant)
		disconnectedParticipant.StateReturns(livekit.ParticipantInfo_DISCONNECTED)

		rm.RemoveParticipant(p.Identity(), p.ID(), types.ParticipantCloseReasonStateDisconnected)
		time.Sleep(defaultDelay)

		require.Equal(t, p, changedParticipant)

		numUpdates := 0
		for _, op := range participants {
			if op == p || op == disconnectedParticipant {
				require.Zero(t, p.SendParticipantUpdateCallCount())
				continue
			}
			fakeP := op.(*typesfakes.FakeLocalParticipant)
			require.Equal(t, 1, fakeP.SendParticipantUpdateCallCount())
			numUpdates += 1
		}
		require.Equal(t, numParticipants-2, numUpdates)
	})

	t.Run("cannot exceed max participants", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 1})
		rm.protoRoom.MaxParticipants = 1
		p := newMockParticipant("second", types.ProtocolVersion(0), false, false)

		err := rm.Join(p, nil, nil, iceServersForRoom)
		require.Equal(t, ErrMaxParticipantsExceeded, err)
	})
}

// various state changes to participant and that others are receiving update
func TestParticipantUpdate(t *testing.T) {
	tests := []struct {
		name         string
		sendToSender bool // should sender receive it
		action       func(p types.LocalParticipant)
	}{
		{
			"track mutes are sent to everyone",
			true,
			func(p types.LocalParticipant) {
				p.SetTrackMuted("", true, false)
			},
		},
		{
			"track metadata updates are sent to everyone",
			true,
			func(p types.LocalParticipant) {
				p.SetMetadata("")
			},
		},
		{
			"track publishes are sent to existing participants",
			true,
			func(p types.LocalParticipant) {
				p.AddTrack(&livekit.AddTrackRequest{
					Type: livekit.TrackType_VIDEO,
				})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rm := newRoomWithParticipants(t, testRoomOpts{num: 3})
			// remember how many times send has been called for each
			callCounts := make(map[livekit.ParticipantID]int)
			for _, p := range rm.GetParticipants() {
				fp := p.(*typesfakes.FakeLocalParticipant)
				callCounts[p.ID()] = fp.SendParticipantUpdateCallCount()
			}

			sender := rm.GetParticipants()[0]
			test.action(sender)

			// go through the other participants, make sure they've received update
			for _, p := range rm.GetParticipants() {
				expected := callCounts[p.ID()]
				if p != sender || test.sendToSender {
					expected += 1
				}
				fp := p.(*typesfakes.FakeLocalParticipant)
				require.Equal(t, expected, fp.SendParticipantUpdateCallCount())
			}
		})
	}
}

func TestPushAndDequeueUpdates(t *testing.T) {
	identity := "test_user"
	publisher1v1 := &livekit.ParticipantInfo{
		Identity:    identity,
		Sid:         "1",
		IsPublisher: true,
		Version:     1,
		JoinedAt:    0,
	}
	publisher1v2 := &livekit.ParticipantInfo{
		Identity:    identity,
		Sid:         "1",
		IsPublisher: true,
		Version:     2,
		JoinedAt:    1,
	}
	publisher2 := &livekit.ParticipantInfo{
		Identity:    identity,
		Sid:         "2",
		IsPublisher: true,
		Version:     1,
		JoinedAt:    2,
	}
	subscriber1v1 := &livekit.ParticipantInfo{
		Identity: identity,
		Sid:      "1",
		Version:  1,
		JoinedAt: 0,
	}
	subscriber1v2 := &livekit.ParticipantInfo{
		Identity: identity,
		Sid:      "1",
		Version:  2,
		JoinedAt: 1,
	}

	requirePIEquals := func(t *testing.T, a, b *livekit.ParticipantInfo) {
		require.Equal(t, a.Sid, b.Sid)
		require.Equal(t, a.Identity, b.Identity)
		require.Equal(t, a.Version, b.Version)
	}
	testCases := []struct {
		name      string
		pi        *livekit.ParticipantInfo
		immediate bool
		existing  *livekit.ParticipantInfo
		expected  []*livekit.ParticipantInfo
		validate  func(t *testing.T, rm *Room, updates []*livekit.ParticipantInfo)
	}{
		{
			name:     "publisher updates are immediate",
			pi:       publisher1v1,
			expected: []*livekit.ParticipantInfo{publisher1v1},
		},
		{
			name: "subscriber updates are queued",
			pi:   subscriber1v1,
		},
		{
			name:     "last version is enqueued",
			pi:       subscriber1v2,
			existing: subscriber1v1,
			validate: func(t *testing.T, rm *Room, _ []*livekit.ParticipantInfo) {
				queued := rm.batchedUpdates[livekit.ParticipantIdentity(identity)]
				require.NotNil(t, queued)
				requirePIEquals(t, subscriber1v2, queued)
			},
		},
		{
			name:      "latest version when immediate",
			pi:        subscriber1v2,
			existing:  subscriber1v1,
			immediate: true,
			expected:  []*livekit.ParticipantInfo{subscriber1v2},
			validate: func(t *testing.T, rm *Room, _ []*livekit.ParticipantInfo) {
				queued := rm.batchedUpdates[livekit.ParticipantIdentity(identity)]
				require.Nil(t, queued)
			},
		},
		{
			name:     "out of order updates are rejected",
			pi:       subscriber1v1,
			existing: subscriber1v2,
			validate: func(t *testing.T, rm *Room, updates []*livekit.ParticipantInfo) {
				queued := rm.batchedUpdates[livekit.ParticipantIdentity(identity)]
				requirePIEquals(t, subscriber1v2, queued)
			},
		},
		{
			name:     "sid change is broadcasted immediately",
			pi:       publisher2,
			existing: subscriber1v2,
			expected: []*livekit.ParticipantInfo{
				{
					Identity: identity,
					Sid:      "1",
					Version:  2,
					State:    livekit.ParticipantInfo_DISCONNECTED,
				},
				publisher2,
			},
		},
		{
			name:     "when switching to publisher, queue is cleared",
			pi:       publisher1v2,
			existing: subscriber1v1,
			expected: []*livekit.ParticipantInfo{publisher1v2},
			validate: func(t *testing.T, rm *Room, updates []*livekit.ParticipantInfo) {
				require.Empty(t, rm.batchedUpdates)
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rm := newRoomWithParticipants(t, testRoomOpts{num: 1})
			if tc.existing != nil {
				// clone the existing value since it can be modified when setting to disconnected
				rm.batchedUpdates[livekit.ParticipantIdentity(tc.existing.Identity)] = proto.Clone(tc.existing).(*livekit.ParticipantInfo)
			}
			updates := rm.pushAndDequeueUpdates(tc.pi, tc.immediate)
			require.Equal(t, len(tc.expected), len(updates))
			for i, item := range tc.expected {
				requirePIEquals(t, item, updates[i])
			}

			if tc.validate != nil {
				tc.validate(t, rm, updates)
			}
		})
	}
}

func TestRoomClosure(t *testing.T) {
	t.Run("room closes after participant leaves", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 1})
		isClosed := false
		rm.OnClose(func() {
			isClosed = true
		})
		p := rm.GetParticipants()[0]
		// allows immediate close after
		rm.protoRoom.EmptyTimeout = 0
		rm.RemoveParticipant(p.Identity(), p.ID(), types.ParticipantCloseReasonClientRequestLeave)

		time.Sleep(time.Duration(RoomDepartureGrace)*time.Second + defaultDelay)

		rm.CloseIfEmpty()
		require.Len(t, rm.GetParticipants(), 0)
		require.True(t, isClosed)

		require.Equal(t, ErrRoomClosed, rm.Join(p, nil, nil, iceServersForRoom))
	})

	t.Run("room does not close before empty timeout", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 0})
		isClosed := false
		rm.OnClose(func() {
			isClosed = true
		})
		require.NotZero(t, rm.protoRoom.EmptyTimeout)
		rm.CloseIfEmpty()
		require.False(t, isClosed)
	})

	t.Run("room closes after empty timeout", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 0})
		isClosed := false
		rm.OnClose(func() {
			isClosed = true
		})
		rm.protoRoom.EmptyTimeout = 1

		time.Sleep(1010 * time.Millisecond)
		rm.CloseIfEmpty()
		require.True(t, isClosed)
	})
}

func TestNewTrack(t *testing.T) {
	t.Run("new track should be added to ready participants", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 3})
		participants := rm.GetParticipants()
		p0 := participants[0].(*typesfakes.FakeLocalParticipant)
		p0.StateReturns(livekit.ParticipantInfo_JOINED)
		p1 := participants[1].(*typesfakes.FakeLocalParticipant)
		p1.StateReturns(livekit.ParticipantInfo_ACTIVE)

		pub := participants[2].(*typesfakes.FakeLocalParticipant)

		// pub adds track
		track := newMockTrack(livekit.TrackType_VIDEO, "webcam")
		trackCB := pub.OnTrackPublishedArgsForCall(0)
		require.NotNil(t, trackCB)
		trackCB(pub, track)
		// only p1 should've been subscribed to
		require.Equal(t, 0, p0.SubscribeToTrackCallCount())
		require.Equal(t, 1, p1.SubscribeToTrackCallCount())
	})
}

func TestActiveSpeakers(t *testing.T) {
	t.Parallel()
	getActiveSpeakerUpdates := func(p *typesfakes.FakeLocalParticipant) [][]*livekit.SpeakerInfo {
		var updates [][]*livekit.SpeakerInfo
		numCalls := p.SendSpeakerUpdateCallCount()
		for i := 0; i < numCalls; i++ {
			infos, _ := p.SendSpeakerUpdateArgsForCall(i)
			updates = append(updates, infos)
		}
		return updates
	}

	audioUpdateDuration := (audioUpdateInterval + 10) * time.Millisecond
	t.Run("participant should not be getting audio updates (protocol 2)", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 1, protocol: 2})
		defer rm.Close()
		p := rm.GetParticipants()[0].(*typesfakes.FakeLocalParticipant)
		require.Empty(t, rm.GetActiveSpeakers())

		time.Sleep(audioUpdateDuration)

		updates := getActiveSpeakerUpdates(p)
		require.Empty(t, updates)
	})

	t.Run("speakers should be sorted by loudness", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 2})
		defer rm.Close()
		participants := rm.GetParticipants()
		p := participants[0].(*typesfakes.FakeLocalParticipant)
		p2 := participants[1].(*typesfakes.FakeLocalParticipant)
		p.GetAudioLevelReturns(20, true)
		p2.GetAudioLevelReturns(10, true)

		speakers := rm.GetActiveSpeakers()
		require.Len(t, speakers, 2)
		require.Equal(t, string(p.ID()), speakers[0].Sid)
		require.Equal(t, string(p2.ID()), speakers[1].Sid)
	})

	t.Run("participants are getting audio updates (protocol 3+)", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 2, protocol: 3})
		defer rm.Close()
		participants := rm.GetParticipants()
		p := participants[0].(*typesfakes.FakeLocalParticipant)
		time.Sleep(time.Millisecond) // let the first update cycle run
		p.GetAudioLevelReturns(30, true)

		speakers := rm.GetActiveSpeakers()
		require.NotEmpty(t, speakers)
		require.Equal(t, string(p.ID()), speakers[0].Sid)

		testutils.WithTimeout(t, func() string {
			for _, op := range participants {
				op := op.(*typesfakes.FakeLocalParticipant)
				updates := getActiveSpeakerUpdates(op)
				if len(updates) == 0 {
					return fmt.Sprintf("%s did not get any audio updates", op.Identity())
				}
			}
			return ""
		})

		// no longer speaking, send update with empty items
		p.GetAudioLevelReturns(127, false)

		testutils.WithTimeout(t, func() string {
			updates := getActiveSpeakerUpdates(p)
			lastUpdate := updates[len(updates)-1]
			if len(lastUpdate) == 0 {
				return "did not get updates of speaker going quiet"
			}
			if lastUpdate[0].Active {
				return "speaker should not have been active"
			}
			return ""
		})
	})

	t.Run("audio level is smoothed", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 2, protocol: 3, audioSmoothIntervals: 3})
		defer rm.Close()
		participants := rm.GetParticipants()
		p := participants[0].(*typesfakes.FakeLocalParticipant)
		op := participants[1].(*typesfakes.FakeLocalParticipant)
		p.GetAudioLevelReturns(30, true)
		convertedLevel := float32(audio.ConvertAudioLevel(30))

		testutils.WithTimeout(t, func() string {
			updates := getActiveSpeakerUpdates(op)
			if len(updates) == 0 {
				return "no speaker updates received"
			}
			lastSpeakers := updates[len(updates)-1]
			if len(lastSpeakers) == 0 {
				return "no speakers in the update"
			}
			if lastSpeakers[0].Level > convertedLevel {
				return ""
			}
			return "level mismatch"
		})

		testutils.WithTimeout(t, func() string {
			updates := getActiveSpeakerUpdates(op)
			if len(updates) == 0 {
				return "no updates received"
			}
			lastSpeakers := updates[len(updates)-1]
			if len(lastSpeakers) == 0 {
				return "no speakers found"
			}
			if lastSpeakers[0].Level > convertedLevel {
				return ""
			}
			return "did not match expected levels"
		})

		p.GetAudioLevelReturns(127, false)
		testutils.WithTimeout(t, func() string {
			updates := getActiveSpeakerUpdates(op)
			if len(updates) == 0 {
				return "no speaker updates received"
			}
			lastSpeakers := updates[len(updates)-1]
			if len(lastSpeakers) == 1 && !lastSpeakers[0].Active {
				return ""
			}
			return "speakers didn't go back to zero"
		})
	})
}

func TestDataChannel(t *testing.T) {
	t.Parallel()

	t.Run("participants should receive data", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 3})
		defer rm.Close()
		participants := rm.GetParticipants()
		p := participants[0].(*typesfakes.FakeLocalParticipant)

		packet := livekit.DataPacket{
			Kind: livekit.DataPacket_RELIABLE,
			Value: &livekit.DataPacket_User{
				User: &livekit.UserPacket{
					ParticipantSid: string(p.ID()),
					Payload:        []byte("message.."),
				},
			},
		}
		p.OnDataPacketArgsForCall(0)(p, &packet)

		// ensure everyone has received the packet
		for _, op := range participants {
			fp := op.(*typesfakes.FakeLocalParticipant)
			if fp == p {
				require.Zero(t, fp.SendDataPacketCallCount())
				continue
			}
			require.Equal(t, 1, fp.SendDataPacketCallCount())
			dp, _ := fp.SendDataPacketArgsForCall(0)
			require.Equal(t, packet.Value, dp.Value)
		}
	})

	t.Run("only one participant should receive the data", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 4})
		defer rm.Close()
		participants := rm.GetParticipants()
		p := participants[0].(*typesfakes.FakeLocalParticipant)
		p1 := participants[1].(*typesfakes.FakeLocalParticipant)

		packet := livekit.DataPacket{
			Kind: livekit.DataPacket_RELIABLE,
			Value: &livekit.DataPacket_User{
				User: &livekit.UserPacket{
					ParticipantSid:  string(p.ID()),
					Payload:         []byte("message to p1.."),
					DestinationSids: []string{string(p1.ID())},
				},
			},
		}
		p.OnDataPacketArgsForCall(0)(p, &packet)

		// only p1 should receive the data
		for _, op := range participants {
			fp := op.(*typesfakes.FakeLocalParticipant)
			if fp != p1 {
				require.Zero(t, fp.SendDataPacketCallCount())
			}
		}
		require.Equal(t, 1, p1.SendDataPacketCallCount())
		dp, _ := p1.SendDataPacketArgsForCall(0)
		require.Equal(t, packet.Value, dp.Value)
	})

	t.Run("publishing disallowed", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 2})
		defer rm.Close()
		participants := rm.GetParticipants()
		p := participants[0].(*typesfakes.FakeLocalParticipant)
		p.CanPublishDataReturns(false)

		packet := livekit.DataPacket{
			Kind: livekit.DataPacket_RELIABLE,
			Value: &livekit.DataPacket_User{
				User: &livekit.UserPacket{
					Payload: []byte{},
				},
			},
		}
		if p.CanPublishData() {
			p.OnDataPacketArgsForCall(0)(p, &packet)
		}

		// no one should've been sent packet
		for _, op := range participants {
			fp := op.(*typesfakes.FakeLocalParticipant)
			require.Zero(t, fp.SendDataPacketCallCount())
		}
	})
}

func TestHiddenParticipants(t *testing.T) {
	t.Run("other participants don't receive hidden updates", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 2, numHidden: 1})
		defer rm.Close()

		pNew := newMockParticipant("new", types.CurrentProtocol, false, false)
		rm.Join(pNew, nil, nil, iceServersForRoom)

		// expect new participant to get a JoinReply
		res := pNew.SendJoinResponseArgsForCall(0)
		require.Equal(t, livekit.RoomID(res.Room.Sid), rm.ID())
		require.Len(t, res.OtherParticipants, 2)
		require.Len(t, rm.GetParticipants(), 4)
		require.NotEmpty(t, res.IceServers)
		require.Equal(t, "testregion", res.ServerInfo.Region)
	})

	t.Run("hidden participant subscribes to tracks", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 2})
		hidden := newMockParticipant("hidden", types.CurrentProtocol, true, false)

		err := rm.Join(hidden, nil, &ParticipantOptions{AutoSubscribe: true}, iceServersForRoom)
		require.NoError(t, err)

		stateChangeCB := hidden.OnStateChangeArgsForCall(0)
		require.NotNil(t, stateChangeCB)
		hidden.StateReturns(livekit.ParticipantInfo_ACTIVE)
		stateChangeCB(hidden, livekit.ParticipantInfo_JOINED)

		require.Equal(t, 2, hidden.SubscribeToTrackCallCount())
	})
}

func TestRoomUpdate(t *testing.T) {
	t.Run("updates are sent when participant joined", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 1})
		defer rm.Close()

		p1 := rm.GetParticipants()[0].(*typesfakes.FakeLocalParticipant)
		require.Equal(t, 0, p1.SendRoomUpdateCallCount())

		p2 := newMockParticipant("p2", types.CurrentProtocol, false, false)
		require.NoError(t, rm.Join(p2, nil, nil, iceServersForRoom))

		// p1 should have received an update
		time.Sleep(2 * defaultDelay)
		require.LessOrEqual(t, 1, p1.SendRoomUpdateCallCount())
		require.EqualValues(t, 2, p1.SendRoomUpdateArgsForCall(p1.SendRoomUpdateCallCount()-1).NumParticipants)
	})

	t.Run("participants should receive metadata update", func(t *testing.T) {
		rm := newRoomWithParticipants(t, testRoomOpts{num: 2})
		defer rm.Close()

		rm.SetMetadata("test metadata...")

		// callbacks are updated from goroutine
		time.Sleep(2 * defaultDelay)

		for _, op := range rm.GetParticipants() {
			fp := op.(*typesfakes.FakeLocalParticipant)
			// room updates are now sent for both participant joining and room metadata
			require.GreaterOrEqual(t, fp.SendRoomUpdateCallCount(), 1)
		}
	})
}

type testRoomOpts struct {
	num                  int
	numHidden            int
	protocol             types.ProtocolVersion
	audioSmoothIntervals uint32
}

func newRoomWithParticipants(t *testing.T, opts testRoomOpts) *Room {
	rm := NewRoom(
		&livekit.Room{Name: "room"},
		nil,
		WebRTCConfig{},
		&config.AudioConfig{
			UpdateInterval:  audioUpdateInterval,
			SmoothIntervals: opts.audioSmoothIntervals,
		},
		&livekit.ServerInfo{
			Edition:  livekit.ServerInfo_Standard,
			Version:  version.Version,
			Protocol: types.CurrentProtocol,
			NodeId:   "testnode",
			Region:   "testregion",
		},
		telemetry.NewTelemetryService(webhook.NewDefaultNotifier("", "", nil), &telemetryfakes.FakeAnalyticsService{}),
		nil,
	)
	for i := 0; i < opts.num+opts.numHidden; i++ {
		identity := livekit.ParticipantIdentity(fmt.Sprintf("p%d", i))
		participant := newMockParticipant(identity, opts.protocol, i >= opts.num, true)
		err := rm.Join(participant, nil, &ParticipantOptions{AutoSubscribe: true}, iceServersForRoom)
		require.NoError(t, err)
		participant.StateReturns(livekit.ParticipantInfo_ACTIVE)
		participant.IsReadyReturns(true)
		// each participant has a track
		participant.GetPublishedTracksReturns([]types.MediaTrack{
			&typesfakes.FakeMediaTrack{},
		})
	}
	return rm
}
