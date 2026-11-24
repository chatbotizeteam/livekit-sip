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
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/protocol/logger"
)

const (
	// maxEventFillFrames is the largest gap, in frames, filled at once for the time covered by DTMF events (5 s).
	// A larger one is most likely an RTP timestamp reset.
	maxEventFillFrames = 250
	// eventFillLag is how far, in frames, the silence written during DTMF events stays behind the clock, so that
	// audio from a phone that keeps sending it during a key press, arriving up to 40 ms late, is not doubled.
	eventFillLag = 2
)

// silenceFiller detects RTP timestamp discontinuities (silence suppression)
// and generates silence samples to fill the gaps before passing packets to the decoder.
//
// It also keeps the audio timeline through DTMF key presses sent as RFC 4733 events, see HandleEvent.
type silenceFiller struct {
	maxGapSize            int
	encodedSink           rtp.Handler
	pcmSink               msdk.PCM16Writer
	rtpSamplesPerFrame    int // For gap detection (based on RTP clock rate)
	pcmSamplesPerFrame    int // For silence generation (based on PCM output rate)
	log                   logger.Logger
	lastTS                atomic.Uint64
	lastSeq               atomic.Uint64
	packets               atomic.Uint64
	gapCount              atomic.Uint64
	gapSizeSum            atomic.Uint64
	lastPrintTime         time.Time

	now         func() time.Time
	eventFrames *atomic.Uint64 // silence frames written for DTMF events and the gaps around them; may be nil

	// Each SSRC of a call is read by its own goroutine and the jitter buffer calls in from its timer, while the
	// filler is shared, so HandleRTP and HandleEvent are serialized.
	mu          sync.Mutex
	audioSSRC   uint32
	lastAudioAt time.Time // arrival of the last audio packet
	ev          eventGap
}

// eventGap is the audio timeline while DTMF events replace audio. Timestamps are in RTP clock units.
type eventGap struct {
	active     bool
	ssrc       uint32
	audioTS    uint32 // timestamp right after the last audio frame, reached at lastAudioAt
	fillTS     uint32 // timestamp of the first frame not written yet
	seen       bool   // an event was decoded in this gap
	eventStart uint32 // start of the latest event
	eventEnd   uint32 // end of the latest event (start plus duration)
	filled     int    // frames written during this gap
}

type SilenceSuppressionOption func(*silenceFiller)

func WithMaxGapSize(maxGapSize int) SilenceSuppressionOption {
	return func(h *silenceFiller) {
		if maxGapSize > 0 {
			h.maxGapSize = maxGapSize
		}
	}
}

// withClock replaces the clock used to pace the silence written during DTMF events (tests).
func withClock(now func() time.Time) SilenceSuppressionOption {
	return func(h *silenceFiller) {
		h.now = now
	}
}

// withEventFrameCounter counts the silence frames written for DTMF events.
func withEventFrameCounter(c *atomic.Uint64) SilenceSuppressionOption {
	return func(h *silenceFiller) {
		h.eventFrames = c
	}
}

func newSilenceFiller(encodedSink rtp.Handler, pcmSink msdk.PCM16Writer, rtpClockRate int, pcmSampleRate int, log logger.Logger, options ...SilenceSuppressionOption) *silenceFiller {
	// TODO: We assume 20ms frame. We would need to adjust this when:
	// - When we add support for other frame durations.
	// - When we add support for re-INVITE sdp renegotiation (maybe, if we don't destroy this and start over).
	h := &silenceFiller{
		maxGapSize:         25, // Default max gap size
		encodedSink:        encodedSink,
		pcmSink:            pcmSink,
		rtpSamplesPerFrame: rtpClockRate / rtp.DefFramesPerSec,
		pcmSamplesPerFrame: pcmSampleRate / rtp.DefFramesPerSec,
		log:                log,
		packets:            atomic.Uint64{},
		lastSeq:            atomic.Uint64{},
		lastTS:             atomic.Uint64{},
		gapCount:           atomic.Uint64{},
		gapSizeSum:         atomic.Uint64{},
		lastPrintTime:      time.Time{},
		now:                time.Now,
	}
	for _, option := range options {
		option(h)
	}
	return h
}

