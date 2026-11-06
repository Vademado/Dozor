// streamcheck measures the camera before WebRTC or the browser, without changing it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph265"
	"github.com/pion/rtp"
)

func main() {
	source := flag.String("source", "rtsp://192.168.1.199:554/live/0", "RTSP source")
	duration := flag.Duration("duration", 15*time.Second, "measurement duration after PLAY")
	flag.Parse()
	if err := run(*source, *duration); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(source string, duration time.Duration) error {
	u, err := base.ParseURL(source)
	if err != nil {
		return errors.New("invalid RTSP URL")
	}
	protocol := gortsplib.ProtocolTCP
	var packets, bytes, lost, complete, invalid, decodeErrors, keys atomic.Uint64
	var last, maxGap atomic.Int64
	c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host, Protocol: &protocol, ReadTimeout: 8 * time.Second, WriteTimeout: 5 * time.Second, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		OnPacketsLost: func(n uint64) { lost.Add(n) }, OnDecodeError: func(error) { decodeErrors.Add(1) }}
	if err = c.Start(); err != nil {
		return errors.New("RTSP client start failed")
	}
	defer c.Close()
	desc, _, err := c.Describe(u)
	if err != nil {
		return fmt.Errorf("RTSP DESCRIBE failed (%T)", err)
	}
	var h265 *format.H265
	var h264 *format.H264
	var video *description.Media
	var selected format.Format
	var decode func(*rtp.Packet) ([][]byte, error)
	var more, nonstarting error
	var codec string
	if video = desc.FindFormat(&h265); video != nil {
		selected = h265
		codec = "H.265"
		d, err := h265.CreateDecoder()
		if err != nil {
			return err
		}
		decode = d.Decode
		more = rtph265.ErrMorePacketsNeeded
		nonstarting = rtph265.ErrNonStartingPacketAndNoPrevious
	} else {
		video = desc.FindFormat(&h264)
		if video == nil {
			return errors.New("no H264/H265 video")
		}
		selected = h264
		codec = "H.264"
		d, err := h264.CreateDecoder()
		if err != nil {
			return err
		}
		decode = d.Decode
		more = rtph264.ErrMorePacketsNeeded
		nonstarting = rtph264.ErrNonStartingPacketAndNoPrevious
	}
	if _, err = c.Setup(desc.BaseURL, video, 0, 0); err != nil {
		return fmt.Errorf("RTSP SETUP failed (%T)", err)
	}
	c.OnPacketRTP(video, selected, func(p *rtp.Packet) {
		packets.Add(1)
		bytes.Add(uint64(p.MarshalSize()))
		now := time.Now().UnixNano()
		previous := last.Swap(now)
		if previous > 0 {
			gap := now - previous
			for old := maxGap.Load(); gap > old; old = maxGap.Load() {
				if maxGap.CompareAndSwap(old, gap) {
					break
				}
			}
		}
		au, e := decode(p)
		if e != nil {
			if !errors.Is(e, more) && !errors.Is(e, nonstarting) {
				invalid.Add(1)
			}
			return
		}
		if len(au) == 0 {
			return
		}
		video, key := false, false
		for _, nal := range au {
			kind := nal[0] & 31
			if codec == "H.265" {
				kind = (nal[0] >> 1) & 63
			}
			if codec == "H.265" && kind <= 31 || codec == "H.264" && kind >= 1 && kind <= 5 {
				video = true
			}
			if codec == "H.265" && kind >= 16 && kind <= 21 || codec == "H.264" && kind == 5 {
				key = true
			}
		}
		if video {
			complete.Add(1)
		}
		if key {
			keys.Add(1)
		}
	})
	if _, err = c.Play(nil); err != nil {
		return fmt.Errorf("RTSP PLAY failed (%T)", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	started := time.Now()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	encoder := json.NewEncoder(os.Stdout)
	report := func() {
		seconds := time.Since(started).Seconds()
		_ = encoder.Encode(map[string]any{"codec": codec, "seconds": seconds, "packets": packets.Load(), "rtp_missing": lost.Load(), "kbps_avg": float64(bytes.Load()) * 8 / 1000 / seconds, "complete_access_units": complete.Load(), "fps_avg": float64(complete.Load()) / seconds, "damaged_access_units": invalid.Load(), "rtp_parse_errors": decodeErrors.Load(), "keyframes": keys.Load(), "max_packet_gap_ms": float64(maxGap.Load()) / 1e6})
	}
	for {
		select {
		case <-ctx.Done():
			report()
			return nil
		case <-ticker.C:
			report()
		case err = <-done:
			report()
			return fmt.Errorf("RTSP ended (%T)", err)
		}
	}
}
