package commands

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
)

func newSSHCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ssh",
		Short: "SSH utilities for the target device",
	}
	cmd.AddCommand(newSSHShellCmd(), newSSHExecCmd())
	return cmd
}

func newSSHShellCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "shell",
		Short: "Open an interactive SSH session to the target device",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSSH(cmd, nil)
		},
	}
}

func newSSHExecCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "exec <cmd> [args...]",
		Short: "Run a command on the device over SSH and stream output",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSSH(cmd, args)
		},
	}
}

// sshUser reads the SSH username from wendy.json in the current directory,
// falling back to "root" if the file is absent or has no ssh.user set.
func sshUser() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "root"
	}
	cfg, err := appconfig.LoadFromFile(filepath.Join(cwd, "wendy.json"))
	if err != nil || cfg.SSH == nil || cfg.SSH.User == "" {
		return "root"
	}
	return cfg.SSH.User
}

// runSSH resolves the target device, derives the SSH user, then execs
// ssh <user>@<host> [remoteArgs...]. Pass nil remoteArgs for an interactive shell.
func runSSH(cmd *cobra.Command, remoteArgs []string) error {
	ctx := cmd.Context()

	target, err := resolveTarget(ctx, SuppressUpdateCheck(), SuppressProvisioningHint())
	if err != nil {
		return err
	}

	if target.Agent == nil {
		target.Close()
		return fmt.Errorf("wendy ssh requires a LAN-connected WendyOS device")
	}

	host := target.Agent.Host
	target.Close()

	user := sshUser()

	sshArgs := []string{fmt.Sprintf("%s@%s", user, host)}
	sshArgs = append(sshArgs, remoteArgs...)

	c := exec.CommandContext(ctx, "ssh", sshArgs...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}
