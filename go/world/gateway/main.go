// Package gateway embeds the signed simulation World service in Wendy hosts.
package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/diagnostics"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/node"
	"github.com/wendylabsinc/wendy/go/world/internal/proposalreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/proposals"
	"github.com/wendylabsinc/wendy/go/world/internal/runtimediag"
	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
	"github.com/wendylabsinc/wendy/go/world/internal/store"
	"github.com/wendylabsinc/wendy/go/world/internal/subscription"
	"github.com/wendylabsinc/wendy/go/world/internal/taskhistory"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/taskrelay"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

var errUnsupportedTaskCatalog = errors.New("unsupported simulation MOVE task catalog")
var errSimulationTransport = errors.New("HTTP runtime requires explicit allow_insecure_simulation; physical operation is unsupported")
var errSimulationClock = errors.New("worldd currently requires a simulation: clock domain")
var errStatePaths = errors.New("state paths must be distinct")
var errInspectionEvidence = errors.New("inspect catalog requires local evidence store")
var errGo2Binding = errors.New("Go2 catalog requires an exact commissioned simulation binding and no supervision")

type taskConfig struct {
	ControlContext    *missioncontrol.SignedContext  `json:"control_context,omitempty"`
	Go2Binding        *tasking.Go2Binding            `json:"go2_binding,omitempty"`
	EnableSupervision bool                           `json:"enable_supervision"`
	Path              string                         `json:"path"`
	Assignee          string                         `json:"assignee"`
	ExecutorPrincipal string                         `json:"executor_principal"`
	Catalog           map[string]mission.SafetyClass `json:"catalog"`
	MaxLeaseMS        int                            `json:"max_lease_ms"`
	MaxTelemetryAgeMS int                            `json:"max_telemetry_age_ms"`
	MaxTasks          int                            `json:"max_tasks"`
}
type evidenceConfig struct {
	ModalityBudgets  map[string]evidence.ModalityBudget `json:"modality_budgets,omitempty"`
	Path             string                             `json:"path"`
	QuotaBytes       int64                              `json:"quota_bytes"`
	MaxBlobBytes     int64                              `json:"max_blob_bytes"`
	MaxRetentionMS   int64                              `json:"max_retention_ms"`
	MaxEntries       int                                `json:"max_entries"`
	MaxPins          int                                `json:"max_pins"`
	MaxPinReferences int                                `json:"max_pin_references"`
	MaxPinsPerOwner  int                                `json:"max_pins_per_owner"`
}
type taskRelayConfig struct {
	Path       string                     `json:"path"`
	MaxEntries int                        `json:"max_entries"`
	MaxBytes   int                        `json:"max_bytes"`
	Routes     map[string]taskrelay.Route `json:"routes"`
}
type annotationClockConfig struct {
	Grant      authority.SignedGrant `json:"grant"`
	PrivateKey []byte                `json:"private_key"`
}
type proposalInboxConfig struct {
	Path       string                            `json:"path"`
	MaxEntries int                               `json:"max_entries"`
	MaxBytes   int                               `json:"max_bytes"`
	Origins    map[string]proposalreplica.Origin `json:"origins"`
}
type proposalConfig struct {
	Export     bool   `json:"export,omitempty"`
	Path       string `json:"path"`
	MaxEntries int    `json:"max_entries"`
	MaxBytes   int    `json:"max_bytes"`
}
type taskHistoryConfig struct {
	Path       string                        `json:"path"`
	Export     bool                          `json:"export"`
	Origins    map[string]taskhistory.Origin `json:"origins"`
	MaxEntries int                           `json:"max_entries"`
	MaxBytes   int                           `json:"max_bytes"`
}
type config struct {
	ReplicaPolicy           *subscription.Policy        `json:"replica_policy,omitempty"`
	ProposalInbox           *proposalInboxConfig        `json:"proposal_inbox,omitempty"`
	TaskHistory             *taskHistoryConfig          `json:"task_history,omitempty"`
	MissionClock            *annotationClockConfig      `json:"mission_clock,omitempty"`
	Proposals               *proposalConfig             `json:"proposals,omitempty"`
	AnnotationClock         *annotationClockConfig      `json:"annotation_clock,omitempty"`
	Diagnostics             *diagnosticsConfig          `json:"diagnostics,omitempty"`
	RequestTrace            *diagnostics.Config         `json:"request_trace,omitempty"`
	SimulationFaults        *simulationFaults           `json:"simulation_faults,omitempty"`
	TaskRelay               *taskRelayConfig            `json:"task_relay,omitempty"`
	Evidence                *evidenceConfig             `json:"evidence,omitempty"`
	Tasks                   *taskConfig                 `json:"tasks,omitempty"`
	Listen                  string                      `json:"listen"`
	DataPath                string                      `json:"data_path"`
	ProofPath               string                      `json:"proof_path"`
	RevocationPath          string                      `json:"revocation_path"`
	ReplayPath              string                      `json:"replay_path"`
	Tenant                  string                      `json:"tenant"`
	Site                    string                      `json:"site"`
	ClockDomain             string                      `json:"clock_domain"`
	Audience                string                      `json:"audience"`
	Frames                  []world.FrameRef            `json:"frames"`
	RootPublicKey           []byte                      `json:"root_public_key"`
	Revocations             authority.SignedRevocations `json:"revocations"`
	PeerGrant               authority.SignedGrant       `json:"peer_grant"`
	PeerPrivateKey          []byte                      `json:"peer_private_key"`
	Peers                   []node.Peer                 `json:"peers"`
	SyncIntervalMS          int                         `json:"sync_interval_ms"`
	AllowInsecureSimulation bool                        `json:"allow_insecure_simulation"`
	MaxRecords              int                         `json:"max_records"`
}

