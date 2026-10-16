# Agent provisioning v2 contract

`contract.json` pins what `wendy.agent.services.v2.WendyProvisioningService`
answers during `wendy device enroll`. Three tests read it:

- `go/internal/agent/services/provisioning_contract_test.go`: the Go agent.
- `swift/WendyAgentCore/Tests/WendyAgentTests/ProvisioningContractTests.swift`:
  the Swift macOS agent.
- `go/internal/cli/cloudenroll/preflight_contract_test.go`: the CLI's
  `IsProvisioned` preflight verdict on each recorded status.

Change the agents and this file together.
