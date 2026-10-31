package central

import (
	"errors"
	"net"
	"testing"
	"time"
)

func TestL2CAPReadDeadlineMayChangeConcurrently(t *testing.T) {
	stream := NewL2CAPStream(&Connection{})
	expired := time.Now().Add(-time.Second)
	if err := stream.SetReadDeadline(expired); err != nil {
		t.Fatal(err)
	}
	started, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		<-started
		for i := 0; i < 1000; i++ {
			// On Darwin, the native write setter reports the unopened handle;
			// the read deadline still updates without touching any hardware.
			_ = stream.SetDeadline(expired.Add(-time.Duration(i)))
		}
	}()
	close(started)
	for i := 0; i < 1000; i++ {
		_, err := stream.Read(make([]byte, 1))
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Errorf("read with expired deadline: %v", err)
			break
		}
	}
	<-done
}
