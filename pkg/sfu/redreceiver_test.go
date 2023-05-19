package sfu

import (
	"testing"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
	"github.com/whoyao/livekit/pkg/sfu/buffer"
)

const tsStep = uint32(48000 / 1000 * 10)

type dummyDowntrack struct {
	TrackSender
	lastReceivedPkt *rtp.Packet
	receivedPkts    []*rtp.Packet
}

func (dt *dummyDowntrack) WriteRTP(p *buffer.ExtPacket, _ int32) error {
	dt.lastReceivedPkt = p.Packet
	dt.receivedPkts = append(dt.receivedPkts, p.Packet)
	return nil
}

func TestRedReceiver(t *testing.T) {
	dt := &dummyDowntrack{TrackSender: &DownTrack{}}

	t.Run("normal", func(t *testing.T) {
		w := &WebRTCReceiver{isRED: true, kind: webrtc.RTPCodecTypeAudio}
		require.Equal(t, w.GetRedReceiver(), w)
		w.isRED = false
		red := w.GetRedReceiver().(*RedReceiver)
		require.NotNil(t, red)
		require.NoError(t, red.AddDownTrack(dt))

		header := rtp.Header{SequenceNumber: 65534, Timestamp: (uint32(1) << 31) - 2*tsStep, PayloadType: 111}
		expectPkt := make([]*rtp.Packet, 0, maxRedCount+1)
		for _, pkt := range generatePkts(header, 10, tsStep) {
			expectPkt = append(expectPkt, pkt)
			if len(expectPkt) > maxRedCount+1 {
				expectPkt = expectPkt[1:]
			}
			red.ForwardRTP(&buffer.ExtPacket{
				Packet: pkt,
			}, 0)
			verifyRedEncodings(t, dt.lastReceivedPkt, expectPkt)
		}
	})

	t.Run("packet lost and jump", func(t *testing.T) {
		w := &WebRTCReceiver{kind: webrtc.RTPCodecTypeAudio}
		red := w.GetRedReceiver().(*RedReceiver)
		require.NoError(t, red.AddDownTrack(dt))

		header := rtp.Header{SequenceNumber: 65534, Timestamp: (uint32(1) << 31) - 2*tsStep, PayloadType: 111}
		expectPkt := make([]*rtp.Packet, 0, maxRedCount+1)
		for i := 0; i < 10; i++ {
			if i%2 == 0 {
				header.SequenceNumber++
				header.Timestamp += tsStep
				expectPkt = append(expectPkt, nil)
				continue
			}
			hbuf, _ := header.Marshal()
			pkt1 := &rtp.Packet{
				Header:  header,
				Payload: hbuf,
			}
			expectPkt = append(expectPkt, pkt1)
			if len(expectPkt) > maxRedCount+1 {
				expectPkt = expectPkt[len(expectPkt)-maxRedCount-1:]
			}
			red.ForwardRTP(&buffer.ExtPacket{
				Packet: pkt1,
			}, 0)
			verifyRedEncodings(t, dt.lastReceivedPkt, expectPkt)
			header.SequenceNumber++
			header.Timestamp += tsStep
		}

		// jump
		header.SequenceNumber += 10
		header.Timestamp += 10 * tsStep

		expectPkt = expectPkt[:0]
		for _, pkt := range generatePkts(header, 3, tsStep) {
			expectPkt = append(expectPkt, pkt)
			if len(expectPkt) > maxRedCount+1 {
				expectPkt = expectPkt[len(expectPkt)-maxRedCount-1:]
			}
			red.ForwardRTP(&buffer.ExtPacket{
				Packet: pkt,
			}, 0)
			verifyRedEncodings(t, dt.lastReceivedPkt, expectPkt)
		}
	})

	t.Run("unorder and repeat", func(t *testing.T) {
		w := &WebRTCReceiver{kind: webrtc.RTPCodecTypeAudio}
		red := w.GetRedReceiver().(*RedReceiver)
		require.NoError(t, red.AddDownTrack(dt))

		header := rtp.Header{SequenceNumber: 65534, Timestamp: (uint32(1) << 31) - 2*tsStep, PayloadType: 111}
		var prevPkts []*rtp.Packet
		for _, pkt := range generatePkts(header, 10, tsStep) {
			red.ForwardRTP(&buffer.ExtPacket{
				Packet: pkt,
			}, 0)
			prevPkts = append(prevPkts, pkt)
		}

		// old unorder data don't have red records
		expectPkt := prevPkts[len(prevPkts)-3 : len(prevPkts)-2]
		red.ForwardRTP(&buffer.ExtPacket{
			Packet: expectPkt[0],
		}, 0)
		verifyRedEncodings(t, dt.lastReceivedPkt, expectPkt)

		// repeat packet only have 1 red records
		expectPkt = prevPkts[len(prevPkts)-2:]
		red.ForwardRTP(&buffer.ExtPacket{
			Packet: expectPkt[1],
		}, 0)
		verifyRedEncodings(t, dt.lastReceivedPkt, expectPkt)
	})

	t.Run("encoding exceed space", func(t *testing.T) {
		w := &WebRTCReceiver{isRED: true, kind: webrtc.RTPCodecTypeAudio}
		require.Equal(t, w.GetRedReceiver(), w)
		w.isRED = false
		red := w.GetRedReceiver().(*RedReceiver)
		require.NotNil(t, red)
		require.NoError(t, red.AddDownTrack(dt))

		header := rtp.Header{SequenceNumber: 65534, Timestamp: (uint32(1) << 31) - 2*tsStep, PayloadType: 111}
		expectPkt := make([]*rtp.Packet, 0, maxRedCount+1)
		for _, pkt := range generatePkts(header, 10, tsStep) {
			// make sure red encodings don't have enough space to encoding redundant packet
			pkt.Payload = make([]byte, 1000)
			expectPkt = append(expectPkt[:0], pkt)
			red.ForwardRTP(&buffer.ExtPacket{
				Packet: pkt,
			}, 0)
			verifyRedEncodings(t, dt.lastReceivedPkt, expectPkt)
		}
	})

	t.Run("large timestamp gap", func(t *testing.T) {
		w := &WebRTCReceiver{isRED: true, kind: webrtc.RTPCodecTypeAudio}
		require.Equal(t, w.GetRedReceiver(), w)
		w.isRED = false
		red := w.GetRedReceiver().(*RedReceiver)
		require.NotNil(t, red)
		require.NoError(t, red.AddDownTrack(dt))

		header := rtp.Header{SequenceNumber: 65534, Timestamp: (uint32(1) << 31) - 2*tsStep, PayloadType: 111}
		// first few packets normal
		expectPkt := make([]*rtp.Packet, 0, maxRedCount+1)
		for _, pkt := range generatePkts(header, 4, tsStep) {
			expectPkt = append(expectPkt, pkt)
			if len(expectPkt) > maxRedCount+1 {
				expectPkt = expectPkt[1:]
			}
			red.ForwardRTP(&buffer.ExtPacket{
				Packet: pkt,
			}, 0)
			verifyRedEncodings(t, dt.lastReceivedPkt, expectPkt)
		}

		// and then a few packets with a large timestmap jump, should contain only primary
		for _, pkt := range generatePkts(header, 4, 40*tsStep) {
			red.ForwardRTP(&buffer.ExtPacket{
				Packet: pkt,
			}, 0)
			verifyRedEncodings(t, dt.lastReceivedPkt, []*rtp.Packet{pkt})
		}
	})
}

