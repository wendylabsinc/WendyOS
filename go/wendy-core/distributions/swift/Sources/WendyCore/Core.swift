internal import CWendyCore

/// Returns 42 from Wendy Core, implemented in Go and linked statically.
public func WendyHelloWorld() -> Int32 {
    CWendyCore.WendyHelloWorld()
}

/// Returns the effective version of the linked Wendy Core engine.
public func WendyCoreVersion() -> String {
    String(cString: CWendyCore.WendyCoreVersion()!)
}
