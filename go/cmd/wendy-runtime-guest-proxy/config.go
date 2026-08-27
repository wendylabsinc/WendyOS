package main

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

type forwardSpec struct {
	port uint32
	path string
}

func parseForwardSpec(value string) (forwardSpec, error) {
	portText, socketPath, ok := strings.Cut(strings.TrimSpace(value), "=")
	if !ok || portText == "" || socketPath == "" {
		return forwardSpec{}, fmt.Errorf("forward %q must be PORT=/absolute/unix/socket", value)
	}
	port, err := strconv.ParseUint(portText, 10, 32)
	if err != nil || port == 0 || port > 65535 {
		return forwardSpec{}, fmt.Errorf("forward %q has an invalid port", value)
	}
	if !filepath.IsAbs(socketPath) || filepath.Clean(socketPath) != socketPath {
		return forwardSpec{}, fmt.Errorf("forward %q must name a clean absolute Unix socket path", value)
	}
	return forwardSpec{port: uint32(port), path: socketPath}, nil
}

func parseForwardSpecs(values []string) ([]forwardSpec, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("at least one --forward PORT=/absolute/unix/socket is required")
	}
	seen := make(map[uint32]struct{}, len(values))
	result := make([]forwardSpec, 0, len(values))
	for _, value := range values {
		spec, err := parseForwardSpec(value)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[spec.port]; exists {
			return nil, fmt.Errorf("duplicate vsock port %d", spec.port)
		}
		seen[spec.port] = struct{}{}
		result = append(result, spec)
	}
	return result, nil
}
