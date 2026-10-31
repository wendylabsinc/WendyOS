# Automatic draft fixes for CLI help synopsis drift

`e2e-synopsis-repair.yml` is prospective automation for completed **push-to-main**
Swift E2E runs. It keeps the existing Sonnet analysis unchanged. It never asks a
model to generate a patch. PR/fork/manual runs cannot enter the repair jobs;
the PR trigger runs mocked fixture tests and one credential-free CLI help test.

The supported edit is one positive `stdout.contains("complete synopsis")`
assertion in `swift/WendyE2ETests/Tests/WendyE2ETests/Wendy*Tests.swift`, inside
`prints command help`, correlated with that test's metadata and its single
successful, unauthenticated `wendy … --help` recording. Only brackets and spacing
may differ, without joining or splitting words. Every word, argument name and flag stays identical. Other failures,
multiple failing assertions, changed test source across the green/red boundary,
or ambiguous evidence produce a reason in the workflow summary and no writes.

## Evidence and verification

The lookup is limited to the latest 100 main push runs and 20 first-parent
commits. Every commit from the source run back to a preceding green must have
exactly one complete first-attempt E2E run. All red artifacts must show the same
single failed assertion; the green artifact must show that exact test passed.
Pending, cancelled, missing, rerun, expired or ambiguous evidence is skipped.
Ubuntu must fail and macOS must pass or be explicitly skipped. The first failing
commit must identify one merged, same-repository, non-bot PR targeting main.
This intentionally cannot attribute the old cancelled-run interval.

Verification runs only the actual failed test with the one literal edited. It
has no publication, Slack or model credentials; checkout authentication is not
persisted. It checks the checkout SHA, exact file change and exactly one passing
XUnit case, so a zero-test run cannot authorize publication. A proof artifact
binds the complete proposal digest, source SHA/run and repair run/attempt.
The help test only launches local shells and the CLI; verification does not
start an agent daemon or use device/cloud credentials.

The fresh publisher rechecks the verification job's success, proof, original
artifacts, history, PR attribution and current main. It only runs trusted
publisher code from the default branch. It does not run repository tests,
hooks, model output or generated code. If main advances, the repair is skipped
rather than rebased automatically. An update racing *after* the final main
check can leave a draft behind main; it still contains only the verified edit
against its original parent, never a write to main.
Downloaded proposal and proof files stay under separate evidence directories;
they cannot overwrite the publisher checkout's script.

## Required configuration (not installed by this PR)

- Variable `E2E_SYNOPSIS_REPAIRS_ENABLED`: exact value `true` to opt in. Leave
  unset to disable repair jobs. Missing `E2E_FIX_BOT_TOKEN` also disables them.
- Secret `E2E_FIX_BOT_TOKEN`: a **dedicated bot account** fine-grained PAT scoped
  only to WendyOS, with **Contents: read/write** and **Pull requests: read/write**.
  These permissions cover creating the branch/draft and commenting on a PR.
  No workflows, administration, merge bypass or release permissions are needed.
  Restrict the bot's push access to `ai/e2e-synopsis/*` using existing repository
  policy when provisioning. Do not reuse the release app or template-sync token.
  GITHUB_TOKEN remains read-only and performs evidence/API reads; the writer
  secret exists only in the publish step. The dedicated token is necessary
  because organization policy blocks GITHUB_TOKEN PR creation and its pushes
  would not trigger ordinary CI.
- Existing secret `CI_SLACK_BOT_TOKEN`: bot `chat:write` permission and membership
  in the feed channel. No Slack history, directory, or identity lookup scope.
- Existing variable `FEED_E2E_SLACK_CHANNEL`: a `C…`/`G…` channel ID or conservative
  `#channel-name`, including `#feed-e2e`. No direct-message targets.
- Variable `E2E_SLACK_AUTHORS`: JSON object mapping exact GitHub login to a verified
  Slack member ID, for example `{"github-login":"U123456789"}` (placeholder,
  **not** a configured identity). Missing/invalid mapping is stated in the draft
  and Slack message; the message does not claim the author was pinged. Missing
  Slack configuration is stated in the draft and skips the Slack attempt.

No settings, secrets, mappings or branch rules are created by this change.
All fixes remain drafts for human inspection, with explicit E2E repair bot
wording and failure/fix links. Nothing marks ready, requests review or merges.

## Duplicates and partial publication

One serialized publisher creates `ai/e2e-synopsis/<first-breaking-commit>` and
puts `<!-- e2e-synopsis-repair:<commit> -->` in the draft and source-PR comment.
It queries **all** PR states for that deterministic branch before writing. An
open, closed or merged fix is reported and never reopened, updated or notified
again. An existing branch without a PR stops for human follow-up. Keep the
branch/PR records as deduplication state; deleting both can remove that guard.

GitHub's create-reference API is used, never update-reference, git push or
force-with-lease. An ambiguous creation failure stops; no write is retried.
The draft's initial body reserves **both** automatic notification attempts
before the source-PR comment and Slack post. Slack action retries are disabled.
If either notification fails, the job fails clearly and later runs do not
retry. A branch created without a draft also needs human follow-up. Operators
inspect the branch, draft, source PR and channel before manually completing
missing steps; no guaranteed-delivery reconciliation is provided.

## Validation

`python3 -m unittest discover -s .github/scripts/tests -p test_e2e_synopsis_repair.py -v`
uses only mocked APIs/artifacts and temporary local Git fixtures. Tests do not
publish repairs or send notifications. Set `E2E_SYNOPSIS_REAL_RESULTS` to the
actual filtered test's XUnit file to also exercise the passing, zero-test and
duplicate-test guards against real Swift output. The PR workflow performs this
integration check on Ubuntu through `E2ETest.sh`; it requires exactly the
`wendy cloud tunnel` help test and starts no agent daemon. The fixture's original failure/literals
come from the Ubuntu artifact for [Swift E2E Tests run 41095365784 — Merge pull
request #2628 from wendylabsinc/fix/drivers-usb-install](https://github.com/wendylabsinc/WendyOS/actions/runs/41095365784).
That run is evidence of the assertion failure, **not** attribution to that PR.

The current actionlint 1.7.12 parser predates `concurrency.queue: max`, already
used by the E2E workflow. Lint this workflow with only that parser diagnostic
excluded; retain the queue so queued publishers are not superseded:

```sh
actionlint -ignore 'unexpected key "queue" for "concurrency" section' .github/workflows/e2e-synopsis-repair.yml
```
