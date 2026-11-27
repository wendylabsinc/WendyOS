package poseprocess

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/pose"
)

func TestWorkerHelper(t *testing.T) {
	if os.Args[len(os.Args)-1] != "pose-helper" {
		t.Skip("subprocess only")
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var r map[string]string
		if json.Unmarshal(scanner.Bytes(), &r) != nil || len(r) != 1 {
			os.Exit(20)
		}
		p := r["image_path"]
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm() != 0600 {
			os.Exit(21)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			os.Exit(22)
		}
		if string(data) == "delay" {
			time.Sleep(time.Second)
		}
		h := sha256.Sum256(data)
		digest := hex.EncodeToString(h[:])
		if string(data) == "tamper" {
			digest = strings.Repeat("b", 64)
		}
		value := pose.ImageResult{ModelID: "test", ModelVersion: "1", ModelSHA256: strings.Repeat("a", 64), ImageSHA256: digest, ImageWidth: 1, ImageHeight: 1, Detections: []pose.Person{}}
		if json.NewEncoder(os.Stdout).Encode(value) != nil {
			os.Exit(23)
		}
	}
	os.Exit(0)
}
func worker(t *testing.T, timeout time.Duration) (*Worker, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	w, err := New(context.Background(), Config{Command: []string{exe, "-test.run=^TestWorkerHelper$", "--", "pose-helper"}, Directory: dir, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := w.Close(); err != nil {
			t.Error(err)
		}
	})
	return w, dir
}
func TestPersistentWorkerBindsEachImageAndCleansFiles(t *testing.T) {
	w, dir := worker(t, 2*time.Second)
	for _, input := range []string{"frame-one", "frame-two"} {
		v, err := w.Infer(context.Background(), []byte(input))
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256([]byte(input))
		if v.ImageSHA256 != hex.EncodeToString(h[:]) {
			t.Fatal("image binding lost")
		}
		files, err := os.ReadDir(dir)
		if err != nil || len(files) != 0 {
			t.Fatalf("temporary images retained: %v %v", files, err)
		}
	}
}
func TestWrongImageOrTimeoutEndsWorker(t *testing.T) {
	for _, input := range []string{"tamper", "delay"} {
		t.Run(input, func(t *testing.T) {
			timeout := 2 * time.Second
			if input == "delay" {
				timeout = 100 * time.Millisecond
			}
			w, _ := worker(t, timeout)
			if _, err := w.Infer(context.Background(), []byte(input)); err == nil {
				t.Fatal("bad response accepted")
			}
			if _, err := w.Infer(context.Background(), []byte("next")); err == nil {
				t.Fatal("failed worker reused")
			}
		})
	}
}
