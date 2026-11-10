# Docs Site

The public WendyOS documentation site is built with Fumadocs and Next.js, then
published as a static export to `https://docs.wendy.dev`.

## URLs

| Purpose | URL |
|---|---|
| Latest stable docs | `https://docs.wendy.dev/latest/` |
| Latest nightly docs | `https://docs.wendy.dev/latest-nightly/` |
| Specific stable release | `https://docs.wendy.dev/release-<version>/` |
| Specific nightly release | `https://docs.wendy.dev/release-nightly-<version>/` |
| Branch preview | `https://docs.wendy.dev/branch-<branch>-<sha>/` |
| Agent docs index for the latest stable docs | `https://docs.wendy.dev/llms.txt` |
| Sitemap and robots.txt for the latest stable docs | `https://docs.wendy.dev/sitemap.xml`, `https://docs.wendy.dev/robots.txt` |

## Source Layout

The Fumadocs app is reached through `docs/`, which is a repository-root symlink
to `go/internal/cli/assets/docs`. Top-level docs content lives as MDX and
`meta.json` files in that tree. Existing lower-level Markdown reference files
are still read from the same tree, but the prep script publishes them under the
`advanced/` section at build time.

```
docs/
  app/          Next.js app router routes, layout, and search
  components/   MDX components, search dialog, and providers
  guides/       Top-level guides and tutorials
  installation/ Top-level setup guides
  lib/          Fumadocs source loader and shared layout config
  scripts/      Content preparation for Fumadocs
  next.config.mjs
  package.json
```

Generated build output is ignored by git: `content/`, `public/`, `.source/`,
`.next/`, `out/`, and `export/`.

## Local Development

```sh
cd docs
npm ci
npm run dev
```

The dev server starts at `http://localhost:3000/`.

When editing source Markdown or MDX files, rerun the content prep script to
refresh generated Fumadocs content:

```sh
cd docs
node scripts/prepare-content.mjs
```

## Local Build

The deployed site uses a path prefix. Set `NEXT_PUBLIC_BASE_PATH` when testing a
prefixed build:

```sh
cd docs
NEXT_PUBLIC_BASE_PATH=/branch-local npm run build
```

The static export is written to `docs/out/`.

## CI And Deploy

The `.github/workflows/fumadocs.yml` workflow runs when `docs`,
`docs/**`, `go/internal/cli/assets/docs/**`, or the workflow file changes.

| Trigger | Behavior |
|---|---|
| `main` branch push | Builds and deploys a branch preview |
| Pull request to `main` from this repository | Builds and deploys a branch preview, then posts or updates a sticky PR comment with the preview URL. Fork PRs do not receive preview comments. |
| Published stable release | Deploys `release-<version>/`, updates `latest/`, and publishes the root `llms.txt`, `llms-full.txt`, `sitemap.xml`, `robots.txt`, and `404.html` |
| Published prerelease/nightly | Deploys `release-nightly-<version>/` and updates `latest-nightly/` |
| Manual dispatch (no inputs) | Builds a branch-style preview artifact without deploying |
| Manual dispatch with `release_tag` input | Deploys a release, identical to a published-release trigger. The `release_prerelease` input selects the target: `false` (default) deploys `release-<version>/` and updates `latest/`; `true` deploys `release-nightly-<version>/` and updates `latest-nightly/`. The dispatch ref must match `release_tag` (dispatch with `--ref "<release_tag>"`), otherwise the deploy fails fast so docs built from one ref are never published under a different release path. |

Preview URLs are posted to same-repository PR comments and are visible to anyone
who can read the PR. This is intentional; deploy paths are public preview paths
and should not include sensitive information.

The deploy job authenticates to GCP with Workload Identity Federation and syncs
static files to `gs://wendy-docs-public/<deploy-path>`. Static exports include
SHA-256 manifests that are verified before each deploy path is synced.
Release deploys attempt to enable bucket object versioning before updating
`latest/` or `latest-nightly/` so alias overwrites remain recoverable. If the
deploy identity lacks bucket-update permission, the deploy verifies the current
state instead: it aborts only when versioning is confirmed disabled (an
overwrite would be unrecoverable), and continues with a warning when versioning
is already enabled out-of-band or cannot be read.

Required GitHub environment variables:

| Variable | Description |
|---|---|
| `GCP_WORKLOAD_IDENTITY_PROVIDER` | Workload Identity provider resource name |
| `GCP_SERVICE_ACCOUNT` | Deploy service account email |
| `GCP_PROJECT_ID` | GCP project ID |

## Hosting

Static files are served from the public `wendy-docs-public` Cloud Storage bucket
through the global external HTTP(S) load balancer for `docs.wendy.dev`. The load
balancer terminates HTTPS and adds security response headers for the public
docs host.

Branch-preview objects under `branch-*` are cleaned up by CI after 30 days.

### Error page

Missing pages return the root `404.html`, which stable release deploys copy
from `latest/404.html`. The bucket serves it only after its website error page
is set, a one-time change that the deploy job does not make. Until then,
missing pages return a Cloud Storage `NoSuchKey` XML error, and the
`Publish root discovery files` step logs a warning. Check the current website
configuration first; directory URLs such as `/latest/` depend on its
`mainPageSuffix` of `index.html`, so pass it again with the error page:

```sh
gcloud storage buckets describe gs://wendy-docs-public --format='yaml(website)'
gcloud storage buckets update gs://wendy-docs-public \
  --web-main-page-suffix=index.html --web-error-page=404.html
```

Verify with `curl -i https://docs.wendy.dev/latest/no-such-page/`, which should
return HTTP 404 with the docs 404 page.

### Vanity redirects

Short marketing paths are served as static redirect pages uploaded to the
bucket root by the `Deploy vanity redirects` step in
`.github/workflows/fumadocs.yml` (main-branch pushes and release deploys only):

