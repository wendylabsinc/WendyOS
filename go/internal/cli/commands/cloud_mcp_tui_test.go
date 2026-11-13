package commands

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/creack/pty"
	"golang.org/x/term"
)

func applyPromptMessage(t *testing.T, m promptModel, msg tea.Msg) (promptModel, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	return next.(promptModel), cmd
}

func promptTestRequest(ctx context.Context) promptRequest {
	return promptRequest{ctx: ctx, question: "Allow this call?", reply: make(chan bool, 1), at: time.Now()}
}

func TestCloudMCPPromptChoices(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys []tea.KeyMsg
		want bool
	}{
		{"enter defaults to no", []tea.KeyMsg{{Type: tea.KeyEnter}}, false},
		{"y", []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune("y")}}, true},
		{"n", []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune("n")}}, false},
		{"left then enter", []tea.KeyMsg{{Type: tea.KeyLeft}, {Type: tea.KeyEnter}}, true},
		{"tab then enter", []tea.KeyMsg{{Type: tea.KeyTab}, {Type: tea.KeyEnter}}, true},
		{"left right enter", []tea.KeyMsg{{Type: tea.KeyLeft}, {Type: tea.KeyRight}, {Type: tea.KeyEnter}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := promptTestRequest(context.Background())
			m, _ := applyPromptMessage(t, promptModel{}, req)
			if view := m.View(); !strings.Contains(view, "Yes") || !strings.Contains(view, "No") {
				t.Fatalf("missing choices: %q", view)
			}
			for _, key := range tc.keys {
				var cmd tea.Cmd
				m, cmd = applyPromptMessage(t, m, promptKey{key: key, at: time.Now()})
				if cmd != nil {
					t.Fatal("answer exited the session")
				}
			}
			select {
			case got := <-req.reply:
				if got != tc.want {
					t.Fatalf("answer = %v, want %v", got, tc.want)
				}
			default:
				t.Fatal("answer was not delivered")
			}
			if m.View() != "" {
				t.Fatal("answered prompt remains visible")
			}
		})
	}
}

func TestCloudMCPPromptDiscardsIdleStaleAndPastedKeys(t *testing.T) {
	y := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}
	m, cmd := applyPromptMessage(t, promptModel{}, promptKey{key: y, at: time.Now()})
	if cmd != nil || m.View() != "" {
		t.Fatal("idle input changed the session")
	}
	req := promptTestRequest(context.Background())
	m, _ = applyPromptMessage(t, m, req)
	m, _ = applyPromptMessage(t, m, promptKey{key: y, at: req.at.Add(-time.Second)})
	y.Paste = true
	m, _ = applyPromptMessage(t, m, promptKey{key: y, at: time.Now()})
	select {
	case answer := <-req.reply:
		t.Fatalf("old or pasted input answered %v", answer)
	default:
	}
	m, _ = applyPromptMessage(t, m, promptKey{key: tea.KeyMsg{Type: tea.KeyEnter}, at: time.Now()})
	if answer := <-req.reply; answer {
		t.Fatal("ignored input changed the default answer")
	}
}

func TestCloudMCPPromptCancellation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		active bool
		key    tea.KeyMsg
	}{
		{"ctrl-c while idle", false, tea.KeyMsg{Type: tea.KeyCtrlC}},
		{"ctrl-c during prompt", true, tea.KeyMsg{Type: tea.KeyCtrlC}},
		{"q during prompt", true, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := promptModel{}
			req := promptTestRequest(context.Background())
			if tc.active {
				m, _ = applyPromptMessage(t, m, req)
			}
			m, cmd := applyPromptMessage(t, m, promptKey{key: tc.key, at: time.Now()})
			if cmd == nil {
				t.Fatal("cancellation did not stop the session")
			}
			if _, ok := cmd().(tea.QuitMsg); !ok {
				t.Fatal("cancellation did not return Quit")
			}
			if m.View() != "" {
				t.Fatal("cancelled prompt remains visible")
			}
			select {
			case answer := <-req.reply:
				t.Fatalf("cancelled prompt answered %v", answer)
			default:
			}
		})
	}
}

func TestCloudMCPPromptCancelledRequestCannotApprove(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	req := promptTestRequest(ctx)
	m, _ := applyPromptMessage(t, promptModel{}, req)
	cancel()
	m, cmd := applyPromptMessage(t, m, promptKey{key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}, at: time.Now()})
	if <-req.reply || cmd != nil || m.View() != "" {
		t.Fatal("cancelled request approved, exited the session, or kept its prompt")
	}
}

func TestCloudMCPPromptTerminalStopsSessionOnCtrlC(t *testing.T) {
	term := newPromptTerminal(context.Background(), nil, io.Discard)
	t.Cleanup(func() { _ = term.close() })
	term.program.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	select {
	case <-term.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("Ctrl-C did not cancel the session context")
	}
	if err := term.close(); err != nil {
		t.Fatalf("close = %v", err)
	}
}

