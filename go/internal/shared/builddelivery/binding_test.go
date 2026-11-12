package builddelivery

import (
	"strings"
	"testing"
	"time"
)

func TestBuildBindingAfterOperatorExpiryDoesNotAdmitNewJob(t *testing.T) {
	f := newFixture(t)
	compact := f.sign(t, f.claims)
	later := f.now.Add(20 * time.Minute)
	if _, err := Admit(compact, f.roots, f.target, later); err == nil {
		t.Fatal("late new admission accepted")
	}
	if _, err := VerifyBuildBinding(compact, sessionID(compact), f.claims.InputSHA256, f.roots, f.target, f.builder, later); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, session, input string
		now                  time.Time
	}{
		{"invented session", strings.Repeat("A", 43), f.claims.InputSHA256, later},
		{"changed input", sessionID(compact), strings.Repeat("B", 43), later},
		{"expired job", sessionID(compact), f.claims.InputSHA256, f.now.Add(2 * time.Hour)},
		{"future job", sessionID(compact), f.claims.InputSHA256, f.now.Add(-time.Second)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := VerifyBuildBinding(compact, tc.session, tc.input, f.roots, f.target, f.builder, tc.now); err == nil {
				t.Fatal("invalid assignment accepted")
			}
		})
	}
}
