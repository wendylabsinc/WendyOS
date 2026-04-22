# kb.no-dispatch-queues plan

Refactor Bonjour registration for Swift Concurrency. Split out of the
`kb.address-feedback` plan because it is orthogonal to the Docker
subprocess work and should land as its own PR.

## Scope

`BonjourAdvertiser` should stop relying on an ad-hoc `DispatchQueue`
and align with Swift Concurrency.

## Changes

- Refactor `swift/WendyAgentCore/Sources/WendyAgent/Services/BonjourAdvertiser.swift`.
- Replace the manually managed `DispatchQueue` state machine with a
  concurrency-native isolated type.
- Evaluate the best fit:
  - an `actor` owning registration state, or
  - a dedicated `@globalActor` if the DNS-SD callback path truly
    requires execution on one serial executor.
- Keep the external API of `BonjourAdvertiser` as stable as possible.

## Design targets

- One place owns `serviceRef`, continuations, and finish state.
- Callback entry points hop into the isolated domain instead of
  mutating shared state directly.
- Remove the need for `@unchecked Sendable` if practical; if not,
  document exactly why it remains necessary.
- Preserve clean shutdown and registration-loss handling.

## Validation

- Verify:
  - successful start
  - callback-driven ready transition
  - graceful shutdown
  - registration loss / error propagation
- Run strict concurrency checks and resolve any new warnings rather
  than suppressing them.

## Deliverables

- One focused commit / PR for the Bonjour concurrency cleanup.
