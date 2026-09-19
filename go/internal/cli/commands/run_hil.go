package commands

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/stagefile"
	stagefilelock "github.com/wendylabsinc/wendy/go/internal/stagefile/lock"
)

// HIL owns a cloud inference deployment and a local forward for the lifetime
// of an attached simulator run. It calls the normal Go deployment path for
// both apps; it neither runs a project script nor changes the default device.
func runHILCommand(ctx context.Context, opts runOptions, peerName string) error {
	if peerName == "" && (opts.yes || !isInteractiveTerminal()) {
		return fmt.Errorf("--hil needs an interactive device picker; use --hil=DEVICE for non-interactive runs")
	}
	if opts.detach || opts.deploy || opts.service != "" || len(opts.fleetDevices) != 0 {
		return fmt.Errorf("HIL requires an attached, single-app run; omit --detach, --deploy and --service")
	}
	name, err := resolveHILSimulator(ctx, deviceFlag, opts.yes)
	if err != nil {
		return err
	}
	// The selected simulator is the target for both deployment and tunnel
	// addressing. Keep this invocation's choice out of persistent defaults.
	previousDevice := deviceFlag
	deviceFlag = vmDeviceIDPrefix + name
	defer func() { deviceFlag = previousDevice }()
	root, err := resolveRunWorkingDir(opts)
	if err != nil {
		return err
	}
	cfg, err := appconfig.LoadFromFile(filepath.Join(root, "wendy.json"))
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid simulator configuration: %w", err)
	}
	if err := validateHILConfig(root, cfg.HIL); err != nil {
		return err
	}
	if len(cfg.Services) != 0 {
		return fmt.Errorf("--hil requires a single simulator app")
	}
	if opts.dockerfile == "" {
		opts.dockerfile = cfg.HIL.SimulatorBuildFile
	}
	if opts.buildType != "" && normalizeBuildType(opts.buildType) != "docker" {
		return fmt.Errorf("--hil requires a Dockerfile or Stagefile simulator build")
	}
	for _, entry := range opts.env {
		if strings.SplitN(entry, "=", 2)[0] == cfg.HIL.URLEnv || (cfg.HIL.TokenEnv != "" && strings.SplitN(entry, "=", 2)[0] == cfg.HIL.TokenEnv) {
			return fmt.Errorf("--hil manages %s; remove that --env override", strings.SplitN(entry, "=", 2)[0])
		}
	}
	store, err := vm.NewStore()
	if err != nil {
		return err
	}
	st, err := store.Status(name)
	if err != nil {
		return err
	}
	if !st.Exists {
		return fmt.Errorf("no VM named %q; create it with 'wendy vm create %s'", name, name)
	}
	if st.Running && st.State.NetMode != vm.NetUser {
		return fmt.Errorf("HIL requires QEMU user networking; restart VM %q with --net user", name)
	}

	// Pin the cloud asset before building. The primary --device still names
	// the simulator throughout both deployments.
	var auth *config.AuthConfig
	if selector, selected, parseErr := parseCloudDeviceSelector(peerName); selected {
		if parseErr != nil {
			return parseErr
		}
		stored, loadErr := config.Load()
		if loadErr != nil {
			return loadErr
		}
		auth, err = selector.auth(stored)
		peerName = strconv.Itoa(int(selector.AssetID))
	} else {
		auth, err = pickAuthEntry("")
	}
	if err != nil {
		return err
	}
	asset, err := pickCloudDeviceMode(withDevicePickerPurpose(ctx, hilInferencePicker), auth, peerName, os.Getenv("WENDY_BROKER_URL"), true, peerName == "")
	if err != nil {
		return err
	}
	if fresh := reloadAuthEntry(auth); fresh != nil {
		auth = fresh
	}
	peer, err := connectCloudAsset(ctx, auth, asset, os.Getenv("WENDY_BROKER_URL"))
	if err != nil {
		return err
	}
	defer peer.Close()
	buildFile := filepath.Clean(cfg.HIL.BuildFile)
	if len(cfg.HIL.BuildFilesByGPUArch) > 0 {
		version, err := agentVersionForRun(ctx, peer)
		if err != nil {
			return fmt.Errorf("reading HIL device GPU architecture: %w", err)
		}
		buildFile = hilBuildFile(cfg.HIL, version.GetGpuArch())
		cliLogln("HIL GPU %s: using %s", version.GetGpuArch(), buildFile)
	}
	staged, cleanup, err := stageHILProject(root, cfg.HIL)
	if err != nil {
		return err
	}
	defer cleanup()
	token := ""
	if cfg.HIL.TokenEnv != "" {
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return err
		}
		token = hex.EncodeToString(secret)
	}
	cliLogln("Deploying HIL inference to %s...", asset.GetName())
	peerOpts := runOptions{
		prefix: staged, dockerfile: buildFile, buildType: "docker",
		builder: opts.builder, buildHost: opts.buildHost, stagefileBackend: opts.stagefileBackend,
		chunking: opts.chunking, yes: opts.yes, detach: true,
		watchTarget: &SelectedDevice{Agent: peer},
	}
	if token != "" {
		peerOpts.env = append(peerOpts.env, cfg.HIL.TokenEnv+"="+token)
	}
	deployErr := runCommand(ctx, peerOpts)
	lockErr := persistHILStagefileLock(staged, filepath.Join(root, cfg.HIL.Project), buildFile)
	if deployErr != nil {
		return errors.Join(fmt.Errorf("deploying HIL inference: %w", deployErr), lockErr)
	}
	if lockErr != nil {
		return lockErr
	}
	// The simulator build must not include the inference staging tree.
	cleanup()

	broker, err := clouddefaults.DialBroker(auth, os.Getenv("WENDY_BROKER_URL"))
	if err != nil {
		return err
	}
	defer broker.Close()
	forward, err := startHILForward(ctx, func(ctx context.Context) (net.Conn, error) {
		return openBrokerTunnel(ctx, broker, auth, asset.GetId(), uint32(cfg.HIL.Port))
	})
	if err != nil {
		return err
	}
	defer forward.Close()
	localURL := "http://" + forward.listener.Addr().String()
	cliLogln("Waiting for HIL inference on %s...", asset.GetName())
	if err := waitHILHealthAuthenticated(ctx, localURL+cfg.HIL.HealthPath, 180*time.Second, token, cfg.HIL.HealthSchema); err != nil {
		return fmt.Errorf("HIL inference did not become ready: %w", err)
	}
	port := forward.listener.Addr().(*net.TCPAddr).Port
	peerURL := fmt.Sprintf("http://10.0.2.2:%d", port)
	keys := make([]string, 0, len(cfg.HIL.Env))
	for key := range cfg.HIL.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	defaults := make([]string, 0, len(keys)+1)
	for _, key := range keys {
		defaults = append(defaults, key+"="+cfg.HIL.Env[key])
	}
	// Explicit user flags override HIL defaults, but never its managed URL.
	opts.env = append(defaults, opts.env...)
	opts.env = append(opts.env, cfg.HIL.URLEnv+"="+peerURL)
	if token != "" {
		opts.env = append(opts.env, cfg.HIL.TokenEnv+"="+token)
	}
	cliSuccess("HIL connected: %s → %s. Keep this command running; Ctrl+C closes the tunnel.", name, asset.GetName())
	return runCommand(ctx, opts)
}

