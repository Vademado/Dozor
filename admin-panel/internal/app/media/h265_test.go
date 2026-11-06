package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph265"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func TestHEVCNegotiationRequiresCodecProfileAndDirection(t *testing.T) {
	capability := codecCapability(codecH265, nil)
	base := "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\nm=video 9 UDP/TLS/RTP/SAVPF 96\r\na=recvonly\r\na=rtpmap:96 H265/90000\r\na=fmtp:96 profile-id=1;tier-flag=0;level-id=153;tx-mode=SRST\r\n"
	for _, tc := range []struct {
		name, sdp       string
		receiving, want bool
	}{
		{"offer", base, true, true},
		{"answer", strings.ReplaceAll(base, "recvonly", "sendonly"), false, true},
		{"wrong direction", strings.ReplaceAll(base, "recvonly", "sendonly"), true, false},
		{"H264 is not HEVC", strings.ReplaceAll(base, "H265/90000", "H264/90000"), true, false},
		{"Main10 is not Main", strings.ReplaceAll(base, "profile-id=1", "profile-id=2"), true, false},
		{"rejected video", strings.ReplaceAll(base, "m=video 9", "m=video 0"), true, false},
		{"inactive", strings.ReplaceAll(base, "recvonly", "inactive"), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasVideoCodec(tc.sdp, capability, tc.receiving); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
	if capability.MimeType != webrtc.MimeTypeH265 {
		t.Fatal("H265 source advertised as another codec")
	}
}

// A separate RFC7798 decoder must reconstruct the exact compressed NALs after
// repacketization; testing only our own headers would miss matching mistakes.
func TestHEVCRepacketizationRoundTrip(t *testing.T) {
	nal := append([]byte{19<<1 | 1, 0x23}, bytes.Repeat([]byte{0x9a}, 4100)...)
	other := []byte{33 << 1, 1, 0x90}
	ap := []byte{48 << 1, 1}
	for _, part := range [][]byte{other, nal} {
		ap = binary.BigEndian.AppendUint16(ap, uint16(len(part)))
		ap = append(ap, part...)
	}
	// Simulate two large incoming TCP FU packets, then split each for SRTP.
	fuStart := append([]byte{49<<1 | 1, 0x23, 0x80 | 19}, nal[2:2102]...)
	fuEnd := append([]byte{49<<1 | 1, 0x23, 0x40 | 19}, nal[2102:]...)
	for _, tc := range []struct {
		name        string
		input, want [][]byte
	}{
		{"single NAL", [][]byte{nal}, [][]byte{nal}},
		{"large incoming FU", [][]byte{fuStart, fuEnd}, [][]byte{nal}},
		{"aggregation packet", [][]byte{ap}, [][]byte{other, nal}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decoder := &rtph265.Decoder{}
			if err := decoder.Init(); err != nil {
				t.Fatal(err)
			}
			var fragments [][]byte
			for _, payload := range tc.input {
				fragments = append(fragments, h265WebRTCPayloads(payload)...)
			}
			var decoded [][]byte
			for i, payload := range fragments {
				if len(payload) > 1200 {
					t.Fatal("oversized WebRTC packet")
				}
				packet := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: uint16(65534 + i), Timestamp: 90000, Marker: i == len(fragments)-1}, Payload: payload}
				got, err := decoder.Decode(packet)
				if err != nil && !errors.Is(err, rtph265.ErrMorePacketsNeeded) {
					t.Fatal(err)
				}
				decoded = append(decoded, got...)
			}
			if len(decoded) != len(tc.want) {
				t.Fatalf("decoded %d NALs want %d", len(decoded), len(tc.want))
			}
			for i := range decoded {
				if !bytes.Equal(decoded[i], tc.want[i]) {
					t.Fatal("compressed bytes or layer/temporal header changed")
				}
			}
		})
	}
}

func TestHEVCParameterSetsAndKeyframeDetection(t *testing.T) {
	for _, kind := range []byte{16, 17, 18, 19, 20, 21} {
		if !h265RandomAccess([]byte{kind << 1, 1, 7}) || !h265RandomAccess([]byte{49 << 1, 1, 0x80 | kind, 7}) {
			t.Fatalf("keyframe type %d missed", kind)
		}
		if h265RandomAccess([]byte{49 << 1, 1, kind, 7}) {
			t.Fatal("middle FU treated as keyframe start")
		}
	}
	for _, payload := range [][]byte{nil, {0}, {48 << 1, 1, 0, 9, 32 << 1, 1}, {48 << 1, 1, 0, 1, 32}, {49 << 1, 1, 0x80}} {
		if h265RandomAccess(payload) {
			t.Fatal("malformed packet treated as keyframe")
		}
	}
}
