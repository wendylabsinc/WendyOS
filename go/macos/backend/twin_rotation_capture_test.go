package backend

import (
	"context"
	"encoding/json"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"os"
	"os/exec"
	"testing"
	"time"
)

// Explicitly opt-in, read-only capture. Never opens a control handle or sends a command.
func TestTwinRotationCapture(t *testing.T) {
	device, output := os.Getenv("WENDY_ROTATION_DEVICE"), os.Getenv("WENDY_ROTATION_OUTPUT")
	if device == "" || output == "" {
		t.Skip("requires an explicit device and local output path")
	}
	b := New()
	defer b.Shutdown()
	cli, err := exec.LookPath("wendy")
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Initialize(cli); err != nil {
		t.Fatal(err)
	}
	session, err := b.Connect(device, false)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := b.connection(session)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	paired, err := conn.UnitreeService.ListUnitreePairings(ctx, &pb.ListUnitreePairingsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	serial := ""
	for _, r := range paired.GetRobots() {
		if r.Paired && r.Connected {
			if serial != "" {
				t.Fatal("select a gateway with exactly one connected robot")
			}
			serial = r.Serial
		}
	}
	if serial == "" {
		t.Fatal("no connected robot")
	}
	stream, err := pb.NewWendyUnitreeRobotServiceClient(conn.Conn).Stream(ctx, &pb.UnitreeStreamRequest{Serial: serial, Topics: []string{"rt/lf/sportmodestate", "rt/utlidar/robot_pose", "rt/utlidar/voxel_map_compressed"}})
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	counts := map[string]int{}
	size := 0
	for size < 32<<20 {
		message, e := stream.Recv()
		if e != nil {
			if ctx.Err() != nil {
				break
			}
			t.Fatal(e)
		}
		packet := message.GetTopic()
		if packet == nil {
			continue
		}
		counts[packet.Topic]++
		size += len(packet.Data)
		if e = encoder.Encode(message); e != nil {
			t.Fatal(e)
		}
	}
	if len(counts) == 0 {
		t.Fatal("no observations received")
	}
	t.Log("captured topic counts", counts)
}
