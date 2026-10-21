# Zowie fork of LiveKit SIP

This repository is Zowie's fork of [livekit/sip](https://github.com/livekit/sip). `zowie-custom` is the Zowie
integration branch: upstream plus the changes listed in `zowie-changelog.md`. Releases are tags on that branch,
`zowie-X.Y.Z`. `zowie-voicebot/livekit-sip/pipeline.sh` builds the image from the tag pinned in
`zowie-voicebot/livekit-sip/sip-source.conf`, never from a moving branch.

## Every change to `zowie-custom` is a new tag

1. The PR to `zowie-custom` adds an entry to `zowie-changelog.md` under a new `## zowie-X.Y.Z` heading (the
   tag records the date):
   commit or PR, files, what it changes and why, and its upstream status ("Zowie only", or the upstream commit
   that makes it redundant). This includes dependency bumps and cherry-picks from upstream. A PR to
   `zowie-custom` without a changelog entry is not ready for review.
2. Version: patch for fixes and dependency bumps, minor for new features or changed defaults, major for a rebase
   onto a newer upstream.
3. After the merge, tag the merge commit and push the tag:

   ```bash
   git tag -a zowie-X.Y.Z -m "zowie-X.Y.Z: <summary>" && git push origin zowie-X.Y.Z
   ```

4. Ship it with an MR in zowie-voicebot that sets `SIP_TAG` in `livekit-sip/sip-source.conf`. Follow the
   deploy order in the changelog entry, if there is one.

Pushed tags are immutable: the repository ruleset "Protect zowie release tags" blocks update, deletion and
force push of `refs/tags/zowie-*` for everyone but repository admins. Images are traced back to their tag, and
rollback means pinning the previous one, so even admins must not move a tag; release a new version instead.

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
