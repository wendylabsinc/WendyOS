#!/usr/bin/env bash
# Run from this directory. Generated files are checked in; protoc is dev-only.
set -eu
cd "$(dirname "$0")"
swift package resolve
swift build --package-path .build/checkouts/swift-protobuf --product protoc-gen-swift
swift build --package-path .build/checkouts/grpc-swift-protobuf --product protoc-gen-grpc-swift
SWIFTGEN="$(swift build --package-path .build/checkouts/swift-protobuf --show-bin-path)/protoc-gen-swift"
GRPCGEN="$(swift build --package-path .build/checkouts/grpc-swift-protobuf --show-bin-path)/protoc-gen-grpc-swift"
source ../../Proto/shared-grpc-api-contracts.sh
SHARED_PROTO_DIR="$(mktemp -d)"
trap 'rm -rf "$SHARED_PROTO_DIR"' EXIT
buf export "$SHARED_GRPC_API_CONTRACTS_INPUT" \
  --path wendy/system/v1/notifications.proto \
  --path wendy/agent/services/v1/wendy_agent_v1_video_service.proto \
  --output "$SHARED_PROTO_DIR"
protoc --proto_path="$SHARED_PROTO_DIR" --proto_path=.build/checkouts/swift-protobuf/Protos/Sources/SwiftProtobuf \
  --plugin="protoc-gen-swift=$SWIFTGEN" --plugin="protoc-gen-grpc-swift=$GRPCGEN" \
  --swift_out=Sources/ThermalNotify/Generated --swift_opt=Visibility=Internal,FileNaming=PathToUnderscores \
  --grpc-swift_out=Sources/ThermalNotify/Generated --grpc-swift_opt=Visibility=Internal,Client=true,Server=false,FileNaming=PathToUnderscores \
  wendy/system/v1/notifications.proto \
  wendy/agent/services/v1/wendy_agent_v1_video_service.proto