| Path | Target |
|---|---|
| `/pi` | `/latest/installation/wendyos-raspberry-pi-5/` |
| `/thor` | `/latest/installation/wendyos-nvidia-jetson-agx-thor/` |
| `/jetson`, `/jetson-orin`, `/jetson-orin-nano` | `/latest/installation/wendyos-nvidia-jetson-orin-nano/` |

Add new entries to the `REDIRECTS` map in that workflow step. Slugs must not
collide with deploy path prefixes (`latest`, `latest-nightly`, `release-*`,
`branch-*`).

### Security headers

The load balancer adds these headers to every `docs.wendy.dev` response. They
are custom response headers on the backend bucket `wendy-docs-backend`, which
the URL map `wendy-docs-url-map` routes `docs.wendy.dev` to, in the GCP project
from `vars.GCP_PROJECT_ID`. No file in this repository manages them.
`install.wendy.dev` and `templates.wendy.dev` share the load balancer but use
other backend buckets without these headers.

| Header | Value |
|---|---|
| `X-Content-Type-Options` | `nosniff` |
| `X-Frame-Options` | `DENY` |
| `Referrer-Policy` | `strict-origin-when-cross-origin` |
| `Permissions-Policy` | `camera=(), microphone=(), geolocation=()` |
| `Content-Security-Policy` | The policy below |

```text
default-src 'self'; base-uri 'self'; object-src 'none'; frame-ancestors 'none'; frame-src https://www.youtube.com https://www.youtube-nocookie.com; img-src 'self' data: blob: https://www.googletagmanager.com https://*.google-analytics.com; font-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline' https://www.googletagmanager.com; connect-src 'self' https://www.googletagmanager.com https://*.google-analytics.com https://*.google.com; worker-src 'self' blob:
```

What the policy means for docs changes:

- Images must come from the docs site itself. Commit them to the docs tree
  instead of linking to another host. The `YouTubeVideo` component uses local
  thumbnails in `images/youtube/` for this reason.
- Iframes can only embed YouTube.
- The Google Analytics hosts follow Google's list for GA4 without Ads features.
  Until 2026-09-27 the policy blocked `gtag.js`, so docs page views before that
  date were not recorded. Turning on Google Signals or linking Google Ads needs
  more hosts.

Read the current headers:

```sh
gcloud compute backend-buckets describe wendy-docs-backend \
  --project=<GCP_PROJECT_ID> --format='yaml(customResponseHeaders)'
```

To change a header, pass the complete list.
`gcloud compute backend-buckets update --custom-response-header` replaces every
custom header, so any header you leave out is removed:

```sh
gcloud compute backend-buckets update wendy-docs-backend --project=<GCP_PROJECT_ID> \
  --custom-response-header="X-Content-Type-Options: nosniff" \
  --custom-response-header="X-Frame-Options: DENY" \
  --custom-response-header="Referrer-Policy: strict-origin-when-cross-origin" \
  --custom-response-header="Permissions-Policy: camera=(), microphone=(), geolocation=()" \
  --custom-response-header="Content-Security-Policy: <complete policy>"
```

Changes reach responses within a few minutes, including responses the CDN has
already cached, so no cache invalidation is needed. Verify with
`curl -sI https://docs.wendy.dev/latest/`, then open a docs page and confirm
the browser console shows no Content Security Policy errors.

## Agent Docs (llms.txt)

Each deploy path also publishes Markdown for AI agents and a sitemap for search
engines. The "View raw Markdown" and "Agent docs index" page actions link to
the Markdown files.

| File | Contents |
|---|---|
| `<deploy-path>/llms.txt` | Index in the [llms.txt](https://llmstxt.org) format: a summary, then each page's Markdown link and description. Top-level pages are under `## Docs` and product pages from `advanced/` under `## Detailed reference`. The generated CLI reference and contributor and internal process pages are under `## Optional` |
| `<deploy-path>/llms-full.txt` | The source of every page in one file |
| `<deploy-path>/<page>.md` | The source of one page, at its page URL plus `.md`, such as `latest/guides/stagefile.md`. The homepage is `index.md` |
| `<deploy-path>/markdown/<page>.md` | The same source, at the path the page actions use |
| `<deploy-path>/sitemap.xml` | Every page that has content. Each hand-written page's `<lastmod>` is the date of the last commit to its source file on the first-parent history, so a change is dated when it reached `main`. Generated pages, such as the CLI reference, have no `<lastmod>` |
| `<deploy-path>/robots.txt` | Allows all crawlers and points to the root sitemap |

`scripts/publish-markdown.mjs` writes the Markdown and llms.txt files and
`scripts/sitemap.mjs` writes the sitemap and robots.txt during content
preparation. The sitemap dates need the full git history, so the build job
checks out with `fetch-depth: 0`. In a shallow clone the build logs a warning and
leaves out the dates it cannot find. Pages without a body, such as legacy
redirect stubs, are left out of llms.txt and the sitemap.

Stable release deploys also copy `latest/llms.txt`, `latest/llms-full.txt`,
`latest/sitemap.xml`, `latest/robots.txt` and `latest/404.html` to the site
root, where agents and crawlers look first, in the `Publish root discovery files`
step of `.github/workflows/fumadocs.yml`. Nightly releases and branch previews
leave the root copies unchanged. Every deploy checks that `index.md` is served as
`text/markdown`.

## Release Notifications

The release workflow adds docs links to Discord notifications:

| Release type | Links |
|---|---|
| Stable | `release-<version>/` and `latest/` |
| Nightly | `release-nightly-<version>/` and `latest-nightly/` |

## Validation

Run these before opening or updating a PR that changes the docs app:

```sh
cd docs
npm run types:check
npm run build
```
