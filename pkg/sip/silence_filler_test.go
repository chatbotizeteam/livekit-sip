// Copyright 2024 LiveKit, Inc.
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
	"testing"
	"time"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/protocol/logger"
	"github.com/stretchr/testify/require"
)

// Both a rtp.Handler and msdk.PCM16Writer designed to test the silence suppression handler
type SilenceSuppressionTester struct {
	audioSampleRate       int
	clockRate             int    // RTP clock rate (may differ from sample rate for G722)
	framesReceived        []bool // true if received a signal frame, false if it was generated silence
	receivedSilenceFrames uint64
	receivedSignalFrames  uint64
	sampleCounts          []int // number of samples in each silence frame
	gapFiller             rtp.Handler
	filler                *silenceFiller
	clock                 time.Time // arrival time of the packet being sent
}

type silenceSuppressionTesterOption func(*SilenceSuppressionTester)

func WithClockRate(clockRate int) silenceSuppressionTesterOption {
	return func(s *SilenceSuppressionTester) {
		s.clockRate = clockRate
	}
}

func newSilenceSuppressionTester(audioSampleRate int, log logger.Logger, options ...silenceSuppressionTesterOption) *SilenceSuppressionTester {
	tester := &SilenceSuppressionTester{
		audioSampleRate:       audioSampleRate,
		clockRate:             audioSampleRate, // default to same as sample rate
		framesReceived:        make([]bool, 0),
		sampleCounts:          make([]int, 0),
		receivedSilenceFrames: 0,
		receivedSignalFrames:  0,
	}
	for _, option := range options {
		option(tester)
	}
	tester.clock = time.Unix(1_000_000, 0)
	tester.filler = newSilenceFiller(tester, tester, tester.clockRate, tester.audioSampleRate, log)
	tester.filler.now = func() time.Time { return tester.clock }
	tester.gapFiller = tester.filler
	return tester
}

func (s *SilenceSuppressionTester) String() string {
	return "SilenceSuppressionTester"
}

func (s *SilenceSuppressionTester) SampleRate() int {
	return s.audioSampleRate
}

func (s *SilenceSuppressionTester) Close() error {
	return nil
}

func (s *SilenceSuppressionTester) WriteSample(sample msdk.PCM16Sample) error {
	s.framesReceived = append(s.framesReceived, false)
	s.sampleCounts = append(s.sampleCounts, len(sample))
	s.receivedSilenceFrames++
	return nil
}

func (s *SilenceSuppressionTester) HandleRTP(h *rtp.Header, payload []byte) error {
	s.framesReceived = append(s.framesReceived, true)
	s.sampleCounts = append(s.sampleCounts, len(payload))
	s.receivedSignalFrames++
	return nil
}

func (s *SilenceSuppressionTester) SendSignalFrames(count int, nextSeq uint16, nextTimestamp uint32) (uint16, uint32, error) {
	samplesPerFrame := s.audioSampleRate / rtp.DefFramesPerSec
	clockPerFrame := s.clockRate / rtp.DefFramesPerSec
	for i := 0; i < count; i++ {
		h := &rtp.Header{
			SequenceNumber: nextSeq,
			Timestamp:      nextTimestamp,
		}
		nextSeq++
		nextTimestamp += uint32(clockPerFrame)
		payload := make([]byte, samplesPerFrame)
		s.clock = s.clock.Add(rtp.DefFrameDur)
		err := s.gapFiller.HandleRTP(h, payload)
		if err != nil {
			return nextSeq, nextTimestamp, err
		}
	}
	return nextSeq, nextTimestamp, nil
}

// SendDTMF sends the telephone-event packets of one key press that starts at eventTS: count packets, step apart.
// Packet i reports a duration of i+1 frames, capped at durFrames, as when a sender repeats the end packet.
func (s *SilenceSuppressionTester) SendDTMF(count, durFrames int, nextSeq uint16, eventTS uint32, step time.Duration) (uint16, error) {
	clockPerFrame := s.clockRate / rtp.DefFramesPerSec
	for i := 0; i < count; i++ {
		dur := min(i+1, durFrames)
		var payload [4]byte
		if _, err := dtmf.Encode(payload[:], dtmf.Event{Digit: '5', Dur: uint16(dur * clockPerFrame), End: dur == durFrames}); err != nil {
			return nextSeq, err
		}
		h := &rtp.Header{
			SequenceNumber: nextSeq,
			Timestamp:      eventTS,
			Marker:         i == 0,
		}
		nextSeq++
		s.clock = s.clock.Add(step)
		if err := s.filler.HandleDTMF(h, payload[:]); err != nil {
			return nextSeq, err
		}
	}
	return nextSeq, nil
}

