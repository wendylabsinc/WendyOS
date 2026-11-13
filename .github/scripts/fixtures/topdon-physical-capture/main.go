// Explicit bench-only capture of unmodified Agent raw frames. It never opens a
// camera node itself, changes a capture mode, or contacts Cloud.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

type receipt struct {
	File       string `json:"file"`
	SHA256     string `json:"sha256"`
	RawSHA256  string `json:"raw_sha256"`
	ReceivedNS int64  `json:"received_ns,string"`
}

type recording struct {
	StableID           string    `json:"stable_id"`
	Started            string    `json:"started"`
	Finished           string    `json:"finished"`
	Status             string    `json:"status"`
	Error              string    `json:"error,omitempty"`
	Frames             []receipt `json:"frames"`
	PhysicalAcceptance string    `json:"physical_acceptance"`
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func capture(ctx context.Context, client pb.WendyVideoServiceClient, stableID, output string, limit int) (err error) {
	if err = os.Mkdir(output, 0o700); err != nil {
		return err
	}
	result := recording{StableID: stableID, Started: time.Now().UTC().Format(time.RFC3339Nano),
		Status: "incomplete", PhysicalAcceptance: "requires_identity_reference_and_replay_review"}
	defer func() {
		result.Finished = time.Now().UTC().Format(time.RFC3339Nano)
		if err != nil {
			result.Error = err.Error()
		}
		data, marshalErr := json.MarshalIndent(result, "", "  ")
		if marshalErr == nil {
			marshalErr = os.WriteFile(filepath.Join(output, "manifest.json"), data, 0o600)
		}
		err = errors.Join(err, marshalErr)
	}()
	listed, err := client.ListVideoDevices(ctx, &pb.ListVideoDevicesRequest{})
	if err != nil {
		return err
	}
	var selected *pb.VideoDevice
	for _, camera := range listed.GetDevices() {
		if camera.GetStableId() == stableID {
			if selected != nil {
				return errors.New("ambiguous stable camera identity")
			}
			selected = camera
		}
	}
	if selected == nil || selected.GetThermal().GetAdapter() != "topdon-tc001-hik-v1" ||
		selected.GetThermal().GetCaptureWidth() != 96 || selected.GetThermal().GetCaptureHeight() != 200 {
		return errors.New("exact camera does not advertise the supported 96x200 HIK capture")
	}
	data, err := protojson.Marshal(selected)
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(output, "camera.json"), data, 0o600); err != nil {
		return err
	}
	// Zero geometry/FPS joins the production shared capture without overriding it.
	stream, err := client.StreamVideo(ctx, &pb.StreamVideoRequest{DeviceId: selected.GetId(), StableId: stableID, Codec: pb.VideoCodec_VIDEO_CODEC_RAW})
	if err != nil {
		return err
	}
	for len(result.Frames) < limit {
		frame, recvErr := stream.Recv()
		received := time.Now().UnixNano()
		if recvErr != nil {
			if status.Code(recvErr) == codes.DeadlineExceeded && errors.Is(ctx.Err(), context.DeadlineExceeded) && len(result.Frames) >= 2 {
				result.Status = "captured"
				return nil
			}
			return fmt.Errorf("capture ended after %d frames: %w", len(result.Frames), recvErr)
		}
		layout := frame.GetRawFormat()
		plane := layout.GetThermalPlane()
		if frame.GetCodec() != pb.VideoCodec_VIDEO_CODEC_RAW || frame.GetTimestampNs() == 0 ||
			layout.GetFourcc() != "YUYV" || layout.GetWidth() != 96 || layout.GetHeight() != 200 || layout.GetBytesPerLine() != 192 ||
			len(frame.GetData()) != 38400 || plane.GetVersion() != 1 || plane.GetAdapter() != "topdon-tc001-hik-v1" ||
			plane.GetWidth() != 96 || plane.GetHeight() != 96 || plane.GetRowStrideBytes() != 192 {
			return errors.New("unexpected raw frame layout or missing timestamp; capture is incomplete")
		}
		data, err := protojson.Marshal(frame)
		if err != nil {
			return err
		}
		name := fmt.Sprintf("frame-%06d.json", len(result.Frames))
		if err = os.WriteFile(filepath.Join(output, name), data, 0o600); err != nil {
			return err
		}
		result.Frames = append(result.Frames, receipt{File: name, SHA256: digest(data), RawSHA256: digest(frame.GetData()), ReceivedNS: received})
	}
	result.Status = "captured"
	return nil
}

func main() {
	socket := flag.String("socket", "", "Observed local Agent admin Unix socket; never a network endpoint")
	stable := flag.String("stable-id", "", "Exact physically confirmed camera stable ID")
	output := flag.String("output", "", "New private output directory")
	duration := flag.Duration("duration", 20*time.Second, "Capture deadline, 5s to 120s")
	limit := flag.Int("frames", 500, "Maximum frames, 2 to 2000; at most about 103 MB")
	flag.Parse()
	info, err := os.Stat(*socket)
	if err != nil || !filepath.IsAbs(*socket) || info.Mode()&os.ModeSocket == 0 || *stable == "" || *output == "" || *duration < 5*time.Second || *duration > 120*time.Second || *limit < 2 || *limit > 2000 || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "provide an existing absolute Unix socket, exact stable ID, new output and bounded duration/frame limit")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *duration)
	defer cancel()
	conn, err := grpc.NewClient("unix://"+*socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err == nil {
		defer conn.Close()
		err = capture(ctx, pb.NewWendyVideoServiceClient(conn), *stable, *output, *limit)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
