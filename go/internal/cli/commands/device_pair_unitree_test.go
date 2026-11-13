package commands

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/unitree"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type unitreePairTestClient struct {
	agentpbv2.WendyUnitreeServiceClient
	discovered, paired                        []*agentpbv2.UnitreeRobot
	knownSerials                              []string
	scans                                     int
	saved                                     *agentpbv2.PairUnitreeRequest
	forgot                                    string
	connected                                 bool
	discoveryErr, listErr, pairErr, forgetErr error
}

func (c *unitreePairTestClient) DiscoverUnitreeRobots(_ context.Context, req *agentpbv2.DiscoverUnitreeRobotsRequest, _ ...grpc.CallOption) (*agentpbv2.DiscoverUnitreeRobotsResponse, error) {
	c.scans++
	c.knownSerials = req.KnownSerials
	return &agentpbv2.DiscoverUnitreeRobotsResponse{Robots: c.discovered}, c.discoveryErr
}

func (c *unitreePairTestClient) ListUnitreePairings(context.Context, *agentpbv2.ListUnitreePairingsRequest, ...grpc.CallOption) (*agentpbv2.ListUnitreePairingsResponse, error) {
	return &agentpbv2.ListUnitreePairingsResponse{Robots: c.paired}, c.listErr
}

func (c *unitreePairTestClient) PairUnitree(_ context.Context, req *agentpbv2.PairUnitreeRequest, _ ...grpc.CallOption) (*agentpbv2.PairUnitreeResponse, error) {
	c.saved = req
	return &agentpbv2.PairUnitreeResponse{Robot: &agentpbv2.UnitreeRobot{Serial: req.Serial, Name: req.Name, Address: req.Address, Paired: true, Connected: c.connected}}, c.pairErr
}

func (c *unitreePairTestClient) ForgetUnitree(_ context.Context, req *agentpbv2.ForgetUnitreeRequest, _ ...grpc.CallOption) (*agentpbv2.ForgetUnitreeResponse, error) {
	c.forgot = req.Serial
	return &agentpbv2.ForgetUnitreeResponse{}, c.forgetErr
}

func unitreePairUpdate(m unitreePairModel, msg tea.Msg) (unitreePairModel, tea.Cmd) {
	next, cmd := m.Update(msg)
	return next.(unitreePairModel), cmd
}

func unitreeKey(key string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)} }

