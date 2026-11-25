// world-native-pair is a private fixture client for existing native Agent RPCs.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	target, err := adminTarget(os.Getenv("WENDY_AGENT_SOCKET"))
	if err != nil {
		writeFailure(os.Stderr, "configuration", err)
		os.Exit(1)
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		writeFailure(os.Stderr, "connection", err)
		os.Exit(1)
	}
	defer conn.Close()
	if err = run(ctx, os.Args[1:], os.Stdout, agentpbv2.NewWendySensorPairingServiceClient(conn)); err != nil {
		writeFailure(os.Stderr, "command", err)
		os.Exit(1)
	}
}

// The fixture selects the same protected local endpoint as its ordinary CLI.
// It never falls back to a guessed socket or a network management address.
func adminTarget(socket string) (string, error) {
	if !filepath.IsAbs(socket) {
		return "", status.Error(codes.InvalidArgument, "configured absolute Agent socket required")
	}
	target := url.URL{Scheme: "unix", Path: socket}
	return target.String(), nil
}

func writeFailure(out io.Writer, stage string, err error) {
	// Server error details can contain native configuration. Preserve only the
	// fixed stage and bounded gRPC classification for retained diagnostics.
	fmt.Fprintf(out, "native pairing %s failed: %s\n", stage, status.Code(err).String())
}

func run(ctx context.Context, args []string, out io.Writer, client agentpbv2.WendySensorPairingServiceClient) error {
	if len(args) == 0 || client == nil {
		return fmt.Errorf("mode required")
	}
	var reply proto.Message
	var err error
	switch args[0] {
	case "pair":
		flags := flag.NewFlagSet("pair", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		principal := flags.String("principal", "", "exact source principal")
		address := flags.String("address", "", "explicit same-node SensorLink endpoint")
		if flags.Parse(args[1:]) != nil || flags.NArg() != 0 {
			return fmt.Errorf("arguments refused")
		}
		identity, e := certs.ParseDevicePrincipal(*principal)
		host, port, addrErr := net.SplitHostPort(*address)
		if e != nil || certs.DeviceSPIFFEURI(identity.TenantUUID, identity.EntityID) != *principal || addrErr != nil || host != "127.0.0.1" || port != "50060" {
			return fmt.Errorf("fixture source binding refused")
		}
		reply, err = client.AddSensorPairing(ctx, &agentpbv2.AddSensorPairingRequest{
			SourcePrincipalUri: *principal, SourceAddress: *address, Transport: "tcp", Name: "MuJoCo camera B", SensorAllowlist: []string{"camera-b"}})
	case "status":
		if len(args) != 1 {
			return fmt.Errorf("arguments refused")
		}
		reply, err = client.ListSensorPairings(ctx, &agentpbv2.ListSensorPairingsRequest{})
	default:
		return fmt.Errorf("mode refused")
	}
	if err != nil {
		return err
	}
	raw, err := (protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}).Marshal(reply)
	if err != nil || len(raw) > 64<<10 {
		return fmt.Errorf("bounded pairing reply required")
	}
	_, err = out.Write(append(raw, '\n'))
	return err
}
