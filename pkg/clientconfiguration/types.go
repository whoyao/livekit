package clientconfiguration

import (
	"github.com/whoyao/protocol/livekit"
)

type ClientConfigurationManager interface {
	GetConfiguration(clientInfo *livekit.ClientInfo) *livekit.ClientConfiguration
}
