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
