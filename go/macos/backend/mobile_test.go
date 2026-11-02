package backend

import (
	"context"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
)

func TestMobileBackgroundCancelsOperationBeforeDrainingDeviceState(t *testing.T) {
	b := New()
	started, finished := make(chan struct{}), make(chan struct{})
	go func() {
		ctx, done := b.begin()
		defer close(finished)
		defer done()
		close(started)
		<-ctx.Done()
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	paused := make(chan error, 1)
	go func() { paused <- b.SetMobileState(false, "", "") }()
	select {
	case err := <-paused:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("suspension waited on an uncancelled operation")
	}
	<-finished
	if b.session != 1 {
		t.Fatal("background did not invalidate device handles")
	}
}

func TestMobileClosesDeviceTransportBeforeStoppingRouter(t *testing.T) {
	if os.Getenv("WENDY_TEST_MOBILE_CLOSE_ORDER") != "1" {
		// Embedded IPC is process-wide; isolate it from desktop backend tests.
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, executable, "-test.run=^TestMobileClosesDeviceTransportBeforeStoppingRouter$")
		command.Env = append(os.Environ(), "WENDY_TEST_MOBILE_CLOSE_ORDER=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("embedded transport shutdown: %v\n%s", err, output)
		}
		return
	}
	sessionbroker.EnableEmbeddedHost()
	for _, shutdown := range []bool{false, true} {
		if err := sessionbroker.SetEmbeddedActive(context.Background(), true); err != nil {
			t.Fatal(err)
		}
		b := New()
		closed := false
		b.conn = &grpcclient.AgentConnection{ExtraClosers: []io.Closer{mobileTransportCloser(func() error {
			closed = true
			if active, _ := sessionbroker.EmbeddedStatus(); !active {
				t.Error("device transport closed after its mesh packet carrier stopped")
			}
			return nil
		})}}
		var err error
		if shutdown {
			err = b.Shutdown()
		} else {
			err = b.SetMobileState(false, "", "")
		}
		if err != nil {
			t.Fatal(err)
		}
		if !closed {
			t.Fatal("device transport remained open")
		}
		if active, _ := sessionbroker.EmbeddedStatus(); active {
			t.Fatal("mesh participant remained active after suspension")
		}
	}
}

type mobileTransportCloser func() error

func (close mobileTransportCloser) Close() error { return close() }
