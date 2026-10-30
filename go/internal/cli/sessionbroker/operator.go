package sessionbroker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/operatormesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshappstream"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// encoding/json can expand one valid URI byte (for example &) to six bytes.
const operatorJSONPrincipalBytes = 6 * meshoperator.MaxPrincipalBytes
const operatorRecipeBytes = operatorJSONPrincipalBytes + 4096
const operatorStateBytes = 4*operatorJSONPrincipalBytes + 4096

// OperatorSpec is a non-secret recipe. Credentials remain in the selected
// persistent profile. One context belongs to an exact operator principal and
// that profile's persisted endpoint instance, independently of its first hop.
type OperatorSpec struct {
	Version       int    `json:"version"`
	Automatic     bool   `json:"automatic,omitempty"`
	Enabled       bool   `json:"enabled,omitempty"`
	Principal     string `json:"principal"`
	RouterAddress string `json:"routerAddress"`
	Transport     string `json:"transport,omitempty"`
	BLEPSM        uint16 `json:"blePsm,omitempty"`
	RouterUUID    string `json:"routerUuid"`
}

// MeshSelector is the canonical full tenant/device selector. Parsing conveys
// identity intent only; the broker's primary TLS verifier supplies trust.
func MeshSelector(device meshid.Device) string {
	return "mesh://" + device.Tenant().String() + "/" + device.UUID().String()
}
func ParseMeshSelector(text string) (meshid.Device, bool, error) {
	if !strings.HasPrefix(text, "mesh:") {
		return meshid.Device{}, false, nil
	}
	u, err := url.Parse(text)
	if err != nil || u.Scheme != "mesh" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return meshid.Device{}, true, errors.New("mesh selector must be mesh://<tenant-uuid>/<device-uuid>")
	}
	tenant, err := meshid.ParseTenant(u.Host)
	if err != nil {
		return meshid.Device{}, true, err
	}
	device, err := meshid.ParseDevice(tenant, strings.TrimPrefix(u.Path, "/"))
	if err != nil || MeshSelector(device) != text {
		return meshid.Device{}, true, errors.New("mesh selector requires canonical tenant and device UUIDs")
	}
	return device, true, nil
}
func operatorPrincipal(cert config.CertificateInfo) (meshoperator.Operator, error) {
	chain, err := certs.ParseCertsFromPEM([]byte(cert.PemCertificate))
	if err != nil || len(chain) == 0 {
		return meshoperator.Operator{}, errors.New("selected primary operator certificate is missing")
	}
	id, known, err := certs.IdentityFromCert(chain[0])
	if err != nil || !known {
		return meshoperator.Operator{}, errors.New("selected certificate has no operator principal")
	}
	return meshoperator.ParsePrincipal(id.Principal)
}
func operatorExpected(device meshid.Device) certs.WendyIdentity {
	return certs.WendyIdentity{EntityType: certs.EntityAsset, EntityID: device.UUID().String(), TenantUUID: device.Tenant().String(), Principal: device.Principal()}
}
func operatorKey(principal string) string { return "operator-context:" + principal }
func (s Spec) paths(dir string) (string, string) {
	if s.Operator != nil {
		return paths(dir, s.Key, certs.WendyIdentity{})
	}
	return paths(dir, s.Key, s.Expected)
}
func (s Spec) validateOperator() error {
	recipe := s.Operator
	if recipe.Version != 1 {
		return errors.New("unsupported operator broker recipe version")
	}
	operator, err := meshoperator.ParsePrincipal(recipe.Principal)
	if err != nil {
		return err
	}
	if recipe.Automatic && recipe.RouterUUID == "" && recipe.RouterAddress == "" {
		if recipe.Transport != "" || recipe.BLEPSM != 0 || s.Key != operatorKey(recipe.Principal) || s.Expected != (certs.WendyIdentity{}) || s.Host != operator.Principal() || s.Addr != s.Host || s.CertFingerprint == "" {
			return errors.New("automatic operator context identity differs")
		}
		return nil
	}
	device, err := meshid.ParseDevice(operator.Tenant(), recipe.RouterUUID)
	if err != nil {
		return err
	}
	switch recipe.Transport {
	case "", "tcp":
		host, port, err := net.SplitHostPort(recipe.RouterAddress)
		p, portErr := strconv.Atoi(port)
		if err != nil || host == "" || portErr != nil || p <= 0 || p > 65535 || len(recipe.RouterAddress) > 512 || recipe.BLEPSM != 0 {
			return errors.New("operator first hop requires a TLS host:port")
		}
	case "ble":
		// A macOS peripheral UUID or Linux LE address is merely a connection hint;
		// TLS still proves the separate exact enrolled router UUID.
		addressOK := false
		if parsed, err := uuid.Parse(recipe.RouterAddress); err == nil && strings.EqualFold(parsed.String(), recipe.RouterAddress) {
			addressOK = true
		}
		if parsed, err := net.ParseMAC(recipe.RouterAddress); err == nil && len(parsed) == 6 {
			addressOK = true
		}
		if !addressOK || (recipe.BLEPSM != 0 && (recipe.BLEPSM < 0x80 || recipe.BLEPSM > 0xff)) {
			return errors.New("BLE operator first hop requires a peripheral UUID or LE address and dynamic PSM")
		}
	default:
		return errors.New("operator transport must be tcp or ble")
	}
	if s.Key != operatorKey(recipe.Principal) || s.Expected != operatorExpected(device) || s.Host != MeshSelector(device) || s.Addr != s.Host || s.CertFingerprint == "" {
		return errors.New("operator broker context and selected router differ")
	}
	return nil
}
func operatorSpec(recipe OperatorSpec, cert config.CertificateInfo) (Spec, error) {
	operator, err := operatorPrincipal(cert)
	if err != nil || operator.Principal() != recipe.Principal {
		return Spec{}, errors.New("operator recipe differs from the selected primary credential")
	}
	if recipe.Automatic && recipe.RouterUUID == "" && recipe.RouterAddress == "" {
		s := Spec{Operator: &recipe, Key: operatorKey(recipe.Principal), Host: operator.Principal(), Addr: operator.Principal(), CertFingerprint: certificateFingerprint(cert)}
		return s, s.validate()
	}
	device, err := meshid.ParseDevice(operator.Tenant(), recipe.RouterUUID)
	if err != nil {
		return Spec{}, err
	}
	s := Spec{Operator: &recipe, Key: operatorKey(recipe.Principal), Host: MeshSelector(device), Addr: MeshSelector(device), Expected: operatorExpected(device), CertFingerprint: certificateFingerprint(cert), ParentPID: 0}
	return s, s.validate()
}
func operatorRecipePath(directory, principal string) string {
	sum := sha256.Sum256([]byte(principal))
	return filepath.Join(directory, "operator-contexts", hex.EncodeToString(sum[:])+".json")
}
func configuredOperator() (config.CertificateInfo, meshoperator.Operator, error) {
	cfg, err := loadConfig()
	if err != nil {
		return config.CertificateInfo{}, meshoperator.Operator{}, err
	}
	auth, err := config.ResolveAuth(cfg, "", nil)
	if err != nil {
		return config.CertificateInfo{}, meshoperator.Operator{}, err
	}
	if len(auth.Certificates) == 0 {
		return config.CertificateInfo{}, meshoperator.Operator{}, errors.New("current context has no primary operator credential")
	}
	certificate := auth.Certificates[0]
	operator, err := operatorPrincipal(certificate)
	return certificate, operator, err
}