func TestDevicePairUnitreeLoginFormAndBackgroundResults(t *testing.T) {
	c := &unitreePairTestClient{}
	var loggedEmail, loggedPassword, loggedRegion string
	h := &unitreePairHandler{ctx: context.Background(), client: c, region: "cn", initialIP: "192.0.2.42",
		login: func(_ context.Context, region, email, password string) (*unitree.Account, error) {
			loggedEmail, loggedPassword, loggedRegion = email, password, region
			return &unitree.Account{Devices: []unitree.Device{{Serial: "GO2-A", Name: "First", EncryptionKey: "key-a"}, {Serial: "GO2-B", Name: "Second", EncryptionKey: "key-b"}}}, nil
		},
	}
	m := newDevicePairModel(&pairTestBluetooth{}, pairTestHandler(&pairTestClient{}), nil, h)
	m, scan := pairUpdate(m, tea.KeyMsg{Type: tea.KeyShiftTab})
	if m.active != pairUnitreeTab || scan == nil || c.scans != 0 {
		t.Fatal("Unitree did not lazily discover on first visit")
	}
	m, _ = pairUpdate(m, scan())
	m, _ = pairUpdate(m, unitreeKey("i"))
	if m.unitree.form != unitreeLoginForm {
		t.Fatal("connecting by IP without an account did not request login")
	}
	m, _ = pairUpdate(m, unitreeKey("qfr@example.com"))
	m, _ = pairUpdate(m, tea.KeyMsg{Type: tea.KeyTab})
	for _, key := range []string{"q", "f", "r", "-secret"} {
		m, _ = pairUpdate(m, unitreeKey(key))
	}
	if m.done || m.active != pairUnitreeTab || strings.Contains(m.View(), "qfr-secret") || m.unitree.password.Value() != "qfr-secret" {
		t.Fatal("login form leaked the password or dispatched shortcuts")
	}
	m, submit := pairUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	if submit == nil || m.unitree.password.Value() != "" || !m.unitree.busy || m.unitree.form != unitreeNoForm {
		t.Fatal("login submission did not clear credentials and start the request")
	}
	m, _ = pairUpdate(m, tea.KeyMsg{Type: tea.KeyTab})
	m, discover := pairUpdate(m, submit())
	if loggedEmail != "qfr@example.com" || loggedPassword != "qfr-secret" || loggedRegion != "cn" || m.active != pairBluetoothTab || m.unitree.account == nil || discover == nil {
		t.Fatal("login response was not routed to the Unitree tab")
	}
	m, _ = pairUpdate(m, discover())
	if strings.Join(c.knownSerials, ",") != "GO2-A,GO2-B" || len(m.unitree.rows) != 2 || c.saved != nil {
		t.Fatal("login did not rediscover account robots or picked a key automatically")
	}
	// The user explicitly selects the second account robot for the requested IP.
	m.active = pairUnitreeTab
	m.unitree.table.SetCursor(1)
	m, pair := pairUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	if pair == nil {
		t.Fatal("selected account robot cannot be paired by IP")
	}
	m, _ = pairUpdate(m, pair())
	if c.saved == nil || c.saved.Serial != "GO2-B" || c.saved.Address != "192.0.2.42" || c.saved.EncryptionKey != "key-b" || c.saved.Token != "" {
		t.Fatal("IP pairing did not use the explicitly selected account robot's key")
	}
	if strings.Contains(m.unitree.message, "Connected to") || !strings.Contains(m.unitree.message, "not connected") {
		t.Fatal("unverified connection was reported as connected")
	}
	for _, secret := range []string{"key-a", "key-b", "token-secret", "qfr-secret"} {
		if strings.Contains(m.View(), secret) {
			t.Fatal("secret reached the Unitree view")
		}
	}
}

func TestUnitreeManualIPValidationAndSavedReconnect(t *testing.T) {
	c := &unitreePairTestClient{
		paired:     []*agentpbv2.UnitreeRobot{{Serial: "SAVED", Name: "Saved Go2", Address: "192.0.2.1", Paired: true}},
		discovered: []*agentpbv2.UnitreeRobot{{Serial: "SAVED", Address: "192.0.2.1"}}, connected: true,
	}
	m := newUnitreePairModel(&unitreePairHandler{ctx: context.Background(), client: c})
	m, _ = unitreePairUpdate(m, m.Init()())
	if strings.Join(c.knownSerials, ",") != "SAVED" {
		t.Fatal("discovery did not include the saved robot serial")
	}
	m, _ = unitreePairUpdate(m, unitreeKey("i"))
	if m.form != unitreeIPForm {
		t.Fatal("saved pairing requested cloud login")
	}
	m.address.SetValue("192.0.2.42:9991")
	m, cmd := unitreePairUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || m.form != unitreeIPForm || !strings.Contains(m.message, "IPv4") {
		t.Fatal("invalid manual IP was submitted")
	}
	m.address.SetValue("192.0.2.42")
	m, cmd = unitreePairUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = unitreePairUpdate(m, cmd())
	if c.saved == nil || c.saved.Serial != "SAVED" || c.saved.Address != "192.0.2.42" || c.saved.EncryptionKey != "" || c.saved.Token != "" || !m.rows[0].Connected || m.rows[0].Address != "192.0.2.42" {
		t.Fatal("saved pairing reconnect did not use agent-held credentials")
	}
	if m.form != unitreeNoForm || !strings.Contains(m.message, "Connected to") {
		t.Fatal("verified connection did not finish the IP form")
	}
	c.forgetErr = errors.New("device offline")
	m, cmd = unitreePairUpdate(m, unitreeKey("f"))
	m, _ = unitreePairUpdate(m, cmd())
	if len(m.paired) != 1 || !strings.Contains(m.message, "device offline") || m.busy {
		t.Fatal("failed forget discarded saved pairing")
	}
	c.forgetErr = nil
	m, cmd = unitreePairUpdate(m, unitreeKey("f"))
	m, _ = unitreePairUpdate(m, cmd())
	if c.forgot != "SAVED" || len(m.paired) != 0 || len(m.rows) != 0 {
		t.Fatal("saved offline robot could not be forgotten")
	}
}

