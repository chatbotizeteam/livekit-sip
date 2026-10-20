// Copyright 2025 LiveKit, Inc.
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
	"testing"

	msdk "github.com/livekit/media-sdk"
	"github.com/livekit/media-sdk/rtp"
	"github.com/livekit/media-sdk/sdp"
	"github.com/stretchr/testify/require"
)

func audioCodecByName(t *testing.T, name string) rtp.AudioCodec {
	t.Helper()
	for _, c := range msdk.Codecs() {
		if c.Info().SDPName == name {
			ac, ok := c.(rtp.AudioCodec)
			require.True(t, ok, "%s is not an audio codec", name)
			return ac
		}
	}
	t.Fatalf("codec %s is not registered", name)
	return nil
}

func TestWithAudioCodecAttrs(t *testing.T) {
	for _, tc := range []struct {
		codec string
		rate  string
	}{
		{codec: "PCMA/8000", rate: "8000"},
		{codec: "PCMU/8000", rate: "8000"},
		{codec: "G722/8000", rate: "16000"},
	} {
		t.Run(tc.codec, func(t *testing.T) {
			conf := &MediaConf{MediaConfig: sdp.MediaConfig{Audio: sdp.AudioConfig{Codec: audioCodecByName(t, tc.codec)}}}
			attrs := withAudioCodecAttrs(map[string]string{"sip.trunkPhoneNumber": "+48123456789"}, conf)
			require.Equal(t, map[string]string{
				"sip.trunkPhoneNumber": "+48123456789",
				AttrSIPAudioCodec:      tc.codec,
				AttrSIPAudioSampleRate: tc.rate,
			}, attrs)
		})
	}

	t.Run("nil attributes are allocated", func(t *testing.T) {
		conf := &MediaConf{MediaConfig: sdp.MediaConfig{Audio: sdp.AudioConfig{Codec: audioCodecByName(t, "PCMA/8000")}}}
		require.Equal(t, map[string]string{
			AttrSIPAudioCodec:      "PCMA/8000",
			AttrSIPAudioSampleRate: "8000",
		}, withAudioCodecAttrs(nil, conf))
	})

	t.Run("no negotiated audio leaves attributes unchanged", func(t *testing.T) {
		attrs := map[string]string{"sip.trunkPhoneNumber": "+48123456789"}
		require.Equal(t, attrs, withAudioCodecAttrs(attrs, nil))
		require.Equal(t, attrs, withAudioCodecAttrs(attrs, &MediaConf{}))
		require.Nil(t, withAudioCodecAttrs(nil, nil))
	})
}
