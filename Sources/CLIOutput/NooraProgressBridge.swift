internal import Noora

/// Bridge between CLIOutput.ProgressBarUpdate and Noora.ProgressBarUpdate.
/// This file isolates the type disambiguation needed when both modules define ProgressBarUpdate.
///
/// We use the Double-based progressBarStep overload and handle detail updates
/// through a separate mechanism, since the module name `Noora` collides with the
/// `Noora` class name making `Noora.ProgressBarUpdate` unresolvable.
func _withLabeledProgressBarImpl<T: Sendable>(
    message: String,
    operation: @escaping @Sendable (@escaping (ProgressBarUpdate) -> Void) async throws -> T
) async throws -> T {
    try await noora.progressBarStep(message: message) { updateDouble in
        try await operation { update in
            updateDouble(update.progress)
        }
    }
}