func TestUnitreeIPv4ValidationMatchesDiscovery(t *testing.T) {
	for _, address := range []string{"192.0.2.42", "169.254.1.42"} {
		if got, err := unitreePairIPv4(" " + address + " "); err != nil || got != address {
			t.Fatalf("pairable discovery address %q: %q, %v", address, got, err)
		}
	}
	for _, address := range []string{"2001:db8::42", "fe80::42", "::ffff:192.0.2.42", "127.0.0.1", "255.255.255.255", "224.0.0.1", "0.0.0.0", "robot.local", "192.0.2.42:9991"} {
		if _, err := unitreePairIPv4(address); err == nil {
			t.Fatalf("unpairable address %q accepted", address)
		}
	}
}

func TestUnitreeFailedDiscoveryDoesNotKeepStaleConnectionStatus(t *testing.T) {
	c := &unitreePairTestClient{paired: []*agentpbv2.UnitreeRobot{{Serial: "SAVED", Paired: true, Connected: true, Address: "192.0.2.42"}},
		discovered: []*agentpbv2.UnitreeRobot{{Serial: "SAVED", Paired: true, Connected: true, Address: "192.0.2.42"}},
	}
	m := newUnitreePairModel(&unitreePairHandler{ctx: context.Background(), client: c})
	m, _ = unitreePairUpdate(m, m.Init()())
	c.paired = []*agentpbv2.UnitreeRobot{{Serial: "SAVED", Paired: true, Address: "192.0.2.42"}}
	c.discoveryErr = errors.New("LAN unavailable")
	m, _ = unitreePairUpdate(m, m.Init()())
	if m.rows[0].Connected || !m.rows[0].Paired || !strings.Contains(m.scanError, "LAN unavailable") {
		t.Fatal("old discovery connection status overrode the latest saved pairing")
	}
}

func TestUnitreeOldAgentScanShowsUpgradeGuidance(t *testing.T) {
	unknown := status.Error(codes.Unimplemented, "unknown service wendy.agent.services.v2.WendyUnitreeService")
	c := &unitreePairTestClient{listErr: unknown, discoveryErr: unknown}
	m := newUnitreePairModel(&unitreePairHandler{ctx: context.Background(), client: c})
	m, _ = unitreePairUpdate(m, m.Init()())
	if m.scanError != unitreeAgentTooOld {
		t.Fatalf("old agent scan error = %q, want upgrade guidance", m.scanError)
	}
}

func TestUnitreeDiscoveryKeepsAccountAlias(t *testing.T) {
	c := &unitreePairTestClient{connected: true, discovered: []*agentpbv2.UnitreeRobot{{Serial: "MINE", Name: "Unitree Go2", Address: "192.0.2.42"}}}
	m := newUnitreePairModel(&unitreePairHandler{ctx: context.Background(), client: c})
	m.account = &unitree.Account{Devices: []unitree.Device{{Serial: "MINE", Name: "Rex", EncryptionKey: "mine"}}}
	m, _ = unitreePairUpdate(m, m.Init()())
	if len(m.rows) != 1 || m.rows[0].Name != "Rex" || m.rows[0].Address != "192.0.2.42" {
		t.Fatalf("discovery overwrote the account alias: %+v", m.rows)
	}
	m, cmd := unitreePairUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = unitreePairUpdate(m, cmd())
	if c.saved == nil || c.saved.Name != "Rex" {
		t.Fatal("pairing persisted the generic discovery name instead of the alias")
	}
}

