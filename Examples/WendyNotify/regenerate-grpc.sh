#!/bin/sh
# Run from this directory. Generated files are checked in; protoc is dev-only.
set -eu
cd "$(dirname "$0")"
swift package resolve
swift build --package-path .build/checkouts/swift-protobuf --product protoc-gen-swift
swift build --package-path .build/checkouts/grpc-swift-protobuf --product protoc-gen-grpc-swift
SWIFTGEN="$(swift build --package-path .build/checkouts/swift-protobuf --show-bin-path)/protoc-gen-swift"
GRPCGEN="$(swift build --package-path .build/checkouts/grpc-swift-protobuf --show-bin-path)/protoc-gen-grpc-swift"
protoc --proto_path=../../Proto --proto_path=.build/checkouts/swift-protobuf/Protos/Sources/SwiftProtobuf \
  --plugin="protoc-gen-swift=$SWIFTGEN" --plugin="protoc-gen-grpc-swift=$GRPCGEN" \
  --swift_out=Sources/WendyNotify/Generated --swift_opt=Visibility=Internal,FileNaming=PathToUnderscores \
  --grpc-swift_out=Sources/WendyNotify/Generated --grpc-swift_opt=Visibility=Internal,Client=true,Server=false,FileNaming=PathToUnderscores \
  wendy/system/v1/notifications.proto
