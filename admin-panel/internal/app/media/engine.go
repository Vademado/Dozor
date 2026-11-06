// Package media owns a native RTSP/TCP -> H.264/H.265 RTP -> WebRTC pipeline.
// No external processes, transcoding, CDN, STUN or TURN service are used.
package media

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"guard.local/admin/internal/app/domain"
)

const maxViewers = 8

type Answer struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	SDP  string `json:"sdp"`
}

type viewer struct {
	id         string
	generation uint64
	pc         *webrtc.PeerConnection
	track      *webrtc.TrackLocalStaticRTP
	queue      chan *queuedFrame
	ctx        context.Context
	cancel     context.CancelFunc
	resync     atomic.Bool
	codec      string
}

type Engine struct {
	control       sync.Mutex
	mu            sync.RWMutex
	root          context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	generation    uint64
	source        string
	status        domain.StreamStatus
	client        *gortsplib.Client
	videoMedia    *description.Media
	ssrc          uint32
	vps, sps, pps []byte
	codec         string
	lastPLI       time.Time
	assembler     frameAssembler
	viewers       map[string]*viewer
}

func New(ctx context.Context) *Engine {
	return &Engine{root: ctx, viewers: make(map[string]*viewer), status: domain.StreamStatus{Codecs: []string{}}}
}

// Start is idempotent and nonblocking. Only one RTSP connection is made per source.
func (e *Engine) Start(_ context.Context, source string) error {
	if _, err := base.ParseURL(source); err != nil {
		return errors.New("некорректный RTSP-адрес")
	}
	e.control.Lock()
	defer e.control.Unlock()
	e.mu.Lock()
	if e.status.Registered && e.source == source {
		e.mu.Unlock()
		return nil
	}
	oldCancel := e.cancel
	e.generation++
	generation := e.generation
	ctx, cancel := context.WithCancel(e.root)
	e.cancel = cancel
	e.source = source
	e.status = domain.StreamStatus{Registered: true, Codecs: []string{}, State: "connecting"}
	e.vps, e.sps, e.pps, e.codec = nil, nil, nil, ""
	e.mu.Unlock()
	if oldCancel != nil {
		oldCancel()
	}
	e.closeGeneration(0)
	e.wg.Add(1)
	go func() { defer e.wg.Done(); e.run(ctx, source, generation) }()
	return nil
}

func (e *Engine) Stop(context.Context) error {
	e.control.Lock()
	defer e.control.Unlock()
	e.mu.Lock()
	cancel := e.cancel
	e.cancel = nil
	e.generation++
	e.status = domain.StreamStatus{Codecs: []string{}, State: "stopped"}
	e.source = ""
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	e.closeGeneration(0)
	return nil
}

func (e *Engine) Close() { _ = e.Stop(context.Background()); e.wg.Wait() }

func (e *Engine) Status(context.Context) (domain.StreamStatus, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	s := e.status
	s.CheckedAt = time.Now().UTC()
	s.Viewers = len(e.viewers)
	s.HasProducer = s.Online && !s.LastPacketAt.IsZero() && time.Since(s.LastPacketAt) < 10*time.Second
	s.Codecs = append([]string{}, s.Codecs...)
	return s, nil
}

func (e *Engine) run(ctx context.Context, source string, generation uint64) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := e.read(ctx, source, generation)
		if ctx.Err() != nil {
			return
		}
		e.mu.Lock()
		if e.generation != generation {
			e.mu.Unlock()
			return
		}
		e.status.Online = false
		e.status.HasProducer = false
		e.status.Error = publicError(err)
		e.status.State = "retrying"
		e.mu.Unlock()
		e.closeGeneration(generation)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff = min(backoff*2, 15*time.Second)
	}
}

