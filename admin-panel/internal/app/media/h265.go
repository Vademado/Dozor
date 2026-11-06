package media

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

const codecH264 = "H.264"
const codecH265 = "H.265"

var ErrBrowserCodec = errors.New("кодек не поддерживается браузером")

// codecCapability describes the actual compressed source; no pixels are decoded.
func codecCapability(codec string, sps []byte) webrtc.RTPCodecCapability {
	if codec == codecH265 {
		profile, tier, level := uint8(1), uint8(0), uint8(93)
		var parsed h265.SPS
		if parsed.Unmarshal(sps) == nil {
			profile = parsed.ProfileTierLevel.GeneralProfileIdc
			tier = parsed.ProfileTierLevel.GeneralTierFlag
			level = parsed.ProfileTierLevel.GeneralLevelIdc
		}
		return webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH265, ClockRate: 90000, SDPFmtpLine: fmt.Sprintf("profile-id=%d;tier-flag=%d;level-id=%d;tx-mode=SRST", profile, tier, level)}
	}
	fmtp := "level-asymmetry-allowed=1;packetization-mode=1"
	if len(sps) >= 4 {
		fmtp += ";profile-level-id=" + hex.EncodeToString(sps[1:4])
	}
	return webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: fmtp}
}

// hasVideoCodec rejects offers/answers without an active, matching video track.
// Presence of H.264 must never be mistaken for support of an H.265 source.
func hasVideoCodec(raw string, capability webrtc.RTPCodecCapability, receiving bool) bool {
	var session sdp.SessionDescription
	if session.Unmarshal([]byte(raw)) != nil {
		return false
	}
	name := strings.TrimPrefix(strings.ToLower(capability.MimeType), "video/") + "/90000"
	for _, medium := range session.MediaDescriptions {
		if medium.MediaName.Media != "video" || medium.MediaName.Port.Value == 0 {
			continue
		}
		if _, inactive := medium.Attribute("inactive"); inactive {
			continue
		}
		if _, sendonly := medium.Attribute("sendonly"); sendonly && receiving {
			continue
		}
		if _, recvonly := medium.Attribute("recvonly"); recvonly && !receiving {
			continue
		}
		for _, attribute := range medium.Attributes {
			if attribute.Key != "rtpmap" {
				continue
			}
			fields := strings.Fields(attribute.Value)
			if len(fields) != 2 || !strings.EqualFold(fields[1], name) {
				continue
			}
			for _, pt := range medium.MediaName.Formats {
				if pt == fields[0] {
					if strings.EqualFold(capability.MimeType, webrtc.MimeTypeH265) {
						parameters := ""
						for _, a := range medium.Attributes {
							if a.Key == "fmtp" && strings.HasPrefix(a.Value, pt+" ") {
								parameters = strings.TrimPrefix(a.Value, pt+" ")
								break
							}
						}
						if !matchingHEVCProfile(capability.SDPFmtpLine, parameters) {
							continue
						}
					}
					return true
				}
			}
		}
	}
	return false
}

// Pion can fall back to matching MIME alone. A forwarding server cannot convert
// Main/Main10 profiles, so require the source profile, tier and SRST explicitly.
func matchingHEVCProfile(source, receiver string) bool {
	parse := func(line string) map[string]string {
		p := map[string]string{"profile-id": "1", "tier-flag": "0", "tx-mode": "SRST"}
		for _, part := range strings.Split(line, ";") {
			key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
			if ok {
				p[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
			}
		}
		return p
	}
	a, b := parse(source), parse(receiver)
	return a["profile-id"] == b["profile-id"] && a["tier-flag"] == b["tier-flag"] && strings.EqualFold(a["tx-mode"], b["tx-mode"])
}

// RFC 7798, single NAL units and aggregation packets (AP), with no DONL.
func h265NALs(payload []byte) [][]byte {
	if len(payload) < 2 {
		return nil
	}
	kind := (payload[0] >> 1) & 63
	if kind < 48 {
		return [][]byte{payload}
	}
	if kind != 48 {
		return nil
	}
	var out [][]byte
	for rest := payload[2:]; len(rest) >= 2; {
		size := int(binary.BigEndian.Uint16(rest))
		rest = rest[2:]
		if size < 2 || size > len(rest) {
			return nil
		}
		out = append(out, rest[:size])
		rest = rest[size:]
		if len(rest) == 1 {
			return nil
		}
	}
	return out
}

func h265RandomAccess(payload []byte) bool {
	if len(payload) < 2 {
		return false
	}
	if (payload[0]>>1)&63 == 49 {
		if len(payload) < 4 || payload[2]&0x80 == 0 {
			return false
		}
		kind := payload[2] & 63
		return kind >= 16 && kind <= 21 // BLA, IDR and CRA
	}
	for _, nal := range h265NALs(payload) {
		kind := (nal[0] >> 1) & 63
		if kind >= 16 && kind <= 21 {
			return true
		}
	}
	return false
}

// Re-fragment oversized RTSP/TCP payloads into <=1200-byte WebRTC payloads.
// H.265 has a TWO-byte NAL header and FU type 49; H.264's FU-A layout differs.
func h265WebRTCPayloads(payload []byte) [][]byte {
	if len(payload) < 2 {
		return nil
	}
	if len(payload) <= 1200 {
		return [][]byte{payload}
	}
	kind := (payload[0] >> 1) & 63
	if kind == 48 {
		var out [][]byte
		for _, nal := range h265NALs(payload) {
			out = append(out, h265WebRTCPayloads(nal)...)
		}
		return out
	}
	var header [2]byte
	var data []byte
	var starts, ends bool
	if kind == 49 {
		if len(payload) < 4 {
			return nil
		}
		header = [2]byte{payload[0], payload[1]}
		kind = payload[2] & 63
		data = payload[3:]
		starts = payload[2]&0x80 != 0
		ends = payload[2]&0x40 != 0
	} else if kind < 48 {
		header = [2]byte{payload[0]&0x81 | 49<<1, payload[1]}
		data = payload[2:]
		starts, ends = true, true
	} else {
		return nil
	}
	var out [][]byte
	for offset := 0; offset < len(data); offset += 1197 {
		end := min(offset+1197, len(data))
		fu := kind
		if offset == 0 && starts {
			fu |= 0x80
		}
		if end == len(data) && ends {
			fu |= 0x40
		}
		fragment := make([]byte, 3+end-offset)
		fragment[0], fragment[1], fragment[2] = header[0], header[1], fu
		copy(fragment[3:], data[offset:end])
		out = append(out, fragment)
	}
	return out
}
