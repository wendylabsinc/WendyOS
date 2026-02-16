import Foundation
internal import Noora

#if os(macOS) || os(iOS) || os(tvOS) || os(watchOS)
    import Darwin
#elseif canImport(Glibc)
    @preconcurrency import Glibc
#elseif canImport(Musl)
    @preconcurrency import Musl
#elseif os(Windows)
    import ucrt
    import WinSDK
#endif

// Helper to flush stdout in Swift 6
@inline(__always)
private func flushStdout() {
    #if os(Linux)
        fflush(nil)
    #else
        fflush(stdout)
    #endif
}

/// Prompt for password input with styled rendering and masked characters
/// - Parameters:
///   - title: The title displayed above the prompt (e.g., "Enter WiFi password")
///   - prompt: The prompt label (e.g., "Password")
/// - Returns: The password entered by the user
/// - Throws: `CancellationError` if the user presses Ctrl+C
func CLIOutput_secureTextPrompt(title: String, prompt: String) throws -> String {
    // Print styled title
    print(title.bold)
    // Use the simple prompt with masking
    return try securePasswordPrompt("  \(prompt): ")
}

/// Prompt for password input with masked characters (shows * for each character)
private func securePasswordPrompt(_ prompt: String) throws -> String {
    // Print prompt without newline
    print(prompt, terminator: "")
    flushStdout()

    #if os(Windows)
        // Windows implementation using Console API
        let handle = GetStdHandle(STD_INPUT_HANDLE)

        // Check for invalid handle
        guard handle != INVALID_HANDLE_VALUE else {
            // Fall back to unmasked input
            print()
            return readLine() ?? ""
        }

        var oldMode: DWORD = 0
        GetConsoleMode(handle, &oldMode)
        // Disable echo and line input
        SetConsoleMode(handle, oldMode & ~(DWORD(ENABLE_ECHO_INPUT) | DWORD(ENABLE_LINE_INPUT)))

        defer {
            SetConsoleMode(handle, oldMode)
            print()  // Print newline after input
        }

        var password = ""
        while true {
            var char: WCHAR = 0
            var charsRead: DWORD = 0
            let readResult = ReadConsoleW(handle, &char, 1, &charsRead, nil)

            // Check for read failure or EOF
            if !readResult || charsRead == 0 {
                break
            }

            if char == 13 || char == 10 {  // Enter
                break
            } else if char == 3 {  // Ctrl+C
                throw CancellationError()
            } else if char == 8 {  // Backspace
                if !password.isEmpty {
                    password.removeLast()
                    print("\u{8} \u{8}", terminator: "")
                    flushStdout()
                }
            } else if let scalar = UnicodeScalar(UInt16(char)) {
                password.append(Character(scalar))
                print("*", terminator: "")
                flushStdout()
            }
        }

        return password
    #else
        // Unix implementation using termios
        var oldTermios = termios()
        tcgetattr(STDIN_FILENO, &oldTermios)

        var newTermios = oldTermios
        // Disable echo, canonical mode, and signal generation
        // ISIG disabled so Ctrl+C comes through as character (ASCII 3) instead of SIGINT
        newTermios.c_lflag &= ~tcflag_t(ECHO | ICANON | ISIG)
        tcsetattr(STDIN_FILENO, TCSANOW, &newTermios)

        defer {
            // Restore terminal settings
            tcsetattr(STDIN_FILENO, TCSANOW, &oldTermios)
            print()  // Print newline after input
        }

        var password = ""
        while true {
            let char = getchar()
            // 10 = newline (\n), 13 = carriage return (\r)
            if char == EOF || char == 10 || char == 13 || char == 4 {  // EOF, Enter, or Ctrl+D
                break
            } else if char == 3 {  // Ctrl+C (ETX)
                return ""
            } else if char == 127 || char == 8 {  // Backspace or Delete
                if !password.isEmpty {
                    password.removeLast()
                    // Move cursor back, print space, move back again
                    print("\u{8} \u{8}", terminator: "")
                    flushStdout()
                }
            } else if char >= 32 && char <= 126 {
                // Valid printable ASCII character
                password.append(Character(UnicodeScalar(UInt8(char))))
                print("*", terminator: "")
                flushStdout()
            }
            // Ignore other control characters and non-ASCII bytes
        }

        return password
    #endif
}
