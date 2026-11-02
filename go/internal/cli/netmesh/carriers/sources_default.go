package carriers

// Native sources are installed by the later host-discovery layer. Explicit
// seeds and injected Sources already use this same bounded candidate manager.
func defaultSources(Config) []Source { return nil }