func TestCloudMCPPromptReadBatchCannotApproveAnotherQuestion(t *testing.T) {
	first := promptTestRequest(context.Background())
	m, _ := applyPromptMessage(t, promptModel{}, first)
	m, _ = applyPromptMessage(t, m, promptInputRead{at: time.Now()})
	y := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}
	m, _ = applyPromptMessage(t, m, promptKey{key: y, at: m.inputAt})
	if !<-first.reply {
		t.Fatal("first key did not approve the first question")
	}
	second := promptTestRequest(context.Background())
	m, _ = applyPromptMessage(t, m, second)
	m, _ = applyPromptMessage(t, m, promptKey{key: y, at: m.inputAt})
	select {
	case answer := <-second.reply:
		t.Fatalf("leftover key from the first read answered the next question: %v", answer)
	default:
	}
	m, _ = applyPromptMessage(t, m, promptInputRead{at: time.Now()})
	_, _ = applyPromptMessage(t, m, promptKey{key: y, at: m.inputAt})
	if !<-second.reply {
		t.Fatal("fresh input did not approve the second question")
	}
}

func TestCloudMCPPromptTerminalClosedInputStopsSession(t *testing.T) {
	term := newPromptTerminal(context.Background(), strings.NewReader(""), io.Discard)
	t.Cleanup(func() { _ = term.close() })
	select {
	case <-term.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("closed input left the session running")
	}
	if term.ask(context.Background(), "Allow?") {
		t.Fatal("closed input approved a request")
	}
	if err := term.close(); err != nil {
		t.Fatalf("close = %v", err)
	}
}

type promptFailingReader struct{ err error }

func (r promptFailingReader) Read([]byte) (int, error) { return 0, r.err }

func TestCloudMCPPromptTerminalReturnsInputError(t *testing.T) {
	want := errors.New("terminal read failed")
	term := newPromptTerminal(context.Background(), promptFailingReader{err: want}, io.Discard)
	t.Cleanup(func() { _ = term.close() })
	select {
	case <-term.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("input failure left the session running")
	}
	if err := term.close(); !errors.Is(err, want) {
		t.Fatalf("close = %v, want %v", err, want)
	}
}

type promptQuestionOutput struct{ seen chan string }

func (w *promptQuestionOutput) Write(p []byte) (int, error) {
	for _, question := range []string{"First request?", "Second request?"} {
		if strings.Contains(string(p), question) {
			select {
			case w.seen <- question:
			default:
			}
		}
	}
	return len(p), nil
}

func TestCloudMCPPromptTerminalDiscardsExtraKeysBetweenRequests(t *testing.T) {
	in, keyboard := io.Pipe()
	out := &promptQuestionOutput{seen: make(chan string, 8)}
	term := newPromptTerminal(context.Background(), in, out)
	t.Cleanup(func() {
		_ = keyboard.Close()
		_ = in.Close()
		_ = term.close()
	})
	answers := make(chan bool, 2)
	go func() {
		answers <- term.ask(context.Background(), "First request?")
		answers <- term.ask(context.Background(), "Second request?")
	}()
	waitQuestion := func(want string) {
		t.Helper()
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		for {
			select {
			case got := <-out.seen:
				if got == want {
					return
				}
			case <-timer.C:
				t.Fatalf("question %q was not displayed", want)
			}
		}
	}
	waitAnswer := func(want bool) {
		t.Helper()
		select {
		case got := <-answers:
			if got != want {
				t.Fatalf("answer = %v, want %v", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("prompt did not answer")
		}
	}
	waitQuestion("First request?")
	if _, err := keyboard.Write([]byte("y\ry")); err != nil {
		t.Fatal(err)
	}
	waitAnswer(true)
	waitQuestion("Second request?")
	select {
	case got := <-answers:
		t.Fatalf("extra input answered the next request: %v", got)
	default:
	}
	if _, err := keyboard.Write([]byte("n")); err != nil {
		t.Fatal(err)
	}
	waitAnswer(false)
}

func TestCloudMCPPromptTerminalRestoresRawTerminal(t *testing.T) {
	keyboard, tty, err := pty.Open()
	if errors.Is(err, pty.ErrUnsupported) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer keyboard.Close()
	defer tty.Close()
	before, err := term.GetState(int(tty.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	out := &promptQuestionOutput{seen: make(chan string, 8)}
	session := newPromptTerminal(context.Background(), tty, out)
	t.Cleanup(func() { _ = session.close() })
	answer := make(chan bool, 1)
	go func() { answer <- session.ask(context.Background(), "First request?") }()
	select {
	case <-out.seen:
	case <-time.After(time.Second):
		t.Fatal("question was not displayed")
	}
	if _, err := keyboard.Write([]byte("y")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-answer:
		if !got {
			t.Fatal("single y key did not approve")
		}
	case <-time.After(time.Second):
		t.Fatal("terminal waited for Enter after y")
	}
	if _, err := keyboard.Write([]byte{3}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("Ctrl-C did not end the session")
	}
	if err := session.close(); err != nil {
		t.Fatal(err)
	}
	after, err := term.GetState(int(tty.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("terminal settings were not restored")
	}
}
