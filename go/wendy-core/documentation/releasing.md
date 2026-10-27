# Releasing Wendy Core

Core lives in `go/wendy-core/` in WendyOS as a separate Go module. Run commands
from that directory. Publication from WendyOS is disabled pending a separately
approved release design. The old public Core repository stays unchanged.


## One version, multiple incarnations

`wendy-core` is the coordinated release unit: runtime, selected modules, public contracts and included adapters/backends. All Core incarnations use that release identity. Ergonomic SDKs in their future repositories own independent versions and depend on compatible Core packages; an SDK-only edit does not require a Core release.

`VERSION` names the **exact intended release**, either `x.y.z` or `x.y.z-seed.<n>` (positive `n`, no leading zeros), as do checked-in npm/Kotlin metadata (Python uses the PEP 440 mapping) and `documentation/versions/<VERSION>.json`. Do not derive the compatibility number from a timestamp or branch. Compatible fixes require a patch bump, additions a minor bump, and breaking changes a major bump even before 1.0 under this project's strict policy. Go major releases must also follow Go module-path rules.

`scripts/check_release.py` compares the base snapshot against reachable final/seed release tags, ignoring PR/nightly preview tags. It catches changed/removed C operations and additions, not complete ABI, Go API, protobuf, platform SPI, behavior or support compatibility. Those remain review obligations. After stable publication, advance `VERSION` before the next nightly; preview builds cannot keep using an already-released base.

## Channels and build identity

| Source | Effective version | Publication channel |
|---|---|---|
| Local build | `x.y.z-dev+<7-character HEAD commit>` (or `x.y.z-dev` without a checkout) | None |
| PR head | `x.y.z-pr.<number>-<slug>.<run-id>.<attempt>` | PR preview |
| Manual branch build | `x.y.z-pr.manual-<slug>.<run-id>.<attempt>` | Branch preview |
| Passing main build | `x.y.z-nightly.<run-id>.<attempt>` | Nightly |
| Manual build (release tags are inactive) | `x.y.z` or `x.y.z-seed.<n>` | Stable release path |

`scripts/versioning.py` derives the identity once per workflow. Branch slugs are sanitized and bounded; PR number is the primary PR identity. Global GitHub run IDs prevent collisions between workflows. Fresh full rebuilds get a new attempt; retries of failed jobs retain their originating identity and tested bytes. Never overwrite a published version or use a bare, repeatedly published `x.y.z-nightly`.

PRs build their exact head commit, not GitHub's synthetic merge commit. Every native/WASM inventory records the same version, source SHA and originating run/attempt; assembly rejects mixed identities or changed checksums. The final `release.json` records this identity, including package-manager mappings. Generated C/JS contract snapshots describe the stable base, not each transient preview suffix.

Python uses PEP 440: final releases stay `x.y.z`, seed releases map to `x.y.za<n>` (for example `0.1.0a1`, preserving prerelease ordering because `seed` is not a PEP 440 prerelease label), local stays `x.y.z.dev0` even when the engine includes a commit (PyPI rejects local-version segments), and both PR/nightly publications become `x.y.z.dev<run-id * 1000 + attempt>`. Attempts must be below 1000. PR/branch/channel information is retained in `release.json`; PyPI does not accept arbitrary SemVer prereleases or local-version labels. The mapping is deterministic and collision-free for distinct runs/attempts.

npm uses `latest` only for final releases, `seed` for seed prereleases, `nightly` for main, `pr-<number>` for PRs and `preview-<slug>` for manual builds. Moving pointers are conveniences; reproducible preview consumers pin exact versions. Maven previews are immutable versions, not `SNAPSHOT`s. Nested Go module tags use `go/wendy-core/v<effective-version>`, separate from WendyOS root tags. Swift release consumption from WendyOS is unresolved (see below). Local builds do not create tags or update published pointers.

`core.Version()` and its C/language projections report this effective identity, stamped into native/shared, static Swift and WASM engines at Go link time. Ordinary Go module consumers read dependency build information: tagged versions retain their release/prerelease version without `v`, while Go pseudo-versions become the language-neutral `0.1.0-dev+<7-character commit>`. Main-module source builds use `vcs.revision` when available, omitting a dirty-tree marker. Local replacements and source builds without a known Core revision report `0.1.0-dev`; a consuming application's VCS revision is never used as Core's revision. This does not run `git describe`: a Go module dependency has no Git checkout.

