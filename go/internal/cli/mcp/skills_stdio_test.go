package mcp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/mark3labs/mcp-go/server"
)

type skillStdioReadFunc func([]byte) (int, error)

func (f skillStdioReadFunc) Read(p []byte) (int, error) { return f(p) }

func TestSkillStdioReaderPreservesInputErrorDuringCancellation(t *testing.T) {
	inputFailure := errors.New("stdin read failed")
	for _, tc := range []struct {
		name               string
		inputErr           error
		cancelWhileReading bool
	}{
		{"EOF cancels serving", io.EOF, false},
		{"unexpected failure", inputFailure, false},
		{"unexpected failure races cancellation", inputFailure, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			input := &cancelOnEOF{cancel: cancel, reader: skillStdioReadFunc(func([]byte) (int, error) {
				if tc.cancelWhileReading {
					cancel()
				}
				return 0, tc.inputErr
			})}
			reader := &skillStdioReader{ctx: ctx, in: bufio.NewReader(input)}
			if n, err := reader.Read(make([]byte, 1)); n != 0 || !errors.Is(err, tc.inputErr) {
				t.Fatalf("input error replaced during cancellation: n=%d err=%v want=%v", n, err, tc.inputErr)
			}
			if input.closed.Load() != (tc.inputErr == io.EOF) {
				t.Fatal("only EOF should mark a clean client disconnect")
			}
			if !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatalf("terminal input error did not cancel serving: %v", ctx.Err())
			}
		})
	}
}

func TestSkillStdioReaderHonorsCancellationBeforeReading(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	input := skillStdioReadFunc(func([]byte) (int, error) {
		t.Fatal("read input after serving was canceled")
		return 0, io.EOF
	})
	reader := &skillStdioReader{ctx: ctx, in: bufio.NewReader(input)}
	if _, err := reader.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestServeStdioPreservesUnexpectedInputFailure(t *testing.T) {
	want := errors.New("stdin read failed")
	input := skillStdioReadFunc(func([]byte) (int, error) { return 0, want })
	srv := server.NewMCPServer("wendy", "test")
	if err := serveStdioStreamsWithSkills(context.Background(), srv, input, io.Discard, nil); !errors.Is(err, want) {
		t.Fatalf("unexpected input failure became clean shutdown: %v", err)
	}
}
