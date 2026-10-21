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

// Add queues a tone for the digit. It never blocks, since it is called from the RTP path:
// digits arriving while the queue is full are left out of the recording.
func (g *dtmfToneFrames) Add(digit byte) {
	select {
	case g.digits <- digit:
	default:
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

// runDTMFRecordingTrack writes a frame to out every rtp.DefFrameDur until stop is closed.
// The track carries silence between tones on purpose: RTP timestamps advance with the samples written,
// so a track carrying tones only would place every tone right after the previous one in the recording.
func runDTMFRecordingTrack(out msdk.PCM16Writer, frames *dtmfToneFrames, stop <-chan struct{}) {
	defer out.Close()
	buf := make(msdk.PCM16Sample, out.SampleRate()/int(time.Second/rtp.DefFrameDur))
	ticker := time.NewTicker(rtp.DefFrameDur)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		frames.Next(buf, rtp.DefFrameDur)
		if err := out.WriteSample(buf); err != nil {
			return
		}
	}
}
