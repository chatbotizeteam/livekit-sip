# Zowie fork of LiveKit SIP

This repository is Zowie's fork of [livekit/sip](https://github.com/livekit/sip). Zowie's production branch is
`zowie-custom`: upstream plus the changes listed in `zowie-changelog.md`. The image is built from it by
`zowie-voicebot/livekit-sip/pipeline.sh` (branch pinned in `sip-source.conf`).

## Keep `zowie-changelog.md` up to date

Every change that lands on `zowie-custom` must add an entry to `zowie-changelog.md` in the same commit or PR:
date, commit or PR, files, what it changes and why, and its upstream status ("Zowie only", or the upstream
commit that makes it redundant). This includes dependency bumps and cherry-picks from upstream. A PR to
`zowie-custom` without a changelog entry is not ready for review.

When rebasing `zowie-custom` onto a newer upstream, walk the changelog: carry the "Zowie only" entries, drop
the ones upstream has shipped, and update the "Base" section.

## Do not bump `livekit/protocol` past the pin

The pin in `go.mod` keeps `CreateSIPParticipantAffinity` working (details in `zowie-changelog.md`, "Base").
Cherry-picks from upstream must not pull in a newer protocol.

## Build and test

CGO needs libopus, libopusfile and libsoxr. Run the tests with Go 1.25 or newer, as CI does: existing tests use
`testing/synctest`, which does not build on the Go 1.24 toolchain pinned in `go.mod`.

```bash
go build ./... && go test -race ./pkg/...
```
