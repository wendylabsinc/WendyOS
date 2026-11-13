@main
struct LocationPermissionRequestTests {
    @MainActor
    static func main() async {
        await Self.submissionDoesNotCompleteTheWait()
        await Self.synchronousDecisionCompletesOnce()
        await Self.timeoutProvidesFallbackAndAllowsRetry()
        print("Location permission request: 3 regression tests passed")
    }

    @MainActor
    static func submissionDoesNotCompleteTheWait() async {
        let request = LocationPermissionRequest()
        var started = false
        var returned = false
        let task = Task { @MainActor in
            await request.wait(
                timeout: .seconds(60),
                onTimeout: { fatalError("A decision must cancel the timeout") },
                start: { started = true }
            )
            returned = true
        }
        while !started { await Task.yield() }
        // Submission and an unchanged/pending status are not a native decision.
        precondition(request.isWaiting && !returned)
        request.finish()
        request.finish()  // Delegate/app-activation races must not resume twice.
        await task.value
        precondition(returned && !request.isWaiting)
    }

    @MainActor
    static func synchronousDecisionCompletesOnce() async {
        let request = LocationPermissionRequest()
        await request.wait(
            timeout: .zero,
            onTimeout: { fatalError("An immediate decision must beat the timer") },
            start: { request.finish() }
        )
        await Task.yield()
        precondition(!request.isWaiting)
    }

    @MainActor
    static func timeoutProvidesFallbackAndAllowsRetry() async {
        let request = LocationPermissionRequest()
        var timedOut = false
        await request.wait(
            timeout: .zero,
            onTimeout: { timedOut = true },
            start: {}
        )
        precondition(timedOut && !request.isWaiting)
        request.finish()  // A late native result must be harmless.
        await request.wait(
            timeout: .seconds(60),
            onTimeout: { fatalError("A retry decision must cancel its timer") },
            start: { request.finish() }
        )
        precondition(!request.isWaiting)
    }
}
