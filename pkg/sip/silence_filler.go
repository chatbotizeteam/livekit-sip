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
	"sync/atomic"
	"time"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/protocol/logger"
)

// silenceFiller detects RTP timestamp discontinuities (silence suppression)
// and generates silence samples to fill the gaps before passing packets to the decoder.
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

	// DTMF gap state. Audio and DTMF packets come from the same RTP mux, so it is only touched from the RTP read path.
	now         func() time.Time
	lastAudioAt time.Time // arrival of the last audio packet
	dtmfActive  bool      // a DTMF packet arrived since the last audio packet
	dtmfNextTS  uint32    // RTP timestamp up to which the audio timeline is filled during the DTMF gap
	dtmfFilled  int       // silence frames written during the DTMF gap so far
}

type SilenceSuppressionOption func(*silenceFiller)

func WithMaxGapSize(maxGapSize int) SilenceSuppressionOption {
	return func(h *silenceFiller) {
		if maxGapSize > 0 {
			h.maxGapSize = maxGapSize
		}
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
	isSilenceSupression, missingFrameCount := h.isSilenceSuppression(header)
	h.lastAudioAt = h.now()

	if h.dtmfActive {
		// First audio packet after a key press: fill what HandleDTMF has not, whatever the sequence numbers say.
		h.dtmfActive = false
		remaining := int(int32(header.Timestamp-h.dtmfNextTS)) / h.rtpSamplesPerFrame
		if remaining > 0 && remaining <= dtmfMaxResumeGap {
			if err := h.fillWithSilence(remaining); err != nil {
				return err
			}
		} else {
			remaining = 0
		}
		h.log.Debugw("filled DTMF gap",
			"rtpSeq", header.SequenceNumber,
			"rtpTimestamp", header.Timestamp,
			"gapSize", h.dtmfFilled+remaining,
			"filledOnResume", remaining,
		)
		return h.encodedSink.HandleRTP(header, payload)
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

// dtmfMaxResumeGap is the largest gap, in frames, filled at once when audio resumes after a key press (5 s).
// A larger one is most likely an RTP timestamp reset.
const dtmfMaxResumeGap = 250

// HandleDTMF is called for every RFC 4733 telephone-event packet, including the ones that do not start a digit.
//
// A phone that sends a key press as events sends no audio for its duration, usually in the same RTP stream, so
// the next audio packet has both a sequence and a timestamp gap, which HandleRTP alone treats as packet loss.
// The caller's track in the room advances by the frames written to it, not by the clock, so every missing frame
// moves the rest of the caller's audio 20 ms earlier in recordings.
//
// Silence is written while the key is down instead of all at once when audio resumes, so that listeners do not
// receive a burst of frames to play out before the caller's next words. It stays one frame behind both the
// event and the time since the last audio packet: a phone that keeps sending audio during the key press must
// not get silence added, and event timestamps are not trusted beyond the time that has really passed.
func (h *silenceFiller) HandleDTMF(header *rtp.Header, payload []byte) error {
	if h.packets.Load() == 0 {
		return nil // no audio timeline to fill yet
	}
	if !h.dtmfActive {
		h.dtmfActive = true
		h.dtmfNextTS = uint32(h.lastTS.Load()) + uint32(h.rtpSamplesPerFrame)
		h.dtmfFilled = 0
	}
	ev, err := dtmf.Decode(payload)
	if err != nil {
		return nil // HandleRTP still fills the gap when audio resumes
	}
	eventEnd := header.Timestamp + uint32(ev.Dur)
	byEvent := int(int32(eventEnd-h.dtmfNextTS))/h.rtpSamplesPerFrame - 1
	byClock := int(h.now().Sub(h.lastAudioAt)/rtp.DefFrameDur) - 1 - h.dtmfFilled
	frames := min(byEvent, byClock)
	if frames <= 0 {
		return nil
	}
	if err := h.fillWithSilence(frames); err != nil {
		return err
	}
	h.dtmfNextTS += uint32(frames * h.rtpSamplesPerFrame)
	h.dtmfFilled += frames
	return nil
}
