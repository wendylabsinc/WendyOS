//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

type forwardValues []string

func (v *forwardValues) String() string { return fmt.Sprint([]string(*v)) }
func (v *forwardValues) Set(value string) error {
	*v = append(*v, value)
	return nil
}

func main() {
	var values forwardValues
	flag.Var(&values, "forward", "guest vsock PORT=/absolute/unix/socket (repeatable)")
	flag.Parse()

	specs, err := parseForwardSpecs(values)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wendy-runtime-guest-proxy:", err)
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	errCh := make(chan error, len(specs))
	for _, spec := range specs {
		go func() { errCh <- serveForward(ctx, spec) }()
	}
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "wendy-runtime-guest-proxy:", err)
		os.Exit(1)
	}
}

func serveForward(ctx context.Context, spec forwardSpec) error {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("creating vsock port %d: %w", spec.port, err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: spec.port}); err != nil {
		return fmt.Errorf("binding vsock port %d: %w", spec.port, err)
	}
	if err := unix.Listen(fd, 128); err != nil {
		return fmt.Errorf("listening on vsock port %d: %w", spec.port, err)
	}
	go func() {
		<-ctx.Done()
		_ = unix.Close(fd)
	}()

	for {
		clientFD, _, err := unix.Accept(fd)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err == unix.EINTR {
				continue
			}
			return fmt.Errorf("accepting vsock port %d: %w", spec.port, err)
		}
		go proxyConnection(clientFD, spec.path)
	}
}

func proxyConnection(vsockFD int, socketPath string) {
	vsock := os.NewFile(uintptr(vsockFD), "wendy-runtime-vsock")
	if vsock == nil {
		_ = unix.Close(vsockFD)
		return
	}
	defer vsock.Close()

	service, err := net.Dial("unix", socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wendy-runtime-guest-proxy: connecting to %s: %v\n", socketPath, err)
		return
	}
	defer service.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(service, vsock)
		if unixConn, ok := service.(*net.UnixConn); ok {
			_ = unixConn.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(vsock, service)
		_ = unix.Shutdown(vsockFD, unix.SHUT_WR)
	}()
	wg.Wait()
}