Local engines omit dirty-tree markers, matching source-build version reporting. npm and Maven retain the SemVer build metadata unchanged; their local package validators accept it.

Package stamping happens in staging directories, not by editing tracked metadata. Raw C archives carry the effective `VERSION` and the Swift artifact bundle records it; npm tarballs, Python wheels and Maven jars/POM use the appropriate effective version. Ordinary local source dependencies still read the checked-in base metadata; packaged local distributions carry the development version.

## Build and publication boundaries

- `.github/workflows/wendy-core-ci.yml` at the WendyOS root builds PR heads and
  main with filters for `go/wendy-core/**` and `wendy-core-*.yml` workflows.
- `wendy-core-build-core.yml` is the reusable native/WASM build, assembly and
  installed-consumer validation. Shell commands run in `go/wendy-core/`; action
  paths and artifact locations are relative to the WendyOS root.
- `wendy-core-release.yml` is manual-only and builds artifacts without publishing.
  Root tags, nested Go tags and GitHub releases do not trigger it.
- `wendy-core-publish.yml` is a manual-only notice with read-only permissions and
  no publisher steps. Variables such as `ENABLE_CORE_PUBLISH` cannot enable it.
- `scripts/publication.py verify` still validates sealed build artifacts. Its
  publication CLI commands reject execution while migration design is pending.
  Legacy publisher functions remain for reference and their existing unit tests;
  they are not wired to a publication workflow.

## Go and Swift consumers

The module path is `github.com/wendylabsinc/WendyOS/go/wendy-core`; import its
`core` package directly. GitHub repository lookup uses `WendyOS`, not the root
module's historical `github.com/wendylabsinc/wendy` declaration (that repository
lookup returns 404). The Go proxy resolves the real repository path. Nested
module releases must use `go/wendy-core/vX.Y.Z` tags, as specified in the
[Go module reference](https://go.dev/ref/mod#vcs-version). The prefix separates
Core releases from WendyOS's own tags and compatibility comparisons.
No tags are created by this migration. The separate WendyGo SDK needs a
follow-up dependency/import change after this PR merges.

SwiftPM needs `Package.swift` at a fetched repository's root and cannot resolve
Core in this subdirectory. Local path-based builds remain available. Recommend
that WendySwift declare checksummed `binaryTarget` URLs for the XCFramework and
Linux artifact bundle from future WendyOS Core release assets. A publish-only
mirror is an alternative. This is an open design question: neither path nor
publication is implemented or authorized here. The existing WendySwift SDK and
its current Core dependency remain unchanged.

npm/PyPI/Maven coordinates are retained as source metadata, not activated
publications. Enabling any registry, GitHub asset release, tag creation or Swift
mirror requires an approved design and explicit publication authority. The old
public Core repository stays as is; whether to archive it later is Konstantin's
decision.

## Integrity and retries

Build/test once per immutable version; publish those artifacts without a second build. Registries cannot be published transactionally. Rerun failed **publisher** jobs with the original build artifact, not a fresh build under the same version. npm and PyPI retry checks reject existing versions with different bytes; Maven verifies existing coordinates/assets before deployment, and SwiftPM checks its existing tag's parent and manifest. The GitHub Release is finalized after language publication succeeds, does not overwrite assets, marks previews as prereleases and only marks stable as latest. Publication is not canceled midway.

Apache-2.0 `LICENSE`/`NOTICE` accompany all distribution formats. Go's separate BSD notice is retained as `LICENSE-Go.txt`; the Gradle wrapper and its original notices remain in the imported history only. Kotlin builds require Gradle 9.8.1 on `PATH`, installed by the pinned setup-gradle action in CI. Modular packaging must still preserve selective native/WASM composition and one shared runtime rather than separate Go engines per module. No module-selection build system is introduced by these channel workflows.

## Current base

The imported base is `0.1.0-seed.1`. Its numeric development fallback remains
`0.1.0-dev`, and its contract is `documentation/versions/0.1.0-seed.1.json`.
Builds preserve existing version/channel formats. No new release is created by
moving the source into WendyOS.
