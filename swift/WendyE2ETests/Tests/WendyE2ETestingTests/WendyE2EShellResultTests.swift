import Foundation
import Testing

@testable import WendyE2ETesting

@Suite
struct `shell result` {
    /**
     In JSON mode, which the CLI turns on by itself without a terminal, a
     failure is one stderr line: {"error":{"message":...,"next_steps":[...]}}.
     Tests read it as the text a person would see.
     */
    @Test
    func `reads a JSON error envelope as its message and steps`() {
        let stderr =
            "building...\n"
            + #"{"error":{"code":"cli_usage","exit":2,"message":"required flag(s) \"id\" not set","retryable":false,"next_steps":["Run 'wendy device audio set-default --help' for usage."]}}"#
            + "\n"
        let text = WendyE2EShellResult.readableErrorText(stderr)

        #expect(text.contains("required flag(s) \"id\" not set"))
        #expect(
            text
                == "building...\nrequired flag(s) \"id\" not set\n  Run 'wendy device audio set-default --help' for usage.\n"
        )
    }

    @Test
    func `leaves plain text and other JSON unchanged`() {
        let stderr = "✗ unknown flag: --bogus\n{\"progress\":1}\n{not json \"error\"\r\n"
        #expect(
            WendyE2EShellResult.readableErrorText(stderr)
                == "✗ unknown flag: --bogus\n{\"progress\":1}\n{not json \"error\"\n"
        )
    }
}
