package services

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// ros2StreamTestTimeout bounds each streaming test. The bug these cover was not
// only "returns nil silently" — it deadlocked: the scanner stopped consuming, the
// exec goroutine blocked mid-Write into the pipe, and <-execDone never returned.
// Without an explicit bound a regression burns the whole `go test` timeout and
// reports as an unrelated package-level failure.
const ros2StreamTestTimeout = 30 * time.Second

// withDeadline runs fn and fails the test if it has not returned in time, so a
// hang is reported as a hang, at the right test, immediately.
func withDeadline(t *testing.T, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(ros2StreamTestTimeout):
		t.Fatalf("timed out after %s: the RPC did not return (deadlock?)", ros2StreamTestTimeout)
		return nil
	}
}

// EchoTopic scans `ros2 topic echo` output with a bufio.Scanner capped at 4 MiB
// and never checked scanner.Err(). A single YAML document over the cap — a
// PointCloud2, a sensor_msgs/Image, an occupancy grid, i.e. exactly the topics
// people reach for echo to inspect — made Scan() return false with
// bufio.ErrTooLong. execErr was nil, so EchoTopic returned nil, the CLI saw a
// clean EOF, and the user got exit 0 with no output and no error (or worse, the
// "no active publishers" notice, which is a lie).

// echoRuntime streams the given stdout payload for `topic echo`, and answers the
// `topic list` that routing uses.
func echoRuntime(topic, payload string) *fakeROS2Runtime {
	return &fakeROS2Runtime{
		sidecar: ROS2Sidecar{Name: "sc", Distro: "humble", DomainID: 5},
		execFn: func(_ context.Context, opts ROS2ExecOptions, stdout, _ io.Writer) (int, error) {
			switch strings.Join(opts.Args, " ") {
			case "topic list":
				_, _ = io.WriteString(stdout, topic+"\n")
				return 0, nil
			case "topic echo " + topic:
				_, _ = io.WriteString(stdout, payload)
				return 0, nil
			}
			return 1, nil
		},
	}
}

func TestROS2Service_EchoTopic_OversizedMessageIsAnError(t *testing.T) {
	// One YAML document larger than the scanner's token cap, on a single line —
	// which is how a big base64/uint8[] field arrives.
	huge := strings.Repeat("x", 5*1024*1024)
	payload := "data: " + huge + "\n---\n"

	rt := echoRuntime("/camera/points", payload)
	svc := newTestROS2Service(t, rt, t.TempDir())
	stream := &fakeServerStream[agentpbv2.ROS2Message]{ctx: context.Background()}

	err := withDeadline(t, func() error {
		return svc.EchoTopic(&agentpbv2.EchoROS2TopicRequest{Topic: "/camera/points"}, stream)
	})
	if err == nil {
		t.Fatal("EchoTopic returned nil for an oversized message; the client cannot " +
			"distinguish that from a topic with no publishers")
	}
	if got := status.Code(err); got != codes.ResourceExhausted {
		t.Errorf("status code = %v, want ResourceExhausted (err: %v)", got, err)
	}
	if !strings.Contains(err.Error(), "/camera/points") {
		t.Errorf("error should name the topic, got: %v", err)
	}
	if len(stream.sent) != 0 {
		t.Errorf("sent %d messages, want 0 (the document never completed)", len(stream.sent))
	}
}

func TestROS2Service_EchoTopic_NormalSizedMessagesStillStream(t *testing.T) {
	payload := "data: hello\n---\ndata: world\n---\n"
	rt := echoRuntime("/chatter", payload)
	svc := newTestROS2Service(t, rt, t.TempDir())
	stream := &fakeServerStream[agentpbv2.ROS2Message]{ctx: context.Background()}

	if err := withDeadline(t, func() error {
		return svc.EchoTopic(&agentpbv2.EchoROS2TopicRequest{Topic: "/chatter"}, stream)
	}); err != nil {
		t.Fatalf("EchoTopic: %v", err)
	}
	if len(stream.sent) != 2 {
		t.Fatalf("sent %d messages, want 2", len(stream.sent))
	}
	if got := stream.sent[0].GetYaml(); got != "data: hello\n" {
		t.Errorf("first message = %q", got)
	}
}