func (e *Engine) read(ctx context.Context, source string, generation uint64) error {
	u, err := base.ParseURL(source)
	if err != nil {
		return err
	}
	protocol := gortsplib.ProtocolTCP
	c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host, Protocol: &protocol, ReadTimeout: 8 * time.Second, WriteTimeout: 5 * time.Second, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, UserAgent: "Dozor/1.0"}
	c.OnPacketsLost = func(lost uint64) {
		e.mu.Lock()
		if e.generation == generation {
			e.status.RTPMissing += lost
		}
		e.mu.Unlock()
	}
	c.OnDecodeError = func(error) {
		e.mu.Lock()
		if e.generation == generation {
			e.status.RTPParseErrors++
		}
		e.mu.Unlock()
	}
	if err = c.Start(); err != nil {
		return err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	desc, _, err := c.Describe(u)
	if err != nil {
		return err
	}
	var h265 *format.H265
	var h264 *format.H264
	var videoFormat format.Format
	var vps, sps, pps []byte
	codec := codecH265
	videoMedia := desc.FindFormat(&h265)
	if videoMedia != nil {
		if h265.MaxDONDiff != 0 {
			return errors.New("unsupported H265 DONL")
		}
		videoFormat = h265
		vps, sps, pps = h265.SafeParams()
	} else {
		codec = codecH264
		videoMedia = desc.FindFormat(&h264)
		if videoMedia == nil {
			return errors.New("unsupported codec")
		}
		if h264.PacketizationMode > 1 {
			return errors.New("unsupported H264 packetization")
		}
		videoFormat = h264
		sps, pps = h264.SafeParams()
	}
	if _, err = c.Setup(desc.BaseURL, videoMedia, 0, 0); err != nil {
		return err
	}
	e.mu.Lock()
	if e.generation != generation {
		e.mu.Unlock()
		return context.Canceled
	}
	e.client, e.videoMedia = c, videoMedia
	e.vps, e.sps, e.pps = append([]byte(nil), vps...), append([]byte(nil), sps...), append([]byte(nil), pps...)
	e.codec = codec
	e.assembler = frameAssembler{}
	e.status.Codecs = []string{codec}
	e.mu.Unlock()
	c.OnPacketRTP(videoMedia, videoFormat, func(packet *rtp.Packet) { e.broadcast(generation, packet) })
	if _, err = c.Play(nil); err != nil {
		return err
	}
	e.mu.Lock()
	if e.generation == generation {
		e.status.Online = true
		e.status.Error = ""
		e.status.State = "streaming"
	}
	e.mu.Unlock()
	return c.Wait()
}

func publicError(err error) string {
	if err == nil {
		return "RTSP-соединение закрыто"
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "unsupported h265 donl"):
		return "H.265: режим RTP с DONL не поддерживается; нужен sprop-max-don-diff=0"
	case strings.Contains(message, "unsupported h264 packetization"):
		return "H.264: требуется packetization-mode 0 или 1"
	case strings.Contains(message, "unsupported codec"):
		return "Камера отдаёт неподдерживаемый формат. Доступны H.264 и H.265 без перекодирования."
	case strings.Contains(message, "401") || strings.Contains(message, "unauthorized"):
		return "RTSP: неверный логин или пароль камеры"
	case strings.Contains(message, "404"):
		return "RTSP: поток не найден. Проверьте путь /live/0"
	case strings.Contains(message, "refused"):
		return "RTSP-порт не принимает соединение. Проверьте запуск rkipc"
	case strings.Contains(message, "timeout") || strings.Contains(message, "deadline"):
		return "RTSP: тайм-аут. Проверьте питание платы, роутер, Wi-Fi и порт 554"
	default:
		return "Не удалось прочитать RTSP-поток. Проверьте адрес, доступность камеры и rkipc"
	}
}

