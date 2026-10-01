package services

import "testing"

func TestParseAppConfigRefusesReservedModelIDs(t *testing.T) {
	if _, err := parseAppConfig([]byte(`{"appId":"sh.wendy.model.m-1a2b3c4d"}`)); err == nil {
		t.Fatal("the agent accepted a user app with a model host's identity")
	}
	if _, err := parseAppConfig([]byte(`{"appId":"com.example.app"}`)); err != nil {
		t.Fatalf("an ordinary app was refused: %v", err)
	}
}
