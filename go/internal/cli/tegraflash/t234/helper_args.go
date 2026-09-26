//go:build darwin || linux || windows

package t234

import (
	"fmt"
	"io"
	"strconv"
)

// HelperRequest is one parsed `__t234-write` invocation: a LUN unmount or
// eject, enabling media polling, or exactly one writer operation. Stage2
// builds the argument lists (see flash.go); this parser is shared by the
// privileged helper subcommand (macOS/Linux, re-exec'd under sudo) and the
// in-process path (Windows, where the whole process is already elevated), so
// both sides always agree. Unmount and Eject are privileged too (umount,
// diskutil and eject all need root on Linux/macOS), so they route through the
// same helper; Writer.Device carries their target.
type HelperRequest struct {
	Unmount bool
	// Eject ejects only the LUN's medium; the USB device stays attached.
	Eject bool
	// PollMedia turns on host media polling for the LUNs of Session.
	PollMedia bool
	Session   string
	Writer    WriterOptions
}

// Args serializes the request into the flag list ParseWriterArgs parses back —
// the argv protocol of the sudo re-exec boundary on macOS/Linux (Windows runs
// requests in-process and never serializes). TestHelperArgsRoundTrip pins the
// two directions against each other.
func (r HelperRequest) Args() []string {
	switch {
	case r.Unmount:
		return []string{"--unmount", "--device", r.Writer.Device}
	case r.Eject:
		return []string{"--eject", "--device", r.Writer.Device}
	case r.PollMedia:
		return []string{"--poll-media", "--session", r.Session}
	}
	w := r.Writer
	args := []string{"--device", w.Device}
	switch {
	case w.Blob != "":
		args = append(args, "--blob", w.Blob)
	case w.WritePlan:
		args = append(args, "--write-plan", "--layout", w.LayoutPath, "--images", w.ImagesDir, "--rootfs-device", w.RootfsDevice)
	case w.DumpTo != "":
		args = append(args, "--dump", w.DumpTo, "--bytes", strconv.FormatInt(w.DumpBytes, 10))
	}
	return args
}

// ParseWriterArgs parses the flag-style argument list passed to the
// `__t234-write` helper.
func ParseWriterArgs(args []string) (HelperRequest, error) {
	var req HelperRequest
	next := func(i int, flag string) (string, error) {
		if i+1 >= len(args) {
			return "", fmt.Errorf("%s requires a value", flag)
		}
		return args[i+1], nil
	}
	for i := 0; i < len(args); i++ {
		var err error
		switch flag := args[i]; flag {
		case "--device":
			req.Writer.Device, err = next(i, flag)
			i++
		case "--blob":
			req.Writer.Blob, err = next(i, flag)
			i++
		case "--write-plan":
			req.Writer.WritePlan = true
		case "--layout":
			req.Writer.LayoutPath, err = next(i, flag)
			i++
		case "--images":
			req.Writer.ImagesDir, err = next(i, flag)
			i++
		case "--rootfs-device":
			req.Writer.RootfsDevice, err = next(i, flag)
			i++
		case "--dump":
			req.Writer.DumpTo, err = next(i, flag)
			i++
		case "--bytes":
			var v string
			if v, err = next(i, flag); err == nil {
				req.Writer.DumpBytes, err = strconv.ParseInt(v, 10, 64)
			}
			i++
		case "--unmount":
			req.Unmount = true
		case "--eject":
			req.Eject = true
		case "--poll-media":
			req.PollMedia = true
		case "--session":
			req.Session, err = next(i, flag)
			i++
		default:
			return HelperRequest{}, fmt.Errorf("unknown __t234-write flag %q", flag)
		}
		if err != nil {
			return HelperRequest{}, err
		}
	}
	return req, nil
}

// RunHelperRequest executes a parsed helper request, writing "PROGRESS"
// lines to progress (may be nil).
func RunHelperRequest(req HelperRequest, progress io.Writer) error {
	switch {
	case req.Unmount:
		return unmountUMSDisk(UMSDisk{DevPath: req.Writer.Device})
	case req.Eject:
		return ejectUMSDisk(UMSDisk{DevPath: req.Writer.Device})
	case req.PollMedia:
		return enableMediaPolling(req.Session)
	}
	opts := req.Writer
	opts.Progress = progress
	return RunWriter(opts)
}
