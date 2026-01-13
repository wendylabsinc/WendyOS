#!/bin/bash

swiftly run swift build --scratch-path .agent-build --product wendy-agent --swift-sdk aarch64-swift-linux-musl && wendy device update --binary .agent-build/aarch64-swift-linux-musl/debug/wendy-agent
