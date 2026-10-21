// Copyright 2026 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sip

import (
	"bytes"
	"math"
	"time"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/media-sdk/tones"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"

	"github.com/livekit/sip/pkg/media/opus"
)

const (
	dtmfRecordingToneDur    = 120 * time.Millisecond
	dtmfRecordingToneVolume = math.MaxInt16 / 4
	dtmfRecordingQueueSize  = 16
	// dtmfRecordingMaxErrors consecutive failed writes (1 s of frames) stop the track, as on the microphone path.
	dtmfRecordingMaxErrors = 50
	// dtmfRecordingMaxTailFrames bounds how long silence after a tone goes through the encoder (1 s of frames).
	dtmfRecordingMaxTailFrames = 50

	dtmfRecordingTrackSuffix = "-dtmf-recording"
	dtmfRecordingTrackSource = livekit.TrackSource_SCREEN_SHARE_AUDIO
)

// dtmfRecordingSilence is the packet libopus produces for every frame of digital silence once the encoder
// has settled after a tone: a 20 ms CELT-only frame with the silence flag set, which decodes to zeros.
// Encoding silence costs about as much CPU as encoding speech, so the track sends this packet as is.
var dtmfRecordingSilence = opus.Sample{0xf8, 0xff, 0xfe}

func dtmfRecordingTrackName(identity string) string {
	return identity + dtmfRecordingTrackSuffix
}

// isDTMFRecordingTrack reports whether a track is the DTMF recording track of a SIP participant.
// Another SIP participant in the room, e.g. the transfer leg, must not mix it into its call,
// or the other party would hear the key presses.
func isDTMFRecordingTrack(kind lksdk.ParticipantKind, identity string, source livekit.TrackSource, name string) bool {
	return kind == lksdk.ParticipantSIP && source == dtmfRecordingTrackSource && name == dtmfRecordingTrackName(identity)
}

// dtmfToneFrames renders the DTMF recording track frame by frame:
// a tone for every queued digit, silence otherwise.
type dtmfToneFrames struct {
	digits    chan byte
	freq      []tones.Hz
	ts        time.Duration
	remaining time.Duration
}

func newDTMFToneFrames() *dtmfToneFrames {
	return &dtmfToneFrames{digits: make(chan byte, dtmfRecordingQueueSize)}
}

// Add queues a tone for the digit and reports whether it was queued. It never blocks, since it is called
// from the RTP path: digits arriving while the queue is full are left out of the recording.
func (g *dtmfToneFrames) Add(digit byte) bool {
	select {
	case g.digits <- digit:
		return true
	default:
		return false
	}
}

// Next fills buf with the next frame of length dur and reports whether the frame carries a tone.
func (g *dtmfToneFrames) Next(buf msdk.PCM16Sample, dur time.Duration) bool {
	if g.remaining <= 0 {
		g.freq = nil
		select {
		case digit := <-g.digits:
			_, g.freq = dtmf.Tone(digit)
			g.ts, g.remaining = 0, dtmfRecordingToneDur
		default:
		}
	}
	g.ts = tones.Generate(buf, g.ts, dur, dtmfRecordingToneVolume, g.freq)
	g.remaining -= dur
	return g.freq != nil
}

// dtmfOpusWriter writes the DTMF recording track as Opus: tones through the encoder, silence as
// dtmfRecordingSilence. After a tone the encoder keeps running on silence until it produces
// dtmfRecordingSilence itself, so the decoder gets the tone's own fade-out before the cached packets.
type dtmfOpusWriter struct {
	out      opus.Writer
	enc      msdk.PCM16Writer
	tap      *dtmfOpusTap
	encoding bool
	tail     int
}

// dtmfOpusTap passes the encoder's packets to the track and notes whether the last one was silence.
type dtmfOpusTap struct {
	opus.Writer
	settled bool
}

func (t *dtmfOpusTap) WriteSample(sample opus.Sample) error {
	t.settled = bytes.Equal(sample, dtmfRecordingSilence)
	return t.Writer.WriteSample(sample)
}

func newDTMFOpusWriter(out opus.Writer, log logger.Logger) (*dtmfOpusWriter, error) {
	tap := &dtmfOpusTap{Writer: out}
	enc, err := opus.Encode(tap, channels, log)
	if err != nil {
		return nil, err
	}
	return &dtmfOpusWriter{out: out, enc: enc, tap: tap}, nil
}

func (w *dtmfOpusWriter) SampleRate() int {
	return w.out.SampleRate()
}

// WriteFrame writes one frame of the track; tone reports whether the frame carries a tone.
func (w *dtmfOpusWriter) WriteFrame(frame msdk.PCM16Sample, tone bool) error {
	switch {
	case tone:
		w.encoding, w.tail = true, 0
	case w.encoding:
		w.tail++
		w.encoding = !w.tap.settled && w.tail <= dtmfRecordingMaxTailFrames
	}
	if w.encoding {
		return w.enc.WriteSample(frame)
	}
	return w.out.WriteSample(dtmfRecordingSilence)
}

func (w *dtmfOpusWriter) Close() error {
	return w.out.Close()
}

// runDTMFRecordingTrack writes a frame to out for every rtp.DefFrameDur of wall time until stop is closed.
// The track carries silence between tones on purpose: RTP timestamps advance with the samples written,
// so a track carrying tones only would place every tone right after the previous one in the recording.
// For the same reason it counts frames against the clock, not ticks: a ticker drops ticks when the loop
// is late, and every dropped tick would leave the track a frame behind for the rest of the call.
// It returns nil when stop is closed, or the last error after dtmfRecordingMaxErrors failed writes in a row.
func runDTMFRecordingTrack(out *dtmfOpusWriter, frames *dtmfToneFrames, stop <-chan struct{}) error {
	defer out.Close()
	buf := make(msdk.PCM16Sample, out.SampleRate()/int(time.Second/rtp.DefFrameDur))
	ticker := time.NewTicker(rtp.DefFrameDur)
	defer ticker.Stop()
	next := time.Now().Add(rtp.DefFrameDur)
	errorCnt := 0
	for {
		select {
		case <-stop:
			return nil
		case <-ticker.C:
		}
		for now := time.Now(); !next.After(now); next = next.Add(rtp.DefFrameDur) {
			tone := frames.Next(buf, rtp.DefFrameDur)
			if err := out.WriteFrame(buf, tone); err != nil {
				errorCnt++
				if errorCnt >= dtmfRecordingMaxErrors {
					return err
				}
				continue
			}
			errorCnt = 0
		}
	}
}