func TestUnitreeLoginRefreshesSavedRobotKey(t *testing.T) {
	c := &unitreePairTestClient{paired: []*agentpbv2.UnitreeRobot{{Serial: "SAVED", Paired: true, Address: "192.0.2.42"}}}
	m := newUnitreePairModel(&unitreePairHandler{ctx: context.Background(), client: c})
	m.account = &unitree.Account{Devices: []unitree.Device{{Serial: "SAVED", EncryptionKey: "fresh-key"}}}
	m, _ = unitreePairUpdate(m, m.Init()())
	if strings.Join(c.knownSerials, ",") != "SAVED" {
		t.Fatal("saved and account robot duplicated in discovery request")
	}
	m, cmd := unitreePairUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = unitreePairUpdate(m, cmd())
	if c.saved == nil || c.saved.EncryptionKey != "fresh-key" || c.saved.Token != "" {
		t.Fatal("explicit login did not replace the saved robot's credentials")
	}
}

func TestUnitreeLegacyAccountRobotUsesNonSecretMarkerWithoutKey(t *testing.T) {
	c := &unitreePairTestClient{connected: true}
	m := newUnitreePairModel(&unitreePairHandler{ctx: context.Background(), client: c, initialIP: "192.0.2.42"})
	m.account = &unitree.Account{Devices: []unitree.Device{{Serial: "LEGACY", Name: "Older Go2"}}}
	m, _ = unitreePairUpdate(m, m.Init()())
	m, cmd := unitreePairUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("account robot without an encryption key was blocked before the agent could select its firmware protocol")
	}
	m, _ = unitreePairUpdate(m, cmd())
	if c.saved == nil || c.saved.Serial != "LEGACY" || c.saved.EncryptionKey != "" || c.saved.Token != unitreeAccountFlowMarker || c.saved.Address != "192.0.2.42" || !m.rows[0].Connected {
		t.Fatal("legacy robot did not receive only a non-secret account-flow marker")
	}
}

func TestUnitreeNeverUsesUnmatchedAccountKey(t *testing.T) {
	c := &unitreePairTestClient{discovered: []*agentpbv2.UnitreeRobot{{Serial: "OTHER", Name: "Other", Address: "192.0.2.42"}}}
	m := newUnitreePairModel(&unitreePairHandler{ctx: context.Background(), client: c})
	m.account = &unitree.Account{Devices: []unitree.Device{{Serial: "MINE", Name: "Mine", EncryptionKey: "mine"}}}
	m, _ = unitreePairUpdate(m, m.Init()())
	m.table.SetCursor(1)
	m, cmd := unitreePairUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || c.saved != nil || !strings.Contains(m.message, "matching account serial") {
		t.Fatal("unmatched LAN robot received account credentials")
	}
}

func TestUnitreeFailedScansRetainRobotsAndOperationErrors(t *testing.T) {
	c := &unitreePairTestClient{paired: []*agentpbv2.UnitreeRobot{{Serial: "SAVED", Paired: true, Address: "192.0.2.42"}}}
	m := newUnitreePairModel(&unitreePairHandler{ctx: context.Background(), client: c})
	m, _ = unitreePairUpdate(m, m.Init()())
	c.listErr, c.discoveryErr = errors.New("list unavailable"), errors.New("LAN unavailable")
	m, _ = unitreePairUpdate(m, m.Init()())
	if len(m.rows) != 1 || !strings.Contains(m.View(), "list unavailable") || !strings.Contains(m.View(), "LAN unavailable") {
		t.Fatal("failed scan lost robots or error details")
	}
	m, cmd := unitreePairUpdate(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatal("unknown pairing state allowed reconnect")
	}
	m.message = "Connection failed previously"
	c.listErr, c.discoveryErr = nil, nil
	m, _ = unitreePairUpdate(m, m.Init()())
	if !strings.Contains(m.View(), "Connection failed previously") || m.scanError != "" {
		t.Fatal("successful refresh erased operation error or kept stale scan error")
	}
}

