package appconfig

import (
	"strings"
	"testing"
)

func TestUserAppIDsCannotUseTheModelPrefix(t *testing.T) {
	if err := ValidateUserAppID("sh.wendy.model.m-1a2b3c4d"); err == nil {
		t.Fatal("a user app took a model host's identity")
	}
	if err := ValidateUserAppID("sh.wendy.models-demo"); err != nil {
		t.Fatalf("an unrelated id was refused: %v", err)
	}
	// The agent's own model hosts still pass the plain syntax check.
	if err := ValidateAppID("sh.wendy.model.m-1a2b3c4d"); err != nil {
		t.Fatalf("ValidateAppID rejected a model host id: %v", err)
	}
	cfg := &AppConfig{AppID: "sh.wendy.model.x"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("Validate = %v, want the reservation", err)
	}
}
