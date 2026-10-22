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
	"strconv"
)

// withAudioCodecAttrs adds the negotiated audio codec to participant attributes,
// so that agents can adapt to the audio bandwidth of the call (e.g. pick the speech
// recognition sample rate). The map is allocated if needed; nil config leaves it unchanged.
func withAudioCodecAttrs(attrs map[string]string, conf *MediaConf) map[string]string {
	if conf == nil || conf.Audio.Codec == nil {
		return attrs
	}
	info := conf.Audio.Codec.Info()
	if info.SDPName == "" || info.SampleRate <= 0 {
		return attrs
	}
	if attrs == nil {
		attrs = make(map[string]string, 2)
	}
	attrs[AttrSIPAudioCodec] = info.SDPName
	attrs[AttrSIPAudioSampleRate] = strconv.Itoa(info.SampleRate)
	return attrs
}
