package backend

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
)

type NetworkSettings struct {
	Topology                                    *pb.LocalMeshTopology
	TopologyError                               string
	WiFi                                        *pb.GetWiFiStatusResponse
	Networks                                    []*pb.ListWiFiNetworksResponse_WiFiNetwork
	Known                                       []*pb.ListKnownWiFiNetworksResponse_KnownWiFiNetwork
	Mesh                                        *pb.LocalMeshStatus
	WiFiError, ScanError, KnownError, MeshError string
}

func (b *Backend) NetworkSettings(session uint64) (NetworkSettings, error) {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return NetworkSettings{}, errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		return NetworkSettings{}, err
	}
	return readNetworkSettings(ctx, conn.Conn), nil
}

func readNetworkSettings(ctx context.Context, conn grpc.ClientConnInterface) NetworkSettings {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	wifi := pb.NewWendyWiFiServiceClient(conn)
	mesh := pb.NewWendyLocalMeshAdminServiceClient(conn)
	var result NetworkSettings
	var wg sync.WaitGroup
	wg.Add(5)
	go func() {
		defer wg.Done()
		value, err := wifi.GetWiFiStatus(ctx, &pb.GetWiFiStatusRequest{})
		if err != nil {
			result.WiFiError = err.Error()
		} else {
			result.WiFi = value
			result.WiFiError = value.GetErrorMessage()
		}
	}()
	go func() {
		defer wg.Done()
		value, err := wifi.ListWiFiNetworks(ctx, &pb.ListWiFiNetworksRequest{})
		if err != nil {
			result.ScanError = err.Error()
		} else {
			result.Networks = value.GetNetworks()
		}
	}()
	go func() {
		defer wg.Done()
		value, err := wifi.ListKnownWiFiNetworks(ctx, &pb.ListKnownWiFiNetworksRequest{})
		if err != nil {
			result.KnownError = err.Error()
		} else {
			result.Known = value.GetNetworks()
		}
	}()
	go func() {
		defer wg.Done()
		value, err := mesh.GetLocalMeshStatus(ctx, &pb.GetLocalMeshStatusRequest{})
		if err != nil {
			result.MeshError = err.Error()
		} else {
			result.Mesh = value
		}
	}()
	go func() {
		defer wg.Done()
		value, err := mesh.GetLocalMeshTopology(ctx, &pb.GetLocalMeshTopologyRequest{})
		if err != nil {
			result.TopologyError = err.Error()
		} else {
			result.Topology = value
		}
	}()
	wg.Wait()
	return result
}

func (b *Backend) JoinWiFi(session uint64, ssid, password string, security int32, hidden bool) error {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return errors.New("select a device first")
	}
	if err := b.validSession(session); err != nil {
		return err
	}
	if b.lite != nil {
		return b.joinLiteWiFi(ssid, password)
	}
	conn, err := b.connection(session)
	if err != nil {
		return err
	}
	if strings.TrimSpace(ssid) == "" || len([]byte(ssid)) > 32 {
		return errors.New("enter a Wi-Fi network name of 1–32 bytes")
	}
	if security < 0 || security > 6 {
		return errors.New("unsupported Wi-Fi security")
	}
	if security == 6 {
		return errors.New("enterprise Wi-Fi credentials are not supported by this connection form")
	}
	req := &pb.ConnectToWiFiRequest{Ssid: ssid, Password: password, Hidden: &hidden}
	if security != 0 {
		value := pb.WiFiSecurityType(security)
		req.Security = &value
	}
	wifi := pb.NewWendyWiFiServiceClient(conn.Conn)
	if password == "" && security > 1 {
		known, err := wifi.ListKnownWiFiNetworks(ctx, &pb.ListKnownWiFiNetworksRequest{})
		if err != nil {
			return err
		}
		found := false
		for _, network := range known.GetNetworks() {
			if network.GetSsid() == ssid {
				found = true
				break
			}
		}
		if !found {
			return errors.New("enter the password for this Wi-Fi network")
		}
		// Security hints update saved profiles. Omit them to reuse stored credentials.
		req.Security, req.Hidden = nil, nil
	}
	value, err := wifi.ConnectToWiFi(ctx, req)
	if err != nil {
		return err
	}
	return wifiActionResult(value.GetSuccess(), value.GetErrorMessage())
}
func (b *Backend) DisconnectWiFi(session uint64) error {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		return err
	}
	value, err := pb.NewWendyWiFiServiceClient(conn.Conn).DisconnectWiFi(ctx, &pb.DisconnectWiFiRequest{})
	if err != nil {
		return err
	}
	return wifiActionResult(value.GetSuccess(), value.GetErrorMessage())
}
func (b *Backend) ForgetWiFi(session uint64, ssid string) error {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		return err
	}
	if strings.TrimSpace(ssid) == "" {
		return errors.New("choose a saved Wi-Fi network")
	}
	value, err := pb.NewWendyWiFiServiceClient(conn.Conn).ForgetWiFiNetwork(ctx, &pb.ForgetWiFiNetworkRequest{Ssid: ssid})
	if err != nil {
		return err
	}
	return wifiActionResult(value.GetSuccess(), value.GetErrorMessage())
}
func wifiActionResult(success bool, detail string) error {
	if success {
		return nil
	}
	if detail == "" {
		detail = "the device did not complete the Wi-Fi request"
	}
	return errors.New(detail)
}

// Only changed fields are sent so unrelated saved settings are preserved.
func meshSettingsPatch(changed, enabled uint32) (*pb.ConfigureLocalMeshRequest, error) {
	if changed == 0 || changed & ^uint32(127) != 0 || enabled & ^uint32(127) != 0 {
		return nil, fmt.Errorf("invalid mesh settings update")
	}
	req := &pb.ConfigureLocalMeshRequest{}
	fields := []**bool{&req.Participate, &req.Roam, &req.ShareUplink, &req.Nan, &req.Ble, &req.Ethernet, &req.InfrastructureWifi}
	for i, field := range fields {
		if changed&(1<<i) != 0 {
			value := enabled&(1<<i) != 0
			*field = &value
		}
	}
	return req, nil
}
func (b *Backend) ConfigureMesh(session uint64, changed, enabled uint32) error {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		return err
	}
	req, err := meshSettingsPatch(changed, enabled)
	if err != nil {
		return err
	}
	_, err = pb.NewWendyLocalMeshAdminServiceClient(conn.Conn).ConfigureLocalMesh(ctx, req)
	return err
}
