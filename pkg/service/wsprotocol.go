package service

import (
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/whoyao/protocol/livekit"
	"github.com/whoyao/protocol/logger"

	"github.com/whoyao/livekit/pkg/rtc/types"
)

const (
	pingFrequency = 10 * time.Second
	pingTimeout   = 2 * time.Second
)

type WSSignalConnection struct {
	conn    types.WebsocketClient
	mu      sync.Mutex
	useJSON bool
}

func NewWSSignalConnection(conn types.WebsocketClient) *WSSignalConnection {
	wsc := &WSSignalConnection{
		conn:    conn,
		mu:      sync.Mutex{},
		useJSON: false,
	}
	go wsc.pingWorker()
	return wsc
}

func (c *WSSignalConnection) ReadRequest() (*livekit.SignalRequest, int, error) {
	for {
		// handle special messages and pass on the rest
		messageType, payload, err := c.conn.ReadMessage()
		if err != nil {
			return nil, 0, err
		}

		msg := &livekit.SignalRequest{}
		switch messageType {
		case websocket.BinaryMessage:
			if c.useJSON {
				c.mu.Lock()
				// switch to protobuf if client supports it
				c.useJSON = false
				c.mu.Unlock()
			}
			// protobuf encoded
			err := proto.Unmarshal(payload, msg)
			return msg, len(payload), err
		case websocket.TextMessage:
			c.mu.Lock()
			// json encoded, also write back JSON
			c.useJSON = true
			c.mu.Unlock()
			err := protojson.Unmarshal(payload, msg)
			return msg, len(payload), err
		default:
			logger.Debugw("unsupported message", "message", messageType)
			return nil, len(payload), nil
		}
	}
}

func (c *WSSignalConnection) WriteResponse(msg *livekit.SignalResponse) (int, error) {
	var msgType int
	var payload []byte
	var err error

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.useJSON {
		msgType = websocket.TextMessage
		payload, err = protojson.Marshal(msg)
	} else {
		msgType = websocket.BinaryMessage
		payload, err = proto.Marshal(msg)
	}
	if err != nil {
		return 0, err
	}

	return len(payload), c.conn.WriteMessage(msgType, payload)
}

func (c *WSSignalConnection) pingWorker() {
	for {
		<-time.After(pingFrequency)
		err := c.conn.WriteControl(websocket.PingMessage, []byte(""), time.Now().Add(pingTimeout))
		if err != nil {
			return
		}
	}
}
