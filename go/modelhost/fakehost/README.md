# fakehost

A stand-in model host for the agent's model service
(`specs/2026-09-25-model-watch-design.md`). It speaks the host side of the
contract in `go/internal/agent/models`:

- It reports `ready`, then sends a `model.status` heartbeat every 5 s.
- It reports a `person` entering, then leaving, every `FAKEHOST_PERIOD_SECONDS`
  (default 20).
- It never reads the camera node it is given, so it tests the agent's model
  service, not perception.

Once published, reference it from a development catalog by digest, and point
the agent at that catalog with `WENDY_MODEL_CATALOG_FILE`. The smoke test in
`specs/2026-09-25-model-watch-plan-2-agent-service.md` (Task 17) walks through it.
