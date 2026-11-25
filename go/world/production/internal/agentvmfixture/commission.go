package agentvmfixture

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/agentmodule"
	"github.com/wendylabsinc/wendy/go/world/production/appservice"
	"github.com/wendylabsinc/wendy/go/world/production/hostpath"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

const MultimodalApp = "com.wendy.world.multimodal"
const MultimodalProofReader = "proof-reader"

// CommissionSpec is supplied by the isolated runtime's trusted test harness
// after it observes the scene's actual CNI endpoint and nonce-bound clock. It
// contains a token path, never a token. Writing policy does not authenticate or
// activate a source: agentmodule.Initialize independently checks live time and
// the pinned scene identity before reporting success. Failed initialization can
// leave partial store files and requires a fresh fixture, not automatic repair.
type CommissionSpec struct {
	IsolatedSimulationNetwork bool           `json:"isolated_simulation_network"`
	SceneURL                  string         `json:"scene_url"`
	ReadTokenFile             string         `json:"read_token_file"`
	RunID                     string         `json:"run_id"`
	BootID                    string         `json:"boot_id"`
	PackageSHA256             string         `json:"package_sha256"`
	Frame                     world.FrameRef `json:"frame"`
	AnchorUnixNS              int64          `json:"anchor_unix_ns"`
	Site                      string         `json:"site"`
	AppID                     string         `json:"app_id"`
	// Opt-in audit access only. Runtime generation comes from normal
	// deployment, and claim retention remains the publisher's explicit request.
	HistoryReader bool `json:"history_reader,omitempty"`
}

type CommissionReceipt struct {
	Kind          string `json:"kind"`
	State         string `json:"state"`
	ConfigPath    string `json:"config_path"`
	ConfigSHA256  string `json:"config_sha256"`
	AppID         string `json:"app_id"`
	Site          string `json:"site"`
	RunID         string `json:"run_id"`
	BootID        string `json:"boot_id"`
	ClockDomain   string `json:"clock_domain"`
	HistoryReader bool   `json:"history_reader,omitempty"`
}

func cleanAbsolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/"
}
func boundedText(s string, max int) bool {
	return s != "" && len(s) <= max && utf8.ValidString(s) && strings.TrimSpace(s) == s && strings.IndexFunc(s, unicode.IsControl) < 0
}
func missing(path string) bool { _, e := os.Lstat(path); return errors.Is(e, os.ErrNotExist) }

func validateCommission(directory string, s CommissionSpec) error {
	if !s.IsolatedSimulationNetwork || s.AppID != MultimodalApp || !boundedText(s.Site, 128) || !boundedText(s.BootID, 256) || s.Frame.Validate() != nil || !boundedText(s.Frame.ID, 256) || !boundedText(s.Frame.Revision, 256) || s.AnchorUnixNS <= 0 {
		return errors.New("exact isolated simulation commissioning required")
	}
	if !boundedText(s.RunID, 64) || strings.IndexFunc(s.RunID, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.')
	}) >= 0 {
		return errors.New("bounded simulation run ID required")
	}
	sum, err := hex.DecodeString(s.PackageSHA256)
	if err != nil || len(sum) != 32 || hex.EncodeToString(sum) != s.PackageSHA256 {
		return errors.New("exact package digest required")
	}
	u, err := url.Parse(s.SceneURL)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return errors.New("explicit isolated scene origin required")
	}
	ip, err := netip.ParseAddr(u.Hostname())
	port, pe := strconv.Atoi(u.Port())
	if err != nil || pe != nil || port < 1 || port > 65535 || ip.Zone() != "" || !(ip.IsPrivate() || ip.IsLoopback()) {
		return errors.New("numeric private scene endpoint required")
	}
	if !cleanAbsolute(s.ReadTokenFile) || !strings.HasPrefix(s.ReadTokenFile, directory+string(filepath.Separator)) {
		return errors.New("scene token must stay inside the protected fixture directory")
	}
	st, err := os.Lstat(s.ReadTokenFile)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
		return errors.New("private regular scene token required")
	}
	token, err := agentmodule.ReadCommissionedFile(s.ReadTokenFile, 256)
	if err != nil {
		return errors.New("protected scene token unavailable")
	}
	value := strings.TrimSpace(string(token))
	if len(value) < 32 || len(value) > 256 || strings.IndexFunc(value, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	}) >= 0 {
		return errors.New("invalid scene token encoding")
	}
	return nil
}

