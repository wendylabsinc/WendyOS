package vm

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

type cancelTestImage struct {
	read          func([]byte) (int, error)
	writerToCalls int
}

func (r *cancelTestImage) Read(p []byte) (int, error) { return r.read(p) }

func (r *cancelTestImage) WriteTo(io.Writer) (int64, error) {
	r.writerToCalls++
	return 0, errors.New("optimized copy bypassed cancellation checks")
}

func TestCreateFromContextAlreadyCanceledHasNoSideEffects(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	image := &cancelTestImage{read: func([]byte) (int, error) {
		t.Fatal("read image after cancellation")
		return 0, io.EOF
	}}
	err := s.CreateFromContext(ctx, "dev", image, 0, 1024, Meta{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CreateFromContext() = %v, want cancellation", err)
	}
	if _, err := os.Stat(s.Root); !os.IsNotExist(err) {
		t.Fatalf("canceled create touched the VM store: %v", err)
	}
	if image.writerToCalls != 0 {
		t.Fatal("canceled create used the source's optimized copy")
	}
}

func TestCreateFromContextCancelsDuringCopyAndAllowsRetry(t *testing.T) {
	for _, withProfile := range []bool{false, true} {
		for _, end := range []string{"more data", "final chunk", "end of stream"} {
			name := end
			if withProfile {
				name += " with robot profile"
			}
			t.Run(name, func(t *testing.T) {
				s := newTestStore(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var profile *RobotProfile
				if withProfile {
					p, err := NewRobotProfile(RobotKindG1, "sha256:"+strings.Repeat("0", 64), "bundle")
					if err != nil {
						t.Fatal(err)
					}
					profile = &p
				}
				create := func(ctx context.Context, image io.Reader) error {
					if profile != nil {
						return s.CreateFromWithRobotProfileContext(ctx, "dev", image, 0, 1024, Meta{}, profile)
					}
					return s.CreateFromContext(ctx, "dev", image, 0, 1024, Meta{})
				}
				reads := 0
				image := &cancelTestImage{read: func(p []byte) (int, error) {
					reads++
					if reads == 1 {
						return copy(p, "partial image"), nil
					}
					if reads > 2 {
						t.Error("copy kept reading after cancellation")
						return 0, io.EOF
					}
					// Cancel after the copy has written its first chunk to disk.
					partial, err := os.ReadFile(s.DiskPath("dev"))
					if err != nil || string(partial) != "partial image" {
						t.Fatalf("copy did not start before cancellation: %q, %v", partial, err)
					}
					cancel()
					switch end {
					case "more data":
						return copy(p, "discarded"), nil
					case "final chunk":
						return copy(p, "discarded"), io.EOF
					default:
						return 0, io.EOF
					}
				}}
				if err := create(ctx, image); !errors.Is(err, context.Canceled) {
					t.Fatalf("CreateFromContext() = %v, want cancellation", err)
				}
				if reads != 2 || image.writerToCalls != 0 {
					t.Fatalf("reads=%d optimized copies=%d", reads, image.writerToCalls)
				}
				if _, err := os.Stat(s.Dir("dev")); !os.IsNotExist(err) {
					t.Fatalf("partial VM survived cancellation: %v", err)
				}
				if err := create(context.Background(), strings.NewReader("retry image")); err != nil {
					t.Fatalf("retry after cancellation: %v", err)
				}
				if _, exists := s.ReadMeta("dev"); !exists {
					t.Fatal("retry did not write VM metadata")
				}
				if profile != nil {
					got, exists, err := s.ReadRobotProfile("dev")
					if err != nil || !exists || got.Kind != profile.Kind {
						t.Fatalf("retry profile: %+v %v %v", got, exists, err)
					}
				}
			})
		}
	}
}
