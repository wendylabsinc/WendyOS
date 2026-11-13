---
name: wendy-pr-workflow
description: Use when preparing Wendy repo commits, GitHub PRs, review-thread cleanup, or CI fixes, especially for wendy-agent Go changes.
---

# Wendy PR Workflow

Use this for Wendy Labs PR, CI, and review cleanup work.

## Before editing

1. Check branch, remote, and dirty state.
2. Preserve unrelated worktree changes. Stage only intended files.
3. If the user gave a PR URL or review URL, inspect unresolved review threads, not only flat comments.
4. If the user gave a failed workflow or job URL, start from the failing logs and reproduce the relevant command locally where feasible.

## Validation defaults

For `wendy-agent` Go changes:

```bash
cd <wendyos-checkout>/go
gofmt -w <changed-go-files>
go test ./internal/cli/commands ./internal/agent/services
git diff --check
```

Adjust packages to match touched files. When CI uses race-sensitive checks, match the workflow flags locally instead of relying on plain `go test`.

## GitHub review cleanup

When resolving review conversations, use GraphQL `reviewThreads` so thread state is accurate. After patching and pushing, resolve only the addressed threads, then re-query to confirm `isResolved`.

Useful shape:

```bash
gh api graphql -f owner=wendylabsinc -f repo=wendy-agent -F number=<pr-number> -f query='
query($owner:String!, $repo:String!, $number:Int!) {
  repository(owner:$owner, name:$repo) {
    pullRequest(number:$number) {
      reviewThreads(first:100) {
        nodes {
          id
          isResolved
          isOutdated
          path
          line
          comments(first:10) { nodes { url body author { login } } }
        }
      }
    }
  }
}'
```

Resolve an addressed thread:

```bash
gh api graphql -f threadId=<thread-id> -f query='
mutation($threadId:ID!) {
  resolveReviewThread(input:{threadId:$threadId}) {
    thread { id isResolved }
  }
}'
```

## Publish rules

- If the user asked for a ready PR, create a ready-for-review PR, not a draft.
- If the user asked to push directly to `main`, inspect divergence first so unrelated local commits are not bundled.
- Keep PR summaries short and concrete. Mention tests actually run.
