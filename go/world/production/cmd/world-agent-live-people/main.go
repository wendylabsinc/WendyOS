package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/liveapp"
	"github.com/wendylabsinc/wendy/go/world/production/livepeople"
	"github.com/wendylabsinc/wendy/go/world/production/poseprocess"
)

func main() { os.Exit(run()) }
func run() int {
	flags := flag.NewFlagSet("world-agent-live-people", flag.ContinueOnError)
	path := flags.String("config", "", "absolute public-root simulation commissioning file")
	python := flags.String("pose-python", "/usr/local/bin/python3", "absolute pose Python executable")
	script := flags.String("pose-script", "/app/perception/pose.py", "absolute image-only worker script")
	model := flags.String("pose-model", "/app/model.onnx", "absolute commissioned ONNX model")
	if flags.Parse(os.Args[1:]) != nil || flags.NArg() != 0 || *path == "" || !filepath.IsAbs(*python) || !filepath.IsAbs(*script) || !filepath.IsAbs(*model) {
		return 2
	}
	cfg, err := livepeople.LoadConfig(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "live people configuration refused")
		return 2
	}
	conn, err := liveapp.Dial(os.Getenv("WENDY_WORLD_SOCKET"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "explicit World service socket required")
		return 2
	}
	defer conn.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dir, err := os.MkdirTemp("", "world-people-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "pose temporary directory unavailable")
		return 1
	}
	defer os.RemoveAll(dir)
	worker, err := poseprocess.New(ctx, poseprocess.Config{Command: []string{*python, *script, "--model", *model, "--serve"}, Directory: dir, Timeout: 4 * time.Second})
	if err != nil {
		fmt.Fprintln(os.Stderr, "image-only pose worker unavailable")
		return 1
	}
	defer worker.Close()
	encoder := json.NewEncoder(os.Stdout)
	summary, err := livepeople.RunNative(ctx, cfg, appv1.NewWorldAppServiceClient(conn), worker.Infer, func(r livepeople.Result) error { return encoder.Encode(r) })
	if err != nil {
		fmt.Fprintln(os.Stderr, "live people stopped:", err)
	}
	if e := encoder.Encode(struct {
		Summary livepeople.Summary `json:"summary"`
	}{summary}); e != nil || err != nil {
		return 1
	}
	return 0
}
