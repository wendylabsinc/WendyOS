# Install Script Hosting

The public install one-liners are served from Google Cloud, not GitHub Pages:

```bash
curl -fsSL https://install.wendy.dev/cli.sh   | bash
curl -fsSL https://install.wendy.sh/agent.sh | bash
```

## Why

`install.wendy.dev` was previously a GitHub Pages custom domain (deployed by the
now-removed `.github/workflows/static.yml`). GitHub Pages soft-throttles a single
IP and returns `403` when many people install at once from one network (e.g. a
workshop behind one NAT). The scripts also resolved their version from the
GitHub REST API (60 requests/hour per IP), which failed the same way. Both are
now served from GCS.

## What is hosted

| Object | Served at | Content-Type |
|---|---|---|
| `agent.sh` | `https://install.wendy.sh/agent.sh` | `text/x-shellscript` |
| `cli.sh` | `https://install.wendy.dev/cli.sh` | `text/x-shellscript` |
| `manifest.json` | `https://install.wendy.sh/manifest.json` | `application/json` |

`manifest.json` carries `{ "latest": "<version>", "latest_nightly": "<version>" }`.
The scripts read `latest` to resolve the install version and fall back to the
GitHub API only if the manifest is unreachable.

## Publishing

The `publish-install-scripts` job in `.github/workflows/build.yml` uploads the
two scripts and merges the channel pointer into `manifest.json` on every `main`
push and on stable release, using `.github/scripts/publish-install-scripts.sh`.
It authenticates with the same Workload Identity Federation identity as the docs
and agent-mirror publishers.

Objects are uploaded with `Cache-Control: public, max-age=300`, so changes take
effect within about five minutes.

## Infrastructure

Static files live in the public bucket `wendy-install-public` and are served
through the **same** global external HTTP(S) load balancer that fronts
`docs.wendy.dev` (see `docs-site.md`). The load balancer terminates HTTPS; a host
rule routes `install.wendy.sh` to a backend bucket for `wendy-install-public`.

### One-time maintainer runbook (LB + cert + DNS)

> **Provision before merge:** the bucket and its IAM bindings (step 1 below)
> must exist BEFORE this branch merges to `main` — the `publish-install-scripts`
> job runs on every `main` push and will fail if the bucket or write IAM isn't
> in place yet.

Run these once, from a shell authenticated to the Wendy GCP project. Replace
`<PROJECT>` with the project id, `<URL_MAP>` / `<TARGET_PROXY>` / `<CERT>` with
the existing docs load-balancer resource names (find them with
`gcloud compute url-maps list`, `gcloud compute target-https-proxies list`,
`gcloud compute ssl-certificates list`).

```bash
# 1. Create the public bucket and make its objects world-readable.
gcloud storage buckets create gs://wendy-install-public \
  --project=<PROJECT> --location=us --uniform-bucket-level-access
gcloud storage buckets add-iam-policy-binding gs://wendy-install-public \
  --member=allUsers --role=roles/storage.objectViewer

# Grant the CI/WIF deploy service account (vars.GCP_SERVICE_ACCOUNT) write access
# so the publish-install-scripts job can upload objects and update the manifest.
gcloud storage buckets add-iam-policy-binding gs://wendy-install-public \
  --member=serviceAccount:<SERVICE_ACCOUNT> --role=roles/storage.objectAdmin

# 2. Seed the bucket so the LB has objects to serve before DNS cutover.
#    (CI republishes these on every build; this is just for verification.)
gcloud storage cp go/internal/cli/assets/docs/agent.sh gs://wendy-install-public/agent.sh \
  --content-type=text/x-shellscript --cache-control="public, max-age=300"
gcloud storage cp go/internal/cli/assets/docs/cli.sh gs://wendy-install-public/cli.sh \
  --content-type=text/x-shellscript --cache-control="public, max-age=300"

# 3. Back the bucket with a backend bucket and add a host rule to the docs LB.
gcloud compute backend-buckets create wendy-install-backend \
  --gcs-bucket-name=wendy-install-public --enable-cdn --project=<PROJECT>
gcloud compute url-maps add-path-matcher <URL_MAP> \
  --path-matcher-name=install \
  --default-backend-bucket=wendy-install-backend \
  --new-hosts=install.wendy.sh \
  --global \
  --project=<PROJECT>

# 4. Add install.wendy.sh to a Google-managed certificate and attach it.
#    Managed certs take minutes-to-tens-of-minutes to become ACTIVE.
gcloud compute ssl-certificates create wendy-install-cert \
  --domains=install.wendy.sh --global --project=<PROJECT>
gcloud compute target-https-proxies update <TARGET_PROXY> \
  --ssl-certificates=<CERT>,wendy-install-cert --global --project=<PROJECT>

# 5. Find the LB's global anycast IP.
gcloud compute forwarding-rules list --global --project=<PROJECT>
```

Then, in the DNS zone for `wendy.sh`:

- **Remove** the existing `install.wendy.sh` record pointing at GitHub Pages
  (`CNAME -> wendylabsinc.github.io`) and the GitHub Pages custom-domain binding
  in the repository's Pages settings.
- **Add** `install.wendy.sh` `A` (and `AAAA` if the LB has IPv6) records → the
  load balancer's global IP from step 5.

Verify before announcing:

```bash
curl -fsSL https://install.wendy.sh/manifest.json
curl -fsSL https://install.wendy.sh/agent.sh | head -1   # -> #!/usr/bin/env bash
```

### After cutover

Once `install.wendy.sh` resolves to the load balancer, flip the
`publish-install-scripts` job's "Verify scripts are reachable" step in
`.github/workflows/build.yml` from
`https://storage.googleapis.com/wendy-install-public/<name>` back to
`https://install.wendy.sh/<name>` (a comment in that step already flags this).