func (h *silenceFiller) String() string {
	return fmt.Sprintf("SilenceFiller(%d) -> %s", h.maxGapSize, h.encodedSink.String())
}

func (h *silenceFiller) isSilenceSuppression(header *rtp.Header) (bool, int) {
	packets := h.packets.Add(1)
	lastSeq := uint16(h.lastSeq.Swap(uint64(header.SequenceNumber)))
	lastTS := uint32(h.lastTS.Swap(uint64(header.Timestamp)))
	if packets == 1 {
		return false, 0
	}

	currentTS := header.Timestamp
	currentSeq := header.SequenceNumber

	expectedSeq := lastSeq + 1
	expectedTS := lastTS + uint32(h.rtpSamplesPerFrame)

	seqDiff := currentSeq - expectedSeq
	tsDiff := currentTS - expectedTS

	// A key characteristic of DTX or silence supression is no sequence gaps, but >1 frame TS gaps
	if seqDiff != 0 {
		// Also filters out out-of-order packets
		return false, 0
	}

	missedFrames := int(tsDiff) / int(h.rtpSamplesPerFrame)
	if missedFrames == 0 {
		// Needs to be after / int(h.rtpSamplesPerFrame),
		// since some RTP resets may result in a 0 < TS diff < h.rtpSamplesPerFrame
		return false, 0
	}

	// Silence supression happened - sequential packets (no loss), but with a gap in timestamp
	return true, missedFrames
}

func (h *silenceFiller) fillWithSilence(framesToFill int) error {
	for ; framesToFill > 0; framesToFill-- {
		silence := make(msdk.PCM16Sample, h.pcmSamplesPerFrame)
		if err := h.pcmSink.WriteSample(silence); err != nil {
			return err
		}
	}
	return nil
}

func (h *silenceFiller) HandleRTP(header *rtp.Header, payload []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	isSilenceSupression, missingFrameCount := h.isSilenceSuppression(header)
	h.audioSSRC = header.SSRC
	h.lastAudioAt = h.now()

	if h.ev.active {
		h.ev.active = false
		if header.SSRC == h.ev.ssrc {
			if err := h.resumeAfterEvents(header.Timestamp); err != nil {
				return err
			}
			// The sequence numbers skipped the event packets, so this is not silence suppression.
			return h.encodedSink.HandleRTP(header, payload)
		}
	}

	if isSilenceSupression && missingFrameCount <= h.maxGapSize*100 {
		//  <= h.maxGapSize * 100 done to filter out what is most likely an RTP reset
		count := h.gapCount.Add(1)
		sum := h.gapSizeSum.Add(uint64(missingFrameCount))
		if time.Since(h.lastPrintTime) > 15*time.Second {
			h.lastPrintTime = time.Now()
			h.log.Infow("timestamp gap",
				"rtpSeq", header.SequenceNumber,
				"rtpTimestamp", header.Timestamp,
				"rtpMarker", header.Marker,
				"gapCount", count,
				"gapSize", missingFrameCount,
				"gapSizeSum", sum, // Used to get averages and figure out outliers between prints
			)
		}

		// Don't cause a flood of silence packets on too large of a gap (large loss, RTP TS or Seq reset)
		if missingFrameCount <= h.maxGapSize {
			err := h.fillWithSilence(missingFrameCount)
			if err != nil {
				return err
			}
		}
	}
	return h.encodedSink.HandleRTP(header, payload)
}

