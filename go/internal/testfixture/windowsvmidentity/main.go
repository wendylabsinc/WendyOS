// windowsvmidentity observes the fresh VM's read-only Agent identity over the
// owned QEMU loopback forward. CLI device info intentionally omits these fields.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

type identity struct {
	Address      string `json:"address"`
	Nonce        string `json:"nonce"`
	Hostname     string `json:"hostname"`
	BootID       string `json:"bootId"`
	BootTimeNS   uint64 `json:"bootTimeNs"`
	Version      string `json:"version"`
	OS           string `json:"os"`
	OSVersion    string `json:"osVersion"`
	Architecture string `json:"cpuArchitecture"`
	DeviceType   string `json:"deviceType"`
}

func observe(ctx context.Context, address, nonce string) (identity, error) {
	host, portText, err := net.SplitHostPort(address)
	port, portErr := strconv.Atoi(portText)
	decoded, nonceErr := hex.DecodeString(nonce)
	if err != nil || host != "127.0.0.1" || portErr != nil || port < 1024 || port >= 65535 || nonceErr != nil || len(decoded) != 16 {
		return identity{}, errors.New("exact loopback forward and 128-bit fixture nonce required")
	}
	conn, err := grpcclient.Connect(ctx, address)
	if err != nil {
		return identity{}, err
	}
	defer conn.Close()
	response, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		return identity{}, err
	}
	if response.GetHostname() == "" {
		return identity{}, errors.New("actual Agent response omitted persistent hostname")
	}
	network, err := conn.ContainerService.GetNetworkStats(ctx, &agentpb.GetNetworkStatsRequest{})
	if err != nil {
		return identity{}, err
	}
	if network.GetBootId() == "" || network.GetSampledAtBoottimeNs() == 0 {
		return identity{}, errors.New("actual Agent response omitted kernel boot identity")
	}
	return identity{Address: address, Nonce: nonce, Hostname: response.GetHostname(), BootID: network.GetBootId(), BootTimeNS: network.GetSampledAtBoottimeNs(),
		Version: response.GetVersion(), OS: response.GetOs(), OSVersion: response.GetOsVersion(),
		Architecture: response.GetCpuArchitecture(), DeviceType: response.GetDeviceType()}, nil
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: windowsvmidentity 127.0.0.1:OWNED_PORT NONCE")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	value, err := observe(ctx, os.Args[1], os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err = json.NewEncoder(os.Stdout).Encode(value); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
