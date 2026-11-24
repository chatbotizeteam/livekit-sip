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
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/protocol/logger"
	"github.com/stretchr/testify/require"
)

const (
	testAudioSSRC = 0x1111
	testOtherSSRC = 0x2222
)

var errTestWrite = errors.New("write failed")

// dtmfCall plays a phone on the SIP leg against a silenceFiller: audio frames, RFC 4733 key presses and silence
// suppression, on a clock the test moves. The filler's output is recorded in order: true for an audio frame
// passed to the decoder, false for a silence frame.
type dtmfCall struct {
	t          *testing.T
	f          *silenceFiller
	now        time.Time
	tsPerFrame uint32
	pcmRate    int
	seq        uint16
	ts         uint32
	ssrc       uint32
	eventSSRC  uint32
	eventSeq   uint16 // used only when events have their own SSRC
	out        []bool
	samples    []int
	failWrites bool
	frames     atomic.Uint64
}

func newDTMFCall(t *testing.T, clockRate, pcmRate int, startTS uint32) *dtmfCall {
	c := &dtmfCall{
		t:          t,
		now:        time.Unix(1_700_000_000, 0),
		tsPerFrame: uint32(clockRate / rtp.DefFramesPerSec),
		pcmRate:    pcmRate,
		seq:        1000,
		ts:         startTS,
		ssrc:       testAudioSSRC,
		eventSSRC:  testAudioSSRC,
		eventSeq:   40000,
	}
	c.f = newSilenceFiller(c, c, clockRate, pcmRate, logger.GetLogger(),
		withClock(func() time.Time { return c.now }), withEventFrameCounter(&c.frames))
	return c
}

func (c *dtmfCall) String() string  { return "dtmfCall" }
func (c *dtmfCall) SampleRate() int { return c.pcmRate }
func (c *dtmfCall) Close() error    { return nil }

func (c *dtmfCall) HandleRTP(_ *rtp.Header, payload []byte) error {
	c.out = append(c.out, true)
	return nil
}

func (c *dtmfCall) WriteSample(s msdk.PCM16Sample) error {
	if c.failWrites {
		return errTestWrite
	}
	c.out = append(c.out, false)
	c.samples = append(c.samples, len(s))
	return nil
}

func (c *dtmfCall) tick(ms int) {
	c.now = c.now.Add(time.Duration(ms) * time.Millisecond)
}

func (c *dtmfCall) units(ms int) uint32 {
	return uint32(ms) * c.tsPerFrame / 20
}

// audio sends n audio frames, one every 20 ms.
func (c *dtmfCall) audio(n int) {
	for range n {
		c.sendAudio()
		c.tick(20)
	}
}

func (c *dtmfCall) sendAudio() {
	h := &rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: c.seq, Timestamp: c.ts, SSRC: c.ssrc}
	c.seq++
	c.ts += c.tsPerFrame
	require.NoError(c.t, c.f.HandleRTP(h, make([]byte, 160)))
}

// pause is silence suppression: no packets for ms.
func (c *dtmfCall) pause(ms int) {
	c.tick(ms)
	c.ts += c.units(ms)
}

func (c *dtmfCall) sendEvent(start uint32, durMs int, end, marker bool) error {
	var payload [4]byte
	_, err := dtmf.Encode(payload[:], dtmf.Event{Digit: '5', Volume: 10, Dur: uint16(c.units(durMs)), End: end})
	require.NoError(c.t, err)
	seq := &c.seq
	if c.eventSSRC != c.ssrc {
		seq = &c.eventSeq
	}
	h := &rtp.Header{Version: 2, PayloadType: 101, SequenceNumber: *seq, Timestamp: start, Marker: marker, SSRC: c.eventSSRC}
	*seq++
	return c.f.HandleEvent(h, payload[:])
}