// serve owns all opened state until HTTP handlers and workers have stopped.
func serve(parent context.Context, path string, options Options, ready func(string)) error {
	if err := parent.Err(); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open node configuration: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	_ = f.Close()
	if err != nil {
		return fmt.Errorf("read node configuration: %w", err)
	}
	if len(data) > 1<<20 {
		return errors.New("configuration exceeds 1 MiB")
	}
	var cfg config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&cfg); err != nil {
		return fmt.Errorf("decode node configuration: %w", err)
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return errors.New("trailing config data")
	}
	if !cfg.AllowInsecureSimulation {
		return errSimulationTransport
	}
	if cfg.Listen == "" || cfg.DataPath == "" || cfg.Tenant == "" || cfg.Site == "" || cfg.Audience == "" || len(cfg.RootPublicKey) != ed25519.PublicKeySize {
		return errors.New("missing node identity or storage configuration")
	}
	if len(cfg.ClockDomain) < 11 || cfg.ClockDomain[:11] != "simulation:" {
		return errSimulationClock
	}
	if err = validateSimulationFaults(cfg); err != nil {
		return err
	}
	if _, _, err = net.SplitHostPort(cfg.Listen); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if cfg.MaxRecords == 0 {
		cfg.MaxRecords = 2048
	}
	if cfg.MaxRecords < 1 || cfg.MaxRecords > 10000 {
		return errors.New("max_records must be 1..10000")
	}
	if cfg.SyncIntervalMS == 0 {
		cfg.SyncIntervalMS = 500
	}
	if cfg.SyncIntervalMS < 100 || cfg.SyncIntervalMS > 60000 {
		return errors.New("sync_interval_ms must be 100..60000")
	}
	if cfg.ReplicaPolicy != nil {
		normalized, e := cfg.ReplicaPolicy.Normalize()
		if e != nil {
			return e
		}
		if normalized.Tenant != cfg.Tenant || normalized.Site != cfg.Site {
			return subscription.ErrMismatch
		}
		cfg.ReplicaPolicy = &normalized
	}
	if cfg.ProofPath == "" {
		cfg.ProofPath = cfg.DataPath + ".proofs"
	}
	if cfg.RevocationPath == "" {
		cfg.RevocationPath = cfg.DataPath + ".revocations"
	}
	if cfg.ReplayPath == "" {
		cfg.ReplayPath = cfg.DataPath + ".replay"
	}
	statePaths := []string{cfg.DataPath, cfg.ProofPath, cfg.ReplayPath, cfg.RevocationPath}
	if cfg.TaskHistory != nil {
		if cfg.TaskHistory.Path == "" {
			cfg.TaskHistory.Path = cfg.DataPath + ".task-history"
		}
		statePaths = append(statePaths, cfg.TaskHistory.Path)
	}
	if cfg.TaskRelay != nil {
		if cfg.TaskRelay.Path == "" {
			cfg.TaskRelay.Path = cfg.DataPath + ".task-relay"
		}
		statePaths = append(statePaths, cfg.TaskRelay.Path)
	}
	if cfg.Evidence != nil {
		if cfg.Evidence.Path == "" {
			cfg.Evidence.Path = cfg.DataPath + ".evidence"
		}
		if cfg.Evidence.MaxRetentionMS < 0 || cfg.Evidence.MaxRetentionMS > int64((30*24*time.Hour)/time.Millisecond) {
			return errors.New("invalid evidence retention")
		}
		// Validate policy before opening any runtime state. Open repeats this check.
		if _, err := evidence.NormalizeModalityBudgets(cfg.Evidence.ModalityBudgets, cfg.Evidence.QuotaBytes, time.Duration(cfg.Evidence.MaxRetentionMS)*time.Millisecond); err != nil {
			return err
		}
		statePaths = append(statePaths, cfg.Evidence.Path)
	}
	if cfg.Tasks != nil {
		if cfg.Tasks.Path == "" {
			cfg.Tasks.Path = cfg.DataPath + ".tasks"
		}
		if cfg.Tasks.MaxLeaseMS == 0 {
			cfg.Tasks.MaxLeaseMS = 500
		}
		if cfg.Tasks.MaxTelemetryAgeMS == 0 {
			cfg.Tasks.MaxTelemetryAgeMS = 500
		}
		if cfg.Tasks.MaxTasks == 0 {
			cfg.Tasks.MaxTasks = 1024
		}
		if cfg.Tasks.Assignee == "" || cfg.Tasks.ExecutorPrincipal == "" || len(cfg.Tasks.Catalog) == 0 || len(cfg.Frames) == 0 || cfg.Tasks.MaxLeaseMS < 1 || cfg.Tasks.MaxLeaseMS > 500 || cfg.Tasks.MaxTelemetryAgeMS < 1 || cfg.Tasks.MaxTelemetryAgeMS > 500 || cfg.Tasks.MaxTasks < 1 || cfg.Tasks.MaxTasks > 10000 {
			return errors.New("invalid simulation task configuration")
		}
		if len(cfg.Tasks.Catalog) < 1 || len(cfg.Tasks.Catalog) > 2 {
			return errUnsupportedTaskCatalog
		}
		for typ, class := range cfg.Tasks.Catalog {
			if (typ != "wendy.nav.go_to.v1" && typ != tasking.InspectType && typ != tasking.Go2GoToType) || class != mission.Move {
				return errUnsupportedTaskCatalog
			}
		}
		if err := validateGo2TaskConfiguration(cfg); err != nil {
			return err
		}
		if _, inspect := cfg.Tasks.Catalog[tasking.InspectType]; inspect && cfg.Evidence == nil {
			return errInspectionEvidence
		}
		statePaths = append(statePaths, cfg.Tasks.Path)
	}
	if cfg.Proposals != nil {
		if cfg.Tasks == nil {
			return errors.New("proposals require configured local tasks")
		}
		if cfg.Proposals.Path == "" {
			cfg.Proposals.Path = cfg.DataPath + ".proposals"
		}
		if cfg.Proposals.MaxEntries == 0 {
			cfg.Proposals.MaxEntries = 1000
		}
		if cfg.Proposals.MaxBytes == 0 {
			cfg.Proposals.MaxBytes = 16 << 20
		}
		if cfg.Proposals.MaxEntries < 1 || cfg.Proposals.MaxEntries > 10000 || cfg.Proposals.MaxBytes < proposals.MaxRecordBytes || cfg.Proposals.MaxBytes > 64<<20 {
			return errors.New("invalid proposal capacity")
		}
		statePaths = append(statePaths, cfg.Proposals.Path)
	}
	if cfg.ProposalInbox != nil {
		pc := cfg.ProposalInbox
		if pc.Path == "" {
			pc.Path = cfg.DataPath + ".proposal-inbox"
		}
		if pc.MaxEntries == 0 {
			pc.MaxEntries = 1000
		}
		if pc.MaxBytes == 0 {
			pc.MaxBytes = 16 << 20
		}
		if err = proposalreplica.ValidateConfig(proposalreplica.Config{Tenant: cfg.Tenant, Site: cfg.Site, Origins: pc.Origins, MaxEntries: pc.MaxEntries, MaxBytes: pc.MaxBytes}); err != nil {
			return err
		}
		statePaths = append(statePaths, pc.Path)
	}
	if err = validateStatePaths(statePaths, path, cfg.ReplayPath); err != nil {
		return err
	}
	protected := append(append([]string{}, statePaths...), path, cfg.ReplayPath+authority.ReplayStagingSuffix)
	if err = validateDiagnosticPaths(cfg.Diagnostics, cfg.RequestTrace, protected); err != nil {
		return err
	}
	owned := append([]string(nil), protected...)
	for _, statePath := range statePaths {
		owned = append(owned, statePath+".lock")
	}
	if cfg.Diagnostics != nil {
		owned = append(owned, cfg.Diagnostics.Path, cfg.Diagnostics.Path+".lock")
	}
	if cfg.RequestTrace != nil {
		owned = append(owned, cfg.RequestTrace.Path, cfg.RequestTrace.Path+".lock", cfg.RequestTrace.TriggerPath, cfg.RequestTrace.TriggerPath+".lock")
	}
	if err = validateHostStatePaths(owned, options); err != nil {
		return err
	}

	var metrics *runtimeobs.Collector
	var diagnosticWriter *runtimediag.Writer
	if cfg.Diagnostics != nil {
		metrics = runtimeobs.New()
		diagnosticWriter, err = runtimediag.Open(cfg.Diagnostics.Path, metrics)
		if err != nil {
			return err
		}
		defer diagnosticWriter.Flush()
	}
	var traceWriter *diagnostics.Writer
	var traceCollector *diagnostics.Collector
	if cfg.RequestTrace != nil {
		traceWriter, err = diagnostics.Open(*cfg.RequestTrace)
		if err != nil {
			return err
		}
		traceCollector = traceWriter.Collector()
	}
	replay, err := authority.OpenReplay(cfg.ReplayPath, 100000)
	if err != nil {
		return fmt.Errorf("open replay ledger: %w", err)
	}
	defer replay.Close()
	replay.SetMetrics(metrics)
	verifier, err := authority.NewVerifier(authority.Config{Metrics: metrics, RootPublicKey: cfg.RootPublicKey, Tenant: cfg.Tenant, Site: cfg.Site, Domain: "simulation", Audience: cfg.Audience, Now: time.Now, Replay: replay, MaxBodyBytes: node.MaxBodyBytes})
	if err != nil {
		return err
	}
	savedRevocations, err := node.LoadRevocations(cfg.RevocationPath, cfg.Revocations)
	if err != nil {
		return fmt.Errorf("load revocations: %w", err)
	}
	if err = verifier.UpdateRevocations(savedRevocations); err != nil {
		return fmt.Errorf("revocations: %w", err)
	}
	s, err := store.OpenSubscribed(cfg.DataPath, world.Config{Tenant: cfg.Tenant, Site: cfg.Site, ClockDomain: cfg.ClockDomain, Now: time.Now, MaxEntries: cfg.MaxRecords, MaxFutureSkew: 100 * time.Millisecond, AllowSynthetic: true, Frames: cfg.Frames}, cfg.ReplicaPolicy)
	if err != nil {
		return fmt.Errorf("open world replica: %w", err)
	}
	defer s.Close()
	s.SetMetrics(metrics)
	nc := node.Config{Metrics: metrics, Trace: traceCollector, Audience: cfg.Audience, Tenant: cfg.Tenant, Site: cfg.Site, ProofPath: cfg.ProofPath, RevocationPath: cfg.RevocationPath, MaxRecords: cfg.MaxRecords}
	if cfg.MissionClock != nil {
		nc.MissionClock = &node.MissionClockConfig{Credentials: node.Credentials{Grant: cfg.MissionClock.Grant, PrivateKey: cfg.MissionClock.PrivateKey}, RunID: cfg.ClockDomain[11:], ClockDomain: cfg.ClockDomain}
	}
	if cfg.AnnotationClock != nil {
		nc.AnnotationClock = &node.AnnotationClockConfig{Credentials: node.Credentials{Grant: cfg.AnnotationClock.Grant, PrivateKey: cfg.AnnotationClock.PrivateKey}, RunID: cfg.ClockDomain[11:], ClockDomain: cfg.ClockDomain}
	}
	if cfg.TaskRelay != nil {
		rc := cfg.TaskRelay
		relay, err := taskrelay.Open(rc.Path, taskrelay.Config{Tenant: cfg.Tenant, Site: cfg.Site, Domain: "simulation", Routes: rc.Routes, Verifier: verifier, Now: time.Now, MaxEntries: rc.MaxEntries, MaxBytes: rc.MaxBytes})
		if err != nil {
			return fmt.Errorf("open task relay: %w", err)
		}
		defer relay.Close()
		nc.TaskRelay, nc.TaskRoutes = relay, rc.Routes
		nc.TaskStatusCredentials = node.Credentials{Grant: cfg.PeerGrant, PrivateKey: cfg.PeerPrivateKey}
	}
	if cfg.Evidence != nil {
		ec := cfg.Evidence
		blobs, err := evidence.Open(ec.Path, evidence.Config{ModalityBudgets: ec.ModalityBudgets, Tenant: cfg.Tenant, Site: cfg.Site, QuotaBytes: ec.QuotaBytes, MaxBlobBytes: ec.MaxBlobBytes, MaxRetention: time.Duration(ec.MaxRetentionMS) * time.Millisecond, MaxEntries: ec.MaxEntries, MaxPins: ec.MaxPins, MaxPinReferences: ec.MaxPinReferences, MaxPinsPerOwner: ec.MaxPinsPerOwner, Now: time.Now})
		if err != nil {
			return fmt.Errorf("open evidence store: %w", err)
		}
		defer blobs.Close()
		nc.Evidence = blobs
	}
	if cfg.Tasks != nil {
		origin := time.Now()
		tc := cfg.Tasks
		taskOptions := tasking.Config{RunID: cfg.ClockDomain[11:], Site: cfg.Site, Assignee: tc.Assignee, ExecutorPrincipal: tc.ExecutorPrincipal, Domain: mission.Simulation, Catalog: tc.Catalog, Now: time.Now, Monotonic: func() time.Duration { return time.Since(origin) }, MaxLease: time.Duration(tc.MaxLeaseMS) * time.Millisecond, MaxTelemetryAge: time.Duration(tc.MaxTelemetryAgeMS) * time.Millisecond, MaxTasks: tc.MaxTasks, ValidateAuthority: node.MissionAuthority(verifier), ValidateInspection: node.InspectionEvidence(nc.Evidence)}
		taskOptions.Go2Binding = tc.Go2Binding
		taskOptions.ControlContext = tc.ControlContext
		if tc.ControlContext != nil {
			if err := verifier.VerifyMissionControlContext(*tc.ControlContext); err != nil {
				return fmt.Errorf("mission control context: %w", err)
			}
		}
		taskOptions.Metrics = metrics
		taskOptions.ValidateAuthorityContext = node.MissionAuthorityContext(verifier)
		if tc.EnableSupervision {
			taskOptions.Tenant, taskOptions.Audience, taskOptions.GatewayPrincipal = cfg.Tenant, cfg.Audience, cfg.PeerGrant.Grant.Principal
			taskOptions.ValidateSupervisor = node.SupervisorAuthority(verifier, cfg.PeerGrant)
			taskOptions.ValidateSupervisorContext = node.SupervisorAuthorityContext(verifier, cfg.PeerGrant)
			taskOptions.ValidateSupervisorsContext = node.SupervisorAuthorityBatchContext(verifier, cfg.PeerGrant)
			nc.SupervisionEnabled = true
			nc.TaskStatusCredentials = node.Credentials{Grant: cfg.PeerGrant, PrivateKey: cfg.PeerPrivateKey}
		}
		tasks, err := tasking.Open(tc.Path, taskOptions)
		if err != nil {
			return fmt.Errorf("open task store: %w", err)
		}
		defer tasks.Close()
		nc.Tasks = tasks
		nc.TaskAssignee = tc.Assignee
		nc.TaskFrames = cfg.Frames
	}
	if cfg.TaskHistory != nil {
		hc := cfg.TaskHistory
		history, e := taskhistory.Open(hc.Path, taskhistory.Config{Tenant: cfg.Tenant, Site: cfg.Site, Origins: hc.Origins, Verifier: verifier, MaxEntries: hc.MaxEntries, MaxBytes: hc.MaxBytes, Now: time.Now})
		if e != nil {
			return e
		}
		defer history.Close()
		nc.TaskHistory = history
		nc.ExportTaskHistory = hc.Export
		nc.TaskStatusCredentials = node.Credentials{Grant: cfg.PeerGrant, PrivateKey: cfg.PeerPrivateKey}
	}
	if cfg.Proposals != nil {
		pc := cfg.Proposals
		proposalStore, err := proposals.Open(pc.Path, proposals.Config{Tenant: cfg.Tenant, Site: cfg.Site, RunID: cfg.ClockDomain[11:], Audience: cfg.Audience, Assignee: cfg.Tasks.Assignee, Catalog: cfg.Tasks.Catalog, Now: time.Now, MaxEntries: pc.MaxEntries, MaxBytes: pc.MaxBytes})
		if err != nil {
			return fmt.Errorf("open proposals: %w", err)
		}
		defer proposalStore.Close()
		nc.Proposals = proposalStore
		if pc.Export {
			nc.ProposalExportCredentials = &node.Credentials{Grant: cfg.PeerGrant, PrivateKey: cfg.PeerPrivateKey}
		}
	}
	if cfg.ProposalInbox != nil {
		pc := cfg.ProposalInbox
		inbox, err := proposalreplica.Open(pc.Path, proposalreplica.Config{Tenant: cfg.Tenant, Site: cfg.Site, Origins: pc.Origins, Verifier: verifier, MaxEntries: pc.MaxEntries, MaxBytes: pc.MaxBytes})
		if err != nil {
			return fmt.Errorf("open proposal inbox: %w", err)
		}
		defer inbox.Close()
		nc.ProposalInbox = inbox
	}
	if err = applySimulationFaults(cfg, replay, nc.Tasks); err != nil {
		return err
	}
	runtime, err := node.New(nc, s, verifier)
	if err != nil {
		return fmt.Errorf("initialize world node: %w", err)
	}
	if err := runtime.ReconcileTaskEvidence(); err != nil {
		return fmt.Errorf("reconcile task evidence: %w", err)
	}
	if len(cfg.Peers) > 0 && len(cfg.PeerPrivateKey) != ed25519.PrivateKeySize {
		return errors.New("peer signing key required")
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	retentionDone := make(chan struct{})
	go func() {
		defer close(retentionDone)
		if nc.Tasks == nil || nc.Evidence == nil {
			return
		}
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := runtime.ReconcileTaskEvidence(); err != nil {
					log.Printf("task evidence maintenance: %v", err)
				}
			}
		}
	}()
	diagnosticsDone := make(chan struct{})
	go func() {
		defer close(diagnosticsDone)
		if err := diagnosticWriter.Run(ctx); err != nil {
			log.Print("diagnostics output failed")
		}
	}()
	traceDone := make(chan struct{})
	go func() {
		defer close(traceDone)
		if traceWriter != nil {
			if err := traceWriter.Run(ctx); err != nil {
				log.Print("request trace output failed; node continues")
			}
		}
	}()
	var handlerMu sync.Mutex
	var handlers sync.WaitGroup
	stopping := false
	handler := runtime.Handler()
	guarded := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerMu.Lock()
		if stopping {
			handlerMu.Unlock()
			http.Error(w, "World gateway stopping", http.StatusServiceUnavailable)
			return
		}
		handlers.Add(1)
		handlerMu.Unlock()
		defer handlers.Done()
		handler.ServeHTTP(w, r)
	})
	server := &http.Server{BaseContext: func(net.Listener) context.Context { return ctx }, Handler: guarded, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 64 << 10}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	credentials := node.Credentials{Grant: cfg.PeerGrant, PrivateKey: cfg.PeerPrivateKey}
	// Local delivery/status work has its own clock and never waits for a peer's
	// network timeout. The executor and actuator remain independent processes.
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		if cfg.TaskRelay == nil {
			return
		}
		ticker := time.NewTicker(time.Duration(cfg.SyncIntervalMS) * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := runtime.ProcessTaskRelay(); err != nil {
					log.Printf("local task relay: %v", err)
				}
			}
		}
	}()
	syncDone := make(chan struct{})
	go func() {
		defer close(syncDone)
		ticker := time.NewTicker(time.Duration(cfg.SyncIntervalMS) * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, peer := range cfg.Peers {
					if err := runtime.Sync(ctx, peer, credentials); err != nil && ctx.Err() == nil {
						log.Printf("peer %s: %v", peer.Audience, err)
					}
				}
			}
		}
	}()
	log.Printf("World simulation node %s listening on %s", cfg.Audience, listener.Addr())
	ready(listener.Addr().String())
	select {
	case <-ctx.Done():
	case err = <-done:
		cancel()
	}
	handlerMu.Lock()
	stopping = true
	handlerMu.Unlock()
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	shutdownErr := server.Shutdown(shutdown)
	if shutdownErr != nil {
		_ = server.Close()
	}
	cancel()
	<-syncDone
	<-relayDone
	<-retentionDone
	<-diagnosticsDone
	<-traceDone
	handlers.Wait()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return shutdownErr
}
