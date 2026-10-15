package containerd

import (
	"testing"

	"github.com/containerd/containerd/v2/pkg/cio"
)

func TestAttachmentClosePreservesSurvivingTaskFIFOs(t *testing.T) {
	removed := false
	original := cio.NewFIFOSet(cio.Config{Stdout: "/task/stdout", Stderr: "/task/stderr"}, func() error {
		removed = true
		return nil
	})
	attached := preserveTaskFIFOs(original)
	if attached.Config != original.Config {
		t.Fatal("attachment changed the task's FIFO paths")
	}
	if err := attached.Close(); err != nil || removed {
		t.Fatalf("detaching removed surviving task FIFOs: err=%v, removed=%v", err, removed)
	}
	_ = original.Close()
	if !removed {
		t.Fatal("original owner could not retire exited task FIFOs")
	}
}

func TestLateOutputExitDoesNotForgetReplacement(t *testing.T) {
	c := &Client{}
	c.trackTaskOutput("app", 100)
	c.trackTaskOutput("app", 200)
	c.forgetTaskOutput("app", 100)
	if c.outputPIDs["app"] != 200 {
		t.Fatal("old output reader released the replacement task's ownership")
	}
	c.forgetTaskOutput("app", 200)
	if _, tracked := c.outputPIDs["app"]; tracked {
		t.Fatal("exited task's output reader remained tracked")
	}
}
