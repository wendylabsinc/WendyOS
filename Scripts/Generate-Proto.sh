#!/bin/bash
set -e

# This script generates the WendyAgentGRPC Swift code using Swift Package Manager plugins.
# It should be run from the root of the project.

echo "Generating Swift gRPC code using Swift Package Manager plugins..."

# Clean existing generated code
rm -rf Sources/WendyAgentGRPC/Proto
mkdir -p Sources/WendyAgentGRPC/Proto

rm -rf Sources/WendyCloudGRPC/Proto
mkdir -p Sources/WendyCloudGRPC/Proto

rm -rf Sources/ContainerdGRPC/Proto  
mkdir -p Sources/ContainerdGRPC/Proto
rm -rf Sources/ContainerdGRPCTypes/Proto
mkdir -p Sources/ContainerdGRPCTypes/Proto
rm -rf Sources/OpenTelemetryGRPCTypes/Proto
mkdir -p Sources/OpenTelemetryGRPCTypes/Proto

# Use Swift Package Manager plugin for gRPC code generation
# Note: This uses the new grpc-swift-2 plugin system instead of protoc directly

echo "Generating WendyAgent gRPC code..."
swift package --allow-writing-to-package-directory generate-grpc-code-from-protos \
    --access-level public \
    --output-path Sources/WendyAgentGRPC/Proto \
    --import-path Proto \
    -- Proto/wendy/agent/services/v1/*.proto

echo "Generating WendyCloud gRPC code..."
swift package --allow-writing-to-package-directory generate-grpc-code-from-protos \
    --access-level public \
    --output-path Sources/WendyCloudGRPC/Proto \
    --import-path Proto \
    -- Proto/cloud/*.proto

echo "Generating Containerd gRPC code..."  
swift package --allow-writing-to-package-directory generate-grpc-code-from-protos \
    --access-level public \
    --no-servers \
    --output-path Sources/ContainerdGRPC/Proto \
    --import-path Proto \
    --import-path /opt/homebrew/include \
    -- $(find Proto/github.com/containerd/containerd/api/services -name "*.proto")

echo "Generating Containerd types..."
swift package --allow-writing-to-package-directory generate-grpc-code-from-protos \
    --access-level public \
    --no-servers \
    --no-clients \
    --output-path Sources/ContainerdGRPCTypes/Proto \
    --import-path Proto \
    --import-path /opt/homebrew/include \
    -- $(find Proto/github.com/containerd/containerd/api/types -name "*.proto") $(find Proto/google -name "*.proto")

echo "Generating OpenTelemetry gRPC code..."
swift package --allow-writing-to-package-directory generate-grpc-code-from-protos \
    --access-level public \
    --output-path Sources/OpenTelemetryGRPC/Proto \
    --import-path Proto \
    -- $(find Proto/opentelemetry/proto/collector/logs/v1 -name "*.proto") $(find Proto/opentelemetry/proto/collector/metrics/v1 -name "*.proto") $(find Proto/opentelemetry/proto/collector/trace/v1 -name "*.proto") $(find Proto/google -name "*.proto") $(find Proto/opentelemetry/proto/common -name "*.proto") $(find Proto/opentelemetry/proto/logs/v1 -name "*.proto") $(find Proto/opentelemetry/proto/metrics/v1 -name "*.proto") $(find Proto/opentelemetry/proto/trace/v1 -name "*.proto") $(find Proto/opentelemetry/proto/resource/v1 -name "*.proto")

echo "gRPC code generation complete!"

# Fix duplicate file name conflicts
echo "Fixing file name conflicts..."
if [ -f "Sources/ContainerdGRPC/Proto/github.com/containerd/containerd/api/services/ttrpc/events/v1/events.grpc.swift" ]; then
    mv Sources/ContainerdGRPC/Proto/github.com/containerd/containerd/api/services/ttrpc/events/v1/events.grpc.swift \
       Sources/ContainerdGRPC/Proto/github.com/containerd/containerd/api/services/ttrpc/events/v1/ttrpc_events.grpc.swift
fi

if [ -f "Sources/ContainerdGRPC/Proto/github.com/containerd/containerd/api/services/ttrpc/events/v1/events.pb.swift" ]; then
    mv Sources/ContainerdGRPC/Proto/github.com/containerd/containerd/api/services/ttrpc/events/v1/events.pb.swift \
       Sources/ContainerdGRPC/Proto/github.com/containerd/containerd/api/services/ttrpc/events/v1/ttrpc_events.pb.swift
fi

echo "File conflicts resolved!"
