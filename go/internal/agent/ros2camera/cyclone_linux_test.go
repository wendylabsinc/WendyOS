//go:build linux

package ros2camera

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/rtps"
)

// Run only in disposable Linux network and mount namespaces, with ROS 2
// Humble's rclpy, sensor_msgs and rmw_cyclonedds_cpp environment sourced.
// The real publisher, UDP sockets, fragment reassembly and image decoder run;
// the V4L2 sink is replaced because the fixture needs no camera/kernel module.
func TestCycloneImageLinux(t *testing.T) {
	if os.Getenv("WENDY_TEST_ROS2") != "1" {
		t.Skip("requires isolated Linux net/mount namespaces, root, ip and ROS 2 CycloneDDS")
	}
	if os.Geteuid() != 0 {
		t.Fatal("requires root")
	}
	for _, ns := range []string{"net", "mnt"} {
		self, err := os.Readlink("/proc/self/ns/" + ns)
		if err != nil {
			t.Fatal(err)
		}
		init, err := os.Readlink("/proc/1/ns/" + ns)
		if err != nil {
			t.Fatal(err)
		}
		if self == init {
			t.Fatalf("requires private %s namespace", ns)
		}
	}
	ip := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
			t.Fatalf("ip %v: %s: %v", args, out, err)
		}
	}
	ip("link", "set", "lo", "up")
	for _, lan := range []bool{false, true} {
		t.Run(fmt.Sprintf("LAN=%v", lan), func(t *testing.T) {
			iface, pubIface := "lo", "lo"
			var prefix []string
			if lan {
				ns := fmt.Sprintf("wendy-camera-%d", os.Getpid())
				ip("netns", "add", ns)
				t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", ns).Run() })
				ip("link", "add", "agent0", "type", "veth", "peer", "name", "publisher0")
				t.Cleanup(func() { _ = exec.Command("ip", "link", "del", "agent0").Run() })
				ip("link", "set", "publisher0", "netns", ns)
				ip("addr", "add", "192.0.2.1/24", "dev", "agent0")
				ip("link", "set", "agent0", "up")
				ip("-n", ns, "link", "set", "lo", "up")
				ip("-n", ns, "addr", "add", "192.0.2.2/24", "dev", "publisher0")
				ip("-n", ns, "link", "set", "publisher0", "up")
				iface, pubIface = "agent0", "publisher0"
				prefix = []string{"ip", "netns", "exec", ns}
			}
			dir := t.TempDir()
			config := fmt.Sprintf(`<CycloneDDS><Domain><General><Interfaces><NetworkInterface name="%s" multicast="true"/></Interfaces><AllowMulticast>false</AllowMulticast></General><Discovery><ParticipantIndex>auto</ParticipantIndex><MaxAutoParticipantIndex>64</MaxAutoParticipantIndex><Peers><Peer Address="127.0.0.1"/></Peers></Discovery><Tracing><Verbosity>finest</Verbosity><OutputFile>%s</OutputFile></Tracing></Domain></CycloneDDS>`, pubIface, filepath.Join(dir, "cyclone.log"))
			if lan {
				config = fmt.Sprintf(`<CycloneDDS><Domain><General><Interfaces><NetworkInterface name="%s"/></Interfaces><AllowMulticast>true</AllowMulticast></General><Tracing><Verbosity>finest</Verbosity><OutputFile>%s</OutputFile></Tracing></Domain></CycloneDDS>`, pubIface, filepath.Join(dir, "cyclone.log"))
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			m := NewManager(ctx, zap.NewNop(), &fakeLoopback{}, filepath.Join(dir, "registry.json"), nil, nil)
			m.hostInterfaces = func() ([]string, error) { return []string{iface}, nil }
			m.acquire = func(ctx context.Context, cfg rtps.Config) (discoveryLease, error) {
				cfg.Logf = t.Logf
				l, err := m.pool.Acquire(ctx, cfg)
				if err == nil {
					ep := rtps.Endpoint{Topic: "rt/unused", Type: TypeImage, GUID: rtps.GUID{EntityID: 99}}
					if err := l.Subscribe(ep); err != nil {
						return nil, err
					}
					l.Unsubscribe(ep.GUID)
				}
				return l, err
			}
			writer := &cycloneWriter{}
			m.newWriter = func(string) cameraWriter { return writer }
			m.Start()
			t.Cleanup(m.Shutdown)
			// Establish discovery history before this publisher joins. A shared
			// participant can have this history from any earlier ROS consumer.
			m.Refresh(ctx)
			args := append(prefix, "python3", "testdata/image_publisher.py")
			cmd := exec.CommandContext(ctx, args[0], args[1:]...)
			cmd.Env = append(os.Environ(), "RMW_IMPLEMENTATION=rmw_cyclonedds_cpp", "CYCLONEDDS_URI="+config)
			log, err := os.Create(filepath.Join(dir, "publisher.log"))
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stdout, cmd.Stderr = log, log
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cancel()
				_ = cmd.Wait()
				_ = log.Close()
				for _, name := range []string{"publisher.log", "cyclone.log"} {
					data, _ := os.ReadFile(filepath.Join(dir, name))
					if dest := os.Getenv("WENDY_ROS2_EVIDENCE"); dest != "" {
						_ = os.WriteFile(filepath.Join(dest, fmt.Sprintf("lan-%v-%s", lan, name)), data, 0600)
					}
					if name == "publisher.log" {
						t.Logf("%s: %s", name, data)
					}
				}
			})
			deadline := time.Now().Add(15 * time.Second)
			var cameras []Camera
			for time.Now().Before(deadline) {
				cameras = m.List()
				if len(cameras) != 0 {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if len(cameras) != 1 {
				t.Fatalf("cameras: %+v", cameras)
			}
			t.Logf("discovered %+v", cameras[0])
			cam, release, err := m.Acquire(ctx, cameras[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			release()
			cam, release, err = m.Acquire(ctx, cameras[0].ID)
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			defer release()
			writer.mu.Lock()
			defer writer.mu.Unlock()
			if writer.frame.Width != 640 || writer.frame.Height != 360 || writer.frame.Codec != CodecMJPEG || len(writer.frame.Data) == 0 {
				t.Fatalf("invalid decoded frame: %dx%d codec=%d bytes=%d", writer.frame.Width, writer.frame.Height, writer.frame.Codec, len(writer.frame.Data))
			}
			t.Logf("acquired %s: 640x360 MJPEG, %d bytes", cam.Topic, len(writer.frame.Data))
		})
	}
}

type cycloneWriter struct {
	mu    sync.Mutex
	frame Frame
}

func (w *cycloneWriter) WriteFrame(f Frame) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.frame = f
	return nil
}
func (*cycloneWriter) Close() error { return nil }
