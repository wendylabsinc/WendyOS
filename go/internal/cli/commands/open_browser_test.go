package commands

import (
	"bytes"
	"fmt"
	"testing"
)

func TestOpenBrowserCmd_MissingArg(t *testing.T) {
	cmd := newOpenBrowserCmd()
	cmd.SetArgs([]string{})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing URL argument")
	}
}

func TestOpenBrowserCmd_MissingScheme(t *testing.T) {
	cmd := newOpenBrowserCmd()
	cmd.SetArgs([]string{"example.com"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for URL without scheme")
	}
}

func TestOpenBrowserCmd_InvalidURL(t *testing.T) {
	cmd := newOpenBrowserCmd()
	cmd.SetArgs([]string{"://bad"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestOpenBrowserCmd_ValidURL_Success(t *testing.T) {
	original := openBrowser
	t.Cleanup(func() { openBrowser = original })
	openBrowser = func(url string) error { return nil }

	cmd := newOpenBrowserCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"https://example.com"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := "Opening https://example.com in default browser...\n"
	if stdout.String() != expected {
		t.Errorf("stdout = %q, want %q", stdout.String(), expected)
	}
}

func TestOpenBrowserCmd_ValidURL_Fallback(t *testing.T) {
	original := openBrowser
	t.Cleanup(func() { openBrowser = original })
	openBrowser = func(url string) error { return fmt.Errorf("no display") }

	cmd := newOpenBrowserCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"https://example.com"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := stderr.String(); got != "Could not open browser: no display\n" {
		t.Errorf("stderr = %q, want fallback warning", got)
	}
	if got := stdout.String(); got != "https://example.com\n" {
		t.Errorf("stdout = %q, want URL printed as fallback", got)
	}
}

func TestOpenBrowserCmd_MissingHost(t *testing.T) {
	cmd := newOpenBrowserCmd()
	cmd.SetArgs([]string{"http://"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for URL without host")
	}
}

func TestOpenBrowserCmd_TooManyArgs(t *testing.T) {
	cmd := newOpenBrowserCmd()
	cmd.SetArgs([]string{"https://a.com", "https://b.com"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for too many arguments")
	}
}