// PrepareOperator stores the selected first hop and joins through the existing
// detached session broker. Commands retain only their local IPC connection.
func PrepareOperator(ctx context.Context, certificate config.CertificateInfo, address, router string) (*grpcclient.AgentConnection, error) {
	return PrepareOperatorTransport(ctx, certificate, address, router, "", 0)
}

// PrepareOperatorTransport retains an explicit operator attachment over one
// selected carrier. The broker's endpoint identity remains carrier-independent.
func PrepareOperatorTransport(ctx context.Context, certificate config.CertificateInfo, address, router, transport string, psm uint16) (*grpcclient.AgentConnection, error) {
	operator, err := operatorPrincipal(certificate)
	if err != nil {
		return nil, err
	}
	if transport == "tcp" {
		transport = ""
	}
	recipe := OperatorSpec{Version: 1, Principal: operator.Principal(), RouterAddress: address, RouterUUID: router, Transport: transport, BLEPSM: psm}
	return prepareOperatorRecipe(ctx, certificate, recipe)
}

func prepareOperatorRecipe(ctx context.Context, certificate config.CertificateInfo, recipe OperatorSpec) (*grpcclient.AgentConnection, error) {
	spec, err := operatorSpec(recipe, certificate)
	if err != nil {
		return nil, err
	}
	if !configDirOwnershipSafe() {
		return nil, errors.New("operator profile is not owned by the current user")
	}
	directory, err := configDir()
	if err != nil {
		return nil, err
	}
	path := operatorRecipePath(directory, recipe.Principal)
	if err = ensurePrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if err = writeOperatorRecipe(ctx, spec, path, recipe); err != nil {
		return nil, err
	}
	return connectOrStartOperator(ctx, spec, &certificate)
}
func writeOperatorRecipe(ctx context.Context, spec Spec, path string, recipe OperatorSpec) error {
	if existing, err := readOperatorRecipe(path); err == nil && existing == recipe {
		return nil
	}
	dir, err := brokerDir()
	if err != nil {
		return err
	}
	if err = ensurePrivateDir(dir); err != nil {
		return err
	}
	socket, _ := spec.paths(dir)
	wait, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var startup *os.File
	for startup == nil {
		file, locked, err := acquireIdentityLock(socket + ".startup")
		if err != nil {
			return err
		}
		if locked {
			startup = file
			break
		}
		select {
		case <-wait.Done():
			return wait.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	defer func() { releaseLock(startup); startup.Close() }()
	if existing, err := readOperatorRecipe(path); err == nil && sameOperatorRecipe(existing, recipe) {
		// Enabling retained routing intent changes no identity or carrier.
		// Publish it atomically without draining existing transit streams.
		data, e := json.Marshal(recipe)
		if e != nil {
			return e
		}
		return writePrivateFile(path, data)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	owner, locked, err := acquireIdentityLock(socket)
	if err != nil {
		return err
	}
	if !locked {
		return errors.New("run 'wendy device wendy-net disconnect' before changing a live operator first hop")
	}
	defer func() { releaseLock(owner); owner.Close() }()
	data, err := json.Marshal(recipe)
	if err != nil {
		return err
	}
	return writePrivateFile(path, data)
}
func operatorRecipeCurrent(spec Spec) error {
	directory, err := configDir()
	if err != nil {
		return err
	}
	current, err := readOperatorRecipe(operatorRecipePath(directory, spec.Operator.Principal))
	if err != nil {
		return err
	}
	if !sameOperatorRecipe(current, *spec.Operator) {
		return errors.New("configured operator first hop changed; resolve the mesh selector again")
	}
	return nil
}

func readOperatorRecipe(path string) (OperatorSpec, error) {
	var recipe OperatorSpec
	if err := validatePrivateFile(path, false); err != nil {
		return recipe, err
	}
	file, err := os.Open(path)
	if err != nil {
		return recipe, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, operatorRecipeBytes+1))
	if err != nil {
		return recipe, err
	}
	if len(data) > operatorRecipeBytes {
		return recipe, errors.New("oversized operator recipe")
	}
	err = json.Unmarshal(data, &recipe)
	return recipe, err
}

// ConnectOperator resolves an explicit target through the selected operator
// context. A remote target authenticates independently over its native agent
// TLS connection; the adjacent peer continues to reuse attachment TLS.
func ConnectOperator(ctx context.Context, selector string) (*grpcclient.AgentConnection, error) {
	device, matched, err := ParseMeshSelector(selector)
	if err != nil {
		return nil, err
	}
	if !matched {
		return nil, errors.New("not a mesh selector")
	}
	certificate, operator, err := configuredOperator()
	if err != nil {
		return nil, err
	}
	if device.Tenant() != operator.Tenant() {
		return nil, errors.New("mesh target differs from the selected operator tenant")
	}
	directory, err := configDir()
	if err != nil {
		return nil, err
	}
	recipe, err := readOperatorRecipe(operatorRecipePath(directory, operator.Principal()))
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		adjacent, prepareErr := PrepareOperatorAutomatic(ctx, certificate)
		if prepareErr != nil {
			return nil, prepareErr
		}
		_ = adjacent.Close()
		// Reload the saved exact peer after automatic preparation. The target
		// remains the caller's full tenant/device selector.
		return ConnectOperator(ctx, selector)
	}
	spec, err := operatorSpec(recipe, certificate)
	if err != nil {
		return nil, err
	}
	adjacent, err := connectOrStartOperator(ctx, spec, &certificate)
	if err != nil || spec.Expected == operatorExpected(device) {
		return adjacent, err
	}
	_ = adjacent.Close()
	return connectOperatorProxy(ctx, spec, &certificate, device)
}

var launchOperatorBroker = startSpec

func connectOrStartOperator(ctx context.Context, spec Spec, certificate *config.CertificateInfo) (*grpcclient.AgentConnection, error) {
	if host := embeddedHost.Load(); host != nil && !host.active.Load() {
		return nil, ErrEmbeddedSuspended
	}
	if !configDirOwnershipSafe() {
		return nil, errors.New("operator profile is not owned by the current user")
	}
	wait, cancel := context.WithTimeout(ctx, operatormesh.JoinSetupTimeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var last error
	var launchedAt time.Time
	var startup *os.File
	defer func() {
		if startup != nil {
			releaseLock(startup)
			startup.Close()
		}
	}()
	for {
		if err := operatorRecipeCurrent(spec); err != nil {
			return nil, err
		}
		client, err := connectOperatorProxy(wait, spec, certificate)
		if err == nil {
			return client, nil
		}
		last = err
		if errors.Is(err, errOperatorCredentialsChanged) || errors.Is(err, errOperatorProtocolChanged) {
			if err = disconnectOperatorSpec(wait, spec); err != nil {
				return nil, err
			}
		}
		if launchedAt.IsZero() || time.Since(launchedAt) >= time.Second {
			// Avoid forking helpers while another context owner is joining/draining.
			// Every child still takes this exact lock before any network handshake.
			dir, dirErr := brokerDir()
			if dirErr != nil {
				return nil, dirErr
			}
			if err = ensurePrivateDir(dir); err != nil {
				return nil, err
			}
			socket, _ := spec.paths(dir)
			if startup == nil {
				file, locked, lockErr := acquireIdentityLock(socket + ".startup")
				if lockErr != nil {
					return nil, lockErr
				}
				if locked {
					startup = file
				}
			}
			if startup == nil {
				select {
				case <-wait.Done():
					return nil, wait.Err()
				case <-ticker.C:
					continue
				}
			}
			file, locked, lockErr := acquireIdentityLock(socket)
			if lockErr != nil {
				return nil, lockErr
			}
			if locked {
				releaseLock(file)
				file.Close()
				if err = launchOperatorBroker(spec); err != nil {
					return nil, err
				}
			}
			launchedAt = time.Now()
		}
		select {
		case <-wait.Done():
			return nil, fmt.Errorf("operator broker did not become ready: %w", errors.Join(wait.Err(), last))
		case <-ticker.C:
		}
	}
}

var errOperatorCredentialsChanged = errors.New("selected operator credential changed")
var errOperatorProtocolChanged = errors.New("operator broker IPC protocol changed")

// DisconnectOperator withdraws only the current operator's retained context.
// The exact-owned broker observes unpublication and cancels its attachment;
// waiting for its lock joins shutdown before a replacement can be prepared.
func DisconnectOperator(ctx context.Context) error {
	certificate, operator, err := configuredOperator()
	if err != nil {
		return err
	}
	directory, err := configDir()
	if err != nil {
		return err
	}
	recipe, err := readOperatorRecipe(operatorRecipePath(directory, operator.Principal()))
	if err != nil {
		return err
	}
	spec, err := operatorSpec(recipe, certificate)
	if err != nil {
		return err
	}
	return disconnectOperatorSpec(ctx, spec)
}
func disconnectOperatorSpec(ctx context.Context, spec Spec) error {
	dir, err := brokerDir()
	if err != nil {
		return err
	}
	if !configDirOwnershipSafe() {
		return errors.New("operator profile is not owned by current user")
	}
	if err = ensurePrivateDir(dir); err != nil {
		return err
	}
	socket, state := spec.paths(dir)
	until, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		// State may not exist yet while the owner is still doing its first TLS
		// handshake. Keep joining that owner; remove only this context's IPC paths.
		if err = os.Remove(state); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err = os.Remove(socket); err != nil && !os.IsNotExist(err) {
			return err
		}
		file, locked, err := acquireIdentityLock(socket)
		if err != nil {
			return err
		}
		if locked {
			releaseLock(file)
			file.Close()
			return nil
		}
		select {
		case <-until.Done():
			return until.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func connectOperatorProxy(ctx context.Context, spec Spec, certificate *config.CertificateInfo, targets ...meshid.Device) (*grpcclient.AgentConnection, error) {
	dir, err := brokerDir()
	if err != nil {
		return nil, err
	}
	if err = validatePrivateDir(dir); err != nil {
		return nil, err
	}
	socket, statePath := spec.paths(dir)
	if err = validatePrivateFile(socket, true); err != nil {
		return nil, err
	}
	if err = validatePrivateFile(statePath, false); err != nil {
		return nil, err
	}
	file, err := os.Open(statePath)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, operatorStateBytes+1))
	file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if len(data) > operatorStateBytes {
		return nil, errors.New("oversized operator broker state")
	}
	var state stateFile
	if json.Unmarshal(data, &state) == nil && state.Spec.Operator != nil && sameOperatorRecipe(*state.Spec.Operator, *spec.Operator) && state.Spec.Key == spec.Key && state.Spec.CertFingerprint != spec.CertFingerprint {
		return nil, errOperatorCredentialsChanged
	}
	if json.Unmarshal(data, &state) != nil || state.Spec.Operator == nil || !sameOperatorRecipe(*state.Spec.Operator, *spec.Operator) || state.Spec.Expected != spec.Expected || state.Spec.CertFingerprint != spec.CertFingerprint || state.Spec.Key != spec.Key {
		return nil, errors.New("operator broker is owned by different selected credentials or first hop")
	}
	if state.IPCVersion != operatorIPCVersion {
		return nil, errOperatorProtocolChanged
	}
	host, expected := spec.Host, spec.Expected
	var options []grpc.DialOption
	timeout := healthTimeout
	if len(targets) > 0 {
		host, expected = MeshSelector(targets[0]), operatorExpected(targets[0])
		options = operatorTargetOptions(host)
	}
	client, err := grpcclient.ConnectSessionProxy(ctx, socket, host, host, certificate, expected, options...)
	if err != nil {
		return nil, err
	}
	probe, cancel := context.WithTimeout(ctx, timeout)
	request, reply := rawMessage{}, rawMessage{}
	err = client.Conn.Invoke(probe, operatorContextMethod, &request, &reply, grpc.ForceCodec(rawCodec{}), grpc.MaxCallRecvMsgSize(operatorContextBytes))
	cancel()
	var observation operatorContextStatus
	if err == nil && (json.Unmarshal(reply, &observation) != nil || observation.Version != operatorIPCVersion || observation.Principal != spec.Operator.Principal) {
		err = errors.New("operator broker context status differs")
	}
	if err != nil {
		client.Close()
		return nil, err
	}
	// Secondary registry/image connections share this exact context. The broker
	// rechecks the selected device's live route; registry TLS stays end to end.
	client.RegistryDialer = func(ctx context.Context, port int) (net.Conn, error) {
		return openOperatorTCP(ctx, client.Conn, host, port, nil)
	}
	client.OperatorMeshTarget, _, _ = ParseMeshSelector(host)
	client.AppDialer = func(ctx context.Context, port uint16) (net.Conn, error) {
		return meshappstream.Dial(ctx, client.Conn, port)
	}
	client.Reconnect = func(ctx context.Context) (*grpcclient.AgentConnection, error) { return ConnectOperator(ctx, host) }
	return client, nil
}

var openOperatorSession = operatormesh.Open

func runOperator(ctx context.Context, dir string, spec Spec, idleTTL time.Duration) error {
	selected, err := configuredOperatorCertificate(spec)
	if err != nil {
		return err
	}
	directory, err := configDir()
	if err != nil {
		return err
	}
	owner, cancel := context.WithCancel(ctx)
	defer cancel()
	attachment := &operatorAttachment{directory: directory, spec: spec, certificate: selected}
	initial, err := attachment.open(owner, *spec.Operator)
	if err != nil {
		return err
	}
	attachment.replace(initial)
	finished := make(chan struct{})
	go func() { defer close(finished); defer cancel(); _ = attachment.run(owner) }()
	defer func() { cancel(); <-finished }()
	remotes := newOperatorTargets(initial.session, nil, selected, directory, spec)
	remotes.currentAdjacent = attachment.adjacent
	remotes.localSnapshot = func() localmesh.NodeSnapshot {
		current, err := attachment.snapshot()
		if err != nil {
			return localmesh.NodeSnapshot{}
		}
		return current.session.Snapshot()
	}
	remotes.packetDial = attachment.DialContext
	remotes.appDial = attachment.DialAppTLS
	defer remotes.Close()
	return serveResolvedLocal(owner, dir, spec, nil, idleTTL, func() bool {
		if host := embeddedHost.Load(); host != nil && host.active.Load() {
			return true // The foreground app owns the routing lease.
		}
		if !spec.Operator.Automatic {
			return true
		}
		if latest, err := readOperatorRecipe(operatorRecipePath(directory, spec.Operator.Principal)); err == nil && sameOperatorRecipe(latest, *spec.Operator) && latest.Enabled {
			return true
		}
		current, err := attachment.snapshot()
		return err == nil && current.session.HasRecentTransit(DefaultIdleTTL)
	}, remotes.Resolve, remotes.localManagement, true)
}

// Routing intent stays on private local IPC and is removed before forwarding.
func operatorTargetOptions(selector string) []grpc.DialOption {
	selected := func(ctx context.Context) context.Context {
		md, _ := metadata.FromOutgoingContext(ctx)
		md = md.Copy()
		md.Set(operatorTargetHeader, selector)
		return metadata.NewOutgoingContext(ctx, md)
	}
	return []grpc.DialOption{
		grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoke grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return invoke(selected(ctx), method, req, reply, cc, opts...)
		}),
		grpc.WithStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, stream grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			return stream(selected(ctx), desc, cc, method, opts...)
		}),
	}
}

// Enabled is local routing intent, not carrier identity or an authorization.
func sameOperatorRecipe(a, b OperatorSpec) bool { a.Enabled = false; b.Enabled = false; return a == b }
