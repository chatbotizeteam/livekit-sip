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
	"slices"
	"sync"
	"testing"
	"time"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/media-sdk/tones"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/stretchr/testify/require"
)

const dtmfToneFrameCount = int(dtmfRecordingToneDur / rtp.DefFrameDur)

func newFrameBuf() msdk.PCM16Sample {
	return make(msdk.PCM16Sample, RoomSampleRate/int(time.Second/rtp.DefFrameDur))
}

func isSilent(buf msdk.PCM16Sample) bool {
	return !slices.ContainsFunc(buf, func(s int16) bool { return s != 0 })
}

func expectedToneFrames(digit byte) []msdk.PCM16Sample {
	_, freq := dtmf.Tone(digit)
	var (
		frames []msdk.PCM16Sample
		ts     time.Duration
	)
	for range dtmfToneFrameCount {
		buf := newFrameBuf()
		ts = tones.Generate(buf, ts, rtp.DefFrameDur, dtmfRecordingToneVolume, freq)
		frames = append(frames, buf)
	}
	return frames
}

func nextFrames(g *dtmfToneFrames, n int) []msdk.PCM16Sample {
	var frames []msdk.PCM16Sample
	for range n {
		buf := newFrameBuf()
		g.Next(buf, rtp.DefFrameDur)
		frames = append(frames, buf)
	}
	return frames
}

func TestDTMFToneFramesSilentWithoutDigits(t *testing.T) {
	g := newDTMFToneFrames()
	for _, frame := range nextFrames(g, 10) {
		require.True(t, isSilent(frame))
	}
}

func TestDTMFToneFramesPlaysToneThenSilence(t *testing.T) {
	g := newDTMFToneFrames()
	g.Add('5')

	frames := nextFrames(g, dtmfToneFrameCount+2)

	require.Equal(t, expectedToneFrames('5'), frames[:dtmfToneFrameCount])
	for _, frame := range frames[dtmfToneFrameCount:] {
		require.True(t, isSilent(frame))
	}
}

func TestDTMFToneFramesPlaysQueuedDigitsInOrder(t *testing.T) {
	g := newDTMFToneFrames()
	g.Add('1')
	g.Add('#')

	frames := nextFrames(g, 2*dtmfToneFrameCount)

	require.Equal(t, expectedToneFrames('1'), frames[:dtmfToneFrameCount])
	require.Equal(t, expectedToneFrames('#'), frames[dtmfToneFrameCount:])
}

func TestDTMFToneFramesDigitDuringToneWaitsForItsEnd(t *testing.T) {
	g := newDTMFToneFrames()
	g.Add('1')
	first := nextFrames(g, 1)
	g.Add('2')

	frames := append(first, nextFrames(g, 2*dtmfToneFrameCount-1)...)

	require.Equal(t, expectedToneFrames('1'), frames[:dtmfToneFrameCount])
	require.Equal(t, expectedToneFrames('2'), frames[dtmfToneFrameCount:])
}

func TestDTMFToneFramesUnknownEventIsSilent(t *testing.T) {
	g := newDTMFToneFrames()
	g.Add('x')

	for _, frame := range nextFrames(g, dtmfToneFrameCount) {
		require.True(t, isSilent(frame))
	}
}

func TestDTMFToneFramesAddNeverBlocks(t *testing.T) {
	g := newDTMFToneFrames()
	done := make(chan struct{})
	go func() {
		for range 2 * dtmfRecordingQueueSize {
			g.Add('1')
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Add blocked on a full queue")
	}
}

type recordingPCMWriter struct {
	mu     sync.Mutex
	frames []msdk.PCM16Sample
	closed bool
}

func (w *recordingPCMWriter) String() string  { return "recordingPCMWriter" }
func (w *recordingPCMWriter) SampleRate() int { return RoomSampleRate }

func (w *recordingPCMWriter) WriteSample(sample msdk.PCM16Sample) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.frames = append(w.frames, slices.Clone(sample))
	return nil
}

func (w *recordingPCMWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	return nil
}

func (w *recordingPCMWriter) snapshot() ([]msdk.PCM16Sample, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.frames), w.closed
}

func TestRunDTMFRecordingTrackWritesSilenceAndTones(t *testing.T) {
	out := &recordingPCMWriter{}
	frames := newDTMFToneFrames()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		runDTMFRecordingTrack(out, frames, stop)
		close(done)
	}()

	require.Eventually(t, func() bool {
		written, _ := out.snapshot()
		return len(written) >= 3
	}, time.Second, rtp.DefFrameDur)
	frames.Add('9')
	require.Eventually(t, func() bool {
		written, _ := out.snapshot()
		return slices.ContainsFunc(written, func(f msdk.PCM16Sample) bool { return !isSilent(f) })
	}, time.Second, rtp.DefFrameDur)

	close(stop)
	<-done
	written, closed := out.snapshot()
	require.True(t, closed)
	require.True(t, isSilent(written[0]))
	for _, frame := range written {
		require.Len(t, frame, RoomSampleRate/int(time.Second/rtp.DefFrameDur))
	}
}

func TestIsDTMFRecordingTrack(t *testing.T) {
	const identity = "sip_+48123456789"
	name := dtmfRecordingTrackName(identity)
	cases := []struct {
		label    string
		kind     lksdk.ParticipantKind
		identity string
		source   livekit.TrackSource
		name     string
		want     bool
	}{
		{"recording track of a SIP participant", lksdk.ParticipantSIP, identity, dtmfRecordingTrackSource, name, true},
		{"caller microphone", lksdk.ParticipantSIP, identity, livekit.TrackSource_UNKNOWN, identity, false},
		{"same name with another source", lksdk.ParticipantSIP, identity, livekit.TrackSource_MICROPHONE, name, false},
		{"recording track name of another participant", lksdk.ParticipantSIP, "sip_other", dtmfRecordingTrackSource, name, false},
		{"screen share audio of a non-SIP participant", lksdk.ParticipantStandard, identity, dtmfRecordingTrackSource, name, false},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			require.Equal(t, c.want, isDTMFRecordingTrack(c.kind, c.identity, c.source, c.name))
		})
	}
}
