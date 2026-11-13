# Agent provisioning v2 contract

`contract.json` pins what `wendy.agent.services.v2.WendyProvisioningService`
answers during `wendy device enroll`. Three tests read it:

- `go/internal/agent/services/provisioning_contract_test.go`: the Go agent.
- `swift/WendyAgentCore/Tests/WendyAgentTests/ProvisioningContractTests.swift`:
  the Swift macOS agent.
- `go/internal/cli/cloudenroll/preflight_contract_test.go`: the CLI's
  `IsProvisioned` preflight verdict on each recorded status.

Enrollment and preflight steps use shared `code`, `message`, and `response`
expectations. Reset refusal steps use `agentExpectations.go` and
`agentExpectations.swift`: Go supports Cloud-authenticated reset, while Swift
keeps direct PKI reset disabled. The replay calls have no authenticated peer,
so Go rejects v2 resets at its mTLS check. Both agents must retain the identity
after these refusals. The Go fixture omits its trusted clock, so its status
does not advertise authenticated reset support.

Change the agents and this file together. Keep common behavior in shared
expectations and record intentional differences explicitly.
