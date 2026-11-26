package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func progressAfterUpdate(t *testing.T, msg ProgressUpdateMsg) ProgressModel {
	t.Helper()
	m, _ := NewProgress("Writing...").Update(msg)
	return m.(ProgressModel)
}

func TestProgressViewByteInfo(t *testing.T) {
	t.Run("renders written and total when both known", func(t *testing.T) {
		m := progressAfterUpdate(t, ProgressUpdateMsg{
			Percent: 0.25,
			Written: 50 * 1024 * 1024,
			Total:   200 * 1024 * 1024,
		})
		if view := m.View(); !strings.Contains(view, "(50.0 MiB / 200.0 MiB)") {
			t.Errorf("View() = %q; want it to contain %q", view, "(50.0 MiB / 200.0 MiB)")
		}
	})

	t.Run("renders written alone when total is unknown", func(t *testing.T) {
		m := progressAfterUpdate(t, ProgressUpdateMsg{
			Percent: 0.25,
			Written: 50 * 1024 * 1024,
		})
		view := m.View()
		if !strings.Contains(view, "(50.0 MiB)") {
			t.Errorf("View() = %q; want it to contain %q", view, "(50.0 MiB)")
		}
		// A total renders as "(written / total)"; match the separator
		// specifically so an unrelated "/" in a rotating hint (e.g.
		// "Claude/Codex") doesn't trip this check.
		if strings.Contains(view, " / ") {
			t.Errorf("View() = %q; must not render a total when none is known", view)
		}
	})

	t.Run("done with unknown total keeps written-only counter", func(t *testing.T) {
		m := progressAfterUpdate(t, ProgressUpdateMsg{
			Percent: 1.0,
			Written: 50 * 1024 * 1024,
		})
		next, _ := m.Update(ProgressDoneMsg{})
		if view := next.View(); !strings.Contains(view, "(50.0 MiB)") {
			t.Errorf("View() after done = %q; want it to contain %q", view, "(50.0 MiB)")
		}
	})
}

// A transient bar is one labelled line while it runs and disappears when done,
// so the caller's ✓ line can take its place; a failure stays visible.
func TestTransientProgressIsOneLineAndClearsWhenDone(t *testing.T) {
	m := NewProgress("Writing to /dev/disk4").WithoutHints().Transient()
	running, _ := m.Update(ProgressUpdateMsg{Percent: 0.5, Written: 1 << 30, Total: 2 << 30})
	view := running.(ProgressModel).View()
	if strings.Count(strings.TrimSuffix(view, "\n"), "\n") != 0 || !strings.Contains(view, "Writing to /dev/disk4") {
		t.Fatalf("running view %q; want one labelled line", view)
	}
	done, _ := running.(ProgressModel).Update(ProgressDoneMsg{})
	if v := done.(ProgressModel).View(); v != "" {
		t.Fatalf("done view %q; want nothing left on screen", v)
	}
	failed, _ := running.(ProgressModel).Update(ProgressDoneMsg{Err: errors.New("device not configured")})
	if v := failed.(ProgressModel).View(); !strings.Contains(v, "device not configured") {
		t.Fatalf("failed view %q; want the failure kept on screen", v)
	}
}

func TestTransientProgressFitsTheTerminal(t *testing.T) {
	m := NewProgress("Downloading WendyOS 0.20.2").WithoutHints().Transient()
	sized, _ := m.Update(tea.WindowSizeMsg{Width: 80})
	running, _ := sized.(ProgressModel).Update(ProgressUpdateMsg{Percent: 0.08, Written: 1023*1024*1024 + 900*1024*1024/1000, Total: 12_500 * 1024 * 1024})
	line := strings.TrimSuffix(running.(ProgressModel).View(), "\n")
	if w := lipgloss.Width(line); w > 80 {
		t.Fatalf("line is %d columns at an 80-column terminal: %q", w, line)
	}
}

func TestTransientProgressShowsBytesWithoutATotal(t *testing.T) {
	m := NewProgress("Downloading WendyOS 0.20.2").WithoutHints().Transient()
	running, _ := m.Update(ProgressUpdateMsg{Percent: 0, Written: 420 * 1024 * 1024})
	if v := running.(ProgressModel).View(); !strings.Contains(v, "420.0 MiB") {
		t.Fatalf("view %q; want the bytes so far when the total is unknown", v)
	}
}
