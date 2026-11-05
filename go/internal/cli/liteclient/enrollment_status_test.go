package liteclient

import (
	"testing"
	"time"
)

func TestEnrollmentStatusIsReadOnlyAndHonorsTimeout(t *testing.T) {
	client, device := connectedOverPipe(t)
	request := make(chan bool, 1)
	go func() {
		message, err := newDirectLink(device).recv(time.Second)
		request <- err == nil && message.GetCommand().GetEnrollmentChallenge().GetStatusOnly()
		// An old or busy device never sends a response.
	}()
	start := time.Now()
	if _, err := client.EnrollmentStatus(20 * time.Millisecond); err == nil {
		t.Fatal("expected enrollment status timeout")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("status ignored discovery timeout: %v", elapsed)
	}
	if !<-request {
		t.Fatal("discovery did not request read-only status")
	}
}