// key is a key press of durMs that replaces audio, with an event update every everyMs and three end packets.
// Audio resumes on the real clock, so its timestamp moves on by the duration of the key.
func (c *dtmfCall) key(durMs, everyMs int) {
	start := c.ts
	elapsed := 0
	for elapsed+everyMs < durMs {
		c.tick(everyMs)
		elapsed += everyMs
		require.NoError(c.t, c.sendEvent(start, elapsed, false, elapsed == everyMs))
	}
	c.tick(durMs - elapsed)
	for range 3 {
		require.NoError(c.t, c.sendEvent(start, durMs, true, false))
	}
	c.ts = start + c.units(durMs)
}

// keyWithAudio is a key press of durMs during which the phone keeps sending audio.
func (c *dtmfCall) keyWithAudio(durMs, everyMs int) {
	start := c.ts
	for elapsed := 20; elapsed <= durMs; elapsed += 20 {
		c.sendAudio()
		c.tick(20)
		if elapsed%everyMs == 0 {
			require.NoError(c.t, c.sendEvent(start, elapsed, elapsed == durMs, elapsed == everyMs))
		}
	}
}

func (c *dtmfCall) silence() int {
	n := 0
	for _, isAudio := range c.out {
		if !isAudio {
			n++
		}
	}
	return n
}

// requireTimeline checks the frames written so far: audio frames and silence frames, in total.
func (c *dtmfCall) requireTimeline(audio, silence int) {
	c.t.Helper()
	require.Equal(c.t, silence, c.silence(), "silence frames")
	require.Equal(c.t, audio, len(c.out)-c.silence(), "audio frames")
	require.Equal(c.t, uint64(silence), c.frames.Load(), "event silence counter")
	for i, n := range c.samples {
		require.Equal(c.t, c.pcmRate/rtp.DefFramesPerSec, n, "samples in silence frame %d", i)
	}
}

func TestSilenceFillerDTMFKeepsTimeline(t *testing.T) {
	for _, tc := range []struct {
		name  string
		keyMs int
		every int
	}{
		{"200ms key, updates every 50ms", 200, 50},
		{"200ms key, updates every 20ms", 200, 20},
		{"160ms key", 160, 50},
		{"800ms key, longer than the silence suppression limit", 800, 50},
		{"6s key, longer than one event fill", 6000, 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newDTMFCall(t, 8000, 8000, 10000)
			c.audio(50)
			c.key(tc.keyMs, tc.every)
			before := c.silence()
			c.audio(50)
			keyFrames := tc.keyMs / 20
			c.requireTimeline(100, keyFrames)
			// Written while the key is down, not when audio resumes.
			require.GreaterOrEqual(t, before, keyFrames-2, "silence written during the key")
		})
	}
}

func TestSilenceFillerDTMFPacesSilenceWithTheClock(t *testing.T) {
	c := newDTMFCall(t, 8000, 8000, 10000)
	c.audio(10)
	start := c.ts
	// A 1 s key: after every update the silence written so far follows the time the key has been down.
	for elapsed := 50; elapsed < 1000; elapsed += 50 {
		c.tick(50)
		require.NoError(t, c.sendEvent(start, elapsed, false, elapsed == 50))
		got := c.silence()
		require.LessOrEqual(t, got, elapsed/20, "silence ahead of the clock at %d ms", elapsed)
		require.GreaterOrEqual(t, got, elapsed/20-2, "silence behind the clock at %d ms", elapsed)
	}
	// Event timestamps claiming more than the clock has seen are not trusted.
	require.NoError(t, c.sendEvent(start, 5000, false, false))
	require.LessOrEqual(t, c.silence(), 950/20)
}

func TestSilenceFillerDTMFConsecutiveKeys(t *testing.T) {
	t.Run("short pause between keys is filled", func(t *testing.T) {
		c := newDTMFCall(t, 8000, 8000, 10000)
		c.audio(20)
		c.key(200, 50)
		c.pause(100)
		c.key(200, 50)
		c.audio(20)
		c.requireTimeline(40, 25)
	})
	t.Run("long pause between keys is silence suppression and is not filled", func(t *testing.T) {
		c := newDTMFCall(t, 8000, 8000, 10000)
		c.audio(20)
		c.key(200, 50)
		c.pause(1000)
		c.key(200, 50)
		c.audio(20)
		c.requireTimeline(40, 20)
	})
	t.Run("nine keys", func(t *testing.T) {
		c := newDTMFCall(t, 8000, 8000, 10000)
		c.audio(20)
		for range 9 {
			c.key(160, 50)
			c.audio(10)
		}
		c.audio(20)
		c.requireTimeline(130, 9*8)
	})
}

