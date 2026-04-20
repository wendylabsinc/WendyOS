package tui

import (
	"fmt"
	"strings"
	"testing"
)

func TestSpinnerModel_Init(t *testing.T) {
	s := NewSpinner("Loading...")
	cmd := s.Init()
	if cmd == nil {
		t.Error("expected non-nil Init cmd (spinner tick)")
	}
}

func TestSpinnerModel_DoneMsg(t *testing.T) {
	s := NewSpinner("Working...")

	// Before done.
	if s.Done() {
		t.Error("expected Done() = false initially")
	}

	// Send a SpinnerDoneMsg.
	result := "completed"
	model, cmd := s.Update(SpinnerDoneMsg{Result: result, Err: nil})
	updated := model.(SpinnerModel)

	if !updated.Done() {
		t.Error("expected Done() = true after SpinnerDoneMsg")
	}

	res, err := updated.Result()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if res != result {
		t.Errorf("Result() = %v; want %v", res, result)
	}

	// cmd should be tea.Quit.
	if cmd == nil {
		t.Error("expected non-nil quit cmd")
	}
}

func TestSpinnerModel_ErrorDoneMsg(t *testing.T) {
	s := NewSpinner("Working...")

	testErr := fmt.Errorf("something failed")
	model, _ := s.Update(SpinnerDoneMsg{Result: nil, Err: testErr})
	updated := model.(SpinnerModel)

	if !updated.Done() {
		t.Error("expected Done() = true")
	}

	_, err := updated.Result()
	if err != testErr {
		t.Errorf("err = %v; want %v", err, testErr)
	}

	view := updated.View()
	if !strings.Contains(view, "Error") {
		t.Errorf("error view should contain 'Error', got: %q", view)
	}
}

func TestSpinnerModel_View(t *testing.T) {
	s := NewSpinner("Processing...")
	view := s.View()
	if !strings.Contains(view, "Processing...") {
		t.Errorf("view should contain title, got: %q", view)
	}
}

func TestRenderTable(t *testing.T) {
	headers := []string{"Name", "Status", "Version"}
	rows := [][]string{
		{"app-one", "running", "1.0"},
		{"app-two", "stopped", "2.0"},
	}

	output := RenderTable(headers, rows)
	if output == "" {
		t.Fatal("expected non-empty table output")
	}

	// Check that all data appears in the output.
	for _, h := range headers {
		if !strings.Contains(output, h) {
			t.Errorf("table missing header %q", h)
		}
	}
	for _, row := range rows {
		for _, cell := range row {
			if !strings.Contains(output, cell) {
				t.Errorf("table missing cell %q", cell)
			}
		}
	}
}

func TestRenderTable_Empty(t *testing.T) {
	output := RenderTable([]string{}, nil)
	if output != "" {
		t.Errorf("expected empty output for no headers, got: %q", output)
	}
}

func TestRenderTable_NoRows(t *testing.T) {
	headers := []string{"Name", "Value"}
	output := RenderTable(headers, nil)
	if output == "" {
		t.Fatal("expected non-empty output with headers only")
	}
	if !strings.Contains(output, "Name") {
		t.Error("expected headers in output")
	}
}

func TestProgressModel_Init(t *testing.T) {
	p := NewProgress("Downloading...")
	cmd := p.Init()
	// ProgressModel.Init returns nil since there's no initial tick needed.
	if cmd != nil {
		t.Error("expected nil Init cmd for progress model")
	}
}

func TestProgressModel_DoneMsg(t *testing.T) {
	p := NewProgress("Uploading...")

	model, cmd := p.Update(ProgressDoneMsg{Err: nil})
	updated := model.(ProgressModel)

	if updated.Err() != nil {
		t.Errorf("unexpected error: %v", updated.Err())
	}

	if cmd == nil {
		t.Error("expected non-nil quit cmd")
	}
}

