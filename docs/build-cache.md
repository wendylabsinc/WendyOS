# CI build cache (snapshot + S3 sstate, downloads, hashserv)

The `Build WendyOS Images` workflow runs a two-layer cache to turn a cold
~1 h Yocto build into a warm incremental build:

- **L1 — runs-on EBS snapshot** (`runs-on/snapshot@v1`). Mounts an EBS
  volume at `sstate-cache/` and `downloads/` and snapshots it on workflow
  end. No copy at start; survives across runs on the same runner family.
- **L2 — S3 mirror.** Shared across every matrix entry (rpi3/4/5/jetson)
  and every runner family. Acts as the cold-start seed when L1 misses, and
  as the durable backup when a snapshot is GC'd or you change runner
  configuration.

Only `sstate-cache/` and `downloads/` are snapshotted at L1 — never
`build/`. `build/` contains mender per-run state and was the source of
the original "snapshots are flaky for mender" issue. sstate is content-
hashed and is always safe to reuse. The per-device `hashserv.db` lives
inside `build/`, so it bypasses L1 and is stored in S3 only.

## What the workflow expects

| Kind   | Name                          | Purpose                                                           |
|--------|-------------------------------|-------------------------------------------------------------------|
| secret | `AWS_BUILD_CACHE_ROLE_ARN`    | IAM role assumed via GitHub OIDC; needs r/w on the cache bucket   |
| var    | `WENDYOS_BUILD_CACHE_BUCKET`  | S3 bucket name (e.g. `wendyos-build-cache`)                       |
| var    | `WENDYOS_BUILD_CACHE_REGION`  | AWS region of the bucket (e.g. `us-east-1`)                       |

## Bucket layout

```
s3://<bucket>/
├── sstate-cache/                  # shared across every device — BitBake keys are content hashes
├── downloads/                     # shared across every device — source tarballs / git mirrors
└── hashserv/<device>/hashserv.db  # per-device hash equivalence DB (avoids concurrent-writer races)
```

`sstate-cache/` and `downloads/` are deliberately not partitioned by device
or branch: BitBake's hashing already guarantees correct reuse, and sharing
them lets the rpi3 job benefit from artifacts produced by the rpi5 job and
vice versa.

`hashserv/<device>/hashserv.db` is per-device because matrix entries run
concurrently and SQLite doesn't tolerate concurrent writers from
independent runners. Hashserv benefit is mostly within-device anyway
(it caches "this task hash maps to this output hash" for the same MACHINE).

## One-time AWS setup

```bash
BUCKET=wendyos-build-cache
REGION=us-east-1

# 1. Create the bucket (block public access, enable encryption).
# Note: us-east-1 is the one region that REJECTS --create-bucket-configuration.
# For any other region you must add: --create-bucket-configuration "LocationConstraint=$REGION"
aws s3api create-bucket --bucket "$BUCKET" --region "$REGION"
aws s3api put-public-access-block --bucket "$BUCKET" --public-access-block-configuration \
  "BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true"
aws s3api put-bucket-encryption --bucket "$BUCKET" --server-side-encryption-configuration \
  '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"}}]}'

# 2. (Optional) Lifecycle: prune sstate objects untouched for 90 days.
#    Sstate hashes self-invalidate, so old objects are dead weight.
aws s3api put-bucket-lifecycle-configuration --bucket "$BUCKET" \
  --lifecycle-configuration file://docs/build-cache-lifecycle.json
```

You also need a GitHub OIDC provider in IAM (one-time per AWS account) and
an IAM role whose trust policy allows the `wendylabsinc/wendyos` repo to
assume it. The role's permissions policy needs:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    { "Effect": "Allow",
      "Action": ["s3:ListBucket"],
      "Resource": "arn:aws:s3:::wendyos-build-cache" },
    { "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"],
      "Resource": "arn:aws:s3:::wendyos-build-cache/*" }
  ]
}
```

Trust policy (replace `<account-id>`):

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": { "Federated": "arn:aws:iam::<account-id>:oidc-provider/token.actions.githubusercontent.com" },
    "Action": "sts:AssumeRoleWithWebIdentity",
    "Condition": {
      "StringEquals": { "token.actions.githubusercontent.com:aud": "sts.amazonaws.com" },
      "StringLike":   { "token.actions.githubusercontent.com:sub": "repo:wendylabsinc/wendyos:*" }
    }
  }]
}
```

Then in repo settings:
- *Secrets and variables → Actions → Secrets*: add `AWS_BUILD_CACHE_ROLE_ARN`.
- *Secrets and variables → Actions → Variables*: add `WENDYOS_BUILD_CACHE_BUCKET` and `WENDYOS_BUILD_CACHE_REGION`.

## Cold start vs. warm start

The first build after any of these events will be cold (or partly cold)
and will *seed* the cache — no faster than today, but it pays for every
subsequent build:

- New bucket / wiped bucket.
- A change to a global path (distro conf, shared recipe, Makefile,
  bootstrap.sh, the workflow itself) — this invalidates a wide swath of
  sstate keys via hash propagation.
- A meaningful change inside a board's template — invalidates that board's
  hashserv DB but downloads and most sstate still hit.

Plain incremental commits should hit warm cache and complete in 5–15 min
instead of ~60 min.

## Operational notes

- Cache misses don't fail the build. The restore and save steps are
  best-effort (`|| true`) — a transient S3 issue degrades to a cold build,
  it doesn't break CI.
- The save step runs `if: always()`, so even a failed build seeds whatever
  sstate it produced. Good for fixing a flaky recipe and re-running.
- `aws s3 sync --size-only` is used for save: sstate filenames are
  content-hashed, so identical size means identical content. This avoids
  re-uploading siblings whose mtime drifted.
- The per-device hashserv DB is small (low MB) and stored as a single
  object — `aws s3 cp` rather than `sync`.
