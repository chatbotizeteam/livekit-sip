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
	"errors"
	"math"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/media-sdk/tones"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/livekit/sip/pkg/media/opus"
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
	queued := make(chan []bool, 1)
	go func() {
		var res []bool
		for range 2 * dtmfRecordingQueueSize {
			res = append(res, g.Add('1'))
		}
		queued <- res
	}()

	select {
	case res := <-queued:
		for i, ok := range res {
			require.Equal(t, i < dtmfRecordingQueueSize, ok, "digit %d", i)
		}
	case <-time.After(time.Second):
		t.Fatal("Add blocked on a full queue")
	}
}

type recordingPCMWriter struct {
	mu     sync.Mutex
	frames []msdk.PCM16Sample
}

func (w *recordingPCMWriter) String() string  { return "recordingPCMWriter" }
func (w *recordingPCMWriter) SampleRate() int { return RoomSampleRate }
func (w *recordingPCMWriter) Close() error    { return nil }

func (w *recordingPCMWriter) WriteSample(sample msdk.PCM16Sample) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.frames = append(w.frames, slices.Clone(sample))
	return nil
}

type recordingOpusWriter struct {
	mu      sync.Mutex
	packets []opus.Sample
	closed  bool
}

func (w *recordingOpusWriter) String() string  { return "recordingOpusWriter" }
func (w *recordingOpusWriter) SampleRate() int { return RoomSampleRate }

func (w *recordingOpusWriter) WriteSample(sample opus.Sample) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.packets = append(w.packets, slices.Clone(sample))
	return nil
}

func (w *recordingOpusWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	return nil
}

func (w *recordingOpusWriter) snapshot() ([]opus.Sample, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.packets), w.closed
}

func newTestDTMFOpusWriter(t *testing.T, out opus.Writer) *dtmfOpusWriter {
	w, err := newDTMFOpusWriter(out, logger.GetLogger())
	require.NoError(t, err)
	return w
}

func isCachedSilence(p opus.Sample) bool {
	return bytes.Equal(p, dtmfRecordingSilence)
}

func TestDTMFRecordingSilenceDecodesToZeros(t *testing.T) {
	out := &recordingPCMWriter{}
	dec, err := opus.Decode(out, channels, logger.GetLogger())
	require.NoError(t, err)
	for range 3 {
		require.NoError(t, dec.WriteSample(dtmfRecordingSilence))
	}
	require.Len(t, out.frames, 3)
	for _, frame := range out.frames {
		require.Len(t, frame, RoomSampleRate/int(time.Second/rtp.DefFrameDur))
		require.True(t, isSilent(frame))
	}
}

func TestDTMFOpusWriterSendsCachedSilenceWithoutTones(t *testing.T) {
	out := &recordingOpusWriter{}
	w := newTestDTMFOpusWriter(t, out)
	g := newDTMFToneFrames()
	buf := newFrameBuf()
	for range 100 {
		require.NoError(t, w.WriteFrame(buf, g.Next(buf, rtp.DefFrameDur)))
	}
	packets, _ := out.snapshot()
	require.Len(t, packets, 100)
	for _, p := range packets {
		require.True(t, isCachedSilence(p))
	}
}

// The encoder must settle on dtmfRecordingSilence by itself after a tone. If a libopus update changes the packet
// it produces for silence, the encoder never matches it and runs for dtmfRecordingMaxTailFrames after every tone.
func TestDTMFOpusWriterEncodesTonesAndSettlesOnCachedSilence(t *testing.T) {
	out := &recordingOpusWriter{}
	w := newTestDTMFOpusWriter(t, out)
	g := newDTMFToneFrames()
	buf := newFrameBuf()
	write := func(n int) {
		for range n {
			require.NoError(t, w.WriteFrame(buf, g.Next(buf, rtp.DefFrameDur)))
		}
	}
	for _, digits := range []string{"5", "1#", "0"} {
		write(10)
		for _, d := range []byte(digits) {
			g.Add(d)
		}
		before, _ := out.snapshot()
		write(len(digits)*dtmfToneFrameCount + dtmfRecordingMaxTailFrames + 10)
		after, _ := out.snapshot()
		packets := after[len(before):]

		for _, p := range packets[:len(digits)*dtmfToneFrameCount] {
			require.False(t, isCachedSilence(p), "tone frame sent as cached silence")
		}
		settled := slices.IndexFunc(packets, isCachedSilence)
		require.Positive(t, settled)
		require.Less(t, settled, len(digits)*dtmfToneFrameCount+dtmfRecordingMaxTailFrames/2, "encoder did not settle on dtmfRecordingSilence")
		for _, p := range packets[settled:] {
			require.True(t, isCachedSilence(p))
		}
	}
}

