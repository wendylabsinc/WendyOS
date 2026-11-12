package tui

import (
	"bytes"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// syncBuffer is a bytes.Buffer safe for Bubble Tea's renderer goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestPromptOutputDefaultsToStdout(t *testing.T) {
	if w, ok := PromptOutput(); ok {
		t.Fatalf("PromptOutput() = %v, true; want unset", w)
	}
	if got := PromptWriter(); got != os.Stdout {
		t.Fatalf("PromptWriter() = %v; want os.Stdout", got)
	}
}

func TestSetPromptOutputRestoresPrevious(t *testing.T) {
	var first, second bytes.Buffer
	restoreFirst := SetPromptOutput(&first)
	restoreSecond := SetPromptOutput(&second)
	if got := PromptWriter(); got != &second {
		t.Fatalf("PromptWriter() = %v; want the second writer", got)
	}
	restoreSecond()
	if got := PromptWriter(); got != &first {
		t.Fatalf("after restore PromptWriter() = %v; want the first writer", got)
	}
	restoreFirst()
	if _, ok := PromptOutput(); ok {
		t.Fatal("prompt output still set after both restores")
	}
}

func TestConfirmDrawsOnPromptOutput(t *testing.T) {
	var out syncBuffer
	t.Cleanup(SetPromptOutput(&out))

	ok, err := Confirm("Erase the drive?", tea.WithInput(strings.NewReader("y")))
	if err != nil || !ok {
		t.Fatalf("Confirm() = %v, %v; want true, nil", ok, err)
	}
	// The prompt quits before a frame is painted; its terminal setup still lands here.
	if out.String() == "" {
		t.Fatal("nothing was drawn on the prompt output")
	}
}

func TestPromptTextDrawsOnPromptOutput(t *testing.T) {
	var out syncBuffer
	t.Cleanup(SetPromptOutput(&out))

	got, err := PromptText("Device name", "", nil, tea.WithInput(strings.NewReader("lab\r")))
	if err != nil || got != "lab" {
		t.Fatalf("PromptText() = %q, %v; want lab, nil", got, err)
	}
	if out.String() == "" {
		t.Fatal("nothing was drawn on the prompt output")
	}
}

func TestExplicitOutputOverridesPromptOutput(t *testing.T) {
	var out syncBuffer
	t.Cleanup(SetPromptOutput(&out))

	if _, err := ConfirmWithIO("Erase the drive?", strings.NewReader("n")); err != nil {
		t.Fatalf("ConfirmWithIO() error = %v", err)
	}
	if out.String() != "" {
		t.Fatalf("prompt output = %q; want nothing when the caller passes its own output", out.String())
	}
}

func TestNewProgressProgramFollowsPromptOutput(t *testing.T) {
	var out syncBuffer
	t.Cleanup(SetPromptOutput(&out))

	p := NewProgressProgram(quitOnceModel{}, tea.WithInput(strings.NewReader("")))
	if _, err := p.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "progress frame") {
		t.Fatalf("prompt output = %q; want the progress frame", out.String())
	}
}

func TestPromptOutputConcurrentAccess(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			SetPromptOutput(io.Discard)()
		}()
		go func() {
			defer wg.Done()
			_ = PromptWriter()
		}()
	}
	wg.Wait()
}
