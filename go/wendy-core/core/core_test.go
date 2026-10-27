package core

import "testing"

func TestWendyHelloWorld(t *testing.T) {
	if value := WendyHelloWorld(); value != 42 {
		t.Fatalf("WendyHelloWorld() = %d, want 42", value)
	}
}
