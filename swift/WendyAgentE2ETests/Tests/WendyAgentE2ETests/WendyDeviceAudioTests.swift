import Testing

@Suite(.serialized)
struct `wendy device audio` {
    @Test
    func `describes audio subcommands`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device audio list` {
    @Test
    func `lists audio devices on the selected device`() async throws {
        // TODO: implement.
    }

    @Test
    func `'--json' formats audio devices as JSON`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device audio listen` {
    @Test
    func `starts listening to the selected audio input`() async throws {
        // TODO: implement.
    }

    @Test
    func `fails clearly when the audio input is unavailable`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device audio monitor` {
    @Test
    func `streams audio level updates`() async throws {
        // TODO: implement.
    }

    @Test
    func `fails clearly when audio monitoring is unavailable`() async throws {
        // TODO: implement.
    }
}

// MARK: -

@Suite(.serialized)
struct `wendy device audio set-default` {
    @Test
    func `sets the default audio device`() async throws {
        // TODO: implement.
    }

    @Test
    func `fails clearly when the audio device is unknown`() async throws {
        // TODO: implement.
    }
}
