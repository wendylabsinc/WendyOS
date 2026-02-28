// Example app that crashes on launch to test swift-backtrace functionality

func crash() -> Never {
    fatalError("Intentional crash to test backtraces")
}

print("About to crash...")
crash()