// CommissionMultimodal writes an exclusive fixed-scope World configuration for
// this test scene. It does not replace policy, create stores, contact a robot,
// acquire time, sign device data, or start the agent.
func CommissionMultimodal(directory, stateDirectory, specPath string) (CommissionReceipt, error) {
	if !cleanAbsolute(directory) || !cleanAbsolute(stateDirectory) || directory == stateDirectory || !cleanAbsolute(specPath) {
		return CommissionReceipt{}, errors.New("distinct protected fixture and state paths required")
	}
	a, err := load(directory)
	if err != nil {
		return CommissionReceipt{}, err
	}
	if !missing(a.public.ConfigPath) || !missing(a.public.SimulationClockPath) {
		return CommissionReceipt{}, errors.New("World already commissioned; create a new fixture")
	}
	raw, err := agentmodule.ReadCommissionedFile(specPath, 16<<10)
	var s CommissionSpec
	if err != nil || jws.DecodeStrict(raw, &s) != nil {
		return CommissionReceipt{}, errors.New("strict protected commissioning specification required")
	}
	// encoding/json treats null as an unchanged bool. The optional flag must
	// be absent or an actual JSON boolean, never an ambiguous null/default.
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return CommissionReceipt{}, errors.New("strict commissioning specification required")
	}
	if flag, present := fields["history_reader"]; present {
		flag = bytes.TrimSpace(flag)
		if !bytes.Equal(flag, []byte("true")) && !bytes.Equal(flag, []byte("false")) {
			return CommissionReceipt{}, errors.New("history_reader must be a boolean")
		}
	}
	if err := validateCommission(directory, s); err != nil {
		return CommissionReceipt{}, err
	}
	if err := hostpath.EnsureDirectory(stateDirectory, 0700, true); err != nil {
		return CommissionReceipt{}, err
	}
	entries, err := os.ReadDir(stateDirectory)
	if err != nil || len(entries) != 0 {
		return CommissionReceipt{}, errors.New("fresh empty World state directory required")
	}
	var grants []agentmodule.Grant
	var subscriptions []serviceaccess.Source
	for _, sensor := range [][2]string{{"camera-a", "camera-a"}, {"gas", "gas-1"}, {"audio", "audio-1"}, {"sensorlink-bridge", "camera-b"}} {
		grants = append(grants, agentmodule.Grant{App: MultimodalApp, Service: sensor[0], Publish: []serviceorigin.PublishScope{{ComponentType: sensorrecord.ComponentType, Stream: sensor[1]}}})
		subscriptions = append(subscriptions, serviceaccess.Source{App: MultimodalApp, Service: sensor[0], ComponentType: sensorrecord.ComponentType, Stream: sensor[1]})
	}
	grants = append(grants, agentmodule.Grant{App: MultimodalApp, Service: "fusion", Publish: []serviceorigin.PublishScope{{ComponentType: resultrecord.ComponentType, Stream: "hazard"}}, Subscribe: subscriptions})
	if s.HistoryReader {
		history := append([]serviceaccess.Source(nil), subscriptions...)
		history = append(history, serviceaccess.Source{App: MultimodalApp, Service: "fusion", ComponentType: resultrecord.ComponentType, Stream: "hazard"})
		grants = append(grants, agentmodule.Grant{App: MultimodalApp, Service: MultimodalProofReader, Subscribe: history})
	}
	policy := agentmodule.Policy{Version: 1, Tenant: a.public.Tenant, Site: s.Site, Device: a.public.Principal, ExecutionDomain: "simulation", SampleClockDomain: "simulation:" + s.RunID, Frames: []world.FrameRef{s.Frame}, Grants: grants, NotBefore: a.public.NotBefore, NotAfter: a.public.NotAfter}
	for _, g := range grants {
		if (serviceorigin.Scope{Tenant: policy.Tenant, Site: policy.Site, Device: policy.Device, App: g.App, Service: g.Service, ExecutionDomain: policy.ExecutionDomain, DeploymentGeneration: 1, Publish: g.Publish}).Validate() != nil {
			return CommissionReceipt{}, errors.New("invalid fixture authority scope")
		}
	}
	cfg := agentmodule.FileConfig{Policy: policy, RootsDER: [][]byte{a.public.RootDER}, SocketDirectory: filepath.Join(stateDirectory, "sockets"), JournalPath: filepath.Join(stateDirectory, "journal", "journal.db"), ProofArchivePath: filepath.Join(stateDirectory, "archive", "proofs.db"), EvidenceDirectory: filepath.Join(stateDirectory, "evidence"), SimulationClock: &agentmodule.SimulationClockConfig{URL: s.SceneURL, ReadTokenFile: s.ReadTokenFile, RunID: s.RunID, BootID: s.BootID, PackageSHA256: s.PackageSHA256, Frame: s.Frame, AnchorUnixNS: s.AnchorUnixNS}, Limits: appservice.Limits{MessagesPerSecond: 200, MessageBurst: 400, BytesPerSecond: 8 << 20, ByteBurst: 16 << 20}}
	if err := appservice.ValidateLimits(cfg.Limits); err != nil {
		return CommissionReceipt{}, err
	}
	if err := writeJSON(a.public.ConfigPath, cfg); err != nil {
		return CommissionReceipt{}, err
	}
	data, err := agentmodule.ReadCommissionedFile(a.public.ConfigPath, 64<<10)
	if err != nil {
		return CommissionReceipt{}, err
	}
	return CommissionReceipt{Kind: Kind, State: "configured_not_activated", ConfigPath: a.public.ConfigPath, ConfigSHA256: digest(data), AppID: MultimodalApp, Site: s.Site, RunID: s.RunID, BootID: s.BootID, ClockDomain: policy.SampleClockDomain, HistoryReader: s.HistoryReader}, nil
}
