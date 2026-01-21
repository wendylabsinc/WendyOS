#!/bin/bash
set -e

swiftly run swift build --product wendy-agent --swift-sdk aarch64-swift-linux-musl