func (e *Engine) broadcast(generation uint64, packet *rtp.Packet) {
	requestKey := false
	e.mu.Lock()
	defer func() {
		e.mu.Unlock()
		if requestKey {
			e.requestKeyframe()
		}
	}()
	if generation != e.generation {
		return
	}
	e.status.Bytes += uint64(packet.MarshalSize())
	e.status.RTPPackets++
	e.status.LastPacketAt = time.Now().UTC()
	var nals [][]byte
	if e.codec == codecH265 {
		nals = h265NALs(packet.Payload)
	} else {
		nals = nalsInPayload(packet.Payload)
	}
	for _, nal := range nals {
		if len(nal) > 0 {
			kind := nal[0] & 31
			if e.codec == codecH265 {
				kind = (nal[0] >> 1) & 63
			}
			switch {
			case e.codec == codecH265 && kind == 32:
				e.vps = append([]byte(nil), nal...)
			case e.codec == codecH265 && kind == 33 || e.codec != codecH265 && kind == 7:
				e.sps = append([]byte(nil), nal...)
			case e.codec == codecH265 && kind == 34 || e.codec != codecH265 && kind == 8:
				e.pps = append([]byte(nil), nal...)
			}
		}
	}
	e.ssrc = packet.SSRC
	queued, broken := e.assembler.push(packet, e.codec, time.Now())
	if broken {
		e.status.IncompleteFrames++
		for _, peer := range e.viewers {
			peer.resync.Store(true)
		}
		requestKey = len(e.viewers) > 0
	}
	if queued == nil {
		return
	}
	e.status.FrameCount++
	e.status.LastFrameAt = queued.completedAt
	if queued.key {
		queued.vps = e.vps
		queued.sps = e.sps
		queued.pps = e.pps
	}
	for _, peer := range e.viewers {
		if peer.generation != generation {
			continue
		}
		select {
		case peer.queue <- queued:
		default:
			e.status.QueueResets++
			requestKey = true
			peer.resync.Store(true)
		drain:
			for {
				select {
				case <-peer.queue:
				default:
					break drain
				}
			}
			select {
			case peer.queue <- queued:
			default:
			}
		}
	}
}

func nalsInPayload(payload []byte) [][]byte {
	if len(payload) == 0 {
		return nil
	}
	kind := payload[0] & 31
	if kind > 0 && kind < 24 {
		return [][]byte{payload}
	}
	if kind != 24 {
		return nil
	}
	var nals [][]byte
	for rest := payload[1:]; len(rest) >= 2; {
		size := int(binary.BigEndian.Uint16(rest))
		rest = rest[2:]
		if size == 0 || size > len(rest) {
			break
		}
		nals = append(nals, rest[:size])
		rest = rest[size:]
	}
	return nals
}

func startsIDR(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	if payload[0]&31 == 28 {
		return len(payload) > 1 && payload[1]&0x80 != 0 && payload[1]&31 == 5
	}
	for _, nal := range nalsInPayload(payload) {
		if len(nal) > 0 && nal[0]&31 == 5 {
			return true
		}
	}
	return false
}

