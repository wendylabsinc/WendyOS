package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
)

func TestRefreshingPickerModel_InitLoadsItems(t *testing.T) {
	m := newRefreshingPickerModel(context.Background(), "Select", time.Millisecond, func(context.Context) ([]tui.PickerItem, error) {
		return []tui.PickerItem{{Name: "alpha", Value: "alpha"}}, nil
	})

	msg := m.Init()()
	loadMsg, ok := msg.(refreshingPickerLoadMsg)
	if !ok {
		t.Fatalf("Init returned %T, want refreshingPickerLoadMsg", msg)
	}
	if loadMsg.err != nil {
		t.Fatalf("load error = %v", loadMsg.err)
	}
	if got := len(loadMsg.items); got != 1 {
		t.Fatalf("items = %d, want 1", got)
	}
}

func TestRefreshingPickerModel_LoadResultReplacesItemsAndSchedulesRefresh(t *testing.T) {
	m := newRefreshingPickerModel(context.Background(), "Select", 3*time.Second, func(context.Context) ([]tui.PickerItem, error) {
		return nil, nil
	})

	updated, cmd := m.Update(refreshingPickerLoadMsg{items: []tui.PickerItem{
		{Name: "alpha", Value: "alpha"},
	}})
	if cmd == nil {
		t.Fatal("expected load result to schedule refresh")
	}
	rm := updated.(refreshingPickerModel)
	if rm.interval.next != time.Second {
		t.Fatalf("next interval = %v, want 1s", rm.interval.next)
	}
	if view := rm.View(); !strings.Contains(view, "alpha") {
		t.Fatalf("expected view to contain refreshed item, got %q", view)
	}

	updated, cmd = rm.Update(refreshingPickerLoadMsg{items: []tui.PickerItem{
		{Name: "beta", Value: "beta"},
	}})
	if cmd == nil {
		t.Fatal("expected second load result to schedule refresh")
	}
	rm = updated.(refreshingPickerModel)
	if rm.interval.next != 2*time.Second {
		t.Fatalf("next interval = %v, want 2s", rm.interval.next)
	}
	view := rm.View()
	if strings.Contains(view, "alpha") {
		t.Fatalf("stale item remained after replacement: %q", view)
	}
	if !strings.Contains(view, "beta") {
		t.Fatalf("expected view to contain replacement item, got %q", view)
	}
}

func TestRefreshingPickerModel_EmptyLoadKeepsPickerOpen(t *testing.T) {
	m := newRefreshingPickerModel(context.Background(), "Select", time.Millisecond, func(context.Context) ([]tui.PickerItem, error) {
		return nil, nil
	})

	updated, cmd := m.Update(refreshingPickerLoadMsg{})
	if cmd == nil {
		t.Fatal("expected empty load to schedule refresh")
	}
	rm := updated.(refreshingPickerModel)
	if rm.err != nil {
		t.Fatalf("unexpected error: %v", rm.err)
	}
	if !strings.Contains(rm.View(), "Scanning") {
		t.Fatalf("expected empty refreshing picker to keep scanning, got %q", rm.View())
	}
}

func TestRefreshingPickerModel_LoadErrorQuits(t *testing.T) {
	wantErr := errors.New("boom")
	m := newRefreshingPickerModel(context.Background(), "Select", time.Millisecond, func(context.Context) ([]tui.PickerItem, error) {
		return nil, nil
	})

	updated, cmd := m.Update(refreshingPickerLoadMsg{err: wantErr})
	if cmd == nil {
		t.Fatal("expected load error to quit")
	}
	rm := updated.(refreshingPickerModel)
	if !errors.Is(rm.err, wantErr) {
		t.Fatalf("error = %v, want %v", rm.err, wantErr)
	}
}

func TestRefreshingPickerModel_SelectsItem(t *testing.T) {
	m := newRefreshingPickerModel(context.Background(), "Select", time.Millisecond, func(context.Context) ([]tui.PickerItem, error) {
		return nil, nil
	})
	updated, _ := m.Update(refreshingPickerLoadMsg{items: []tui.PickerItem{
		{Name: "alpha", Value: "alpha"},
	}})
	rm := updated.(refreshingPickerModel)

	updated, cmd := rm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected enter to quit")
	}
	rm = updated.(refreshingPickerModel)
	if rm.picker.Selected() == nil {
		t.Fatal("expected selected item")
	}
	if got := rm.picker.Selected().Value.(string); got != "alpha" {
		t.Fatalf("selected value = %q, want alpha", got)
	}
}