func TestSilenceFillerDTMFSilenceSuppressionAroundKey(t *testing.T) {
	for _, tc := range []struct {
		name          string
		beforeMs      int
		afterMs       int
		wantedSilence int
	}{
		{"short pause before the key is filled", 300, 0, 15 + 10},
		{"long pause before the key is not filled", 2000, 0, 10},
		{"short pause after the key is filled", 0, 300, 10 + 15},
		{"long pause after the key is not filled", 0, 2000, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newDTMFCall(t, 8000, 8000, 10000)
			c.audio(20)
			c.pause(tc.beforeMs)
			c.key(200, 50)
			c.pause(tc.afterMs)
			c.audio(20)
			c.requireTimeline(40, tc.wantedSilence)
		})
	}
}

func TestSilenceFillerDTMFPhoneKeepsSendingAudio(t *testing.T) {
	for _, every := range []int{20, 40, 60} {
		c := newDTMFCall(t, 8000, 8000, 10000)
		c.audio(20)
		c.keyWithAudio(400, every)
		c.audio(20)
		c.requireTimeline(60, 0)
	}
}

func TestSilenceFillerDTMFAudioAroundTheFirstEvent(t *testing.T) {
	// The last audio frame arrives after the first event packet; the phone then sends only events for 800 ms.
	c := newDTMFCall(t, 8000, 8000, 10000)
	c.audio(20)
	start := c.ts
	c.tick(20)
	require.NoError(t, c.sendEvent(start, 20, false, true))
	c.sendAudio()
	for elapsed := 70; elapsed < 800; elapsed += 50 {
		c.tick(50)
		require.NoError(t, c.sendEvent(start, elapsed, false, false))
	}
	c.tick(30)
	for range 3 {
		require.NoError(t, c.sendEvent(start, 800, true, false))
	}
	c.ts = start + c.units(800)
	c.audio(20)
	c.requireTimeline(41, 39)
}

func TestSilenceFillerDTMFPhoneSendsAudioInBursts(t *testing.T) {
	// Audio during the key press, arriving two frames at a time every 40 ms, with events in between.
	c := newDTMFCall(t, 8000, 8000, 10000)
	c.audio(20)
	start := c.ts
	for elapsed := 40; elapsed <= 400; elapsed += 40 {
		c.tick(20)
		require.NoError(t, c.sendEvent(start, elapsed-20, false, elapsed == 40))
		c.tick(20)
		c.sendAudio()
		c.sendAudio()
		require.NoError(t, c.sendEvent(start, elapsed, elapsed == 400, false))
	}
	c.audio(20)
	c.requireTimeline(60, 0)
}