func verifyRedEncodings(t *testing.T, red *rtp.Packet, redPkts []*rtp.Packet) {
	solidPkts := make([]*rtp.Packet, 0, len(redPkts))
	for _, pkt := range redPkts {
		if pkt != nil {
			solidPkts = append(solidPkts, pkt)
		}
	}
	pktsFromRed, err := extractPktsFromRed(red, 0xFF)
	require.NoError(t, err)
	require.Len(t, pktsFromRed, len(solidPkts))
	for i, pkt := range pktsFromRed {
		verifyEncodingEqual(t, pkt, solidPkts[i])
	}
}

func verifyPktsEqual(t *testing.T, p1s, p2s []*rtp.Packet) {
	require.Len(t, p1s, len(p2s))
	for i, pkt := range p1s {
		verifyEncodingEqual(t, pkt, p2s[i])
	}
}

func verifyEncodingEqual(t *testing.T, p1, p2 *rtp.Packet) {
	require.Equal(t, p1.Header.Timestamp, p2.Header.Timestamp)
	require.Equal(t, p1.PayloadType, p2.PayloadType)
	require.EqualValues(t, p1.Payload, p2.Payload, "seq1 %s", p1.SequenceNumber)
}

func generatePkts(header rtp.Header, count int, tsStep uint32) []*rtp.Packet {
	pkts := make([]*rtp.Packet, 0, count)
	for i := 0; i < count; i++ {
		hbuf, _ := header.Marshal()
		pkts = append(pkts, &rtp.Packet{
			Header:  header,
			Payload: hbuf,
		})
		header.SequenceNumber++
		header.Timestamp += tsStep
	}
	return pkts
}

func generateRedPkts(t *testing.T, pkts []*rtp.Packet, redCount int) []*rtp.Packet {
	redPkts := make([]*rtp.Packet, 0, len(pkts))
	for i, pkt := range pkts {
		encodingPkts := make([]*rtp.Packet, 0, redCount)
		for j := i - redCount; j < i; j++ {
			if j < 0 {
				continue
			}
			encodingPkts = append(encodingPkts, pkts[j])
		}
		buf := make([]byte, mtuSize)
		redPkt := *pkt
		encoded, err := encodeRedForPrimary(encodingPkts, pkt, buf)
		require.NoError(t, err)
		redPkt.Payload = buf[:encoded]
		redPkts = append(redPkts, &redPkt)
	}
	return redPkts
}

