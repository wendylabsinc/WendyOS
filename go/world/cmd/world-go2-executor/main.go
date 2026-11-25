// world-go2-executor runs one explicitly selected signed simulation mission.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/executor"
	"github.com/wendylabsinc/wendy/go/world/internal/go2executor"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

func readFile(path string, private bool) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 65536 || (private && info.Mode().Perm()&0077 != 0) {
		return nil, errors.New("configuration must be a bounded regular file with private credentials")
	}
	b, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(b) > 65536 {
		return nil, errors.New("configuration exceeds bound")
	}
	return b, nil
}

func writeFault(path string, data []byte) error {
	if len(data) > go2executor.MaxFaultJSONBytes {
		return errors.New("fault snapshot exceeds bound")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Write(append(data, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

const maxObservationEvents = 1200
const maxCommandEvents = 1200
const maxObservationJSONBytes = 32 << 10
const maxObservationLogBytes = 8 << 20

var errObservationLimit = errors.New("observation diagnostic exceeds byte bound")

type observationLogState struct {
	capture, command               uint64
	observations, commands         int
	observationLimit, commandLimit bool
	lastSelection                  *go2executor.SelectionDiagnostic
	selections                     int
	selectionLimit                 bool
}

func observationEvent(s go2executor.State, task, run string, epoch uint64) map[string]any {
	if s.Scan.TiltedPose != nil {
		pose := *s.Scan.TiltedPose
		s.Scan.TiltedPose = &pose
	}
	event := map[string]any{
		"event": "executor_observation", "simulation": true, "operational_authority": false,
		"task_id": task, "run_id": run, "executor_epoch": epoch,
		"capture_sequence": s.CaptureSequence, "odom_captured_unix_ns": s.OdomCapturedUnixNS,
		"scan_captured_unix_ns": s.ScanCapturedUnixNS, "odometry_epoch_age_s": s.OdometryEpochAgeS,
		"pose": s.Pose, "body_velocity": s.BodyVelocity, "roll_rad": s.RollRad, "pitch_rad": s.PitchRad,
		"tilted_pose":        s.Scan.TiltedPose,
		"full_attitude_role": "original_consumed_normalized_odometry_orientation_not_oracle",
		"odom_publisher":     s.OdomPublisher, "scan_publisher": s.ScanPublisher,
		"pose_covariance": s.PoseCovariance, "twist_covariance": s.TwistCovariance,
		"position_uncertainty_m": s.Scan.PositionUncertaintyM, "yaw_uncertainty_rad": s.Scan.YawUncertaintyRad,
	}
	if c := s.Cloud; c != nil {
		event["cloud"] = map[string]any{
			"model": c.Model, "source_digest": c.SourceDigest, "frame": c.Frame,
			"publisher_gid": c.PublisherGID, "serialized_sha256": c.SerializedSHA256,
			"captured_unix_ns": c.CapturedUnixNS, "sequence": c.Sequence, "observed_epoch": c.ObservedEpoch,
			"scan_sha256": c.ScanSHA256, "odom_sha256": c.OdomSHA256, "epoch_sha256": c.EpochSHA256,
			"mount_sha256": c.MountSHA256, "cloud_mount_sha256": c.CloudMountSHA256,
			"epoch_publisher": c.EpochPublisher, "mount_publisher": c.MountPublisher,
			"cloud_mount_publisher": c.CloudMountPublisher,
		}
	}
	return event
}

// events keeps the existing one-row-per-capture stream. A command is attached
// only to its exact consumed capture. Otherwise it has a separate event so an
// old ACK can never silently appear to describe a newer observation.
func (l *observationLogState) events(s go2executor.State, hasState bool, command *go2executor.CommandDiagnostic, task, run string, epoch uint64, selections ...*go2executor.SelectionDiagnostic) []map[string]any {
	var events []map[string]any
	var observation map[string]any
	if hasState && s.CaptureSequence > l.capture {
		l.capture = s.CaptureSequence
		if l.observations < maxObservationEvents {
			observation = observationEvent(s, task, run, epoch)
			events = append(events, observation)
			l.observations++
		} else if !l.observationLimit {
			l.observationLimit = true
			events = append(events, map[string]any{"event": "executor_diagnostic_limit", "stream": "observations", "limit": maxObservationEvents, "task_id": task, "run_id": run, "executor_epoch": epoch, "simulation": true, "operational_authority": false})
		}
	}
	if command != nil && command.Request.Sequence > l.command && command.Request.TaskID == task && command.Request.ExecutorEpoch == epoch {
		l.command = command.Request.Sequence
		if l.commands < maxCommandEvents {
			l.commands++
			if observation != nil && command.Request.CaptureSequence == s.CaptureSequence {
				observation["command"] = command
				observation["command_observation_join"] = "exact_capture_sequence"
			} else {
				events = append(events, map[string]any{
					"event": "executor_command", "simulation": true, "operational_authority": false,
					"task_id": task, "run_id": run, "executor_epoch": epoch, "command": command,
					"command_observation_join": "separate_event_no_current_capture_join",
				})
			}
		} else if !l.commandLimit {
			l.commandLimit = true
			events = append(events, map[string]any{"event": "executor_diagnostic_limit", "stream": "commands", "limit": maxCommandEvents, "task_id": task, "run_id": run, "executor_epoch": epoch, "simulation": true, "operational_authority": false})
		}
	}
	if len(selections) == 1 && selections[0] != nil {
		selection := selections[0]
		if selection.TaskID == task && selection.RunID == run && selection.ExecutorEpoch == epoch && selection.CaptureSequence > 0 && !equalSelection(l.lastSelection, selection) {
			value := *selection
			if selection.Selected != nil {
				selected := *selection.Selected
				value.Selected = &selected
			}
			l.lastSelection = &value
			if l.selections < maxCommandEvents {
				l.selections++
				if observation != nil && value.CaptureSequence == s.CaptureSequence {
					observation["selection"] = &value
					observation["selection_observation_join"] = "exact_capture_sequence"
				} else {
					events = append(events, map[string]any{"event": "executor_selection", "simulation": true, "operational_authority": false, "task_id": task, "run_id": run, "executor_epoch": epoch, "selection": &value, "selection_observation_join": "separate_event_no_current_capture_join"})
				}
			} else if !l.selectionLimit {
				l.selectionLimit = true
				events = append(events, map[string]any{"event": "executor_diagnostic_limit", "stream": "selections", "limit": maxCommandEvents, "task_id": task, "run_id": run, "executor_epoch": epoch, "simulation": true, "operational_authority": false})
			}
		}
	}
	return events
}

func equalSelection(a, b *go2executor.SelectionDiagnostic) bool {
	if a == nil || b == nil {
		return a == b
	}
	if (a.Selected == nil) != (b.Selected == nil) || (a.Selected != nil && *a.Selected != *b.Selected) {
		return false
	}
	x, y := *a, *b
	x.Selected, y.Selected = nil, nil
	return x == y
}

func writeObservation(w io.Writer, value map[string]any, remaining int) (int, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return 0, err
	}
	if len(data) > maxObservationJSONBytes || len(data)+1 > remaining {
		return 0, errObservationLimit
	}
	n, err := w.Write(append(data, '\n'))
	if err == nil && n != len(data)+1 {
		return n, io.ErrShortWrite
	}
	return n, err
}

func main() {
	credentials := flag.String("credentials", "", "private simulation assignee configuration")
	adapter := flag.String("adapter", "", "numeric loopback bridge URL")
	token := flag.String("adapter-token-file", "", "private bridge token file")
	binding := flag.String("binding", "", "commissioned Go2 execution binding")
	alignment := flag.String("alignment", "", "exact commissioned alignment JSON")
	profile := flag.String("profile", "", "exact commissioned navigation profile JSON")
	task := flag.String("task-id", "", "one newly accepted signed task; no automatic next task")
	faultPath := flag.String("fault-snapshot", "", "write one unsigned simulation diagnostic to a new private file")
	faultLog := flag.Bool("fault-snapshot-log", false, "also emit that bounded diagnostic once to private app logs")
	observationLog := flag.Bool("observation-log", false, "emit bounded unsigned consumed-pose and bridge-publication diagnostics for verification")
	flag.Parse()
	if flag.NArg() != 0 || *task == "" {
		fatal(errors.New("explicit task ID required"))
	}
	var cfg client.Config
	raw, err := readFile(*credentials, true)
	if err != nil {
		fatal(err)
	}
	if authority.DecodeStrict(raw, &cfg) != nil || cfg.Grant.Grant.Domain != "simulation" || cfg.Grant.Grant.Site == "" {
		fatal(errors.New("private simulation assignee credentials required"))
	}
	c, err := client.New(cfg)
	if err != nil {
		fatal(err)
	}
	raw, err = readFile(*binding, false)
	if err != nil {
		fatal(err)
	}
	var b tasking.Go2Binding
	if authority.DecodeStrict(raw, &b) != nil || b.Validate() != nil {
		fatal(errors.New("invalid commissioned binding"))
	}
	aJSON, err := readFile(*alignment, false)
	if err != nil {
		fatal(err)
	}
	pJSON, err := readFile(*profile, false)
	if err != nil {
		fatal(err)
	}
	t, err := readFile(*token, true)
	if err != nil {
		fatal(err)
	}
	a, err := go2executor.NewHTTPAdapter(*adapter, strings.TrimSpace(string(t)))
	if err != nil {
		fatal(err)
	}
	controller, err := go2executor.New(executor.NodeGateway{Client: c}, a, go2executor.Config{TaskID: *task, Site: cfg.Grant.Grant.Site, Binding: b, AlignmentJSON: aJSON, ProfileJSON: pJSON})
	if err != nil {
		fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	last := ""
	readyReported := false
	faultReported := false
	var observations observationLogState
	observationBytes := 0
	observationLoggingFailed := false
	recordFault := func() {
		if faultReported || (*faultPath == "" && !*faultLog) {
			return
		}
		shot, available := controller.FaultSnapshot()
		if !available {
			return
		}
		faultReported = true
		data, encodeErr := json.Marshal(shot)
		if encodeErr != nil {
			fmt.Fprintln(os.Stderr, "fault_snapshot_encoding_failed")
			return
		}
		if *faultPath != "" && writeFault(*faultPath, data) != nil {
			fmt.Fprintln(os.Stderr, "fault_snapshot_write_failed")
		}
		if *faultLog {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"event": "executor_fault_snapshot", "snapshot": json.RawMessage(data)})
		}
	}
	err = controller.Run(ctx, func(d tasking.Decision, e error) {
		recordFault()
		if *observationLog && !observationLoggingFailed {
			s, available := controller.Observation()
			command, _ := controller.CommandObservation()
			selection, _ := controller.SelectionObservation()
			for _, event := range observations.events(s, available, command, *task, b.RunID, controller.Generation(), selection) {
				n, logErr := writeObservation(os.Stdout, event, maxObservationLogBytes-observationBytes)
				observationBytes += n
				if logErr != nil {
					observationLoggingFailed = true
					if errors.Is(logErr, errObservationLimit) {
						fmt.Fprintln(os.Stderr, "observation_diagnostic_byte_limit")
					} else {
						fmt.Fprintln(os.Stderr, "observation_diagnostic_write_failed")
					}
					break
				}
			}
		}
		if !readyReported && controller.Generation() > 0 {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"event": "executor_ready", "simulation": true, "task_id": *task, "run_id": b.RunID, "executor_epoch": controller.Generation()})
			readyReported = true
		}
		state := fmt.Sprintf("%s/%s/%d/%v", d.Record.Spec.ID, d.Action, d.Record.Status.Version, e)
		if state == last {
			return
		}
		last = state
		event := map[string]any{"task_id": d.Record.Spec.ID, "action": d.Action, "status": d.Record.Status, "simulation": true}
		if e != nil {
			event["error"] = e.Error()
		}
		_ = json.NewEncoder(os.Stdout).Encode(event)
	})
	recordFault()
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
