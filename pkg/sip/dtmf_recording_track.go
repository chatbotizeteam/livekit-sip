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
	"math"
	"time"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/media-sdk/tones"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

const (
	dtmfRecordingToneDur    = 120 * time.Millisecond
	dtmfRecordingToneVolume = math.MaxInt16 / 4
	dtmfRecordingQueueSize  = 16
	// dtmfRecordingMaxErrors consecutive failed writes (1 s of frames) stop the track, as on the microphone path.
	dtmfRecordingMaxErrors = 50

	dtmfRecordingTrackSuffix = "-dtmf-recording"
	dtmfRecordingTrackSource = livekit.TrackSource_SCREEN_SHARE_AUDIO
)

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

// Next fills buf with the next frame of length dur.
func (g *dtmfToneFrames) Next(buf msdk.PCM16Sample, dur time.Duration) {
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
}

// runDTMFRecordingTrack writes a frame to out for every rtp.DefFrameDur of wall time until stop is closed.
// The track carries silence between tones on purpose: RTP timestamps advance with the samples written,
// so a track carrying tones only would place every tone right after the previous one in the recording.
// For the same reason it counts frames against the clock, not ticks: a ticker drops ticks when the loop
// is late, and every dropped tick would leave the track a frame behind for the rest of the call.
// It returns nil when stop is closed, or the last error after dtmfRecordingMaxErrors failed writes in a row.
func runDTMFRecordingTrack(out msdk.PCM16Writer, frames *dtmfToneFrames, stop <-chan struct{}) error {
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
			frames.Next(buf, rtp.DefFrameDur)
			if err := out.WriteSample(buf); err != nil {
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
