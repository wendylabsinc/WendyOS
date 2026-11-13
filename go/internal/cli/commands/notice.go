package commands

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// noticeOut is where plainNotice writes; tests swap it for a buffer.
var noticeOut io.Writer = os.Stderr

// plainNotice writes one unstyled line to stderr. JSON mode routes notices
// here instead of dropping them: stdout must stay pure JSON, but whoever reads
// stderr (usually an agent's tool output) still learns which device a command
// used, or what to do next. Styling is stripped and whitespace, including
// newlines, collapsed so the notice is exactly one line.
func plainNotice(format string, args ...any) {
	line := strings.Join(strings.Fields(ansi.Strip(fmt.Sprintf(format, args...))), " ")
	if line != "" {
		fmt.Fprintln(noticeOut, line)
	}
}
