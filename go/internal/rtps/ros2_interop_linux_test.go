//go:build linux

package rtps

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/rtps/cdr"
)

// The interop tests publish with this package's writer and require real ROS 2
// nodes to receive the samples. They need a ROS 2 install with rclpy, so they
// run through .github/scripts/test-rtps-publisher-ros2.sh, which starts them
// inside a ROS 2 image, and are skipped everywhere else.
const interopSubscriberEnv = "WENDY_ROS2_INTEROP_SUBSCRIBER"

type interopRecord struct {
	Ready *bool `json:"ready"`

	Temperature *float64 `json:"temperature"`
	Variance    float64  `json:"variance"`
	FrameID     string   `json:"frame_id"`
	Sec         int64    `json:"sec"`

	Data       []int   `json:"data"`
	DataOffset int     `json:"data_offset"`
	Dim        [][]any `json:"dim"`

	Graph *struct {
		Types      []string `json:"types"`
		Publishers int      `json:"publishers"`
	} `json:"graph"`
}

// interopSubscriber runs the rclpy subscriber and collects what it prints.
type interopSubscriber struct {
	cmd   *exec.Cmd
	ready chan struct{}
	done  chan struct{}

	mu      sync.Mutex
	records []interopRecord
	err     error
}

func startInteropSubscriber(ctx context.Context, t *testing.T, rmw string, domain int, topic, typ, reliability string, count int) *interopSubscriber {
	t.Helper()
	cmd := exec.CommandContext(ctx, "python3", os.Getenv(interopSubscriberEnv),
		"--topic", topic, "--type", typ, "--reliability", reliability,
		"--count", strconv.Itoa(count), "--timeout", "30")
	// Loopback only is what an app with ROS 2 scope "app" or "device" gets.
	// WENDY_ROS2_INTEROP_LOCALHOST_ONLY=0 runs the node with wider discovery
	// instead, as scope "network" does, while the writer stays on loopback.
	localhostOnly := os.Getenv("WENDY_ROS2_INTEROP_LOCALHOST_ONLY")
	if localhostOnly == "" {
		localhostOnly = "1"
	}
	cmd.Env = append(os.Environ(),
		"RMW_IMPLEMENTATION="+rmw, "ROS_DOMAIN_ID="+strconv.Itoa(domain), "ROS_LOCALHOST_ONLY="+localhostOnly)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := &interopSubscriber{cmd: cmd, ready: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 1<<20), 1<<20)
		announced := false
		for scanner.Scan() {
			var record interopRecord
			if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
				continue
			}
			if record.Ready != nil {
				if !announced {
					announced = true
					close(s.ready)
				}
				continue
			}
			s.mu.Lock()
			s.records = append(s.records, record)
			s.mu.Unlock()
		}
		err := cmd.Wait()
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		if !announced {
			close(s.ready)
		}
	}()
	return s
}

// interopPublisher is a publishing participant on the loopback interface of
// the ROS 2 container, as the agent would run one inside an app's network.
func interopPublisher(ctx context.Context, t *testing.T, domain int, pub Publication) *Writer {
	t.Helper()
	cfg := Config{DomainID: domain, Interface: "lo", Publish: true}
	if os.Getenv("WENDY_ROS2_INTEROP_VERBOSE") != "" {
		start := time.Now()
		cfg.Logf = func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "    [domain %d +%.3fs] %s\n", domain, time.Since(start).Seconds(), fmt.Sprintf(format, args...))
		}
	}
	p, err := NewParticipant(cfg)
	if err != nil {
		t.Fatal(err)
	}
	go p.Run(ctx)
	w, err := p.NewWriter(pub)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			data, heartbeats := p.WriterHistogram()
			t.Logf("publisher saw %+v; matched readers %d; DATA by writer %v; HEARTBEAT by writer %v", p.Stats(), w.Matched(), data, heartbeats)
		}
	})
	return w
}

// publishUntil writes one sample every 20 ms until the subscriber exits.
func publishUntil(t *testing.T, s *interopSubscriber, write func() error) {
	t.Helper()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			if err := write(); err != nil {
				t.Fatalf("Write: %v", err)
			}
		}
	}
}

func temperaturePayload(frameID string, celsius float64) []byte {
	now := time.Now()
	e := cdr.NewEncoder()
	e.Int32(int32(now.Unix())) // std_msgs/Header: stamp.sec, stamp.nanosec, frame_id
	e.Uint32(uint32(now.Nanosecond()))
	e.String(frameID)
	e.Float64(celsius) // temperature
	e.Float64(0.25)    // variance
	return e.Payload()
}

