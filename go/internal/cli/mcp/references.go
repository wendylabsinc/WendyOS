package mcp

// AgentFacingTexts returns the text this package hands to agents, keyed by a
// label for error messages. The reference tests check that every tool, tool
// group and CLI flag named in it exists.
func AgentFacingTexts() map[string]string {
	return map[string]string{
		"mcp instructions":   serverInstructions,
		"relay instructions": relayInstructions,
		"guide":              guideText,
	}
}
