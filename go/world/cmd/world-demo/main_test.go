package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/scenario"
)

func TestRunWritesCompleteJSONReport(t *testing.T) {
	var out bytes.Buffer
	if err := run(&out); err != nil {
		t.Fatal(err)
	}
	var report scenario.Report
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Schema != "wendy.scenario.partition.v0" || len(report.Steps) != 4 || !report.Checks["latest_observations_reconcile"] {
		t.Fatal("incomplete report")
	}
}

type failedWriter struct{}

func (failedWriter) Write([]byte) (int, error) { return 0, errors.New("output unavailable") }

func TestOutputFailureIsReturned(t *testing.T) {
	if err := run(failedWriter{}); err == nil {
		t.Fatal("discarded output failure")
	}
}
