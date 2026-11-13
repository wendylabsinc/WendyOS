package chat

import (
	"strings"
	"testing"
)

func TestSystemPromptDescribesConnectedTargetRun(t *testing.T) {
	prompt := SystemPrompt("/tmp/project", "")
	if strings.Contains(prompt, "manages cloud connection internally") {
		t.Fatal("system prompt still says run manages the cloud connection itself")
	}
	if !strings.Contains(prompt, "The run tool builds and deploys a local project to the connected device") {
		t.Fatal("system prompt does not describe run's connected-target behavior")
	}
}
