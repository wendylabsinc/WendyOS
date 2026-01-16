import Foundation

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

/// Prompt for password input with masked characters (shows * for each character)
func securePasswordPrompt(_ prompt: String) -> String {
    // Print prompt without newline
    print(prompt, terminator: "")
    fflush(stdout)

    #if os(Windows)
        // Windows implementation using Console API
        let handle = GetStdHandle(STD_INPUT_HANDLE)
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
            ReadConsoleW(handle, &char, 1, &charsRead, nil)

            if char == 13 || char == 10 {  // Enter
                break
            } else if char == 8 {  // Backspace
                if !password.isEmpty {
                    password.removeLast()
                    print("\u{8} \u{8}", terminator: "")
                    fflush(stdout)
                }
            } else {
                password.append(Character(UnicodeScalar(UInt16(char))!))
                print("*", terminator: "")
                fflush(stdout)
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
                    fflush(stdout)
                }
            } else {
                password.append(Character(UnicodeScalar(UInt8(char))))
                print("*", terminator: "")
                fflush(stdout)
            }
        }

        return password
    #endif
}
