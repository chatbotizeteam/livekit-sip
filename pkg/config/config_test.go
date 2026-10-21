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

package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

const minimalConfig = `
redis:
  address: localhost:6379
`

func TestNewConfigEnablesDTMFRecordingToneByDefault(t *testing.T) {
	conf, err := NewConfig(minimalConfig)

	require.NoError(t, err)
	require.True(t, conf.DTMFRecordingTone)
}

func TestNewConfigDTMFRecordingToneCanBeTurnedOff(t *testing.T) {
	conf, err := NewConfig(minimalConfig + "dtmf_recording_tone: false\n")

	require.NoError(t, err)
	require.False(t, conf.DTMFRecordingTone)
}
