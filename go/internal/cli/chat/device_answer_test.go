package chat

import (
	"strings"
	"testing"
)

func TestAnswerStreamHidesThinkingAtEveryChunkBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		implicit          bool
	}{
		{"explicit", "<think>private reasoning</think>Hello!", "Hello!", false},
		{"implicit Qwen", "private reasoning</think>Hello!", "Hello!", true},
		{"plain Qwen", "Hello!", "Hello!", true},
		{"plain", "Hello! 2 < 3", "Hello! 2 < 3", false},
		{"multiple", "Hello<think>private reasoning</think> there<think>more reasoning</think>!", "Hello there!", false},
		{"unfinished", "<think>private reasoning", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for split := 0; split <= len(tc.input); split++ {
				var streamed strings.Builder
				filter := answerStream{implicit: tc.implicit, emit: func(text string) { streamed.WriteString(text) }}
				filter.append(tc.input[:split])
				filter.append(tc.input[split:])
				filter.finish(true)
				if got := streamed.String(); got != tc.want {
					t.Fatalf("split %d: %q, want %q", split, got, tc.want)
				}
				if filter.visible.String() != tc.want {
					t.Fatal("history differed from visible answer")
				}
			}
		})
	}
}
func TestAnswerStreamCancellationDoesNotRevealImplicitReasoning(t *testing.T) {
	filter := answerStream{implicit: true, emit: func(string) { t.Fatal("reasoning was emitted") }}
	filter.append("private reasoning")
	filter.finish(false)
	if filter.visible.Len() != 0 {
		t.Fatal("reasoning entered history")
	}
}