func TestSilenceFillerDTMFEdgeCases(t *testing.T) {
	t.Run("events before any audio", func(t *testing.T) {
		c := newDTMFCall(t, 8000, 8000, 10000)
		c.key(200, 50)
		c.audio(20)
		c.requireTimeline(20, 0)
	})
	t.Run("events of another SSRC are left to silence suppression", func(t *testing.T) {
		c := newDTMFCall(t, 8000, 8000, 10000)
		c.eventSSRC = testOtherSSRC
		c.audio(20)
		c.key(200, 50)
		c.audio(20)
		// Audio sequence numbers have no gap, so silence suppression fills the 10 frames, and only it.
		require.Equal(t, 10, c.silence())
		require.Zero(t, c.frames.Load(), "event silence counter")
	})
	t.Run("malformed event payload", func(t *testing.T) {
		c := newDTMFCall(t, 8000, 8000, 10000)
		c.audio(20)
		c.tick(200)
		h := &rtp.Header{Version: 2, PayloadType: 101, SequenceNumber: c.seq, Timestamp: c.ts, SSRC: testAudioSSRC}
		c.seq++
		require.NoError(t, c.f.HandleEvent(h, []byte{1, 2}))
		c.ts += c.units(200)
		c.audio(20)
		// No duration to pace by: the gap is filled on resume as silence suppression.
		c.requireTimeline(40, 10)
	})
	t.Run("timestamp wrap-around", func(t *testing.T) {
		c := newDTMFCall(t, 8000, 8000, 0xFFFFFF00)
		c.audio(5)
		c.key(400, 50)
		c.audio(20)
		c.requireTimeline(25, 20)
	})
	t.Run("G.722: 8 kHz RTP clock, 16 kHz audio", func(t *testing.T) {
		c := newDTMFCall(t, 8000, 16000, 10000)
		c.audio(20)
		c.key(200, 50)
		c.audio(20)
		c.requireTimeline(40, 10)
	})
	t.Run("timestamp reset when audio resumes", func(t *testing.T) {
		c := newDTMFCall(t, 8000, 8000, 10000)
		c.audio(20)
		c.key(200, 50)
		c.ts += 8000 * 600 // 10 minutes
		c.audio(20)
		// The key is filled; the jump after it is not.
		c.requireTimeline(40, 10)
	})
	t.Run("event claiming more time than has passed", func(t *testing.T) {
		c := newDTMFCall(t, 8000, 8000, 10000)
		c.audio(20)
		c.tick(40)
		require.NoError(t, c.sendEvent(c.ts, 3000, false, true))
		// 60 ms since the last audio packet: at most 2 frames, not the 150 the event claims.
		require.LessOrEqual(t, c.silence(), 2)
	})
	t.Run("failed silence write is reported", func(t *testing.T) {
		c := newDTMFCall(t, 8000, 8000, 10000)
		c.audio(20)
		c.failWrites = true
		c.tick(100)
		require.ErrorIs(t, c.sendEvent(c.ts, 100, false, true), errTestWrite)
	})
}

// countingSink is a decoder and PCM sink that may be called from several goroutines.
type countingSink struct {
	audio   atomic.Uint64
	silence atomic.Uint64
}

func (s *countingSink) String() string  { return "countingSink" }
func (s *countingSink) SampleRate() int { return 8000 }
func (s *countingSink) Close() error    { return nil }

func (s *countingSink) HandleRTP(_ *rtp.Header, _ []byte) error {
	s.audio.Add(1)
	return nil
}

func (s *countingSink) WriteSample(_ msdk.PCM16Sample) error {
	s.silence.Add(1)
	return nil
}

// Audio and events of different SSRCs are read by different goroutines; run with -race.
func TestSilenceFillerDTMFConcurrentStreams(t *testing.T) {
	sink := &countingSink{}
	var eventFrames atomic.Uint64
	f := newSilenceFiller(sink, sink, 8000, 8000, logger.GetLogger(), withEventFrameCounter(&eventFrames))
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := range 500 {
			h := &rtp.Header{SequenceNumber: uint16(2000 + i), Timestamp: uint32(20000 + i*160), SSRC: testAudioSSRC}
			require.NoError(t, f.HandleRTP(h, make([]byte, 160)))
		}
	}()
	sendEvents := func(ssrc uint32, seq uint16) {
		defer wg.Done()
		var payload [4]byte
		for i := range 500 {
			_, err := dtmf.Encode(payload[:], dtmf.Event{Digit: '1', Dur: uint16(i * 8)})
			require.NoError(t, err)
			h := &rtp.Header{SequenceNumber: seq + uint16(i), Timestamp: 20000, SSRC: ssrc}
			require.NoError(t, f.HandleEvent(h, payload[:]))
		}
	}
	go sendEvents(testOtherSSRC, 30000)
	go sendEvents(testAudioSSRC, 50000)
	wg.Wait()
	require.Equal(t, uint64(500), sink.audio.Load())
}
