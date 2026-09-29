package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	wendymcp "github.com/wendylabsinc/wendy/go/internal/cli/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

func simulatorBackend() wendymcp.SimulatorBackend {
	return newSimulatorBackend(vm.NewStore, fetchMCPSimulatorImage)
}

type simulatorImageResolver func(context.Context, string) (string, string, func(), error)

func newSimulatorBackend(newStore func() (*vm.Store, error), resolve simulatorImageResolver) wendymcp.SimulatorBackend {
	return wendymcp.SimulatorBackend{
		List: func(ctx context.Context) ([]wendymcp.SimulatorInfo, error) {
			store, err := newStore()
			if err != nil {
				return nil, err
			}
			names, err := store.List()
			if err != nil {
				return nil, err
			}
			items := make([]wendymcp.SimulatorInfo, 0, len(names))
			for _, name := range names {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				info, err := readMCPSimulator(store, name)
				if err != nil {
					items = append(items, wendymcp.SimulatorInfo{Name: name, Device: "vm:" + name, State: "unknown", Error: err.Error()})
					continue
				}
				items = append(items, *info)
			}
			return items, nil
		},
		Create: func(ctx context.Context, options wendymcp.SimulatorCreateOptions) (*wendymcp.SimulatorInfo, error) {
			if err := options.Validate(); err != nil {
				return nil, err
			}
			store, err := newStore()
			if err != nil {
				return nil, err
			}
			return createMCPSimulator(ctx, store, options, resolve)
		},
		Stop: func(ctx context.Context, name string, force bool, timeout time.Duration) (*wendymcp.SimulatorInfo, error) {
			if timeout < time.Second || timeout > 120*time.Second {
				return nil, fmt.Errorf("shutdown timeout must be between 1 and 120 seconds")
			}
			store, err := newStore()
			if err != nil {
				return nil, err
			}
			if _, err := readMCPSimulator(store, name); err != nil {
				return nil, err
			}
			if err := store.StopContext(ctx, name, force, timeout); err != nil {
				return nil, err
			}
			return readMCPSimulator(store, name)
		},
		Delete: func(ctx context.Context, name string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			store, err := newStore()
			if err != nil {
				return err
			}
			st, err := store.Status(name)
			if err != nil {
				return err
			}
			if !st.Exists {
				return fmt.Errorf("simulator %q: %w", name, os.ErrNotExist)
			}
			// Remove checks again under the lifecycle/run locks. A racing start
			// cannot cause this call to delete a live disk.
			return store.Remove(name)
		},
	}
}

func readMCPSimulator(store *vm.Store, name string) (*wendymcp.SimulatorInfo, error) {
	st, err := store.Status(name)
	if err != nil {
		return nil, err
	}
	if !st.Exists {
		return nil, fmt.Errorf("simulator %q: %w", name, os.ErrNotExist)
	}
	info := &wendymcp.SimulatorInfo{
		Name: name, Device: "vm:" + name, State: vmStateLabel(st),
		Version: st.Meta.ImageVersion, Source: st.Meta.ImageSource,
		DiskBytes: st.Meta.DiskBytes, Address: vmAddress(st),
	}
	if _, ok := store.ReadMeta(name); !ok {
		info.Error = "simulator metadata is unavailable; creation may still be in progress"
		info.State = "unknown"
		return info, nil
	}
	profile, exists, err := store.ReadRobotProfile(name)
	if err != nil {
		info.Error = err.Error()
	} else if exists {
		info.Profile = profile.Kind
	} else {
		info.Profile = "generic"
	}
	return info, nil
}

func createMCPSimulator(ctx context.Context, store *vm.Store, opts wendymcp.SimulatorCreateOptions, resolve simulatorImageResolver) (*wendymcp.SimulatorInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := store.CheckCreatable(opts.Name); err != nil {
		return nil, err
	}
	var profile *vm.RobotProfile
	if opts.Profile != "generic" {
		runtime, err := robotRuntimeForKind(opts.Profile)
		if err != nil {
			return nil, err
		}
		p, err := vm.NewRobotProfile(opts.Profile, runtime.sourceDigest(), runtime.policyBundle)
		if err != nil {
			return nil, err
		}
		profile = &p
	}
	path, version, source := opts.Image, "", "local"
	if path == "" {
		var cleanup func()
		var err error
		path, version, cleanup, err = resolve(ctx, opts.Version)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		source = "release"
	}
	stat, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Size() == 0 {
		return nil, fmt.Errorf("image must be a nonempty regular file")
	}
	stream, err := openLocalImageStream(path)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	meta := vm.Meta{ImageVersion: version, ImageSource: source}
	reader := simulatorContextReader{ctx: ctx, reader: stream}
	if err := store.CreateFromWithRobotProfile(opts.Name, reader, stream.uncompressedSize, int64(opts.DiskGiB)<<30, meta, profile); err != nil {
		return nil, err
	}
	return readMCPSimulator(store, opts.Name)
}

// The standalone CLI's downloader owns a terminal progress program. MCP uses
// explicit contexts and file streams so stdout remains the JSON-RPC channel.
func fetchMCPSimulatorImage(ctx context.Context, version string) (string, string, func(), error) {
	manifest, err := fetchMainManifestContext(ctx)
	if err != nil {
		return "", "", nil, err
	}
	device, exists := manifest.Devices[vmDeviceKey]
	if !exists {
		return "", "", nil, fmt.Errorf("no published %s image; provide a local image", vmDeviceKey)
	}
	if version == "" {
		version = device.Latest
	}
	if version == "" {
		return "", "", nil, fmt.Errorf("no stable simulator release published; provide a version or local image")
	}
	dm, err := fetchDeviceManifestContext(ctx, device.ManifestPath)
	if err != nil {
		return "", "", nil, err
	}
	info, err := getImageInfo(dm, version, vmStorageKey)
	if err != nil {
		return "", "", nil, err
	}
	dir, err := osCacheDir()
	if err != nil {
		return "", "", nil, err
	}
	path, cleanup, err := resolveVMImageWith(dir, info,
		func(info *imageInfo) (string, error) { return downloadMCPSimulatorImage(ctx, dir, info.DownloadURL) },
		func(path, digest string) error { return verifyMCPSimulatorImage(ctx, path, digest) })
	return path, version, cleanup, err
}

func downloadMCPSimulatorImage(ctx context.Context, dir, url string) (path string, retErr error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("simulator image download returned HTTP %d", resp.StatusCode)
	}
	f, err := os.CreateTemp(dir, "simulator-*.image")
	if err != nil {
		return "", err
	}
	path = f.Name()
	defer func() {
		if err := f.Close(); retErr == nil {
			retErr = err
		}
		if retErr != nil {
			_ = os.Remove(path)
			path = ""
		}
	}()
	_, err = io.Copy(f, simulatorContextReader{ctx: ctx, reader: resp.Body})
	return path, err
}

func verifyMCPSimulatorImage(ctx context.Context, path, digest string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, simulatorContextReader{ctx: ctx, reader: f}); err != nil {
		return err
	}
	if actual := hex.EncodeToString(hash.Sum(nil)); actual != digest {
		return fmt.Errorf("simulator image checksum mismatch: got %s, expected %s", actual, digest)
	}
	return nil
}

type simulatorContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r simulatorContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if err == io.EOF && r.ctx.Err() != nil {
		err = r.ctx.Err()
	}
	return n, err
}
