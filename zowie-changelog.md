# Zowie changelog

Everything on the `zowie-custom` branch that is not in upstream [livekit/sip](https://github.com/livekit/sip).
Newest first. When rebasing onto a newer upstream, go through this list: carry every entry still marked
"Zowie only" and drop the ones upstream has since shipped.

## Base

| | |
|---|---|
| Upstream base | [`8d0037a`](https://github.com/livekit/sip/commit/8d0037a1cfbccc4ee4344e760321273ea395b025) (#617, 2026-03-15) |
| `livekit/protocol` | pinned at `v1.44.1-0.20260126181151-edf9ff286d2a` |
| Built by | `zowie-voicebot/livekit-sip/pipeline.sh` from the branch in `sip-source.conf` |

The protocol pin keeps `CreateSIPParticipantAffinity` working. Upstream drops it in 338f4eb (#629, 2026-03-29),
which bumps protocol to v1.45.1 (protocol #1456 "Dropping sip affinity use"); from there psrpc reports
affinity 1 for every node, so outbound routing ignores load, `max_active_calls` and `sip_trunk_ids`.
The newest upstream commit that still has affinity is 450ffb7 (#638, 2026-03-28).

## Changes

### 2026-06-16: Bump media-sdk for the SRTP read-stream leak

- **Commit:** 7b9b3c5 (Rafał Leżanko)
- **Files:** `go.mod`, `go.sum`
- **What:** media-sdk `v0.0.0-20260328053058-9f0edcdf3641`, which fixes a leak of SRTP read streams (ZEM-1251).
- **Upstream status:** shipped as 450ffb7 (#638, 2026-03-28) with the same media-sdk version. Drop this entry
  when rebasing onto 450ffb7 or newer.