func TestUnitreeCredentialsRequireSecureTransportAndErrorsStayPrivate(t *testing.T) {
	for _, conn := range []*grpcclient.AgentConnection{nil, {}, {Host: "127.0.0.1"}, {Host: "192.0.2.1"}} {
		if unitreePairTransportError(conn) == nil {
			t.Fatal("plaintext TCP accepted for Unitree credentials")
		}
	}
	for _, conn := range []*grpcclient.AgentConnection{{IsMTLS: true}, {IsSessionProxy: true}, {Host: "unix:/run/wendy.sock"}} {
		if err := unitreePairTransportError(conn); err != nil {
			t.Fatal(err)
		}
	}
	c := &unitreePairTestClient{pairErr: status.Error(codes.Internal, "secret-key secret-token")}
	h := &unitreePairHandler{ctx: context.Background(), client: c, transportError: errors.New("secure transport required")}
	result := h.pair(&agentpbv2.UnitreeRobot{Serial: "S", Address: "192.0.2.1"}, "secret-key", true)().(unitreePairOpMsg)
	if result.err == nil || c.saved != nil {
		t.Fatal("key sent over insecure transport")
	}
	h.transportError = nil
	result = h.pair(&agentpbv2.UnitreeRobot{Serial: "S", Address: "192.0.2.1"}, "secret-key", true)().(unitreePairOpMsg)
	if result.err == nil || strings.Contains(result.err.Error(), "secret") {
		t.Fatal("pair error exposed credentials")
	}
	h.login = func(context.Context, string, string, string) (*unitree.Account, error) {
		return nil, errors.New("echo password-secret")
	}
	login := h.signIn("email", "password-secret")().(unitreePairLoginMsg)
	if login.err == nil || strings.Contains(login.err.Error(), "password-secret") {
		t.Fatal("login error exposed credentials")
	}
}

func TestDevicePairUnitreeCancelsFormsAndSanitizesView(t *testing.T) {
	h := &unitreePairHandler{ctx: context.Background(), client: &unitreePairTestClient{}}
	m := newDevicePairModel(&pairTestBluetooth{}, pairTestHandler(&pairTestClient{}), nil, h)
	m.active = pairUnitreeTab
	m, _ = pairUpdate(m, m.Init()())
	m, _ = pairUpdate(m, unitreeKey("l"))
	m.unitree.password.SetValue("secret")
	m, quit := pairUpdate(m, tea.KeyMsg{Type: tea.KeyEsc})
	if quit != nil || m.done || m.unitree.form != unitreeNoForm || m.unitree.password.Value() != "" {
		t.Fatal("Escape did not clear credentials and return to the robot list")
	}
	m, _ = pairUpdate(m, unitreeKey("l"))
	m.unitree.password.SetValue("secret")
	m, quit = pairUpdate(m, tea.KeyMsg{Type: tea.KeyCtrlC})
	if quit == nil || !m.done || m.unitree.password.Value() != "" {
		t.Fatal("Ctrl+C retained password or did not quit")
	}
	robot := newUnitreePairModel(nil)
	bad := "Go2\x1b[2J\r\u202e"
	robot.discovered = []*agentpbv2.UnitreeRobot{{Serial: bad, Name: bad, Address: bad}}
	robot.message = bad
	robot.refreshRows("")
	robot, _ = unitreePairUpdate(robot, tea.WindowSizeMsg{Width: 38, Height: 14})
	view := robot.View()
	if strings.Contains(view, "\x1b[2J") || strings.ContainsAny(ansi.Strip(view), "\r\u202e") {
		t.Fatal("robot metadata injected terminal controls")
	}
	for _, line := range strings.Split(view, "\n") {
		if ansi.StringWidth(line) > 38 {
			t.Fatalf("view exceeds terminal width: %q", line)
		}
	}
}

func TestUnitreePairFlagsRejectInvalidInputBeforeConnecting(t *testing.T) {
	for _, flags := range [][]string{{"--ip", ""}, {"--ip", "robot.local"}, {"--ip", "127.0.0.1"}, {"--ip", "::1"}, {"--unitree-region", "other"}, {"--list", "--unitree"}} {
		cmd := newDevicePairCmd()
		if err := cmd.ParseFlags(flags); err != nil {
			t.Fatal(err)
		}
		if err := cmd.RunE(cmd, nil); err == nil {
			t.Fatalf("accepted invalid flags %v", flags)
		}
	}
}
