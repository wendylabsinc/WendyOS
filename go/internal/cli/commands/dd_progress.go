//go:build darwin || linux

package commands

import (
	"bufio"
	"io"
	"strconv"
)

// scanDDProgress parses dd's `status=progress` output and invokes progressFn
// with the running byte count. dd separates in-place updates with '\r' and
// terminates the final summary block with '\n', so we split on either.
//
// Both Linux GNU dd and macOS BSD dd (Monterey+) emit lines whose first
// whitespace-delimited token is the byte count, e.g.:
//
//	524288000 bytes (524 MB, 500 MiB) copied, 1 s, 524 MB/s        (Linux)
//	524288000 bytes (524 MB, 500 MiB) transferred 1.001s, 524 MB/s (macOS)
//	524288000 bytes transferred in 5.123 secs (...)                (macOS final)
//
// "records in" / "records out" lines have a "+0" suffix on the first token
// so ParseInt rejects them and we skip silently — exactly what we want.
func scanDDProgress(r io.Reader, progressFn func(written int64)) {
	if progressFn == nil {
		io.Copy(io.Discard, r) //nolint:errcheck
		return
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	scanner.Split(splitCROrLF)
	for scanner.Scan() {
		line := scanner.Text()
		var token string
		for i, c := range line {
			if c == ' ' || c == '\t' {
				token = line[:i]
				break
			}
		}
		if token == "" {
			continue
		}
		written, err := strconv.ParseInt(token, 10, 64)
		if err != nil {
			continue
		}
		progressFn(written)
	}
}

// splitCROrLF is a bufio.SplitFunc that splits on '\r' or '\n'.
func splitCROrLF(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for i, b := range data {
		if b == '\r' || b == '\n' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}