// A message just under the cap must still get through, so the fix is a real error
// path and not a lowered ceiling.
func TestROS2Service_EchoTopic_LargeButUnderCapSucceeds(t *testing.T) {
	body := strings.Repeat("y", 3*1024*1024)
	payload := "data: " + body + "\n---\n"
	rt := echoRuntime("/big", payload)
	svc := newTestROS2Service(t, rt, t.TempDir())
	stream := &fakeServerStream[agentpbv2.ROS2Message]{ctx: context.Background()}

	if err := withDeadline(t, func() error {
		return svc.EchoTopic(&agentpbv2.EchoROS2TopicRequest{Topic: "/big"}, stream)
	}); err != nil {
		t.Fatalf("EchoTopic: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(stream.sent))
	}
}

// MonitorHz used the default 64 KiB scanner buffer, so a node logging a longer
// line to stdout silently ended the stream as if the topic had stopped
// publishing. The cap is now ros2HzMaxLineBytes and exceeding it is an error.
func TestROS2Service_MonitorHz_LongLineUnderNewCapStillStreams(t *testing.T) {
	// 128 KiB: over the old 64 KiB default, under the new cap. This is the case
	// that used to break in practice.
	long := strings.Repeat("z", 128*1024)
	payload := "average rate: 10.000\n\tmin: 0.099s max: 0.101s std dev: 0.00050s window: 11\n" +
		long + "\n"
	rt := &fakeROS2Runtime{
		sidecar: ROS2Sidecar{Name: "sc", Distro: "humble", DomainID: 5},
		execFn: func(_ context.Context, opts ROS2ExecOptions, stdout, _ io.Writer) (int, error) {
			switch strings.Join(opts.Args, " ") {
			case "topic list":
				_, _ = io.WriteString(stdout, "/chatter\n")
				return 0, nil
			case "topic hz /chatter":
				_, _ = io.WriteString(stdout, payload)
				return 0, nil
			}
			return 1, nil
		},
	}
	svc := newTestROS2Service(t, rt, t.TempDir())
	stream := &fakeServerStream[agentpbv2.ROS2HzSample]{ctx: context.Background()}
	if err := withDeadline(t, func() error {
		return svc.MonitorHz(&agentpbv2.MonitorROS2HzRequest{Topic: "/chatter"}, stream)
	}); err != nil {
		t.Fatalf("a 128 KiB line is under the cap and must not fail: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("sent %d samples, want 1", len(stream.sent))
	}
}

func TestROS2Service_MonitorHz_OversizedLineIsAnError(t *testing.T) {
	// Past the cap: must be an explicit error, not a silently-ended stream.
	long := strings.Repeat("z", ros2HzMaxLineBytes+1024)
	payload := "average rate: 10.000\n" + long + "\n"
	rt := &fakeROS2Runtime{
		sidecar: ROS2Sidecar{Name: "sc", Distro: "humble", DomainID: 5},
		execFn: func(_ context.Context, opts ROS2ExecOptions, stdout, _ io.Writer) (int, error) {
			switch strings.Join(opts.Args, " ") {
			case "topic list":
				_, _ = io.WriteString(stdout, "/chatter\n")
				return 0, nil
			case "topic hz /chatter":
				_, _ = io.WriteString(stdout, payload)
				return 0, nil
			}
			return 1, nil
		},
	}
	svc := newTestROS2Service(t, rt, t.TempDir())
	stream := &fakeServerStream[agentpbv2.ROS2HzSample]{ctx: context.Background()}

	err := withDeadline(t, func() error {
		return svc.MonitorHz(&agentpbv2.MonitorROS2HzRequest{Topic: "/chatter"}, stream)
	})
	if err == nil {
		t.Fatal("MonitorHz returned nil after a scanner error")
	}
	if got := status.Code(err); got != codes.ResourceExhausted {
		t.Errorf("status code = %v, want ResourceExhausted (err: %v)", got, err)
	}
}

func TestROS2Service_MonitorHz_NormalOutputStillStreams(t *testing.T) {
	payload := "average rate: 10.000\n\tmin: 0.099s max: 0.101s std dev: 0.00050s window: 11\n"
	rt := &fakeROS2Runtime{
		sidecar: ROS2Sidecar{Name: "sc", Distro: "humble", DomainID: 5},
		execFn: func(_ context.Context, opts ROS2ExecOptions, stdout, _ io.Writer) (int, error) {
			switch strings.Join(opts.Args, " ") {
			case "topic list":
				_, _ = io.WriteString(stdout, "/chatter\n")
				return 0, nil
			case "topic hz /chatter":
				_, _ = io.WriteString(stdout, payload)
				return 0, nil
			}
			return 1, nil
		},
	}
	svc := newTestROS2Service(t, rt, t.TempDir())
	stream := &fakeServerStream[agentpbv2.ROS2HzSample]{ctx: context.Background()}
	if err := withDeadline(t, func() error {
		return svc.MonitorHz(&agentpbv2.MonitorROS2HzRequest{Topic: "/chatter"}, stream)
	}); err != nil {
		t.Fatalf("MonitorHz: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("sent %d samples, want 1", len(stream.sent))
	}
	if stream.sent[0].GetHz() != 10.0 {
		t.Errorf("hz = %v, want 10", stream.sent[0].GetHz())
	}
}

// A client that goes away mid-stream is not an error, and must stay that way.
func TestROS2Service_EchoTopic_ClientCancelIsNotAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rt := &fakeROS2Runtime{
		sidecar: ROS2Sidecar{Name: "sc", Distro: "humble", DomainID: 5},
		execFn: func(ectx context.Context, opts ROS2ExecOptions, stdout, _ io.Writer) (int, error) {
			if strings.Join(opts.Args, " ") == "topic list" {
				_, _ = io.WriteString(stdout, "/chatter\n")
				return 0, nil
			}
			_, _ = io.WriteString(stdout, "data: a\n---\n")
			<-ectx.Done()
			return 0, ectx.Err()
		},
	}
	svc := newTestROS2Service(t, rt, t.TempDir())
	stream := &fakeServerStream[agentpbv2.ROS2Message]{
		ctx: ctx,
		onSend: func(int) error {
			cancel()
			return nil
		},
	}
	if err := withDeadline(t, func() error {
		return svc.EchoTopic(&agentpbv2.EchoROS2TopicRequest{Topic: "/chatter"}, stream)
	}); err != nil {
		t.Fatalf("client cancel should not be an error, got: %v", err)
	}
}

func TestROS2Service_SubscriptionFailurePreservesDiagnostics(t *testing.T) {
	for _, command := range []string{"echo", "hz"} {
		t.Run(command, func(t *testing.T) {
			rt := &fakeROS2Runtime{
				sidecar: ROS2Sidecar{Name: "sc", Distro: "humble", DomainID: 5},
				execFn: func(_ context.Context, opts ROS2ExecOptions, _, stderr io.Writer) (int, error) {
					if got := strings.Join(opts.Args, " "); got != "topic "+command+" /lowstate" {
						t.Errorf("unexpected command: %s", got)
					}
					// Retain the final diagnostic even after a very long traceback.
					_, _ = io.WriteString(stderr, strings.Repeat("traceback\n", 10000))
					_, _ = io.WriteString(stderr, "Unknown package 'unitree_go'\n")
					return 1, nil
				},
			}
			svc := newTestROS2Service(t, rt, t.TempDir())
			err := withDeadline(t, func() error {
				if command == "echo" {
					return svc.EchoTopic(&agentpbv2.EchoROS2TopicRequest{Topic: "/lowstate"},
						&fakeServerStream[agentpbv2.ROS2Message]{ctx: context.Background()})
				}
				return svc.MonitorHz(&agentpbv2.MonitorROS2HzRequest{Topic: "/lowstate"},
					&fakeServerStream[agentpbv2.ROS2HzSample]{ctx: context.Background()})
			})
			if status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "Unknown package 'unitree_go'") || !strings.Contains(err.Error(), "exit 1") {
				t.Fatalf("expected explicit subscription failure with decoding diagnostic, got %v", err)
			}
			if len(err.Error()) > ros2StderrMaxBytes+256 {
				t.Fatalf("error retained unbounded stderr: %d bytes", len(err.Error()))
			}
		})
	}
}

func TestROS2Service_EchoTopic_MultilineDocumentIsBounded(t *testing.T) {
	payload := strings.Repeat("- 1234567890\n", ros2EchoMaxMessageBytes/10) + "---\n"
	svc := newTestROS2Service(t, echoRuntime("/points", payload), t.TempDir())
	stream := &fakeServerStream[agentpbv2.ROS2Message]{ctx: context.Background()}
	err := withDeadline(t, func() error {
		return svc.EchoTopic(&agentpbv2.EchoROS2TopicRequest{Topic: "/points"}, stream)
	})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("expected oversized document error, got %v", err)
	}
	if len(stream.sent) != 0 {
		t.Fatal("oversized document must not be sent")
	}
}

func TestROS2Service_EchoTopic_CountLimitIgnoresTeardownExit(t *testing.T) {
	rt := &fakeROS2Runtime{
		sidecar: ROS2Sidecar{Name: "sc", Distro: "humble", DomainID: 5},
		execFn: func(ctx context.Context, _ ROS2ExecOptions, stdout, stderr io.Writer) (int, error) {
			_, _ = io.WriteString(stdout, "data: 1\n---\n")
			<-ctx.Done()
			_, _ = io.WriteString(stderr, "subscription interrupted")
			return 130, ctx.Err()
		},
	}
	svc := newTestROS2Service(t, rt, t.TempDir())
	stream := &fakeServerStream[agentpbv2.ROS2Message]{ctx: context.Background()}
	err := withDeadline(t, func() error {
		return svc.EchoTopic(&agentpbv2.EchoROS2TopicRequest{Topic: "/lowstate", Count: 1}, stream)
	})
	if err != nil || len(stream.sent) != 1 {
		t.Fatalf("bounded subscription should succeed after one sample: samples=%d err=%v", len(stream.sent), err)
	}
}
