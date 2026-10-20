# Zowie changelog

Everything on the `zowie-custom` branch that is not in upstream [livekit/sip](https://github.com/livekit/sip),
grouped by release tag (`zowie-X.Y.Z`), newest first. When rebasing onto a newer upstream, go through this list:
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

## zowie-1.1.0 (2026-10-21)

### Expose the negotiated audio codec as SIP participant attributes

- **Commit:** this PR
- **Files:** `pkg/sip/media_attrs.go`, `pkg/sip/media_attrs_test.go`, `pkg/sip/participant.go`,
  `pkg/sip/inbound.go`, `pkg/sip/outbound.go`
- **What:** two new participant attributes: `sip.audioCodec` (SDP name, e.g. `PCMA/8000`) and
  `sip.audioSampleRate` (the codec's real sample rate: `8000` for G.711, `16000` for G.722, whose SDP clock rate
  says 8000). Inbound calls carry them from the moment the participant joins, since the codec is negotiated
  before the room is joined. Outbound calls get them through `SetAttributes` once the callee answers. The
  livekit-agent reads `sip.audioSampleRate` to stream speech to Azure at 8 kHz on narrowband calls, for numbers
  that opt in; without the attribute it keeps 16 kHz.
- **Deploy order:** any. An agent that does not read the attributes ignores them.
- **Upstream status:** Zowie only. Upstream keeps the codec only in `SIPCallInfo.AudioCodec` for analytics
  (checked on upstream main, 2026-10-20).

## zowie-1.0.0 (2026-10-21)

The first tag: the code of `hotfix/srtp-stream-leak`, which images were built from until then, plus this
changelog and `CLAUDE.md`.

### Bump media-sdk for the SRTP read-stream leak

- **Commit:** 7b9b3c5 (Rafał Leżanko, 2026-06-16)
- **Files:** `go.mod`, `go.sum`
- **What:** media-sdk `v0.0.0-20260328053058-9f0edcdf3641`, which fixes a leak of SRTP read streams (ZEM-1251).
- **Upstream status:** shipped as 450ffb7 (#638, 2026-03-28) with the same media-sdk version. Drop this entry
  when rebasing onto 450ffb7 or newer.
