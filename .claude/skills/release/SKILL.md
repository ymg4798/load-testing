---
name: release
description: Prepares an lt release — full verification, changelog from commits since the last tag, version tag proposal — and only tags or pushes after the user confirms. Use when the user asks to release or publish a version.
argument-hint: "[version, e.g. v0.2.0]"
disable-model-invocation: true
---

# /release $ARGUMENTS

1. Must be on `master` with a clean tree. If not, say what is missing and stop.
2. `/verify` (full). Any failure stops the release.
3. Last tag: `git describe --tags --abbrev=0` (none → first release, propose `v0.1.0`).
   Version: `$ARGUMENTS` if given, else propose one from the changes (new features → minor, fixes only → patch).
4. Changelog: group `git log <last-tag>..HEAD --oneline` into Added / Changed / Fixed, in Korean, user-facing
   wording. Prepend it to `CHANGELOG.md` (create it if missing) and show it to the user.
5. Ask the user (AskUserQuestion) to confirm: version, changelog, and whether to push. Nothing below happens
   without a yes.
6. On yes: commit the changelog, `git tag -a <version> -m "<version>"`, and only if they said so,
   `git push origin master <version>` — the GitHub `release.yml` workflow builds the binaries for the tag.
7. Report what was tagged and pushed, and the release page URL if a remote exists.
