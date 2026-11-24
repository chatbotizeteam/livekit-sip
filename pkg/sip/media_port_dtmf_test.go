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
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/dtmf"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/media-sdk/sdp"
	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/logger"
	"github.com/stretchr/testify/require"
)

const testDTMFType = 101

func codecBySDPName(t *testing.T, name string) rtp.AudioCodec {
	for _, c := range msdk.Codecs() {
		if c.Info().SDPName == name {
			return c.(rtp.AudioCodec)
		}
	}
	t.Fatalf("no codec %s", name)
	return nil
}

// dtmfPhone sends RTP to a MediaPort's input pipeline as a phone on the SIP leg does: 20 ms audio frames, and
// key presses as RFC 4733 events in the same stream with no audio while the key is down.
type dtmfPhone struct {
	t       *testing.T
	m       *MediaPort
	payload []byte
	pt      byte
	seq     uint16
	ts      uint32
	elapsed time.Duration
}

func (p *dtmfPhone) send(h *rtp.Header, payload []byte) error {
	hnd := p.m.hnd.Load()
	require.NotNil(p.t, hnd)
	return (*hnd).HandleRTP(h, payload)
}

func (p *dtmfPhone) wait(d time.Duration) {
	time.Sleep(d)
	p.elapsed += d
}

func (p *dtmfPhone) audio(frames int) {
	for range frames {
		h := &rtp.Header{Version: 2, PayloadType: p.pt, SequenceNumber: p.seq, Timestamp: p.ts, SSRC: testAudioSSRC}
		p.seq++
		p.ts += 160
		_ = p.send(h, p.payload) // a failing sink is a test case of its own
		p.wait(rtp.DefFrameDur)
	}
}

func (p *dtmfPhone) key(digit byte, dur time.Duration) {
	const every = 50 * time.Millisecond
	start := p.ts
	event := func(d time.Duration, end, marker bool) {
		var payload [4]byte
		_, err := dtmf.Encode(payload[:], dtmf.Event{Digit: digit, Volume: 10, Dur: uint16(d / time.Millisecond * 8), End: end})
		require.NoError(p.t, err)
		h := &rtp.Header{Version: 2, PayloadType: testDTMFType, SequenceNumber: p.seq, Timestamp: start, Marker: marker, SSRC: testAudioSSRC}
		p.seq++
		_ = p.send(h, payload[:])
	}
	var d time.Duration
	for d+every < dur {
		p.wait(every)
		d += every
		event(d, false, d == every)
	}
	p.wait(dur - d)
	for range 3 {
		event(dur, true, false)
	}
	p.ts = start + uint32(dur/time.Millisecond*8)
}

type dtmfMediaPortCase struct {
	codec  string
	pt     byte
	jitter bool
}

func (c dtmfMediaPortCase) String() string {
	return fmt.Sprintf("%s jitter=%v", c.codec, c.jitter)
}

// withDTMFMediaPort runs fn in a synctest bubble against a MediaPort whose decoded input goes to sink.
func withDTMFMediaPort(t *testing.T, tc dtmfMediaPortCase, sink func(sampleRate int) msdk.PCM16Writer, fn func(p *dtmfPhone, digits func() string)) {
	synctest.Test(t, func(t *testing.T) {
		c1, _ := newUDPPipe()
		m, err := NewMediaPortWith(1, logger.GetLogger(), nil, c1, &MediaOptions{
			IP:                 newIP("1.1.1.1"),
			Ports:              rtcconfig.PortRange{Start: 10000},
			NoInputResample:    true,
			EnableJitterBuffer: tc.jitter,
		}, 8000)
		require.NoError(t, err)
		defer m.Close()

		codec := codecBySDPName(t, tc.codec)
		require.NoError(t, m.SetConfig(&MediaConf{MediaConfig: sdp.MediaConfig{
			Remote: netip.MustParseAddrPort("2.2.2.2:20000"),
			Audio:  sdp.AudioConfig{Codec: codec, Type: tc.pt, DTMFType: testDTMFType},
		}}))
		m.WriteAudioTo(sink(codec.Info().SampleRate))

		var (
			mu  sync.Mutex
			got []byte
		)
		m.HandleDTMF(func(ev dtmf.Event) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, ev.Digit)
		})
		p := &dtmfPhone{t: t, m: m, payload: make([]byte, 160), pt: tc.pt, seq: 1000, ts: 50000}
		fn(p, func() string {
			mu.Lock()
			defer mu.Unlock()
			return string(got)
		})
	})
}

var dtmfMediaPortCases = []dtmfMediaPortCase{
	{"PCMU/8000", 0, false},
	{"PCMU/8000", 0, true},
	{"G722/8000", 9, false},
	{"G722/8000", 9, true},
}

func TestMediaPortDTMFKeepsCallerTimeline(t *testing.T) {
	for _, tc := range dtmfMediaPortCases {
		for _, keys := range []struct {
			name  string
			count int
			dur   time.Duration
		}{
			{"one 200ms key", 1, 200 * time.Millisecond},
			{"nine 160ms keys", 9, 160 * time.Millisecond},
			{"one 800ms key", 1, 800 * time.Millisecond},
		} {
			t.Run(tc.String()+"/"+keys.name, func(t *testing.T) {
				var buf msdk.PCM16Sample
				withDTMFMediaPort(t, tc, func(rate int) msdk.PCM16Writer {
					return msdk.NewPCM16BufferWriter(&buf, rate)
				}, func(p *dtmfPhone, digits func() string) {
					p.audio(50)
					for range keys.count {
						p.key('5', keys.dur)
						p.audio(10)
					}
					p.audio(40)
					p.wait(time.Second) // let the jitter buffer drain
					synctest.Wait()

					rate := p.m.conf.Audio.Codec.Info().SampleRate
					gotMs := len(buf) * 1000 / rate
					wantMs := int((p.elapsed - time.Second) / time.Millisecond)
					require.InDelta(t, wantMs, gotMs, 20, "caller timeline %d ms for %d ms of call", gotMs, wantMs)
					require.Equal(t, keys.count, len(digits()), "digits")
				})
			})
		}
	}
}

// failingWriter is a room track that rejects every frame.
type failingWriter struct{ rate int }

func (w failingWriter) String() string                     { return "failingWriter" }
func (w failingWriter) SampleRate() int                    { return w.rate }
func (w failingWriter) Close() error                       { return nil }
func (w failingWriter) WriteSample(msdk.PCM16Sample) error { return errTestWrite }

func TestMediaPortDTMFDigitSurvivesFailedSilence(t *testing.T) {
	for _, tc := range dtmfMediaPortCases {
		t.Run(tc.String(), func(t *testing.T) {
			withDTMFMediaPort(t, tc, func(rate int) msdk.PCM16Writer {
				return failingWriter{rate: rate}
			}, func(p *dtmfPhone, digits func() string) {
				p.audio(10)
				p.key('7', 200*time.Millisecond)
				p.audio(10)
				p.wait(time.Second)
				synctest.Wait()
				require.Equal(t, "7", digits())
			})
		})
	}
}
