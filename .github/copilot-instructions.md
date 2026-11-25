Review source material only. During code review, skip deterministic protobuf/gRPC contents produced by these repository generators:

- `go/scripts/generate-proto.sh`: `.pb.go` files beneath `go/proto/gen/`.
- `swift/Scripts/GenerateProto.sh`: `.pb.swift` and `.grpc.swift` files beneath `swift/WendyAgentCore/Sources/WendyAgentGRPC/Proto/`, `swift/WendyAgentCore/Sources/WendyCloudGRPC/Proto/` and `swift/WendyAgentCore/Sources/OpenTelemetryGRPC/Proto/`.
- `Examples/WendyNotify/regenerate-grpc.sh`: `Examples/WendyNotify/Sources/WendyNotify/Generated/wendy_system_v1_notifications.pb.swift` and `Examples/WendyNotify/Sources/WendyNotify/Generated/wendy_system_v1_notifications.grpc.swift`.
- `Examples/ThermalNotify/regenerate-grpc.sh`: the `.pb.swift` and `.grpc.swift` files named `wendy_system_v1_notifications` and `wendy_agent_services_v1_wendy_agent_v1_video_service` directly beneath `Examples/ThermalNotify/Sources/ThermalNotify/Generated/`.

Review `.proto` definitions, generators, configuration, external schema/source pins and references, handwritten code, dependency lockfiles and generation/build checks. Unknown files, generated-looking names or headers elsewhere, renames crossing the source/output boundary, and meaningful file-mode changes remain in scope.

If an external source pin changes without the upstream source being supplied, state that upstream inspection is missing. Excluding output or passing generation checks does not establish API or behavior compatibility. Do not claim excluded output was reviewed or dismiss earlier blockers because output is excluded. For generated-only changes, state that no source change was reviewed and preserve unresolved findings.
