package media

import (
	"github.com/pion/rtp"
	"time"
)

const frameQueue = 4
const maxFrameBytes = 8 << 20
const maxFramePackets = 8192
const maxQueueAge = 250 * time.Millisecond

type queuedFrame struct {
	packets       []*rtp.Packet
	vps, sps, pps []byte
	key           bool
	completedAt   time.Time
}

// frameAssembler commits only complete RTP access units. Renumbering a stream
// with a missing FU fragment must never disguise a damaged compressed frame.
// It is called under Engine.mu; frames become immutable when emitted.
type frameAssembler struct {
	seen            bool
	sequence        uint16
	ssrc            uint32
	timestamp       uint32
	packets         []*rtp.Packet
	size            int
	bad, key, video bool
}

func (a *frameAssembler) push(packet *rtp.Packet, codec string, now time.Time) (*queuedFrame, bool) {
	broken := false
	if a.seen && packet.SSRC == a.ssrc && packet.SequenceNumber == a.sequence {
		return nil, false
	}
	gap := a.seen && (packet.SSRC != a.ssrc || packet.SequenceNumber != a.sequence+1)
	if len(a.packets) > 0 && (packet.Timestamp != a.timestamp || packet.SSRC != a.ssrc) {
		broken = true
		a.packets = nil
		a.size = 0
		a.bad = false
		a.key = false
		a.video = false
	}
	if gap {
		broken = true
		if len(a.packets) > 0 {
			a.bad = true
		}
	}
	a.seen = true
	a.sequence = packet.SequenceNumber
	a.ssrc = packet.SSRC
	a.timestamp = packet.Timestamp
	a.size += packet.MarshalSize()
	if a.size > maxFrameBytes || len(a.packets) >= maxFramePackets {
		a.bad = true
		broken = true
	}
	if !a.bad {
		a.packets = append(a.packets, packet.Clone())
	}
	if codec == codecH265 {
		a.key = a.key || h265RandomAccess(packet.Payload)
		if len(packet.Payload) >= 2 {
			kind := (packet.Payload[0] >> 1) & 63
			if kind <= 31 || kind == 49 && len(packet.Payload) >= 3 && packet.Payload[2]&63 <= 31 {
				a.video = true
			}
		}
		for _, nal := range h265NALs(packet.Payload) {
			if (nal[0]>>1)&63 <= 31 {
				a.video = true
			}
		}
	} else {
		a.key = a.key || startsIDR(packet.Payload)
		if len(packet.Payload) > 0 {
			kind := packet.Payload[0] & 31
			if kind >= 1 && kind <= 5 || kind == 28 && len(packet.Payload) >= 2 && packet.Payload[1]&31 <= 5 {
				a.video = true
			}
			for _, nal := range nalsInPayload(packet.Payload) {
				if kind := nal[0] & 31; kind >= 1 && kind <= 5 {
					a.video = true
				}
			}
		}
	}
	if !packet.Marker {
		return nil, broken
	}
	var frame *queuedFrame
	if !a.bad && a.video {
		frame = &queuedFrame{packets: a.packets, key: a.key, completedAt: now}
	}
	a.packets = nil
	a.size = 0
	a.bad = false
	a.key = false
	a.video = false
	return frame, broken
}
