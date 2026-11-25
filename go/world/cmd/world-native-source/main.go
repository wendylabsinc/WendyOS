// world-native-sensor-source is an owned simulation fixture process. It reads
// the Agent's existing enrollment; it never enrolls, renews or copies keys to apps.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/world/production/livesensorlink"
	"golang.org/x/sys/unix"
)

type config struct {
	ConfigDirectory string                            `json:"config_directory"`
	OutputDirectory string                            `json:"output_directory"`
	Source          livesensorlink.NativeSourceConfig `json:"source"`
}

func read(path string, limit int64, private bool) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("invalid file path")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "fixture input")
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0022 != 0 || private && st.Mode().Perm()&0077 != 0 || st.Size() < 1 || st.Size() > limit {
		return nil, errors.New("invalid input file")
	}
	return io.ReadAll(io.LimitReader(f, limit+1))
}

func write(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closed := f.Close()
	if err == nil {
		err = closed
	}
	return err
}

func serve(ctx context.Context, path string) error {
	raw, err := read(path, 32768, false)
	if err != nil {
		return err
	}
	var cfg config
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&cfg) != nil || d.Decode(new(any)) != io.EOF {
		return errors.New("invalid source configuration")
	}
	for _, dir := range []string{cfg.ConfigDirectory, cfg.OutputDirectory} {
		if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
			return errors.New("invalid directory")
		}
		st, e := os.Lstat(dir)
		if e != nil || !st.IsDir() || st.Mode().Perm()&0022 != 0 {
			return errors.New("invalid existing directory")
		}
	}
	stateRaw, err := read(filepath.Join(cfg.ConfigDirectory, "provisioning.json"), 1<<20, true)
	if err != nil {
		return err
	}
	defer clear(stateRaw)
	var state struct {
		Enrolled     bool   `json:"enrolled"`
		CertPEM      string `json:"certPem"`
		ChainPEM     string `json:"chainPem"`
		PrincipalURI string `json:"principalURI"`
		KeyPEM       string `json:"keyPem"`
	}
	if json.Unmarshal(stateRaw, &state) != nil || !state.Enrolled || state.PrincipalURI == "" || state.CertPEM == "" || state.ChainPEM == "" || state.KeyPEM != "" {
		return errors.New("existing native enrollment required")
	}
	key, err := read(filepath.Join(cfg.ConfigDirectory, "device-key.pem"), 65536, true)
	if err != nil {
		return err
	}
	defer clear(key)
	tlsConfig, err := mtls.NewTLSConfig(state.CertPEM, state.ChainPEM, string(key), nil, time.Time{})
	if err != nil {
		return errors.New("native TLS configuration refused")
	}
	listener, err := net.Listen("tcp", cfg.Source.TCPListen)
	if err != nil {
		return err
	}
	listener = tls.NewListener(listener, tlsConfig)
	cfg.Source.Ready = func() error {
		ready, e := json.Marshal(map[string]any{"version": 1, "ready": true, "tcp_listen": cfg.Source.TCPListen, "metadata_listen": cfg.Source.MetadataListen})
		if e != nil {
			return e
		}
		return write(filepath.Join(cfg.OutputDirectory, "ready.json"), ready)
	}
	result, runErr := livesensorlink.RunNativeSourceWithListener(ctx, cfg.Source, listener, func(meta livesensorlink.Metadata, jpeg []byte) error {
		name := fmt.Sprintf("frame-%06d", meta.FrameSequence)
		metadata, e := json.Marshal(meta)
		if e != nil {
			return e
		}
		if e = write(filepath.Join(cfg.OutputDirectory, name+".jpg"), jpeg); e != nil {
			return e
		}
		return write(filepath.Join(cfg.OutputDirectory, name+".json"), metadata)
	})
	if err = json.NewEncoder(os.Stdout).Encode(result); err != nil {
		return err
	}
	return runErr
}

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--help" || os.Args[1] == "-h") {
		fmt.Println("world-native-sensor-source serve --config /absolute/config.json")
		return
	}
	if len(os.Args) < 2 || os.Args[1] != "serve" {
		fmt.Fprintln(os.Stderr, "expected serve --config")
		os.Exit(2)
	}
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	path := flags.String("config", "", "source configuration")
	_ = flags.Parse(os.Args[2:])
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *path == "" || flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "source configuration required")
		os.Exit(2)
	}
	if err := serve(ctx, *path); err != nil {
		fmt.Fprintln(os.Stderr, "native simulation source failed")
		os.Exit(1)
	}
}
