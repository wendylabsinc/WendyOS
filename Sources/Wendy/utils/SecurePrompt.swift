import Foundation
import Noora

#if os(macOS) || os(iOS) || os(tvOS) || os(watchOS)
    import Darwin
#elseif canImport(Glibc)
    import Glibc
#elseif canImport(Musl)
    import Musl
#elseif os(Windows)
    import ucrt
    import WinSDK
#endif

/// Flush stdout - wrapped to handle Swift 6 strict concurrency
@inline(__always)
private func flushStdout() {
    // stdout is a global mutable variable, but fflush is thread-safe
    // and we're doing synchronous terminal I/O
    nonisolated(unsafe) let out = stdout
    fflush(out)
}

/// Prompt for password input with Noora-style rendering and masked characters
/// - Parameters:
///   - title: The title displayed above the prompt (e.g., "Enter WiFi password")
///   - prompt: The prompt label (e.g., "Password")
/// - Returns: The password entered by the user
func secureTextPrompt(title: String, prompt: String) -> String {
    // Print styled title
    print(title.bold)
    // Use the simple prompt with masking
    return securePasswordPrompt("  \(prompt): ")
}

/// Prompt for password input with masked characters (shows * for each character)
func securePasswordPrompt(_ prompt: String) -> String {
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
            if readResult == 0 || charsRead == 0 {
                break
            }

            if char == 13 || char == 10 {  // Enter
                break
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
        newTermios.c_lflag &= ~tcflag_t(ECHO | ICANON)
        tcsetattr(STDIN_FILENO, TCSANOW, &newTermios)

        defer {
            // Restore terminal settings
            tcsetattr(STDIN_FILENO, TCSANOW, &oldTermios)
            print()  // Print newline after input
        }

        var password = ""
        while true {
            let char = getchar()
            if char == EOF || char == Int32(Character("\n").asciiValue!)
                || char == Int32(Character("\r").asciiValue!)
            {
                break
            } else if char == 127 || char == 8 {  // Backspace or Delete
                if !password.isEmpty {
                    password.removeLast()
                    // Move cursor back, print space, move back again
                    print("\u{8} \u{8}", terminator: "")
                    flushStdout()
                }
            } else if char >= 0 && char <= 127 {
                // Valid ASCII character
                password.append(Character(UnicodeScalar(UInt8(char))))
                print("*", terminator: "")
                flushStdout()
            }
            // Ignore non-ASCII bytes (could be part of UTF-8 sequence)
        }

        return password
    #endif
}
