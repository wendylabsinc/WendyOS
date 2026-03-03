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
