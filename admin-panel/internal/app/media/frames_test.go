package media

import (
	"context"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func framePacket(seq uint16, timestamp uint32, marker bool, payload []byte) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: timestamp, SSRC: 1, Marker: marker}, Payload: payload}
}

func TestLargeKeyframeIsOneQueueEntry(t *testing.T) {
	engine := New(context.Background())
	engine.codec = codecH265
	peer := &viewer{queue: make(chan *queuedFrame, frameQueue)}
	engine.viewers["test"] = peer
	for i := 0; i < 500; i++ {
		header := byte(19)
		if i == 0 {
			header |= 0x80
		}
		if i == 499 {
			header |= 0x40
		}
		engine.broadcast(0, framePacket(uint16(i), 90000, i == 499, []byte{49 << 1, 1, header, 0x55}))
		if i < 499 && len(peer.queue) != 0 {
			t.Fatal("partial keyframe exposed to viewer")
		}
	}
	if len(peer.queue) != 1 {
		t.Fatal("large frame overflowed packet-sized queue")
	}
	frame := <-peer.queue
	if !frame.key || len(frame.packets) != 500 || engine.status.FrameCount != 1 || engine.status.QueueResets != 0 {
		t.Fatal("keyframe was split or dropped")
	}
}

func TestMissingFragmentDiscardsAccessUnitAndResyncsViewer(t *testing.T) {
	engine := New(context.Background())
	engine.codec = codecH265
	peer := &viewer{queue: make(chan *queuedFrame, frameQueue)}
	engine.viewers["test"] = peer
	engine.broadcast(0, framePacket(1, 90000, false, []byte{49 << 1, 1, 0x80 | 19, 0x55}))
	engine.broadcast(0, framePacket(3, 90000, true, []byte{49 << 1, 1, 0x40 | 19, 0x55}))
	if len(peer.queue) != 0 || !peer.resync.Load() || engine.status.IncompleteFrames == 0 {
		t.Fatal("damaged frame passed to browser")
	}
	engine.broadcast(0, framePacket(4, 93600, true, []byte{19 << 1, 1, 0x55}))
	if len(peer.queue) != 1 || !(<-peer.queue).key {
		t.Fatal("next complete keyframe not delivered")
	}
}

func TestFrameAssemblerWrapDuplicateAndTimestampChange(t *testing.T) {
	a := frameAssembler{}
	now := time.Now()
	first := framePacket(65535, 90000, false, []byte{28, 0x80 | 5, 0x55})
	if frame, broken := a.push(first, codecH264, now); frame != nil || broken {
		t.Fatal("partial FU emitted")
	}
	if frame, broken := a.push(first, codecH264, now); frame != nil || broken {
		t.Fatal("duplicate disrupted frame")
	}
	frame, broken := a.push(framePacket(0, 90000, true, []byte{28, 0x40 | 5, 0x66}), codecH264, now)
	if broken || frame == nil || !frame.key || len(frame.packets) != 2 {
		t.Fatal("sequence wrap damaged frame")
	}
	first.Payload[2] = 0xff
	if frame.packets[0].Payload[2] != 0x55 {
		t.Fatal("queued bytes alias reused network buffer")
	}
	a.push(framePacket(1, 93600, false, []byte{0x41, 0x55}), codecH264, now)
	frame, broken = a.push(framePacket(2, 97200, true, []byte{0x65, 0x55}), codecH264, now)
	if !broken || frame == nil || len(frame.packets) != 1 || !frame.key {
		t.Fatal("unfinished previous timestamp not discarded")
	}
}
