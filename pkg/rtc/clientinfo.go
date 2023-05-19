package rtc

import (
	"strconv"
	"strings"

	"github.com/whoyao/protocol/livekit"
)

type ClientInfo struct {
	*livekit.ClientInfo
}

func (c ClientInfo) isFirefox() bool {
	return c.ClientInfo != nil && strings.EqualFold(c.ClientInfo.Browser, "firefox")
}

func (c ClientInfo) isSafari() bool {
	return c.ClientInfo != nil && strings.EqualFold(c.ClientInfo.Browser, "safari")
}

func (c ClientInfo) isGo() bool {
	return c.ClientInfo != nil && c.ClientInfo.Sdk == livekit.ClientInfo_GO
}

func (c ClientInfo) SupportsAudioRED() bool {
	return !c.isFirefox() && !c.isSafari()
}

func (c ClientInfo) SupportPrflxOverRelay() bool {
	return !c.isFirefox()
}

// GoSDK(pion) relies on rtp packets to fire ontrack event, browsers and native (libwebrtc) rely on sdp
func (c ClientInfo) FireTrackByRTPPacket() bool {
	return c.isGo()
}

func (c ClientInfo) CanHandleReconnectResponse() bool {
	if c.Sdk == livekit.ClientInfo_JS {
		// JS handles Reconnect explicitly in 1.6.3, prior to 1.6.4 it could not handle unknown responses
		if c.compareVersion("1.6.3") < 0 {
			return false
		}
	}
	return true
}

func (c ClientInfo) SupportsICETCP() bool {
	if c.ClientInfo == nil {
		return false
	}
	if c.ClientInfo.Sdk == livekit.ClientInfo_GO {
		// Go does not support active TCP
		return false
	}
	if c.ClientInfo.Sdk == livekit.ClientInfo_SWIFT {
		// ICE/TCP added in 1.0.5
		return c.compareVersion("1.0.5") >= 0
	}
	// most SDKs support ICE/TCP
	return true
}

func (c ClientInfo) SupportsChangeRTPSenderEncodingActive() bool {
	return !c.isFirefox()
}

// compareVersion compares a semver against the current client SDK version
// returning 1 if current version is greater than version
// 0 if they are the same, and -1 if it's an earlier version
func (c ClientInfo) compareVersion(version string) int {
	if c.ClientInfo == nil {
		return -1
	}
	parts0 := strings.Split(c.ClientInfo.Version, ".")
	parts1 := strings.Split(version, ".")
	ints0 := make([]int, 3)
	ints1 := make([]int, 3)
	for i := 0; i < 3; i++ {
		if len(parts0) > i {
			ints0[i], _ = strconv.Atoi(parts0[i])
		}
		if len(parts1) > i {
			ints1[i], _ = strconv.Atoi(parts1[i])
		}
		if ints0[i] > ints1[i] {
			return 1
		} else if ints0[i] < ints1[i] {
			return -1
		}
	}
	return 0
}