func (s *SilenceSuppressionTester) assertSilenceIndexes(t *testing.T, expectedSize int, silenceIndexes []int) {
	samplesPerFrame := s.audioSampleRate / rtp.DefFramesPerSec
	require.Equal(t, expectedSize, len(s.framesReceived))
	require.Equal(t, expectedSize, len(s.sampleCounts))
	require.Equal(t, uint64(len(silenceIndexes)), s.receivedSilenceFrames)
	require.Equal(t, uint64(expectedSize-len(silenceIndexes)), s.receivedSignalFrames)
	for i, isSignal := range s.framesReceived {
		if slices.Contains(silenceIndexes, i) {
			require.False(t, isSignal, "frame %d should be silence", i)
		} else {
			require.True(t, isSignal, "frame %d should be signal", i)
		}
		require.Equal(t, samplesPerFrame, s.sampleCounts[i], "frame %d should have %d samples instead of %d", i, samplesPerFrame, s.sampleCounts[i])
	}
	for _, index := range silenceIndexes { // Make sure we're not missing any indexes
		if index < 0 || index >= expectedSize {
			t.Fatalf("index %d out of range", index)
		}
	}
}

func TestSilenceSuppressionHandling(t *testing.T) {
	const (
		sampleRate      = 8000
		samplesPerFrame = uint32(sampleRate / rtp.DefFramesPerSec) // 160 samples per 20ms frame
	)

	log := logger.GetLogger()

	t.Run("no gap", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)
		_, _, err := tester.SendSignalFrames(10, 100, 1000)
		require.NoError(t, err)

		tester.assertSilenceIndexes(t, 10, []int{})
	})

	t.Run("single frame gap", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)

		nextSeq := uint16(100)
		nextTimestamp := uint32(10000)
		nextSeq, nextTimestamp, err := tester.SendSignalFrames(5, nextSeq, nextTimestamp)
		require.NoError(t, err)
		missingFrameCount := 1
		nextTimestamp += uint32(missingFrameCount) * samplesPerFrame
		_, _, err = tester.SendSignalFrames(5, nextSeq, nextTimestamp)
		require.NoError(t, err)

		tester.assertSilenceIndexes(t, 11, []int{5})
	})

	t.Run("handful of frames gap", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)

		nextSeq := uint16(100)
		nextTimestamp := uint32(10000)
		nextSeq, nextTimestamp, err := tester.SendSignalFrames(5, nextSeq, nextTimestamp)
		require.NoError(t, err)
		missingFrameCount := 3
		nextTimestamp += uint32(missingFrameCount) * samplesPerFrame
		_, _, err = tester.SendSignalFrames(5, nextSeq, nextTimestamp)
		require.NoError(t, err)

		tester.assertSilenceIndexes(t, 13, []int{5, 6, 7})
	})

	t.Run("large gap that's not filled", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)

		nextSeq := uint16(100)
		nextTimestamp := uint32(10000)
		nextSeq, nextTimestamp, err := tester.SendSignalFrames(5, nextSeq, nextTimestamp)
		require.NoError(t, err)
		missingFrameCount := 50 // Too large, shouldn't be filled
		nextTimestamp += uint32(missingFrameCount) * samplesPerFrame
		_, _, err = tester.SendSignalFrames(5, nextSeq, nextTimestamp)
		require.NoError(t, err)

		tester.assertSilenceIndexes(t, 10, []int{})
	})

	t.Run("timestamp wrap-around no gap", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)

		// Start near wrap-around
		nextSeq := uint16(100)
		nextTimestamp := uint32(0xFFFFFF00) // Near wrap-around
		_, _, err := tester.SendSignalFrames(5, nextSeq, nextTimestamp)
		require.NoError(t, err)

		tester.assertSilenceIndexes(t, 5, []int{})
	})

	t.Run("timestamp wrap-around with gap", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)

		// 2 signal + 3 silence (across wrap-around) + 2 signal = 7 total
		nextSeq := uint16(100)
		nextTimestamp := uint32(0xFFFFFF00)
		nextSeq, nextTimestamp, err := tester.SendSignalFrames(2, nextSeq, nextTimestamp)
		require.NoError(t, err)
		missingFrameCount := 3
		nextTimestamp += uint32(missingFrameCount) * samplesPerFrame
		_, _, err = tester.SendSignalFrames(2, nextSeq, nextTimestamp)
		require.NoError(t, err)

		tester.assertSilenceIndexes(t, 7, []int{2, 3, 4})
	})

	t.Run("sequence wrap-around no gap", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)

		// Start near sequence wrap-around
		nextSeq := uint16(0xFFFE)
		nextTimestamp := uint32(10000)
		_, _, err := tester.SendSignalFrames(4, nextSeq, nextTimestamp)
		require.NoError(t, err)

		tester.assertSilenceIndexes(t, 4, []int{})
	})

	t.Run("sequence wrap-around with gap", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)

		// Start near sequence wrap-around
		nextSeq := uint16(0xFFFE)
		nextTimestamp := uint32(10000)
		nextSeq, nextTimestamp, err := tester.SendSignalFrames(2, nextSeq, nextTimestamp)
		require.NoError(t, err)
		missingFrameCount := 3
		nextTimestamp += uint32(missingFrameCount) * samplesPerFrame
		_, _, err = tester.SendSignalFrames(2, nextSeq, nextTimestamp)
		require.NoError(t, err)

		tester.assertSilenceIndexes(t, 7, []int{2, 3, 4})
	})
}
func TestSilenceSuppressionDifferentCodecs(t *testing.T) {
	log := logger.GetLogger()

	testCases := []struct {
		name        string
		clockRate   int
		sampleRate  int
		description string
	}{
		{
			name:        "PCMU",
			clockRate:   8000,
			sampleRate:  8000,
			description: "PCMU: 8kHz clock rate / 8kHz sample rate",
		},
		{
			name:        "PCMA",
			clockRate:   8000,
			sampleRate:  8000,
			description: "PCMA: 8kHz clock rate / 8kHz sample rate",
		},
		{
			name:        "Opus",
			clockRate:   48000,
			sampleRate:  48000,
			description: "Opus: 48kHz clock rate / 48kHz sample rate",
		},
		{
			name:        "G722",
			clockRate:   8000,
			sampleRate:  16000,
			description: "G722: 8kHz clock rate / 16kHz sample rate",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tester := newSilenceSuppressionTester(tc.sampleRate, log, WithClockRate(tc.clockRate))
			tsPerFrame := uint32(tc.clockRate / rtp.DefFramesPerSec)

			// Send 5 frames, then create a gap of 2 frames, then send 5 more frames
			nextSeq := uint16(100)
			nextTimestamp := uint32(10000)
			nextSeq, nextTimestamp, err := tester.SendSignalFrames(5, nextSeq, nextTimestamp)
			require.NoError(t, err)

			// Create a gap of 2 frames by incrementing timestamp
			missingFrameCount := 2
			nextTimestamp += uint32(missingFrameCount) * tsPerFrame
			nextSeq, nextTimestamp, err = tester.SendSignalFrames(5, nextSeq, nextTimestamp)
			require.NoError(t, err)

			// Should have 5 signal + 2 silence + 5 signal = 12 total frames
			tester.assertSilenceIndexes(t, 12, []int{5, 6})
		})
	}
}