func TestRunDTMFRecordingTrackWritesSilenceAndTones(t *testing.T) {
	out := &recordingOpusWriter{}
	frames := newDTMFToneFrames()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		assert.NoError(t, runDTMFRecordingTrack(newTestDTMFOpusWriter(t, out), frames, stop))
		close(done)
	}()

	require.Eventually(t, func() bool {
		written, _ := out.snapshot()
		return len(written) >= 3
	}, time.Second, rtp.DefFrameDur)
	frames.Add('9')
	require.Eventually(t, func() bool {
		written, _ := out.snapshot()
		return slices.ContainsFunc(written, func(p opus.Sample) bool { return !isCachedSilence(p) })
	}, time.Second, rtp.DefFrameDur)

	close(stop)
	<-done
	written, closed := out.snapshot()
	require.True(t, closed)
	require.True(t, isCachedSilence(written[0]))
}

type stallingOpusWriter struct {
	recordingOpusWriter
	stallAt  int
	stallFor time.Duration
}

func (w *stallingOpusWriter) WriteSample(sample opus.Sample) error {
	written, _ := w.snapshot()
	if len(written) == w.stallAt {
		time.Sleep(w.stallFor)
	}
	return w.recordingOpusWriter.WriteSample(sample)
}

func TestRunDTMFRecordingTrackKeepsUpWithTheClockAfterAStall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		out := &stallingOpusWriter{stallAt: 5, stallFor: 5*rtp.DefFrameDur + rtp.DefFrameDur/2}
		w := newTestDTMFOpusWriter(t, out)
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			assert.NoError(t, runDTMFRecordingTrack(w, newDTMFToneFrames(), stop))
			close(done)
		}()

		time.Sleep(time.Second + rtp.DefFrameDur/2)
		synctest.Wait()
		written, _ := out.snapshot()
		require.Len(t, written, int(time.Second/rtp.DefFrameDur))

		close(stop)
		<-done
	})
}

type failingOpusWriter struct {
	recordingOpusWriter
	failFrom, failTo int
	calls            int
}

func (w *failingOpusWriter) WriteSample(sample opus.Sample) error {
	w.calls++
	if w.calls > w.failFrom && w.calls <= w.failTo {
		return errors.New("write failed")
	}
	return w.recordingOpusWriter.WriteSample(sample)
}

func TestRunDTMFRecordingTrackSurvivesShortWriteErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		out := &failingOpusWriter{failFrom: 5, failTo: 5 + dtmfRecordingMaxErrors - 1}
		w := newTestDTMFOpusWriter(t, out)
		stop := make(chan struct{})
		done := make(chan error, 1)
		go func() { done <- runDTMFRecordingTrack(w, newDTMFToneFrames(), stop) }()

		time.Sleep(2*time.Second + rtp.DefFrameDur/2)
		synctest.Wait()
		written, _ := out.snapshot()
		require.Len(t, written, int(2*time.Second/rtp.DefFrameDur)-(dtmfRecordingMaxErrors-1))

		close(stop)
		require.NoError(t, <-done)
	})
}

func TestRunDTMFRecordingTrackStopsOnPersistentWriteErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		out := &failingOpusWriter{failFrom: 5, failTo: math.MaxInt}
		w := newTestDTMFOpusWriter(t, out)
		stop := make(chan struct{})
		done := make(chan error, 1)
		go func() { done <- runDTMFRecordingTrack(w, newDTMFToneFrames(), stop) }()

		require.Error(t, <-done)
		_, closed := out.snapshot()
		require.True(t, closed)
		require.Equal(t, 5+dtmfRecordingMaxErrors, out.calls)
	})
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