// A project may need different framework versions and base images for Orin
// and newer GPUs. Select by the inference device, never by the simulator.
func hilBuildFile(cfg *appconfig.HILConfig, gpuArch string) string {
	if file := cfg.BuildFilesByGPUArch[gpuArch]; file != "" {
		return filepath.Clean(file)
	}
	return filepath.Clean(cfg.BuildFile)
}

var hilEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validateHILConfig(root string, cfg *appconfig.HILConfig) error {
	if cfg == nil {
		return fmt.Errorf("wendy.json has no hil configuration")
	}
	if cfg.TokenEnv != "" && (!hilEnvName.MatchString(cfg.TokenEnv) || cfg.TokenEnv == cfg.URLEnv) {
		return fmt.Errorf("hil.tokenEnv must be a valid variable distinct from urlEnv")
	}
	if cfg.Port < 1 || cfg.Port > 65535 || !hilEnvName.MatchString(cfg.URLEnv) {
		return fmt.Errorf("hil requires a valid port and urlEnv")
	}
	if !strings.HasPrefix(cfg.HealthPath, "/") || strings.HasPrefix(cfg.HealthPath, "//") || strings.ContainsAny(cfg.HealthPath, "?#\r\n") {
		return fmt.Errorf("hil.healthPath must be an absolute HTTP path")
	}
	if len(cfg.Inputs) == 0 {
		return fmt.Errorf("hil.inputs must name the inference source directories")
	}
	project, err := confinedHILPath(root, cfg.Project)
	if err != nil {
		return err
	}
	peer, err := appconfig.LoadFromFile(filepath.Join(project, "wendy.json"))
	if err != nil {
		return fmt.Errorf("loading HIL inference project: %w", err)
	}
	if err := peer.Validate(); err != nil {
		return fmt.Errorf("invalid HIL inference configuration: %w", err)
	}
	if peer.HIL != nil || len(peer.Services) != 0 {
		return fmt.Errorf("HIL inference must be a single app without nested hil configuration")
	}
	if err := validateDockerfileName(cfg.BuildFile); err != nil {
		return err
	}
	if _, err := confinedHILPath(project, cfg.BuildFile); err != nil {
		return err
	}
	for arch, file := range cfg.BuildFilesByGPUArch {
		if strings.TrimSpace(arch) == "" || file == "" {
			return fmt.Errorf("hil.buildFilesByGPUArch requires nonempty GPU architectures and build files")
		}
		if err := validateDockerfileName(file); err != nil {
			return err
		}
		if _, err := confinedHILPath(project, file); err != nil {
			return err
		}
	}
	if err := validateDockerfileName(cfg.SimulatorBuildFile); err != nil {
		return err
	}
	if _, err := confinedHILPath(root, cfg.SimulatorBuildFile); err != nil {
		return err
	}
	for key, value := range cfg.Env {
		if !hilEnvName.MatchString(key) || key == cfg.URLEnv || key == cfg.TokenEnv || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("invalid hil.env entry %q", key)
		}
	}
	for _, input := range cfg.Inputs {
		// Top-level names keep copy destinations disjoint from the project
		// config and avoid parent/child overlaps during staging.
		if filepath.Base(input) != input || input == "wendy.json" || input == cfg.BuildFile {
			return fmt.Errorf("hil.inputs entries must be top-level directories: %q", input)
		}
		path, err := confinedHILPath(root, input)
		if err != nil {
			return err
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("HIL input %q is not a directory", input)
		}
	}
	return nil
}

