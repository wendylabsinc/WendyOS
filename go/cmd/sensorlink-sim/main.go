// Command sensorlink-sim is a manual end-to-end driver: it serves the
// sensorlink wire protocol over TCP, looping a single JPEG file as a MJPEG
// camera frame, so mcusource.Supervisor / ipcam.NewLoopback can be exercised
// against a real (if fake) remote sensor source without hardware.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink/sim"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:50060", "listen address")
	jpeg := flag.String("jpeg", "", "path to a JPEG file to loop as the camera frame")
	flag.Parse()

	data, err := os.ReadFile(*jpeg)
	if err != nil {
		log.Fatalf("read jpeg: %v", err)
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("sensorlink simulator on %s", *addr)
	_ = sim.Serve(context.Background(), ln, sim.Options{
		Manifest: &sensorlinkpb.SensorManifest{DeviceAssetId: 1, Sensors: []*sensorlinkpb.SensorDescriptor{{
			ChannelId: 1, Kind: sensorlinkpb.SensorDescriptor_CAMERA, Name: "cam0",
			Format: &sensorlinkpb.SensorDescriptor_Video{Video: &sensorlinkpb.VideoFormat{Codec: sensorlinkpb.VideoFormat_MJPEG, Width: 640, Height: 480, Fps: 30}},
		}}},
		Frames:        [][]byte{data},
		FrameInterval: 33 * time.Millisecond,
	})
}
