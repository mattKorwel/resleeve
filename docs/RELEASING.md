# Releasing resleeve

This document describes how resleeve is versioned, when releases are cut, and
the exact steps for cutting one. It is the operational companion to
[`CHANGELOG.md`](../CHANGELOG.md) and
[`.github/workflows/release.yml`](../.github/workflows/release.yml).

## Versioning

resleeve follows [Semantic Versioning](https://semver.org/):

- **MAJOR.MINOR.PATCH**, prefixed with `v` on git tags (e.g. `v0.2.0`).
- Pre-release identifiers (`-rc.N`, `-beta.N`, `-alpha.N`) are supported and
  are automatically flagged as **pre-releases** on the GitHub Release. Example:
  `v0.3.0-rc.1`.
- Pre-1.0 (current): MINOR bumps may include breaking changes. PATCH bumps are
  bug fixes and security fixes only. After 1.0, standard semver applies.

## Cadence

While we are pre-1.0:

- **PATCH** releases (`v0.x.Y+1`) are cut as needed — typically within a few
  days of a notable bug or security fix landing on `main`.
- **MINOR** releases (`v0.X+1.0`) target a roughly **2–3 week** rhythm,
  grouping a coherent set of features (a "round" in resleeve's planning
  vocabulary).
- **Pre-releases** (`v0.X.Y-rc.N`) are cut before a contentious MINOR if
  external smoke is wanted before flipping the `latest` alias.

At 1.0 we will reassess; the working assumption is patch-as-needed,
minor-monthly, and majors only with a deprecation cycle.

## Release procedure

> One person drives a release at a time. There is no auto-bump on push to
> `main` — releases are an explicit, human-authored action.

### 1. Pick the new version

Inspect `CHANGELOG.md`'s `[Unreleased]` section. Choose the version per the
rules above:

- breaking → MINOR (pre-1.0) or MAJOR (1.0+)
- new user-visible feature, backward-compatible → MINOR
- bug fix, security fix, doc-only, internal refactor → PATCH

### 2. Land the changelog

On `main` (or a short-lived release branch if you prefer):

1. In `CHANGELOG.md`, rename the `[Unreleased]` heading to
   `[<version>] - <YYYY-MM-DD>` and add a fresh empty `[Unreleased]` above it.
2. Update the comparison links at the bottom of the file (`[Unreleased]`
   compares against the new version; add a new link for the new version
   comparing against the previous one).
3. Optionally add seeded entries to the new `[Unreleased]` block if you know
   what's coming next.
4. Commit:

   ```
   git commit -m "chore(release): v<version>"
   ```

### 3. Tag and push

```sh
git tag -a v<version> -m "v<version>"
git push origin main
git push origin v<version>
```

The `release` workflow triggers on the tag push. It:

1. Builds resleeve for `{linux,darwin}x{amd64,arm64}` with `CGO_ENABLED=0` and
   `-trimpath`, embedding `Version=<version>` and `BuildSHA=<short sha>` via
   `-ldflags -X`.
2. Computes SHA-256 checksums for each binary.
3. Extracts the matching `## [<version>]` section from `CHANGELOG.md` to use
   as the release body.
4. Creates a GitHub Release at the tag, attaches the binaries + `.sha256`
   files, and sets `--latest` (or `--prerelease` if the version contains a
   pre-release identifier like `-rc.1`).

Watch progress: `gh run watch -R mattkorwel/resleeve` or the Actions tab.

### 4. Verify

Once the workflow is green:

```sh
gh release view v<version> -R mattkorwel/resleeve
```

Spot-check at least one binary on the target OS:

```sh
curl -L -o resleeve \
  https://github.com/mattkorwel/resleeve/releases/download/v<version>/resleeve-darwin-arm64
chmod +x resleeve
./resleeve version          # expect: resleeve <version> (<short-sha>)
```

### 5. Post-release

- Announce in the appropriate channel(s) (none yet; placeholder).
- If a security fix shipped, file the corresponding advisory under the repo's
  **Security → Advisories** tab and cross-link it from the release notes.
- For pre-releases that go GA, cut a fresh non-prerelease tag (`v0.3.0` after
  `v0.3.0-rc.1` proves out) — do **not** edit the `-rc` release in place.

## Manual / re-run

The workflow exposes a `workflow_dispatch` input named `tag` for rebuilding
artifacts at a previously cut tag (e.g. if the runner glitched mid-upload).
Trigger from the Actions tab. The job checks out the given tag, rebuilds, and
re-uploads — it will fail at the `gh release create` step if the release
already exists; delete it first with `gh release delete <tag>` if you really
want a clean re-cut.

## Yanking a release

If a release is broken in a way that can't be fixed forward quickly:

1. Edit the release in the GitHub UI and tick **"This is a pre-release"** or
   mark it as a **draft** (depending on severity).
2. Cut the next PATCH release (`v<version>+1`) with the fix and reference the
   yanked version in its CHANGELOG entry.
3. Do **not** delete the tag — that breaks downstream installers that may have
   pinned to it.

## Version embedding details

`internal/cli/dispatch.go` declares:

```go
var (
    Version  = "0.0.0-dev"
    BuildSHA = "dev"
)
```

Release builds override both via:

```
-ldflags="-s -w \
  -X github.com/mattkorwel/resleeve/internal/cli.Version=<version> \
  -X github.com/mattkorwel/resleeve/internal/cli.BuildSHA=<short-sha>"
```

`resleeve version` prints `resleeve <Version> (<BuildSHA>)`. Local
`go build ./cmd/resleeve` without flags yields the `0.0.0-dev (dev)` default,
which is the correct signal for "this isn't a release build."
