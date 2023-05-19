package routing

import (
	"runtime"
	"time"

	"github.com/whoyao/protocol/livekit"
	"github.com/whoyao/protocol/utils"

	"github.com/whoyao/livekit/pkg/config"
)

type LocalNode *livekit.Node

func NewLocalNode(conf *config.Config) (LocalNode, error) {
	nodeID, err := utils.LocalNodeID()
	if err != nil {
		return nil, err
	}
	if conf.RTC.NodeIP == "" {
		return nil, ErrIPNotSet
	}
	node := &livekit.Node{
		Id:      nodeID,
		Ip:      conf.RTC.NodeIP,
		NumCpus: uint32(runtime.NumCPU()),
		Region:  conf.Region,
		State:   livekit.NodeState_SERVING,
		Stats: &livekit.NodeStats{
			StartedAt: time.Now().Unix(),
			UpdatedAt: time.Now().Unix(),
		},
	}

	return node, nil
}