func confinedHILPath(root, relative string) (string, error) {
	if !filepath.IsLocal(relative) || filepath.Clean(relative) == "." {
		return "", fmt.Errorf("HIL path must stay inside the project: %q", relative)
	}
	path := root
	for _, part := range strings.Split(filepath.Clean(relative), string(filepath.Separator)) {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if err != nil {
			return "", fmt.Errorf("HIL input %q: %w", relative, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("HIL paths cannot contain symbolic links: %q", relative)
		}
	}
	return path, nil
}

func stageHILProject(root string, cfg *appconfig.HILConfig) (string, func(), error) {
	// A project-local staging directory also works with Apple Container, which
	// cannot reliably consume build contexts under /tmp on macOS.
	dir, err := os.MkdirTemp(root, ".wendy-hil-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	copyTree := func(source, destination string) error {
		// CopyFS does not follow symbolic links. Explicitly reject them before
		// writing anything so the staged context cannot reference outside files.
		if err := filepath.WalkDir(source, func(_ string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("HIL source contains symbolic link %q", entry.Name())
			}
			return nil
		}); err != nil {
			return err
		}
		return os.CopyFS(destination, os.DirFS(source))
	}
	if err := copyTree(filepath.Join(root, cfg.Project), dir); err != nil {
		cleanup()
		return "", nil, err
	}
	for _, input := range cfg.Inputs {
		if err := copyTree(filepath.Join(root, input), filepath.Join(dir, input)); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	return dir, cleanup, nil
}

type hilForward struct {
	listener net.Listener
	cancel   context.CancelFunc
	done     chan struct{}
	clients  sync.WaitGroup
}

func startHILForward(parent context.Context, dial func(context.Context) (net.Conn, error)) (*hilForward, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	f := &hilForward{listener: listener, cancel: cancel, done: make(chan struct{})}
	context.AfterFunc(ctx, func() { _ = listener.Close() })
	go func() {
		defer close(f.done)
		for {
			local, err := listener.Accept()
			if err != nil {
				return
			}
			f.clients.Add(1)
			go func() {
				defer f.clients.Done()
				defer local.Close()
				remote, err := dial(ctx)
				if err != nil {
					return
				}
				defer remote.Close()
				stop := context.AfterFunc(ctx, func() { _ = local.Close(); _ = remote.Close() })
				defer stop()
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(remote, local); done <- struct{}{} }()
				go func() { _, _ = io.Copy(local, remote); done <- struct{}{} }()
				<-done
				_ = remote.Close()
				_ = local.Close()
				<-done
			}()
		}
	}()
	return f, nil
}

func (f *hilForward) Close() {
	f.cancel()
	_ = f.listener.Close()
	<-f.done
	f.clients.Wait()
}

func waitHILHealth(ctx context.Context, url string, timeout time.Duration) error {
	return waitHILHealthAuthenticated(ctx, url, timeout, "", "")
}

func waitHILHealthAuthenticated(ctx context.Context, url string, timeout time.Duration, token, schema string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var last error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := client.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 65537))
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK && readErr == nil && len(body) <= 65536 {
				if schema != "" {
					var health struct {
						Schema string `json:"schema"`
					}
					if json.Unmarshal(body, &health) != nil || health.Schema != schema {
						return fmt.Errorf("HIL health response does not match the expected schema %q", schema)
					}
				}
				return nil
			}
			err = fmt.Errorf("health endpoint returned HTTP %d", response.StatusCode)
			if response.StatusCode >= 400 && response.StatusCode < 500 && response.StatusCode != http.StatusRequestTimeout && response.StatusCode != http.StatusTooManyRequests {
				return err
			}
		}
		last = err
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), last)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Persist the selected variant's lock before its temporary build context is removed.
func persistHILStagefileLock(staged, project, buildFile string) error {
	name := stagefile.LockName(filepath.Clean(buildFile))
	if name == "" {
		return nil
	}
	source := filepath.Join(staged, name)
	if _, err := os.Lstat(source); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if _, err := confinedHILPath(staged, name); err != nil {
		return err
	}
	pinned, err := stagefilelock.Load(source)
	if err != nil {
		return fmt.Errorf("reading HIL Stagefile lock: %w", err)
	}
	if pinned == nil {
		return nil
	}
	if err := pinned.Save(filepath.Join(project, name)); err != nil {
		return fmt.Errorf("saving HIL Stagefile lock: %w", err)
	}
	return nil
}