func testRedRedPrimaryReceiver(t *testing.T, maxPktCount, redCount int, sendPktIdx, expectPktIdx []int) {
	dt := &dummyDowntrack{TrackSender: &DownTrack{}}
	w := &WebRTCReceiver{kind: webrtc.RTPCodecTypeAudio}
	require.Equal(t, w.GetPrimaryReceiverForRed(), w)
	w.isRED = true
	red := w.GetPrimaryReceiverForRed().(*RedPrimaryReceiver)
	require.NotNil(t, red)
	require.NoError(t, red.AddDownTrack(dt))

	header := rtp.Header{SequenceNumber: 65530, Timestamp: (uint32(1) << 31) - 2*tsStep, PayloadType: 111}
	primaryPkts := generatePkts(header, maxPktCount, tsStep)
	redPkts := generateRedPkts(t, primaryPkts, redCount)

	for _, i := range sendPktIdx {
		red.ForwardRTP(&buffer.ExtPacket{
			Packet: redPkts[i],
		}, 0)
	}

	expectPkts := make([]*rtp.Packet, 0, len(expectPktIdx))
	for _, i := range expectPktIdx {
		expectPkts = append(expectPkts, primaryPkts[i])
	}

	verifyPktsEqual(t, expectPkts, dt.receivedPkts)
}

func TestRedPrimaryReceiver(t *testing.T) {
	w := &WebRTCReceiver{kind: webrtc.RTPCodecTypeAudio}
	require.Equal(t, w.GetPrimaryReceiverForRed(), w)
	w.isRED = true
	red := w.GetPrimaryReceiverForRed().(*RedPrimaryReceiver)
	require.NotNil(t, red)

	t.Run("packet should send only once", func(t *testing.T) {
		maxPktCount := 19
		var sendPktIndex []int
		for i := 0; i < maxPktCount; i++ {
			sendPktIndex = append(sendPktIndex, i)
		}
		testRedRedPrimaryReceiver(t, maxPktCount, maxRedCount, sendPktIndex, sendPktIndex)
	})

	t.Run("packet duplicate and unorder", func(t *testing.T) {
		maxPktCount := 19
		var sendPktIndex []int
		for i := 0; i < maxPktCount; i++ {
			sendPktIndex = append(sendPktIndex, i)
			if i > 0 {
				sendPktIndex = append(sendPktIndex, i-1)
			}
			sendPktIndex = append(sendPktIndex, i)
		}
		testRedRedPrimaryReceiver(t, maxPktCount, maxRedCount, sendPktIndex, sendPktIndex)
	})

	t.Run("full recover", func(t *testing.T) {
		maxPktCount := 19
		var sendPktIndex, recvPktIndex []int
		for i := 0; i < maxPktCount; i++ {
			recvPktIndex = append(recvPktIndex, i)

			// drop packets covered by red encoding
			if i%(maxRedCount+1) != 0 {
				continue
			}
			sendPktIndex = append(sendPktIndex, i)
		}

		testRedRedPrimaryReceiver(t, maxPktCount, maxRedCount, sendPktIndex, recvPktIndex)
	})

	t.Run("lost 2 but red recover 1", func(t *testing.T) {
		maxPktCount := 19
		sendPktIndex := []int{0, 3, 6, 9, 12}
		recvPktIndex := []int{0, 2, 3, 5, 6, 8, 9, 11, 12}
		testRedRedPrimaryReceiver(t, maxPktCount, 1, sendPktIndex, recvPktIndex)
	})

	t.Run("part recover and long jump", func(t *testing.T) {
		maxPktCount := 50
		sendPktIndex := []int{0, 5, 12, 21 /*long jump*/, 24, 27}
		recvPktIndex := []int{0, 3, 4, 5, 10, 11, 12, 19, 20, 21, 22, 23, 24, 25, 26, 27}
		testRedRedPrimaryReceiver(t, maxPktCount, maxRedCount, sendPktIndex, recvPktIndex)
	})

	t.Run("unorder", func(t *testing.T) {
		maxPktCount := 50
		sendPktIndex := []int{20, 10 /*unorder can't recover*/, 25, 23, 34}
		recvPktIndex := []int{20, 10, 23, 24, 25, 21, 22, 23, 32, 33, 34}
		testRedRedPrimaryReceiver(t, maxPktCount, maxRedCount, sendPktIndex, recvPktIndex)
	})
}

func TestExtractPrimaryEncodingForRED(t *testing.T) {
	header := rtp.Header{SequenceNumber: 65530, Timestamp: (uint32(1) << 31) - 2*tsStep, PayloadType: 111}
	pkts := generatePkts(header, 10, tsStep)
	redPkts := generateRedPkts(t, pkts, 2)

	primaryPkts := make([]*rtp.Packet, 0, len(redPkts))

	for _, redPkt := range redPkts {
		payload, err := extractPrimaryEncodingForRED(redPkt.Payload)
		require.NoError(t, err)
		primaryPkts = append(primaryPkts, &rtp.Packet{
			Header:  redPkt.Header,
			Payload: payload,
		})
	}

	verifyPktsEqual(t, pkts, primaryPkts)
}
