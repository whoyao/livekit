package test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/whoyao/protocol/auth"
	"github.com/whoyao/protocol/livekit"
	"github.com/whoyao/protocol/logger"
	"github.com/whoyao/protocol/utils"
	"github.com/whoyao/protocol/webhook"

	"github.com/whoyao/livekit/pkg/config"
	"github.com/whoyao/livekit/pkg/routing"
	"github.com/whoyao/livekit/pkg/service"
	"github.com/whoyao/livekit/pkg/testutils"
)

func TestWebhooks(t *testing.T) {
	server, ts, finish, err := setupServerWithWebhook()
	require.NoError(t, err)
	defer finish()

	c1 := createRTCClient("c1", defaultServerPort, nil)
	waitUntilConnected(t, c1)
	testutils.WithTimeout(t, func() string {
		if ts.GetEvent(webhook.EventRoomStarted) == nil {
			return "did not receive RoomStarted"
		}
		if ts.GetEvent(webhook.EventParticipantJoined) == nil {
			return "did not receive ParticipantJoined"
		}
		return ""
	})

	// first participant join should have started the room
	started := ts.GetEvent(webhook.EventRoomStarted)
	require.Equal(t, testRoom, started.Room.Name)
	require.NotEmpty(t, started.Id)
	require.Greater(t, started.CreatedAt, time.Now().Unix()-100)
	require.GreaterOrEqual(t, time.Now().Unix(), started.CreatedAt)
	joined := ts.GetEvent(webhook.EventParticipantJoined)
	require.Equal(t, "c1", joined.Participant.Identity)
	ts.ClearEvents()

	// another participant joins
	c2 := createRTCClient("c2", defaultServerPort, nil)
	waitUntilConnected(t, c2)
	defer c2.Stop()
	testutils.WithTimeout(t, func() string {
		if ts.GetEvent(webhook.EventParticipantJoined) == nil {
			return "did not receive ParticipantJoined"
		}
		return ""
	})
	joined = ts.GetEvent(webhook.EventParticipantJoined)
	require.Equal(t, "c2", joined.Participant.Identity)
	ts.ClearEvents()

	// track published
	writers := publishTracksForClients(t, c1)
	defer stopWriters(writers...)
	testutils.WithTimeout(t, func() string {
		ev := ts.GetEvent(webhook.EventTrackPublished)
		if ev == nil {
			return "did not receive TrackPublished"
		}
		require.NotNil(t, ev.Track, "TrackPublished did not include trackInfo")
		require.Equal(t, string(c1.ID()), ev.Participant.Sid)
		return ""
	})
	ts.ClearEvents()

	// first participant leaves
	c1.Stop()
	testutils.WithTimeout(t, func() string {
		if ts.GetEvent(webhook.EventParticipantLeft) == nil {
			return "did not receive ParticipantLeft"
		}
		return ""
	})
	left := ts.GetEvent(webhook.EventParticipantLeft)
	require.Equal(t, "c1", left.Participant.Identity)
	ts.ClearEvents()

	// room closed
	rm := server.RoomManager().GetRoom(context.Background(), testRoom)
	rm.Close()
	testutils.WithTimeout(t, func() string {
		if ts.GetEvent(webhook.EventRoomFinished) == nil {
			return "did not receive RoomFinished"
		}
		return ""
	})
	require.Equal(t, testRoom, ts.GetEvent(webhook.EventRoomFinished).Room.Name)
}

func setupServerWithWebhook() (server *service.LivekitServer, testServer *webhookTestServer, finishFunc func(), err error) {
	conf, err := config.NewConfig("", true, nil, nil)
	if err != nil {
		panic(fmt.Sprintf("could not create config: %v", err))
	}
	conf.WebHook.URLs = []string{"http://localhost:7890"}
	conf.WebHook.APIKey = testApiKey
	conf.Keys = map[string]string{testApiKey: testApiSecret}

	testServer = newTestServer(":7890")
	if err = testServer.Start(); err != nil {
		return
	}

	currentNode, err := routing.NewLocalNode(conf)
	if err != nil {
		return
	}
	currentNode.Id = utils.NewGuid(nodeID1)

	server, err = service.InitializeServer(conf, currentNode)
	if err != nil {
		return
	}

	go func() {
		if err := server.Start(); err != nil {
			logger.Errorw("server returned error", err)
		}
	}()

	waitForServerToStart(server)

	finishFunc = func() {
		server.Stop(true)
		testServer.Stop()
	}
	return
}

type webhookTestServer struct {
	server   *http.Server
	events   map[string]*livekit.WebhookEvent
	lock     sync.Mutex
	provider auth.KeyProvider
}

func newTestServer(addr string) *webhookTestServer {
	s := &webhookTestServer{
		events:   make(map[string]*livekit.WebhookEvent),
		provider: auth.NewFileBasedKeyProviderFromMap(map[string]string{testApiKey: testApiSecret}),
	}
	s.server = &http.Server{
		Addr:    addr,
		Handler: s,
	}
	return s
}

func (s *webhookTestServer) ServeHTTP(_ http.ResponseWriter, r *http.Request) {
	data, err := webhook.Receive(r, s.provider)
	if err != nil {
		logger.Errorw("could not receive webhook", err)
		return
	}

	event := livekit.WebhookEvent{}
	if err = protojson.Unmarshal(data, &event); err != nil {
		logger.Errorw("could not unmarshal event", err)
		return
	}

	s.lock.Lock()
	s.events[event.Event] = &event
	s.lock.Unlock()
}

func (s *webhookTestServer) GetEvent(name string) *livekit.WebhookEvent {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.events[name]
}

func (s *webhookTestServer) ClearEvents() {
	s.lock.Lock()
	s.events = make(map[string]*livekit.WebhookEvent)
	s.lock.Unlock()
}

func (s *webhookTestServer) Start() error {
	l, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		return err
	}
	go s.server.Serve(l)

	// wait for webhook server to start
	ctx, cancel := context.WithTimeout(context.Background(), testutils.ConnectTimeout)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return errors.New("could not start webhook server after timeout")
		case <-time.After(10 * time.Millisecond):
			// ensure we can connect to it
			res, err := http.Get(fmt.Sprintf("http://localhost%s", s.server.Addr))
			if err == nil && res.StatusCode == http.StatusOK {
				return nil
			}
		}
	}
}

func (s *webhookTestServer) Stop() {
	_ = s.server.Shutdown(context.Background())
}
