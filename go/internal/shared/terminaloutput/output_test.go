package terminaloutput

import (
	"bytes"
	"fmt"
	"log"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

func TestSuspendDefersDiagnosticsUntilLastOwnerExits(t *testing.T) {
	var stderr, stdout bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&stderr)
	t.Cleanup(func() { log.SetOutput(previous) })
	outer := Suspend()
	t.Cleanup(outer)
	inner := Suspend()
	t.Cleanup(inner)
	fmt.Fprintln(Writer(&stdout), "background stdout")
	log.Print("discovery backend stopped")
	// This is the default slog path used by meshalpn.Recovery.failed.
	slog.Warn("TLS issuer hints paused after issuer path failure", "duration", "2m0s")
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatal("diagnostics escaped into the live frame")
	}
	outer()
	outer() // release is idempotent
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatal("diagnostics escaped while another view was active")
	}
	inner()
	if stdout.String() != "background stdout\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	for _, want := range []string{"discovery backend stopped", "TLS issuer hints paused"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("missing deferred diagnostic %q: %s", want, &stderr)
		}
	}
	if log.Writer() != &stderr {
		t.Fatal("default logger was not restored")
	}
	fmt.Fprint(Writer(&stdout), "after")
	if !strings.HasSuffix(stdout.String(), "after") {
		t.Fatal("output did not resume")
	}
}

func TestConcurrentDiagnosticsAreBounded(t *testing.T) {
	var dst bytes.Buffer
	release := Suspend()
	t.Cleanup(release)
	w := Writer(&dst)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				_, _ = w.Write(bytes.Repeat([]byte("x"), 1024))
			}
		})
	}
	wg.Wait()
	if dst.Len() != 0 {
		t.Fatal("concurrent writes escaped")
	}
	if output.bytes > maxPendingBytes || output.dropped == 0 {
		t.Fatal("diagnostic queue was not bounded")
	}
	release()
	if dst.Len() > maxPendingBytes {
		t.Fatal("replayed more than the bounded queue")
	}
}
