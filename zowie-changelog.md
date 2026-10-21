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
