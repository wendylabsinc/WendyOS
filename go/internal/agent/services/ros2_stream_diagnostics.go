package services

import (
	"fmt"
	"strings"
)

const ros2StderrMaxBytes = 4096

// ros2StderrTail retains the end of a traceback without allowing an indefinitely
// running sensor subscription to accumulate unlimited stderr. ExecROS2 owns the
// writer until it returns; only then does ros2StreamExitError read it.
type ros2StderrTail struct {
	data []byte
}

func (w *ros2StderrTail) Write(p []byte) (int, error) {
	n := len(p)
	if n >= ros2StderrMaxBytes {
		w.data = append(w.data[:0], p[n-ros2StderrMaxBytes:]...)
	} else {
		if excess := len(w.data) + n - ros2StderrMaxBytes; excess > 0 {
			copy(w.data, w.data[excess:])
			w.data = w.data[:len(w.data)-excess]
		}
		w.data = append(w.data, p...)
	}
	return n, nil
}

// A ROS CLI process can exit unsuccessfully without a container exec error.
// Preserve that distinction so missing typesupport and decode failures cannot
// appear to clients as a successfully observed topic with no messages.
func ros2StreamExitError(code int, execErr error, stderr *ros2StderrTail) error {
	if code == 0 && execErr == nil {
		return nil
	}
	detail := strings.TrimSpace(strings.ToValidUTF8(string(stderr.data), "�"))
	if execErr != nil {
		return fmt.Errorf("process failed (exit %d): %w; %s", code, execErr, detail)
	}
	return fmt.Errorf("process failed (exit %d): %s", code, detail)
}
