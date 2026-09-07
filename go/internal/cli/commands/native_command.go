package commands

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func runNativeCommandWithAgent(ctx context.Context, conn *grpcclient.AgentConnection, cwd string, cfg *appconfig.AppConfig, opts runOptions, version *agentpb.GetAgentVersionResponse) error {
	if err := validateNativeCommandOptions(cfg, opts, version); err != nil {
		return err
	}
	// The agent uses platform to select its native runtime, including when the
	// manifest omitted it and the target supplied the default.
	cfg.Platform = appconfig.PlatformDarwin
	entries, err := assembleNativeCommandSyncEntries(cwd, cfg)
	if err != nil {
		return err
	}
	manifest, _, err := buildCombinedManifest(entries)
	if err != nil {
		return err
	}
	if !path.IsAbs(cfg.Run.Command) {
		command := path.Clean(cfg.Run.Command)
		if !slices.ContainsFunc(manifest.Files, func(f *agentpb.FileSyncEntry) bool { return path.Clean(f.Path) == command && f.Mode&0111 != 0 }) {
			return fmt.Errorf("run.command %q must be an executable in the synced files (use chmod +x for a script)", cfg.Run.Command)
		}
	}
	req := &agentpb.CreateContainerRequest{
		AppName: cfg.AppID, Cmd: cfg.Run.Command, WorkingDir: cfg.Run.Cwd,
		UserArgs: cfg.Run.Args, Env: mergeEnvEntries(resolveServiceEnv(cfg), opts.env), RestartPolicy: resolveRestartPolicy(opts),
	}
	if len(opts.userArgs) > 0 {
		req.UserArgs = opts.userArgs
	}
	hash, err := watchDesiredHash(struct {
		Request *agentpb.CreateContainerRequest
		Config  *appconfig.AppConfig
		Files   *agentpb.FileSyncManifest
	}{req, cfg, manifest})
	if err != nil {
		return err
	}
	key := watchServiceKey(deviceFingerprintKey(version), cfg.AppID, "")
	if opts.isWatch() && opts.watchState.matches(key, hash) && deviceContainerStates(ctx, conn)[strings.ToLower(cfg.ContainerName())] == agentpb.AppRunningState_RUNNING {
		cliLogln("Application %s is unchanged and running.", containerDisplayName(cfg))
		if !opts.detach && !opts.deploy {
			opts.watchState.reapCommand(runPostStartIfReady(ctx, opts.watchState.hookContext(ctx), conn, cfg, opts))
		}
		return nil
	}
	if err := syncFiles(ctx, conn, cfg.AppID, entries); err != nil {
		return fmt.Errorf("syncing native app files: %w", err)
	}
	if err := runMacOSNativeContainer(ctx, conn, cfg, req, opts); err != nil {
		return err
	}
	opts.watchState.record(key, hash)
	return nil
}

func validateNativeCommandOptions(cfg *appconfig.AppConfig, opts runOptions, version *agentpb.GetAgentVersionResponse) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if version.GetOs() != "darwin" {
		return fmt.Errorf("run.command requires a native Darwin agent target")
	}
	if !slices.Contains(version.GetFeatureset(), "native-process-v1") {
		return fmt.Errorf("this Mac agent does not support run.command; update Wendy Agent on the target Mac to a version advertising native-process-v1, then run again")
	}
	if opts.buildType != "" || opts.dockerfile != "" || opts.builder != "" || opts.buildHost != "" || opts.product != "" || opts.debug || opts.gpuArch != "" {
		return fmt.Errorf("run.command cannot be combined with build selections (--build-type, --dockerfile, --builder, --build-host, --product, --debug, or --gpu-arch)")
	}
	return nil
}

func assembleNativeCommandSyncEntries(cwd string, cfg *appconfig.AppConfig) ([]fileSyncEntry, error) {
	var entries []fileSyncEntry
	for _, f := range cfg.Files {
		remote := effectiveRemotePath(f.Path, f.To)
		if !appconfig.SafeNativeRelativePath(remote, true) {
			return nil, fmt.Errorf("synced destination %q must remain inside the app directory", remote)
		}
		entries = append(entries, fileSyncEntry{localPath: filepath.Join(cwd, f.Path), remotePath: remote})
	}
	// Preserve declared mappings. Only add an implicit support file when the
	// same destination isn't already supplied by its file/directory entry.
	add := func(name string, required bool) error {
		candidate := fileSyncEntry{localPath: filepath.Join(cwd, name), remotePath: path.Clean(name)}
		for _, e := range entries {
			coverage, err := syncEntryCoversBrewfile(e, candidate)
			if err != nil {
				return err
			}
			if coverage.covered {
				return nil
			}
		}
		if _, err := os.Stat(candidate.localPath); err != nil {
			if !required && os.IsNotExist(err) {
				return nil
			}
			return err
		}
		entries = append(entries, candidate)
		return nil
	}
	if err := add("wendy.json", true); err != nil {
		return nil, err
	}
	if err := add("sandbox.sb", false); err != nil {
		return nil, err
	}
	if !path.IsAbs(cfg.Run.Command) {
		if err := add(cfg.Run.Command, true); err != nil {
			return nil, fmt.Errorf("run.command: %w", err)
		}
	}
	return appendNativeBrewfileSyncEntry(entries, cwd, cfg)
}