// HandleEvent is called for every RFC 4733 telephone-event packet, before the digit is handled.
//
// A phone that sends a key press as events usually sends no audio for its duration, in the same RTP stream, so the
// next audio packet has both a sequence and a timestamp gap, which HandleRTP alone treats as packet loss. The
// caller's track in the room advances by the frames written to it, not by the clock, so every missing frame would
// move the rest of the caller's audio earlier in recordings.
//
// The time an event covers is filled with silence while the key is down, never further than the time that has
// passed since the last audio packet and eventFillLag frames behind it, so that listeners do not get a burst of
// frames before the caller's next words; the rest is filled when audio resumes. A phone that keeps sending audio
// during the key press gets nothing added: every audio packet closes the gap, and the next event starts before it. Gaps before and after an event, with neither audio nor events, are
// silence suppression and get its limit. Events from another SSRC than the audio are left alone: the audio
// sequence numbers have no gap then, and HandleRTP fills it as silence suppression.
func (h *silenceFiller) HandleEvent(header *rtp.Header, payload []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.packets.Load() == 0 || header.SSRC != h.audioSSRC {
		return nil // no audio timeline for this stream
	}
	e := &h.ev
	if !e.active {
		fillTS := uint32(h.lastTS.Load()) + uint32(h.rtpSamplesPerFrame)
		*e = eventGap{active: true, ssrc: header.SSRC, audioTS: fillTS, fillTS: fillTS, eventStart: fillTS, eventEnd: fillTS}
	}
	ev, err := dtmf.Decode(payload)
	if err != nil {
		return nil // nothing to pace by; the gap is filled as silence suppression when audio resumes
	}
	start := header.Timestamp
	end := start + uint32(ev.Dur)
	if !e.seen || tsAfter(start, e.eventEnd) {
		// The first event of the gap, or the next key: finish the previous event and fill the time in between.
		if e.seen {
			if err := h.fillTo(e.eventEnd, maxEventFillFrames); err != nil {
				return err
			}
		}
		if err := h.fillTo(start, h.maxGapSize); err != nil {
			return err
		}
		e.seen, e.eventStart = true, start
	}
	if tsAfter(end, e.eventEnd) {
		e.eventEnd = end
	}
	// Never ahead of the time that has really passed since the last audio packet, whatever the event says.
	target := e.eventEnd
	if byClock := e.audioTS + h.clockUnitsSince(h.lastAudioAt); tsAfter(target, byClock) {
		target = byClock
	}
	return h.fillTo(target-uint32(eventFillLag*h.rtpSamplesPerFrame), maxEventFillFrames)
}

// resumeAfterEvents fills the rest of the gap when the first audio packet after DTMF events arrives.
func (h *silenceFiller) resumeAfterEvents(ts uint32) error {
	e := &h.ev
	// The time the event covers is known, so it is filled whatever its length.
	eventEnd := e.eventEnd
	if tsAfter(eventEnd, ts) {
		eventEnd = ts
	}
	if err := h.fillTo(eventEnd, maxEventFillFrames); err != nil {
		return err
	}
	// The time after the event, with neither audio nor events, is silence suppression.
	if err := h.fillTo(ts, h.maxGapSize); err != nil {
		return err
	}
	h.log.Debugw("filled DTMF gap", "rtpTimestamp", ts, "gapSize", e.filled)
	return nil
}

// fillTo writes silence from the gap's fill timestamp up to ts when that is at most limit frames, and otherwise
// skips the gap without writing, as silence suppression does with a gap that is too large.
func (h *silenceFiller) fillTo(ts uint32, limit int) error {
	e := &h.ev
	frames := int(int32(ts-e.fillTS)) / h.rtpSamplesPerFrame
	if frames <= 0 {
		return nil
	}
	if frames > limit {
		e.fillTS = ts
		return nil
	}
	if err := h.fillWithSilence(frames); err != nil {
		return err
	}
	e.fillTS += uint32(frames * h.rtpSamplesPerFrame)
	e.filled += frames
	if h.eventFrames != nil {
		h.eventFrames.Add(uint64(frames))
	}
	return nil
}

// clockUnitsSince converts the wall time since t to RTP clock units.
func (h *silenceFiller) clockUnitsSince(t time.Time) uint32 {
	d := h.now().Sub(t)
	if d <= 0 {
		return 0
	}
	return uint32(int64(d) * int64(h.rtpSamplesPerFrame) / int64(rtp.DefFrameDur))
}

// tsAfter reports whether RTP timestamp a is later than b, allowing for wrap-around.
func tsAfter(a, b uint32) bool {
	return int32(a-b) > 0
}
