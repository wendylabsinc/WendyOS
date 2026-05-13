import Testing

@Suite
struct `'wendy os install'` {
    /**
     Displays usage for `wendy os install`. The output includes the command
     synopsis, local flags, inherited global flags, and concise
     descriptions. Help exits successfully, writes to stdout, emits no
     stderr, and leaves configuration, cache, project, cloud, and device
     state untouched.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `prints command help`() async throws {
        // TODO: implement.
    }

    /**
     Writes the selected WendyOS image or firmware to the target drive
     after the user confirms the destructive operation. Output reports
     progress and final device preparation status.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `installs an image to a selected removable drive`() async throws {
        // TODO: implement.
    }

    /**
     When image path, drive id, and `--force` are provided, skips
     interactive pickers and confirmation prompts while preserving the
     same safety checks for drive identity.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `runs non-interactively with image, drive, and force`() async throws {
        // TODO: implement.
    }

    /**
     Internal or non-removable drives are protected. Non-interactive
     installs require the dedicated overwrite flag before any bytes are
     written.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `refuses to overwrite internal drives without explicit consent`() async throws {
        // TODO: implement.
    }

    /**
     WiFi flags and device-name flags are written into first-boot
     configuration for the image. Invalid WiFi definitions fail before
     the target drive is modified.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `preseeds WiFi and device identity when requested`() async throws {
        // TODO: implement.
    }

    /**
     `--pre-enroll` uses the stored auth session to add enrollment data
     to the image. Missing or expired auth fails before writing the
     drive.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `pre-enrolls only with valid cloud authentication`() async throws {
        // TODO: implement.
    }

    /**
     With `--json`, emits structured drive, image, version, and outcome
     metadata. Progress output does not corrupt stdout JSON.
     */
    @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
    func `prints JSON install result for automation`() async throws {
        // TODO: implement.
    }
}
