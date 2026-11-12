# Export regression checks

Run these in the existing documentation build job. They require Python 3.10+
standard library, Git, and the existing Node/Go build dependencies. No browser,
extra runner, device, or deployment is needed.

Run the crawler's own HTTP failure checks once:

```sh
python3 scripts/crawl-export.test.py
```

Check both a published prefix and a preview prefix against real Next exports.
Use a nonempty test analytics ID so accidentally enabling preview analytics is
observable. Existing deployment exports can satisfy the corresponding prefix;
a validation-only export must not replace a deployment artifact.

```sh
NEXT_PUBLIC_SITE_URL=https://docs.wendy.dev \
NEXT_PUBLIC_BASE_PATH=/latest \
NEXT_PUBLIC_GA_MEASUREMENT_ID=G-DOCSREGRESSION npm run build
python3 scripts/crawl-export.py --root out --base /latest \
  --analytics-id G-DOCSREGRESSION --report /tmp/docs-latest-crawl.json

NEXT_PUBLIC_SITE_URL=https://docs.wendy.dev \
NEXT_PUBLIC_BASE_PATH=/branch-regression-docs \
NEXT_PUBLIC_GA_MEASUREMENT_ID=G-DOCSREGRESSION npm run build
python3 scripts/crawl-export.py --root out --base /branch-regression-docs \
  --analytics-id G-DOCSREGRESSION --report /tmp/docs-preview-crawl.json
```

The crawler serves the export over loopback HTTP, requires each local route and
fragment to resolve, follows static redirects with a finite cycle limit, checks
all historical aliases independently of current navigation, and validates
canonical URLs, sitemap destinations, preview robots metadata and analytics
suppression. The exported 404 page must remain non-indexable. Directory listings
cannot mask missing pages. External links are reported but never fetched.
Absolute docs.wendy.dev links are checked against the current export's route and
anchor graph. This does not verify public hosting, older deployed releases, GCS
rewrites, or browser-side navigation.

For the Lite examples, fetch these immutable upstream snapshots into disposable
directories outside the checkout. No checkout of their files is necessary:

```sh
fixtures=$(mktemp -d)
trap 'rm -rf "$fixtures"' EXIT

git init -q "$fixtures/templates"
git -C "$fixtures/templates" fetch -q --depth=1 \
  https://github.com/wendylabsinc/templates.git \
  44d5d1ba804c332c1b6787b9812bbcdd4e005aee
git init -q "$fixtures/native"
git -C "$fixtures/native" fetch -q --depth=1 \
  https://github.com/wendylabsinc/wendy-lite-native-apps.git \
  73271d49c1c6872ca46e63a36e94012c6fd68f2f
python3 scripts/test-example-contract.py --root out \
  --templates "$fixtures/templates" --native-examples "$fixtures/native"
```

The example check parses rendered commands and inline code, resolves the selected
Lite template through the real catalog and template manifest, and checks the
editable source path in the committed Git tree. It also resolves the rendered
native example link against that repository's committed directory and C entry
point. The `blink-rgb-led` CLI template and `blink-rgb` repository example are
intentionally different names. Controls reject the old mismatched prose name,
a nonexistent editable file, and a nonexistent linked example directory.
These checks establish source provenance and path validity; they do not compile,
flash, execute the examples, or assert that GitHub's current main branch is the
pinned revision. Refresh the pins deliberately when the documented examples
change.

To check the root README and PAIR example's local files and docs URLs, add these
arguments to the crawler invocation from the docs directory:

```sh
--source-markdown ../../../../../README.md \
--source-markdown ../../../../../Examples/NvidiaPAIR/README.md
```

Local file references must exist. docs.wendy.dev destinations and fragments use
the same real HTTP export crawl. Other external sites and local Markdown heading
fragments remain outside the check.

From the repository root, validate the selected README, mesh, PAIR, discovery,
debugging, and FAQ command snippets with the actual CLI parser:

```sh
go test -race ./go/internal/cli/commands -run '^TestDocumentationCommand' -count=1
```

This parses 73 current documented command lines with the real Cobra command tree
and typed flags, including hidden compatibility commands that the public CLI
reference omits. It does not call Execute or any deployment, discovery, sign-in,
installation, or device handler. It verifies command/flag existence and flag
value types, not runtime prerequisites, positional-argument requirements in
abbreviated inline mentions, non-Wendy shell commands, or remote PAIR RPC results.
Negative controls reject unknown commands/options, invalid durations, and invalid
booleans. OS image, service-manager, firmware, and platform capability claims
still require their own build/runtime evidence.

# Read-only hosted preview verification

After an existing deployment uploads its branch artifact, the existing deploy
job can verify the public response without publishing anything else:

```sh
python3 scripts/probe-hosted-export.test.py
python3 scripts/probe-hosted-export.py \
  --preview-root /path/to/downloaded/export/branch-NAME-FULL_COMMIT_SHA \
  --preview-base /branch-NAME-FULL_COMMIT_SHA \
  --analytics-id G-7SMVRRYVW6 \
  --report /tmp/docs-hosted-probe.json
```

Use the exact deploy prefix and the matching artifact already verified by the
workflow's checksums. The prefix must end in its full 40-character source commit.
The script records that declared commit and compares preview response bytes with
the supplied export; it does not independently rebuild or attest the artifact.
Keep repository checkout credentials disabled for this read-only step. Upload
the JSON report as evidence even on failure.

The probe accepts only HTTPS `docs.wendy.dev`, disables proxies, and refuses
HTTP redirects before following any destination. It checks the preview homepage,
55 historical redirects, their 49 distinct destinations including the homepage,
and robots.txt against the exact artifact. It checks canonical URLs, accessible
redirect fallbacks, target anchors, noindex/nofollow and absent preview analytics.
It then checks the same compatibility map on public `/latest`, which may be a
different revision and is not compared to the preview artifact. Wildcard robots
rules must exclude the tested preview path; comments or other user-agent groups
do not satisfy that check. Production analytics loading is outside this probe.

The current contract makes 210 requests, with limits of 250 requests, 8 MiB per
body, 10 seconds per request and 180 seconds overall. POSIX wall timers also stop
responses that keep sending bytes below a socket's inactivity timeout. The local
HTTP tests exercise rejection and deadlines without contacting public hosts.
The script never mutates remote state. Passing local tests is not live-hosting
evidence; only a successful report from the deployed exact preview establishes
that result. It does not test browser navigation, JavaScript redirects, root GCS
shortlinks, every site page, or deployment rollback.