func TestProgressModel_ErrorDoneMsg(t *testing.T) {
	p := NewProgress("Uploading...")

	testErr := fmt.Errorf("upload failed")
	model, _ := p.Update(ProgressDoneMsg{Err: testErr})
	updated := model.(ProgressModel)

	if updated.Err() != testErr {
		t.Errorf("Err() = %v; want %v", updated.Err(), testErr)
	}

	view := updated.View()
	if !strings.Contains(view, "Error") {
		t.Errorf("error view should contain 'Error', got: %q", view)
	}
}

func TestProgressModel_WithoutErrorViewSuppressesInlineError(t *testing.T) {
	p := NewProgress("Uploading...").WithoutErrorView()

	testErr := fmt.Errorf("upload failed")
	model, _ := p.Update(ProgressUpdateMsg{
		Percent: 0.5,
		Written: 512,
		Total:   1024,
	})
	p = model.(ProgressModel)
	model, _ = p.Update(ProgressDoneMsg{Err: testErr})
	updated := model.(ProgressModel)

	if updated.Err() != testErr {
		t.Errorf("Err() = %v; want %v", updated.Err(), testErr)
	}

	view := updated.View()
	if strings.Contains(view, "Error") {
		t.Errorf("suppressed error view should not contain inline error text, got: %q", view)
	}
	if !strings.Contains(view, "Uploading...") {
		t.Errorf("suppressed error view should retain the title, got: %q", view)
	}
}

func TestProgressModel_ViewByteCounter(t *testing.T) {
	p := NewProgress("Extracting...")

	// Send an update with Written and Total.
	model, _ := p.Update(ProgressUpdateMsg{
		Percent: 0.44,
		Written: 26 * 1024 * 1024 * 1024, // ~26 GB
		Total:   59 * 1024 * 1024 * 1024, // ~59 GB
	})
	updated := model.(ProgressModel)

	view := updated.View()
	if !strings.Contains(view, "26.0 GiB") {
		t.Errorf("view should contain written bytes '26.0 GiB', got: %q", view)
	}
	if !strings.Contains(view, "59.0 GiB") {
		t.Errorf("view should contain total bytes '59.0 GiB', got: %q", view)
	}
	if !strings.Contains(view, "/") {
		t.Errorf("view should contain '/' separator for byte counter, got: %q", view)
	}
}

func TestProgressModel_ViewNoByteCounterWhenZero(t *testing.T) {
	p := NewProgress("Flashing...")

	// Update with no Written/Total — should not show byte counter.
	model, _ := p.Update(ProgressUpdateMsg{
		Percent: 0.5,
		Written: 0,
		Total:   0,
	})
	updated := model.(ProgressModel)

	view := updated.View()
	if strings.Contains(view, "GiB") || strings.Contains(view, "MiB") || strings.Contains(view, "KiB") {
		t.Errorf("view should not contain byte info when Written/Total are zero, got: %q", view)
	}
}

func TestProgressModel_DoneRendersFullBytes(t *testing.T) {
	p := NewProgress("Extracting...")

	// First send a partial update so the model has byte info.
	model, _ := p.Update(ProgressUpdateMsg{
		Percent: 0.5,
		Written: 500 * 1024 * 1024,  // 500 MB
		Total:   1000 * 1024 * 1024, // 1000 MB
	})
	p = model.(ProgressModel)

	// Now send done.
	model, _ = p.Update(ProgressDoneMsg{Err: nil})
	updated := model.(ProgressModel)

	view := updated.View()
	// Done state should show total/total (not written/total).
	// Both sides should be "1000.0 MiB".
	count := strings.Count(view, "1000.0 MiB")
	if count != 2 {
		t.Errorf("done view should show total/total (two instances of '1000.0 MiB'), got: %q", view)
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		input int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{500 * 1024 * 1024, "500.0 MiB"},
		{1024 * 1024 * 1024, "1.0 GiB"},
		{int64(10.5 * 1024 * 1024 * 1024), "10.5 GiB"},
	}

	for _, tt := range tests {
		got := formatBytes(tt.input)
		if got != tt.want {
			t.Errorf("formatBytes(%d) = %q; want %q", tt.input, got, tt.want)
		}
	}
}
