#!/bin/bash

swiftly run swift build --product wendy-agent --swift-sdk aarch64-swift-linux-musl && .build/arm64-apple-macosx/debug/wendy device update --binary .build/aarch64-swift-linux-musl/debug/wendy-agent
