# Audit1890 own-scope startup boundary

Original `3dda11ee313bc96b3783bae09312a43c843bf8cd` changed the actual Agent's `startMTLSServer` closure from disabling tenant enforcement when its own certificate has no identity scope to refusing to start that server. Interceptor tests with constructed TLSInfo do not exercise this branch.

`test-agent-own-scope.py` uses the existing Agent setup job's compiled binary and already-present Python startup image. It resolves the image to its immutable local ID, builds a small Go test executable and one source-overlay Agent, and starts seven separate disposable containers. Each has its own filesystem, PID namespace and network-none loopback, all capabilities dropped, no devices or exposed host ports, and only readonly fixture/binary mounts. Nothing invokes the Agent on the host.

The five normal cases use actual persisted enrollment and a real, private ECDSA CA. Unknown own scope in strict and grace modes must reach the production refusal log, keep the process alive, and leave both mTLS and plaintext provisioning ports closed. Known legacy and SPIFFE scope, plus explicitly configured off mode, must serve the actual GetAgentVersion RPC over mutually authenticated TLS. The client verifies the issuer, DNS identity and exact seeded Agent certificate. The provisioning JSON and private key must remain byte-for-byte unchanged. The explicit-off case is compatibility coverage, not a suggested deployment policy.

The source overlay restores only the historical enforcement downgrade in actual `main.go`. Both unknown-scope strict/grace cases must then deliver the real authenticated RPC before the named failing assertion. A startup crash, unrelated TLS error, missing listener, skip or timeout does not satisfy the control. The separate direct-MLDSA policy matrix remains independent.

The Agent child has a retained process handle, a bounded graceful stop followed by exact-child kill/wait when necessary, and Linux parent-death signaling. Container PID1 exit bounds all remaining descendants. The external driver supervises Docker commands through the shared process guardian and removes only containers whose exact nonce ownership label matches. Cancellation during cleanup cannot become accepted evidence. Unproven cleanup fails and retains the command logs.

The existing startup job runs:

```sh
python3 .github/scripts/agent_own_scope_test.py
python3 .github/scripts/test-agent-own-scope.py --agent /tmp/wendy-agent --output "$RUNNER_TEMP/agent-own-scope"
```

The preceding existing startup integration must complete first so its local `python:3.13-slim` image is available. No additional image, job, runner or privileged operation is introduced. The integration uses a 75-second startup bound per container, a 95-second Go test deadline, and a 110-second external Docker bound. It tests startup refusal, not graceful-shutdown latency, mesh admission, production PKI issuance, an installed host or deployed Cloud availability.

Local Linux/arm64 fixture compilation and four portable orchestration checks pass. Portable checks cover command isolation, exact-owned cleanup, foreign-container refusal and an actual SIGTERM during injected cleanup; they do not execute Docker or the Agent. Full seven-container runtime and historical-control evidence remain pending the existing hosted startup job. The development host's stalled Docker daemon was not retried.
