package terminaloutput

import (
	"bytes"
	"fmt"
	"log"
	"log/slog"
	"os"
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

func TestBoundedQueueKeepsTheNewestWritesWithoutGaps(t *testing.T) {
	var dst bytes.Buffer
	previous := os.Stderr
	notice, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = notice
	t.Cleanup(func() { os.Stderr = previous })

	release := Suspend()
	t.Cleanup(release)
	w := Writer(&dst)
	chunk := strings.Repeat("x", 1023)
	for i := range 80 {
		fmt.Fprintf(w, "%02d%s\n", i, chunk[2:])
	}
	_, _ = w.Write(bytes.Repeat([]byte("y"), maxPendingBytes+1)) // evicts everything, itself included
	fmt.Fprintln(w, "final")
	release()

	if dst.String() != "final\n" {
		t.Fatalf("replayed %d bytes, want only the write after the oversized one", dst.Len())
	}
	dst.Reset()
	release = Suspend()
	for i := range 80 {
		fmt.Fprintf(w, "%02d%s\n", i, chunk[2:])
	}
	release()
	replayed := dst.String()
	if dst.Len() > maxPendingBytes || !strings.HasPrefix(replayed, "16") || !strings.Contains(replayed, "\n79") {
		t.Fatalf("want a contiguous tail 16..79, got %d bytes starting %q", dst.Len(), replayed[:2])
	}
	got, err := os.ReadFile(notice.Name())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"omitted 81 earlier background diagnostic writes", "omitted 16 earlier background diagnostic writes (16384 bytes)"} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("notice %q missing from %q", want, got)
		}
	}
}

func TestInlineViewPrintsStderrDiagnosticsUntilAFullScreenViewStarts(t *testing.T) {
	previous := os.Stderr
	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = stderr
	t.Cleanup(func() { os.Stderr = previous })
	var printed []string
	var stdout bytes.Buffer
	release := SuspendInline(func(p []byte) bool {
		printed = append(printed, string(p))
		return true
	})
	t.Cleanup(func() { release(nil) })

	fmt.Fprint(Stderr, "live\n")
	fmt.Fprint(Writer(&stdout), "stdout waits\n")
	picker := Suspend()
	fmt.Fprint(Stderr, "under picker\n")
	if strings.Join(printed, "") != "live\n" {
		t.Fatalf("printed %q, want only the write made before the picker", printed)
	}
	picker()
	if strings.Join(printed, "") != "live\nunder picker\n" {
		t.Fatalf("picker exit did not hand its stderr writes to the inline view: %q", printed)
	}
	if stdout.Len() != 0 {
		t.Fatal("stdout write escaped while the inline view was active")
	}
	release([][]byte{[]byte("unprinted\n")})
	got, _ := os.ReadFile(stderr.Name())
	if string(got) != "unprinted\n" || stdout.String() != "stdout waits\n" {
		t.Fatalf("exit replay: stderr %q stdout %q", got, stdout.String())
	}
}

func TestInlineViewRefusalDefersInOrder(t *testing.T) {
	previous := os.Stderr
	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = stderr
	t.Cleanup(func() { os.Stderr = previous })
	release := SuspendInline(func([]byte) bool { return false })
	fmt.Fprint(Stderr, "refused\n")
	release([][]byte{[]byte("older\n")})
	got, _ := os.ReadFile(stderr.Name())
	if string(got) != "older\nrefused\n" {
		t.Fatalf("stderr = %q", got)
	}
}
