package main

import "testing"

func TestCommentLabel(t *testing.T) {
	comment := "Summary. Binding pattern: async-unary. Ownership: inputs are borrowed; outputs are owned. Availability: 0.1.0-draft."

	tests := map[string]string{
		"Binding pattern": "async-unary",
		"Ownership":       "inputs are borrowed; outputs are owned",
		"Availability":    "0.1.0-draft",
		"Missing":         "",
	}
	for label, expected := range tests {
		t.Run(label, func(t *testing.T) {
			if actual := commentLabel(comment, label); actual != expected {
				t.Fatalf("commentLabel(%q) = %q, want %q", label, actual, expected)
			}
		})
	}
}
