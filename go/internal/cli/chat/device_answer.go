package chat

import "strings"

// Some GGUF templates open a thinking block in the prompt, so generated content
// starts inside it and contains only the closing tag. Hold that preamble until
// the closing tag or a completed untagged answer. Never emit partial tag bytes.
type answerStream struct {
	implicit bool
	thinking bool
	pending  string
	visible  strings.Builder
	emit     func(string)
}

func (s *answerStream) write(text string) {
	if text == "" {
		return
	}
	s.visible.WriteString(text)
	if s.emit != nil {
		s.emit(text)
	}
}
func (s *answerStream) append(text string) {
	s.pending += text
	if s.implicit {
		if end := strings.Index(s.pending, "</think>"); end >= 0 {
			s.pending = s.pending[end+len("</think>"):]
			s.implicit = false
		} else {
			return
		}
	}
	s.drain(false)
}
func (s *answerStream) drain(final bool) {
	for s.pending != "" {
		marker := "<think>"
		if s.thinking {
			marker = "</think>"
		}
		if index := strings.Index(s.pending, marker); index >= 0 {
			if !s.thinking {
				s.write(s.pending[:index])
			}
			s.pending = s.pending[index+len(marker):]
			s.thinking = !s.thinking
			continue
		}
		keep := 0
		if !final {
			for n := 1; n < len(marker) && n <= len(s.pending); n++ {
				if strings.HasSuffix(s.pending, marker[:n]) {
					keep = n
				}
			}
		}
		count := len(s.pending) - keep
		if !s.thinking {
			s.write(s.pending[:count])
		}
		s.pending = s.pending[count:]
		return
	}
}
func (s *answerStream) finish(success bool) {
	if !success {
		s.pending = ""
		return
	}
	s.implicit = false
	s.drain(true)
}