// A phone that sends a key press as RFC 4733 events stops sending audio for its duration. The caller's track in the
// room advances by the frames written to it, so the filler has to write silence for that time.
func TestSilenceFillerDTMF(t *testing.T) {
	const (
		sampleRate = 8000
		tsPerFrame = uint32(sampleRate / rtp.DefFramesPerSec)
		startSeq   = uint16(100)
		startTS    = uint32(10000)
		before     = 10 // audio frames before the key press
		after      = 5  // audio frames after it
	)
	log := logger.GetLogger()

	silence := func(from, count int) []int {
		idx := make([]int, count)
		for i := range idx {
			idx[i] = from + i
		}
		return idx
	}

	t.Run("events replace audio in the same stream", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)
		seq, ts, err := tester.SendSignalFrames(before, startSeq, startTS)
		require.NoError(t, err)

		// 5 packets while the key is down, then the end packet repeated twice: 7 frames without audio.
		seq, err = tester.SendDTMF(7, 5, seq, ts, rtp.DefFrameDur)
		require.NoError(t, err)
		// Silence is written while the key is down, one frame behind the event.
		require.EqualValues(t, 4, tester.receivedSilenceFrames)

		_, _, err = tester.SendSignalFrames(after, seq, ts+7*tsPerFrame)
		require.NoError(t, err)
		tester.assertSilenceIndexes(t, before+7+after, silence(before, 7))
	})

	t.Run("events have their own sequence numbers", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)
		seq, ts, err := tester.SendSignalFrames(before, startSeq, startTS)
		require.NoError(t, err)

		_, err = tester.SendDTMF(7, 5, 5000, ts, rtp.DefFrameDur)
		require.NoError(t, err)

		// No sequence gap in the audio: the gap must not be filled twice.
		_, _, err = tester.SendSignalFrames(after, seq, ts+7*tsPerFrame)
		require.NoError(t, err)
		tester.assertSilenceIndexes(t, before+7+after, silence(before, 7))
	})

	t.Run("key held longer than the silence suppression limit", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)
		seq, ts, err := tester.SendSignalFrames(before, startSeq, startTS)
		require.NoError(t, err)

		const held = 40 // 800 ms
		seq, err = tester.SendDTMF(held, held, seq, ts, rtp.DefFrameDur)
		require.NoError(t, err)
		require.EqualValues(t, held-1, tester.receivedSilenceFrames)

		_, _, err = tester.SendSignalFrames(after, seq, ts+held*tsPerFrame)
		require.NoError(t, err)
		tester.assertSilenceIndexes(t, before+held+after, silence(before, held))
	})

	t.Run("two key presses without audio in between", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)
		seq, ts, err := tester.SendSignalFrames(before, startSeq, startTS)
		require.NoError(t, err)

		seq, err = tester.SendDTMF(7, 5, seq, ts, rtp.DefFrameDur)
		require.NoError(t, err)
		seq, err = tester.SendDTMF(7, 5, seq, ts+7*tsPerFrame, rtp.DefFrameDur)
		require.NoError(t, err)

		_, _, err = tester.SendSignalFrames(after, seq, ts+14*tsPerFrame)
		require.NoError(t, err)
		tester.assertSilenceIndexes(t, before+14+after, silence(before, 14))
	})

	t.Run("events arrive in a burst", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)
		seq, ts, err := tester.SendSignalFrames(before, startSeq, startTS)
		require.NoError(t, err)

		// No time has passed since the last audio packet, so nothing may be written yet.
		seq, err = tester.SendDTMF(7, 5, seq, ts, 0)
		require.NoError(t, err)
		require.EqualValues(t, 0, tester.receivedSilenceFrames)

		_, _, err = tester.SendSignalFrames(after, seq, ts+7*tsPerFrame)
		require.NoError(t, err)
		tester.assertSilenceIndexes(t, before+7+after, silence(before, 7))
	})

	t.Run("event timestamps far ahead of the audio", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)
		seq, ts, err := tester.SendSignalFrames(before, startSeq, startTS)
		require.NoError(t, err)

		// Never more silence than the time that has passed since the last audio packet.
		seq, err = tester.SendDTMF(5, 5, seq, ts+1000*tsPerFrame, rtp.DefFrameDur)
		require.NoError(t, err)
		require.EqualValues(t, 4, tester.receivedSilenceFrames)

		_, _, err = tester.SendSignalFrames(after, seq, ts+5*tsPerFrame)
		require.NoError(t, err)
		tester.assertSilenceIndexes(t, before+5+after, silence(before, 5))
	})

	t.Run("event timestamps behind the audio", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)
		seq, ts, err := tester.SendSignalFrames(before, startSeq, startTS)
		require.NoError(t, err)

		seq, err = tester.SendDTMF(5, 5, seq, ts-1000*tsPerFrame, rtp.DefFrameDur)
		require.NoError(t, err)
		require.EqualValues(t, 0, tester.receivedSilenceFrames)

		_, _, err = tester.SendSignalFrames(after, seq, ts+5*tsPerFrame)
		require.NoError(t, err)
		tester.assertSilenceIndexes(t, before+5+after, silence(before, 5))
	})

	t.Run("audio resumes after more than the fill limit", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)
		seq, ts, err := tester.SendSignalFrames(before, startSeq, startTS)
		require.NoError(t, err)

		seq, err = tester.SendDTMF(3, 3, seq, ts, rtp.DefFrameDur)
		require.NoError(t, err)

		// 300 frames (6 s) later: likely a timestamp reset, the rest is not filled.
		_, _, err = tester.SendSignalFrames(after, seq, ts+300*tsPerFrame)
		require.NoError(t, err)
		tester.assertSilenceIndexes(t, before+2+after, silence(before, 2))
	})

	t.Run("gap up to the fill limit is filled when audio resumes", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)
		seq, ts, err := tester.SendSignalFrames(before, startSeq, startTS)
		require.NoError(t, err)

		seq, err = tester.SendDTMF(3, 3, seq, ts, 0)
		require.NoError(t, err)

		_, _, err = tester.SendSignalFrames(after, seq, ts+200*tsPerFrame)
		require.NoError(t, err)
		tester.assertSilenceIndexes(t, before+200+after, silence(before, 200))
	})

	t.Run("audio keeps flowing during the key press", func(t *testing.T) {
		for _, eventFirst := range []bool{false, true} {
			tester := newSilenceSuppressionTester(sampleRate, log)
			seq, ts, err := tester.SendSignalFrames(before, startSeq, startTS)
			require.NoError(t, err)

			// One audio and one event packet every 20 ms, sharing sequence numbers.
			eventTS := ts
			for i := 0; i < 10; i++ {
				var payload [4]byte
				_, err := dtmf.Encode(payload[:], dtmf.Event{Digit: '5', Dur: uint16(uint32(i+1) * tsPerFrame)})
				require.NoError(t, err)
				event := func() {
					require.NoError(t, tester.filler.HandleDTMF(&rtp.Header{SequenceNumber: seq, Timestamp: eventTS, Marker: i == 0}, payload[:]))
					seq++
				}
				tester.clock = tester.clock.Add(rtp.DefFrameDur)
				if eventFirst {
					event()
				}
				tester.clock = tester.clock.Add(-rtp.DefFrameDur) // SendSignalFrames advances the clock itself
				seq, ts, err = tester.SendSignalFrames(1, seq, ts)
				require.NoError(t, err)
				if !eventFirst {
					event()
				}
			}
			tester.assertSilenceIndexes(t, before+10, []int{})
		}
	})

	t.Run("packet loss without events is still not filled", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)
		seq, ts, err := tester.SendSignalFrames(before, startSeq, startTS)
		require.NoError(t, err)

		_, _, err = tester.SendSignalFrames(after, seq+3, ts+3*tsPerFrame)
		require.NoError(t, err)
		tester.assertSilenceIndexes(t, before+after, []int{})
	})

	t.Run("events before any audio", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)
		seq, err := tester.SendDTMF(7, 5, startSeq, startTS, rtp.DefFrameDur)
		require.NoError(t, err)

		_, _, err = tester.SendSignalFrames(after, seq, startTS+7*tsPerFrame)
		require.NoError(t, err)
		tester.assertSilenceIndexes(t, after, []int{})
	})

	t.Run("malformed event", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)
		seq, ts, err := tester.SendSignalFrames(before, startSeq, startTS)
		require.NoError(t, err)

		tester.clock = tester.clock.Add(3 * rtp.DefFrameDur)
		require.NoError(t, tester.filler.HandleDTMF(&rtp.Header{SequenceNumber: seq, Timestamp: ts}, []byte{5}))
		require.EqualValues(t, 0, tester.receivedSilenceFrames)

		// It still marks a key press, so the gap is filled when audio resumes.
		_, _, err = tester.SendSignalFrames(after, seq+1, ts+3*tsPerFrame)
		require.NoError(t, err)
		tester.assertSilenceIndexes(t, before+3+after, silence(before, 3))
	})

	t.Run("timestamp wrap-around during the key press", func(t *testing.T) {
		tester := newSilenceSuppressionTester(sampleRate, log)
		seq, ts, err := tester.SendSignalFrames(before, startSeq, 0xFFFFFFFF-12*tsPerFrame)
		require.NoError(t, err)

		seq, err = tester.SendDTMF(7, 5, seq, ts, rtp.DefFrameDur)
		require.NoError(t, err)
		require.EqualValues(t, 4, tester.receivedSilenceFrames)

		_, _, err = tester.SendSignalFrames(after, seq, ts+7*tsPerFrame)
		require.NoError(t, err)
		tester.assertSilenceIndexes(t, before+7+after, silence(before, 7))
	})

	t.Run("G722", func(t *testing.T) {
		// 8 kHz RTP clock, 16 kHz audio: silence frames must have the PCM frame size.
		tester := newSilenceSuppressionTester(16000, log, WithClockRate(8000))
		seq, ts, err := tester.SendSignalFrames(before, startSeq, startTS)
		require.NoError(t, err)

		seq, err = tester.SendDTMF(7, 5, seq, ts, rtp.DefFrameDur)
		require.NoError(t, err)

		_, _, err = tester.SendSignalFrames(after, seq, ts+7*tsPerFrame)
		require.NoError(t, err)
		tester.assertSilenceIndexes(t, before+7+after, silence(before, 7))
	})
}