// Answer negotiates a video-only peer. The browser never receives RTSP credentials.
func (e *Engine) Answer(ctx context.Context, sdp string) (Answer, error) {
	e.mu.RLock()
	status := e.status
	generation := e.generation
	sps := append([]byte(nil), e.sps...)
	codec := e.codec
	count := len(e.viewers)
	e.mu.RUnlock()
	if !status.Registered {
		return Answer{}, errors.New("просмотр остановлен")
	}
	if !status.Online {
		if status.Error != "" {
			return Answer{}, errors.New(status.Error)
		}
		return Answer{}, errors.New("RTSP: подключаемся к камере; повторите через несколько секунд")
	}
	if count >= maxViewers {
		return Answer{}, errors.New("достигнут лимит: 8 одновременных подключений")
	}
	capability := codecCapability(codec, sps)
	if !hasVideoCodec(sdp, capability, true) {
		return Answer{}, fmt.Errorf("%w: камера использует %s, но браузер не предлагает этот кодек в WebRTC", ErrBrowserCodec, codec)
	}
	setting := webrtc.SettingEngine{}
	setting.SetIncludeLoopbackCandidate(true)
	setting.SetICETimeouts(5*time.Second, 15*time.Second, 2*time.Second)
	mediaEngine := &webrtc.MediaEngine{}
	capability.RTCPFeedback = []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}, {Type: "ccm", Parameter: "fir"}}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: capability, PayloadType: 96}, webrtc.RTPCodecTypeVideo); err != nil {
		return Answer{}, errors.New("не удалось зарегистрировать видеокодек")
	}
	api := webrtc.NewAPI(webrtc.WithSettingEngine(setting), webrtc.WithMediaEngine(mediaEngine))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return Answer{}, errors.New("не удалось создать WebRTC-соединение")
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		pc.Close()
		return Answer{}, err
	}
	id := hex.EncodeToString(random[:])
	peerCtx, cancel := context.WithCancel(e.root)
	peer := &viewer{id: id, generation: generation, pc: pc, queue: make(chan *queuedFrame, frameQueue), ctx: peerCtx, cancel: cancel, codec: codec}
	defer func() {
		if err != nil {
			e.CloseViewer(id)
			cancel()
			pc.Close()
		}
	}()
	peer.track, err = webrtc.NewTrackLocalStaticRTP(capability, "video", "dozor")
	if err != nil {
		return Answer{}, errors.New("не удалось создать видеодорожку")
	}
	sender, err := pc.AddTrack(peer.track)
	if err != nil {
		return Answer{}, errors.New("не удалось добавить видеодорожку")
	}
	pc.OnConnectionStateChange(func(connection webrtc.PeerConnectionState) {
		if connection == webrtc.PeerConnectionStateFailed || connection == webrtc.PeerConnectionStateClosed {
			go e.CloseViewer(id)
		}
		if connection == webrtc.PeerConnectionStateConnected {
			e.requestKeyframe()
		}
	})
	e.mu.Lock()
	if generation != e.generation || len(e.viewers) >= maxViewers {
		e.mu.Unlock()
		err = errors.New("источник изменился или достигнут лимит просмотров")
		return Answer{}, err
	}
	e.viewers[id] = peer
	e.mu.Unlock()
	go func() { peer.writePackets(); e.CloseViewer(id) }()
	go func() {
		for {
			packets, _, readErr := sender.ReadRTCP()
			if readErr != nil {
				return
			}
			for _, p := range packets {
				switch p.(type) {
				case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
					e.requestKeyframe()
				}
			}
		}
	}()
	go func() {
		timer := time.NewTimer(20 * time.Second)
		defer timer.Stop()
		select {
		case <-peerCtx.Done():
			return
		case <-timer.C:
			if pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
				e.CloseViewer(id)
			}
		}
	}()
	if err = pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
		return Answer{}, errors.New("неверное SDP-предложение браузера")
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return Answer{}, fmt.Errorf("%w: не удалось согласовать %s", ErrBrowserCodec, codec)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(answer); err != nil {
		return Answer{}, errors.New("не удалось подготовить WebRTC-ответ")
	}
	if !hasVideoCodec(pc.LocalDescription().SDP, capability, false) {
		err = fmt.Errorf("%w: не согласован профиль %s камеры", ErrBrowserCodec, codec)
		return Answer{}, err
	}
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		err = ctx.Err()
		return Answer{}, err
	case <-timer.C:
		err = errors.New("WebRTC: не удалось собрать локальные ICE-адреса")
		return Answer{}, err
	case <-gathered:
	}
	return Answer{ID: id, Type: "answer", SDP: pc.LocalDescription().SDP}, nil
}

