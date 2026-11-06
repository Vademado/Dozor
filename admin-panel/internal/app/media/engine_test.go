package media

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// fixtureRTSP emits protocol fixtures, not a demonstration camera or fake UI data.
type fixtureRTSP struct{ stream *gortsplib.ServerStream }

func (f *fixtureRTSP) OnDescribe(*gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, f.stream, nil
}
func (f *fixtureRTSP) OnSetup(*gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, f.stream, nil
}
func (f *fixtureRTSP) OnPlay(*gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	return &base.Response{StatusCode: base.StatusOK}, nil
}

func TestNativeRTSPToWebRTCLateViewerAndCleanup(t *testing.T) {
	for _, codec := range []string{codecH264, codecH265} {
		t.Run(codec, func(t *testing.T) { testNativeRTSPToWebRTC(t, codec) })
	}
}

func testNativeRTSPToWebRTC(t *testing.T, codec string) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &fixtureRTSP{}
	server := &gortsplib.Server{RTSPAddress: listener.Addr().String(), Handler: fixture, Listen: func(string, string) (net.Listener, error) { return listener, nil }}
	if err = server.Start(); err != nil {
		listener.Close()
		t.Fatal(err)
	}
	defer server.Close()
	video := &description.Media{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PayloadTyp: 96, PacketizationMode: 1, SPS: []byte{0x67, 0x42, 0xe0, 0x1f, 0x95, 0xa8, 0x14, 0x01, 0x6e, 0x40}, PPS: []byte{0x68, 0xce, 0x3c, 0x80}}}}
	payload := []byte{0x65, 0x88, 0x84, 0x21}
	wantTypes := []byte{7, 8, 5}
	if codec == codecH265 {
		decode := func(value string) []byte {
			data, err := base64.StdEncoding.DecodeString(value)
			if err != nil {
				t.Fatal(err)
			}
			return data
		}
		video.Formats = []format.Format{&format.H265{PayloadTyp: 96, VPS: decode("QAEMAf//AWAAAAMAsAAAAwAAAwB4FwJA"), SPS: decode("QgEBAWAAAAMAsAAAAwAAAwB4oAKggC8c1YgXuRZFL/y5/E/qbgQEBAE="), PPS: decode("RAHAcvBTJA==")}}
		payload = []byte{19 << 1, 1, 0x88, 0x84, 0x21}
		wantTypes = []byte{32, 33, 34, 19}
	}
	fixture.stream = &gortsplib.ServerStream{Server: server, Desc: &description.Session{Medias: []*description.Media{video}}}
	if err = fixture.stream.Initialize(); err != nil {
		t.Fatal(err)
	}
	defer fixture.stream.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine := New(ctx)
	defer engine.Close()
	if err = engine.Start(ctx, "rtsp://"+listener.Addr().String()+"/live/0"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, _ := engine.Status(ctx)
		if status.Online {
			if len(status.Codecs) != 1 || status.Codecs[0] != codec {
				t.Fatalf("wrong source codec: %+v", status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("RTSP not connected: %+v", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if codec == codecH265 {
		badOffer := "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\nm=video 9 UDP/TLS/RTP/SAVPF 96\r\na=recvonly\r\na=rtpmap:96 H264/90000\r\n"
		if _, err = engine.Answer(ctx, badOffer); !errors.Is(err, ErrBrowserCodec) {
			t.Fatalf("H264-only browser accepted H265: %v", err)
		}
		status, _ := engine.Status(ctx)
		if status.Viewers != 0 {
			t.Fatal("rejected offer leaked a viewer")
		}
	}
	// A separate Pion peer stands in for the browser and verifies actual SRTP data.
	setting := webrtc.SettingEngine{}
	setting.SetIncludeLoopbackCandidate(true)
	pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(setting)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	if _, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	received := make(chan *rtp.Packet, 12)
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			packet, _, readErr := track.ReadRTP()
			if readErr != nil {
				return
			}
			select {
			case received <- packet:
			default:
			}
		}
	})
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gather:
	case <-time.After(6 * time.Second):
		t.Fatal("client ICE timeout")
	}
	answer, err := engine.Answer(ctx, pc.LocalDescription().SDP)
	if err != nil {
		t.Fatal(err)
	}
	if err = pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		t.Fatal(err)
	}
	sendCtx, stopSending := context.WithCancel(ctx)
	defer stopSending()
	go func() {
		ticker := time.NewTicker(40 * time.Millisecond)
		defer ticker.Stop()
		var seq uint16
		var timestamp uint32
		for {
			select {
			case <-sendCtx.Done():
				return
			case <-ticker.C:
				seq++
				timestamp += 3600
				_ = fixture.stream.WritePacketRTP(video, &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SSRC: 1234, SequenceNumber: seq, Timestamp: timestamp, Marker: true}, Payload: payload})
			}
		}
	}()
	var packets []*rtp.Packet
	for len(packets) < len(wantTypes) {
		select {
		case packet := <-received:
			packets = append(packets, packet)
		case <-time.After(10 * time.Second):
			t.Fatal("no WebRTC RTP received from RTSP fixture")
		}
	}
	for i, packet := range packets {
		kind := packet.Payload[0] & 31
		if codec == codecH265 {
			kind = (packet.Payload[0] >> 1) & 63
		}
		if kind != wantTypes[i] {
			t.Fatalf("parameter sets/keyframe out of order: index %d type %d", i, kind)
		}
		if i > 0 && packet.SequenceNumber != packets[i-1].SequenceNumber+1 {
			t.Fatal("injected packets broke RTP sequence")
		}
		if packet.Timestamp != packets[0].Timestamp {
			t.Fatal("parameter sets have wrong timestamp")
		}
		if packet.Marker != (i == len(wantTypes)-1) {
			t.Fatal("incorrect access-unit RTP marker")
		}
	}
	engine.CloseViewer(answer.ID)
	status, _ := engine.Status(ctx)
	if status.Viewers != 0 {
		t.Fatal("viewer leaked")
	}
	if err = engine.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	status, _ = engine.Status(ctx)
	if status.Registered || status.Online {
		t.Fatal("stop did not close RTSP")
	}
}

func TestRTSPErrorsDoNotExposeCredentials(t *testing.T) {
	for _, message := range []string{"dial rtsp://root:secret@camera: timeout", "401 for rtsp://root:secret@camera", "unexpected response rtsp://root:secret@camera"} {
		public := publicError(errors.New(message))
		if strings.Contains(public, "secret") || strings.Contains(public, "root:") {
			t.Fatal("RTSP credentials leaked")
		}
	}
}
