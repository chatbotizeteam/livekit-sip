# Zowie changelog

Everything on the `zowie-custom` branch that is not in upstream [livekit/sip](https://github.com/livekit/sip),
grouped by release tag (`zowie-X.Y.Z`; the tag records the date), newest first. When rebasing onto a newer upstream, go through this list:
carry every entry still marked "Zowie only" and drop the ones upstream has since shipped.

## Base

| | |
|---|---|
| Upstream base | [`8d0037a`](https://github.com/livekit/sip/commit/8d0037a1cfbccc4ee4344e760321273ea395b025) (#617, 2026-03-15) |
| `livekit/protocol` | pinned at `v1.44.1-0.20260126181151-edf9ff286d2a` |
| Built by | `zowie-voicebot/livekit-sip/pipeline.sh` from the tag in `sip-source.conf` |

The protocol pin keeps `CreateSIPParticipantAffinity` working. Upstream drops it in 338f4eb (#629, 2026-03-29),
which bumps protocol to v1.45.1 (protocol #1456 "Dropping sip affinity use"); from there psrpc reports
affinity 1 for every node, so outbound routing ignores load, `max_active_calls` and `sip_trunk_ids`.
The newest upstream commit that still has affinity is 450ffb7 (#638, 2026-03-28).

## zowie-1.2.0

Minor: a new feature.

### Expose the negotiated audio codec as SIP participant attributes (AG-1360)

- **PR:** [#2](https://github.com/chatbotizeteam/livekit-sip/pull/2) (`sip-audio-codec-attributes` → `zowie-custom`)
- **Files:** `pkg/sip/media_attrs.go` (new), `pkg/sip/media_attrs_test.go` (new), `pkg/sip/participant.go`,
  `pkg/sip/inbound.go`, `pkg/sip/outbound.go`
- **What:** two new participant attributes: `sip.audioCodec` (SDP name, e.g. `PCMA/8000`) and
  `sip.audioSampleRate` (the codec's real sample rate: `8000` for G.711, `16000` for G.722, whose SDP clock rate
  says 8000). Inbound calls carry them from the moment the participant joins, since the codec is negotiated
  before the room is joined. Outbound calls get them through `SetAttributes` once the callee answers.
  - The livekit-agent (platform/monorepo!17762) streams speech to Azure at 8 kHz when `sip.audioSampleRate` is
    `8000` and the recognizer's `automatically_adjust_sample_rate` is on, **which is the default**. Without the
    attribute it keeps 16 kHz.
- **Deploy order:** this image is what switches calls to 8 kHz. Per region:
  1. The livekit-agent change from platform/monorepo!17762 deployed and every old pod drained.
  2. `automatically_adjust_sample_rate: false` written on the Azure recognizers of the numbers that must stay at
     16 kHz (Payoneer ko-KR and ja-JP; see the MR).
  3. This image on every SIP host of the region.
  - **Rollback:** going back to an image without this change returns every call to 16 kHz; the agent does not
    need to move.
- **Upstream status:** Zowie only. Upstream keeps the codec only in `SIPCallInfo.AudioCodec` for analytics
  (checked on upstream main, 2026-10-20). Upstream 21fa436 (#837, "Support codec parameters") moves the codec
  to `audio.Info`; when rebasing past it, rewrite `withAudioCodecAttrs` and keep `sip.audioCodec` as
  `name/clockRate` (e.g. `PCMA/8000`), which `media_attrs_test.go` checks.

## zowie-1.1.0

Minor: a new feature, on by default.

### DTMF key presses audible in call recordings (AG-1308)

- **PR:** [#1](https://github.com/chatbotizeteam/livekit-sip/pull/1) (`ag-1308-dtmf-recording-tone` → `zowie-custom`)
- **Files:** `pkg/sip/dtmf_recording_track.go` (new), `pkg/sip/room.go`, `pkg/sip/inbound.go`, `pkg/sip/outbound.go`,
  `pkg/config/config.go`, tests
- **What:** the SIP participant publishes a second audio track, `<identity>-dtmf-recording` with source
  `SCREEN_SHARE_AUDIO`, that plays a 120 ms tone for every RFC 4733 digit received from the phone and silence
  otherwise. Room composite egress records it, so key presses become audible in recordings.
  - The caller does not hear it: a SIP participant does not mix its own tracks, and `subscribeTo` skips the DTMF
    recording track of another SIP participant (transfer leg).
  - Agents do not hear it: livekit-agents `RoomIO` takes only the `SOURCE_MICROPHONE` track.
  - The track is published in the background, off the path to answering or dialing the call; digits received
    before it is published are not recorded. Those digits, and digits dropped while 16 are already waiting, are
    logged ("DTMF digit left out of the recording", without the digit's value); the publish itself is logged
    with the time it took ("DTMF recording track published").
  - Frames are counted against the wall clock, so a late loop catches up instead of shifting later tones
    earlier in the recording.
  - Silence goes out as a fixed 3-byte Opus packet (`f8 ff fe`, the packet libopus itself settles on for
    silence after a tone) instead of being encoded: the encoder runs only for a tone and until it settles
    again. Encoding silence costs about as much CPU as encoding speech; with the fixed packet 100 tracks
    take ~0.2 of a core instead of ~1.2 (local benchmark). A test fails if a libopus update changes the packet.
  - The tones are the standard DTMF pairs, so the digits can be told apart in the recording (and decoded from
    it, PINs and card numbers included). This is deliberate: recordings show which key was pressed.
  - A write error does not stop the track; 50 failed writes in a row (1 s) do, as on the microphone path, and
    are logged ("DTMF recording track stopped after repeated write errors"). Later digits are logged with the
    reason "track stopped".
- **Config:** `dtmf_recording_tone`, **on by default**; set it to `false` to turn it off. The key has to be in
  the config file: the aws-iac template for the EC2 hosts must render the value (`true`/`false`), not only
  `true` when the role variable is set, or setting the variable to `false` leaves the track on. Until it does,
  turning it off in production means going back to a SIP image without this change.
- **Deploy order:**
  1. The livekit-agent change from platform/monorepo!17746 (egress `DUAL_CHANNEL_AGENT`, voicemail detection on
     the microphone track only) deployed in EU and US, rolling deploy finished. With the old agent, outbound
     voicemail detection can pick the silent DTMF track and the tones land on the agent's recording channel.
  2. zowie-voicebot builds the image from a `zowie-*` tag (zowie-voicebot!92; before it, `sip-source.conf` builds
     from a branch). Ship `zowie-1.0.0` through it first, then bump to `zowie-1.1.0` separately.
  3. The new image goes out through aws-iac (`livekit_sip_version`) and ansible to **every** `tag_Role_LiveKitSIP`
     group of a region in one window (prod-eu1: public, kruk, mubi; prod-us1). A SIP host without this change
     mixes every audio track in the room, so when the two legs of a transfer run on different versions, the
     other party hears the key presses. The same applies to a rollback: move all groups back together.
  - **Rollback of the agent** below !17746 while the hosts run `zowie-1.1.0` brings back the problem from step 1:
    move the SIP hosts back first.
- **Upstream status:** Zowie only.

## zowie-1.0.0 (2026-10-21)

The first tag: the code of `hotfix/srtp-stream-leak`, which images were built from until then, plus this
changelog and `CLAUDE.md`.

### Bump media-sdk for the SRTP read-stream leak

- **Commit:** 7b9b3c5 (Rafał Leżanko, 2026-06-16)
- **Files:** `go.mod`, `go.sum`
- **What:** media-sdk `v0.0.0-20260328053058-9f0edcdf3641`, which fixes a leak of SRTP read streams (ZEM-1251).
- **Upstream status:** shipped as 450ffb7 (#638, 2026-03-28) with the same media-sdk version. Drop this entry
  when rebasing onto 450ffb7 or newer.