func (p *viewer) writePackets() {
	var random [2]byte
	_, _ = rand.Read(random[:])
	sequence := binary.BigEndian.Uint16(random[:])
	waiting := true
	send := func(payload []byte, timestamp uint32, marker bool) error {
		var fragments [][]byte
		if p.codec == codecH265 {
			fragments = h265WebRTCPayloads(payload)
		} else {
			fragments = webRTCPayloads(payload)
		}
		for i, fragment := range fragments {
			packet := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: sequence, Timestamp: timestamp, Marker: marker && i == len(fragments)-1}, Payload: fragment}
			sequence++
			if err := p.track.WriteRTP(packet); err != nil {
				return err
			}
		}
		return nil
	}
	for {
		select {
		case <-p.ctx.Done():
			return
		case item := <-p.queue:
			if p.resync.Swap(false) {
				waiting = true
			}
			if time.Since(item.completedAt) > maxQueueAge {
				waiting = true
				continue
			}
			if waiting && !item.key {
				continue
			}
			if item.key {
				if len(item.vps) > 0 {
					if send(item.vps, item.packets[0].Timestamp, false) != nil {
						return
					}
				}
				// Parameter sets from SDP/in-band RTP are sent before each IDR. This
				// allows a late viewer to start decoding without a camera restart.
				if len(item.sps) > 0 {
					if send(item.sps, item.packets[0].Timestamp, false) != nil {
						return
					}
				}
				if len(item.pps) > 0 {
					if send(item.pps, item.packets[0].Timestamp, false) != nil {
						return
					}
				}
				waiting = false
			}
			for _, packet := range item.packets {
				if send(packet.Payload, packet.Timestamp, packet.Marker) != nil {
					return
				}
			}
		}
	}
}

// Keep SRTP packets below a conservative LAN MTU, including DTLS/SRTP overhead.
// RTSP/TCP may deliver larger RTP packets than a browser UDP path can carry.
func webRTCPayloads(payload []byte) [][]byte {
	if len(payload) <= 1200 {
		return [][]byte{payload}
	}
	kind := payload[0] & 31
	if kind == 24 {
		var out [][]byte
		for _, nal := range nalsInPayload(payload) {
			out = append(out, webRTCPayloads(nal)...)
		}
		return out
	}
	var indicator, header byte
	var data []byte
	var starts, ends bool
	if kind == 28 && len(payload) > 2 {
		indicator = payload[0]
		header = payload[1] & 31
		data = payload[2:]
		starts = payload[1]&0x80 != 0
		ends = payload[1]&0x40 != 0
	} else {
		indicator = payload[0]&0xe0 | 28
		header = kind
		data = payload[1:]
		starts = true
		ends = true
	}
	var out [][]byte
	for offset := 0; offset < len(data); offset += 1198 {
		end := min(offset+1198, len(data))
		h := header
		if offset == 0 && starts {
			h |= 0x80
		}
		if end == len(data) && ends {
			h |= 0x40
		}
		fragment := make([]byte, 2+end-offset)
		fragment[0], fragment[1] = indicator, h
		copy(fragment[2:], data[offset:end])
		out = append(out, fragment)
	}
	return out
}

func (e *Engine) requestKeyframe() {
	e.mu.Lock()
	if e.client == nil || e.videoMedia == nil || e.ssrc == 0 || time.Since(e.lastPLI) < time.Second {
		e.mu.Unlock()
		return
	}
	e.lastPLI = time.Now()
	client, videoMedia, ssrc := e.client, e.videoMedia, e.ssrc
	e.mu.Unlock()
	_ = client.WritePacketRTCP(videoMedia, &rtcp.PictureLossIndication{MediaSSRC: ssrc})
}

func (e *Engine) CloseViewer(id string) {
	e.mu.Lock()
	peer := e.viewers[id]
	delete(e.viewers, id)
	e.mu.Unlock()
	if peer != nil {
		peer.cancel()
		_ = peer.pc.Close()
	}
}

func (e *Engine) closeGeneration(generation uint64) {
	e.mu.RLock()
	ids := make([]string, 0, len(e.viewers))
	for id, peer := range e.viewers {
		if generation == 0 || peer.generation == generation {
			ids = append(ids, id)
		}
	}
	e.mu.RUnlock()
	for _, id := range ids {
		e.CloseViewer(id)
	}
}

func (e *Engine) String() string {
	return fmt.Sprintf("native RTSP/WebRTC, max viewers=%d", maxViewers)
}
