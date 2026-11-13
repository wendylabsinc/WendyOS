package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/flock"
)

const syntaxErrorCountFile = "syntax-error-count"

func interactiveErrorTerminal() bool {
	isTerminal := func(f *os.File) bool {
		return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
	}
	return isTerminal(os.Stdin) && isTerminal(os.Stderr)
}

// reportCommandResult tracks only interactive invocations, so scripts and
// agents neither advance nor reset the person's streak. Read-only diagnostic
// and update checks must not create or change this cache either. JSON invocations
// in a terminal still count, but never include the hint.
func reportCommandResult(stderr io.Writer, err error, executed *cobra.Command, args []string, interactive bool) int {
	showHint := false
	if interactive && !isReadOnlyInvocation(executed) {
		if dir, cacheErr := config.CacheDir(); cacheErr == nil {
			showHint = recordSyntaxError(filepath.Join(dir, syntaxErrorCountFile), errorClass(err) == "cli_usage")
		}
	}
	exitCode := reportFailure(stderr, err, executed, args)
	if showHint && !commands.JSONErrorsRequested(executed, args) {
		fmt.Fprintln(stderr, "\n"+commands.DiscordSupportPrompt)
	}
	return exitCode
}

// recordSyntaxError returns true only for the second consecutive syntax
// error. Any other outcome clears the counter, including runtime failures
// and cancellation. Cache failures are best-effort and never mask an error.
func recordSyntaxError(path string, syntaxError bool) bool {
	release, err := flock.Acquire(path+".lock", 250*time.Millisecond)
	if err != nil {
		return false
	}
	defer release()

	if !syntaxError {
		_ = os.Remove(path)
		return false
	}

	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false
	}
	count, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		count = 0 // Missing or corrupt cache starts a fresh streak.
	}
	count++
	if err := atomicfile.WritePreservingOwner(path, []byte(strconv.FormatUint(count, 10)+"\n"), 0o600); err != nil {
		return false
	}
	return count == 2
}
