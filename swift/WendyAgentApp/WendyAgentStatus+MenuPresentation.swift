import WendyAgent

internal extension WendyAgentStatus {
    var menuTitle: String {
        switch self {
        case .idle:
            "Idle"
        case .starting:
            "Starting"
        case .running:
            "Running"
        case .stopping:
            "Stopping"
        case .stopped:
            "Stopped"
        case .failed:
            "Failed"
        }
    }

    var menuImageName: String {
        switch self {
        case .running:
            "MenuStatusRunning"
        case .starting, .stopping:
            "MenuStatusTransition"
        case .failed:
            "MenuStatusFailed"
        case .idle, .stopped:
            "MenuStatusIdle"
        }
    }

    var menuFailureDetails: [String] {
        guard case .failed(let message) = self else { return [] }
        return Self.wrapMenuMessage(message, lineLength: 42, maxLines: 3)
    }

    private static func wrapMenuMessage(
        _ message: String,
        lineLength: Int,
        maxLines: Int
    ) -> [String] {
        let words = message.split { $0.isWhitespace || $0.isNewline }
        guard !words.isEmpty else { return ["WendyAgent failed."] }

        var lines: [String] = []
        var currentLine = ""
        var index = words.startIndex

        while index < words.endIndex {
            let wordText = String(words[index])
            let candidate = currentLine.isEmpty ? wordText : currentLine + " " + wordText

            if candidate.count <= lineLength || currentLine.isEmpty {
                currentLine = candidate
                words.formIndex(after: &index)
                continue
            }

            lines.append(currentLine)
            if lines.count == maxLines {
                lines[maxLines - 1] += "…"
                return lines
            }

            currentLine = ""
        }

        if !currentLine.isEmpty {
            lines.append(currentLine)
        }

        guard lines.count > maxLines else { return lines }

        var truncated = Array(lines.prefix(maxLines))
        truncated[maxLines - 1] += "…"
        return truncated
    }
}