func TestROS2NodesReceiveTemperatureSamples(t *testing.T) {
	if os.Getenv(interopSubscriberEnv) == "" {
		t.Skip("needs a ROS 2 install; run .github/scripts/test-rtps-publisher-ros2.sh")
	}
	const topic, count = "/wendy_interop/temperature", 25
	domain := 40
	for _, rmw := range []string{"rmw_cyclonedds_cpp", "rmw_fastrtps_cpp"} {
		for _, reliability := range []string{"reliable", "best_effort"} {
			for _, order := range []string{"subscriber_first", "writer_far_ahead"} {
				domain++
				domain := domain
				t.Run(rmw+"/"+reliability+"/"+order, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
					defer cancel()
					frameID := fmt.Sprintf("wendy-%d-%d", domain, time.Now().UnixNano())
					var next float64
					var w *Writer
					write := func() error {
						next++
						return w.Write(temperaturePayload(frameID, next))
					}
					newWriter := func() {
						w = interopPublisher(ctx, t, domain, Publication{Topic: "rt" + topic, Type: "sensor_msgs::msg::dds_::Temperature_"})
					}
					var floor float64
					var s *interopSubscriber
					if order == "subscriber_first" {
						s = startInteropSubscriber(ctx, t, rmw, domain, topic, "temperature", reliability, count)
						<-s.ready
						newWriter()
					} else {
						// The reader joins a stream that is already hundreds of
						// samples in. A reliable reader must not wait for sample 1.
						newWriter()
						for range 400 {
							if err := write(); err != nil {
								t.Fatal(err)
							}
						}
						floor = next
						s = startInteropSubscriber(ctx, t, rmw, domain, topic, "temperature", reliability, count)
						<-s.ready
					}
					publishUntil(t, s, write)

					s.mu.Lock()
					defer s.mu.Unlock()
					if s.err != nil {
						t.Fatalf("subscriber did not receive %d messages: %v (%d records)", count, s.err, len(s.records))
					}
					var previous float64
					var messages int
					var graph *interopRecord
					for i := range s.records {
						r := &s.records[i]
						if r.Graph != nil {
							graph = r
							continue
						}
						if r.Temperature == nil {
							t.Fatalf("unexpected record %+v", r)
						}
						messages++
						if r.FrameID != frameID || r.Variance != 0.25 || time.Since(time.Unix(r.Sec, 0)) > time.Minute {
							t.Fatalf("message %d decoded as %+v, want frame %q, variance 0.25 and a current stamp", messages, r, frameID)
						}
						value := *r.Temperature
						if value != float64(int64(value)) || value <= previous || value <= floor {
							t.Fatalf("message %d has temperature %v after %v (writer was at %v when the reader started)", messages, value, previous, floor)
						}
						previous = value
					}
					if messages < count {
						t.Fatalf("received %d messages, want %d", messages, count)
					}
					if graph == nil || graph.Graph.Publishers < 1 || len(graph.Graph.Types) != 1 || graph.Graph.Types[0] != "sensor_msgs/msg/Temperature" {
						t.Fatalf("ROS 2 graph reports %+v for the topic, want one publisher of sensor_msgs/msg/Temperature", graph)
					}
				})
			}
		}
	}
}

func TestROS2NodesReceiveRawByteSamples(t *testing.T) {
	if os.Getenv(interopSubscriberEnv) == "" {
		t.Skip("needs a ROS 2 install; run .github/scripts/test-rtps-publisher-ros2.sh")
	}
	const topic, count = "/wendy_interop/raw", 5
	domain := 70
	for _, rmw := range []string{"rmw_cyclonedds_cpp", "rmw_fastrtps_cpp"} {
		domain++
		domain := domain
		t.Run(rmw, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			s := startInteropSubscriber(ctx, t, rmw, domain, topic, "bytes", "reliable", count)
			<-s.ready
			w := interopPublisher(ctx, t, domain, Publication{Topic: "rt" + topic, Type: "std_msgs::msg::dds_::UInt8MultiArray_"})
			// Odd lengths exercise the padding a writer adds after the payload.
			var length int
			publishUntil(t, s, func() error {
				length = length%7 + 1
				e := cdr.NewEncoder()
				e.Uint32(0) // layout.dim: empty sequence
				e.Uint32(0) // layout.data_offset
				data := make([]byte, length)
				for i := range data {
					data[i] = byte(length*16 + i)
				}
				e.Bytes(data)
				return w.Write(e.Payload())
			})

			s.mu.Lock()
			defer s.mu.Unlock()
			if s.err != nil {
				t.Fatalf("subscriber did not receive %d messages: %v (%d records)", count, s.err, len(s.records))
			}
			var messages int
			for _, r := range s.records {
				if r.Graph != nil {
					continue
				}
				messages++
				if len(r.Data) == 0 || len(r.Data) > 7 || r.DataOffset != 0 || len(r.Dim) != 0 {
					t.Fatalf("message %d decoded as %+v", messages, r)
				}
				for i, b := range r.Data {
					if b != len(r.Data)*16+i {
						t.Fatalf("message %d has data %v, want bytes that start at %d", messages, r.Data, len(r.Data)*16)
					}
				}
			}
			if messages < count {
				t.Fatalf("received %d messages, want %d", messages, count)
			}
		})
	}
}
