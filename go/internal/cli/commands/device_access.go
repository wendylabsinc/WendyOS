package commands

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/localkey"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// `wendy device access` and `wendy device keys` manage the access shape of a
// device that is not enrolled: Open (anyone on the network, as before),
// Closed (only trusted keys, each owner, operator or viewer) or Enrolled
// (Wendy Cloud decides). The device enforces everything (owner only, last
// owner irremovable, refused while enrolled); the CLI passes its refusals
// through verbatim.

func newDeviceAccessCmd() *cobra.Command {
	var closeYes, openYes bool
	cmd := &cobra.Command{
		Use:   "access",
		Short: "Show or change who may use a device that is not enrolled",
		Long: "Show the device's access shape, the keyless apps switch and your key's role.\n" +
			"Open: anyone on the network gets in. Closed: only trusted keys get in\n" +
			"(see 'wendy device keys'). Enrolled: Wendy Cloud manages access.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withAccessClient(cmd.Context(), func(c agentpbv2.WendyAccessServiceClient, _ *grpcclient.AgentConnection) error {
				return runDeviceAccessShow(cmd.Context(), c, cmd.OutOrStdout(), jsonOutput)
			})
		},
	}
	closeCmd := &cobra.Command{
		Use:   "close",
		Short: "Close an Open device: only trusted keys get in, this computer's key is the first owner",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if ok, err := confirmAccessChange(cmd.OutOrStdout(), closeYes,
				"Closing the device lets only trusted keys in: this computer's key becomes its owner, and every other computer needs an owner to add its key"); !ok {
				return err
			}
			key, err := localOperatorKeyFn()
			if err != nil {
				return err
			}
			spki, err := localkey.PublicKeyDER(key)
			if err != nil {
				return err
			}
			return withAccessClient(cmd.Context(), func(c agentpbv2.WendyAccessServiceClient, _ *grpcclient.AgentConnection) error {
				return runDeviceAccessClose(cmd.Context(), c, cmd.OutOrStdout(), spki, localKeyLabel(), jsonOutput)
			})
		},
	}
	closeCmd.Flags().BoolVarP(&closeYes, "yes", "y", false, "Close without asking for confirmation")
	openCmd := &cobra.Command{
		Use:   "open",
		Short: "Open a Closed device to anyone on its network and drop every key (owner only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if ok, err := confirmAccessChange(cmd.OutOrStdout(), openYes,
				"Opening the device drops every trusted key and lets anyone on its network in"); !ok {
				return err
			}
			return withAccessClient(cmd.Context(), func(c agentpbv2.WendyAccessServiceClient, conn *grpcclient.AgentConnection) error {
				return runDeviceAccessOpen(cmd.Context(), c, conn, cmd.OutOrStdout(), jsonOutput)
			})
		},
	}
	openCmd.Flags().BoolVarP(&openYes, "yes", "y", false, "Open without asking for confirmation")
	cmd.AddCommand(closeCmd, openCmd, &cobra.Command{
		Use:   "keyless <on|off>",
		Short: "Let callers without a key deploy and manage apps on a Closed device (owner only)",
		Long: "Switch the one keyless feature set of a Closed device: deploying and managing apps\n" +
			"(wendy run, the app commands, logs). Everything else still needs a trusted key.\n" +
			"Off by default.",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"on", "off"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "on" && args[0] != "off" {
				return usageErrorf("want on or off, not %q", args[0])
			}
			return withAccessClient(cmd.Context(), func(c agentpbv2.WendyAccessServiceClient, _ *grpcclient.AgentConnection) error {
				return runDeviceAccessKeyless(cmd.Context(), c, cmd.OutOrStdout(), args[0] == "on", jsonOutput)
			})
		},
	})
	return cmd
}

var accessShapeNames = map[agentpbv2.AccessShape]string{
	agentpbv2.AccessShape_ACCESS_SHAPE_OPEN:     "Open",
	agentpbv2.AccessShape_ACCESS_SHAPE_CLOSED:   "Closed",
	agentpbv2.AccessShape_ACCESS_SHAPE_ENROLLED: "Enrolled",
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func runDeviceAccessShow(ctx context.Context, c agentpbv2.WendyAccessServiceClient, out io.Writer, asJSON bool) error {
	resp, err := c.GetAccess(ctx, &agentpbv2.GetAccessRequest{})
	if err != nil {
		return accessError(err)
	}
	shape := accessShapeNames[resp.GetShape()]
	role := ""
	if resp.GetCallerRole() != agentpbv2.TrustedKeyRole_TRUSTED_KEY_ROLE_UNSPECIFIED {
		role = trustedKeyRoleName(resp.GetCallerRole())
	}
	if asJSON {
		return writeJSON(out, map[string]any{"shape": strings.ToLower(shape), "keylessApps": resp.GetKeylessApps(), "yourRole": role})
	}
	fmt.Fprintf(out, "Access: %s\n", shape)
	if resp.GetShape() == agentpbv2.AccessShape_ACCESS_SHAPE_CLOSED {
		fmt.Fprintf(out, "Keyless apps: %s\n", onOff(resp.GetKeylessApps()))
		if role == "" {
			role = "none (connected without a key)"
		}
		fmt.Fprintf(out, "Your role: %s\n", role)
	}
	return nil
}

func runDeviceAccessClose(ctx context.Context, c agentpbv2.WendyAccessServiceClient, out io.Writer, spki []byte, label string, asJSON bool) error {
	resp, err := c.CloseDevice(ctx, &agentpbv2.CloseDeviceRequest{PublicKey: spki, Label: label})
	if err != nil {
		return accessError(err)
	}
	if asJSON {
		return writeJSON(out, map[string]any{"shape": "closed", "owner": trustedKeyView(resp.GetOwner(), "")})
	}
	fmt.Fprintf(out, "Closed. This computer's key %s is the owner; the device restarts its agent now.\n", resp.GetOwner().GetFingerprint())
	return nil
}

func runDeviceAccessOpen(ctx context.Context, c agentpbv2.WendyAccessServiceClient, conn *grpcclient.AgentConnection, out io.Writer, asJSON bool) error {
	if _, err := c.OpenDevice(ctx, &agentpbv2.OpenDeviceRequest{}); err != nil {
		return accessError(err)
	}
	// The device no longer serves its key port; a pin on it would refuse
	// every later plaintext connection.
	if spki := conn.LocalDeviceKey(); spki != nil {
		store, err := openLocalPinStoreFn()
		if err == nil {
			err = store.ForgetLocal(spki)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not drop this computer's pin on the device key (%v); if connecting fails, run 'wendy device unpin <name>'\n", err)
		}
	}
	if asJSON {
		return writeJSON(out, map[string]any{"shape": "open"})
	}
	fmt.Fprintln(out, "Open. Every key was dropped and anyone on the device's network gets in; the device restarts its agent now.")
	return nil
}

func runDeviceAccessKeyless(ctx context.Context, c agentpbv2.WendyAccessServiceClient, out io.Writer, enabled, asJSON bool) error {
	if _, err := c.SetKeylessApps(ctx, &agentpbv2.SetKeylessAppsRequest{Enabled: enabled}); err != nil {
		return accessError(err)
	}
	if asJSON {
		return writeJSON(out, map[string]any{"keylessApps": enabled})
	}
	if enabled {
		fmt.Fprintln(out, "Keyless apps on: callers without a key may deploy and manage apps. The device restarts its agent now.")
	} else {
		fmt.Fprintln(out, "Keyless apps off: every caller needs a trusted key. The device restarts its agent now.")
	}
	return nil
}

// `wendy device keys`: the keys a Closed device trusts.
func newDeviceKeysCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "Manage the keys a Closed device trusts",
		Long: "Manage the keys a Closed device trusts. The key that closed the device (or that\n" +
			"'wendy os install --close' seeded) is its first owner. Owners add further keys as\n" +
			"owner (everything, plus keys and access), operator (deploy and run apps) or viewer\n" +
			"(read only), and remove them; the last owner key cannot be removed. Requires an owner\n" +
			"key. Refused while the device is enrolled in Wendy Cloud.",
	}
	cmd.AddCommand(newDeviceKeysListCmd(), newDeviceKeysAddCmd(), newDeviceKeysRemoveCmd())
	return cmd
}

// withAccessClient connects to the selected device and runs fn against its
// access service.
func withAccessClient(ctx context.Context, fn func(agentpbv2.WendyAccessServiceClient, *grpcclient.AgentConnection) error) error {
	conn, err := connectToAgent(ctx, SuppressProvisioningHint())
	if err != nil {
		return err
	}
	defer conn.Close()
	return fn(agentpbv2.NewWendyAccessServiceClient(conn.Conn), conn)
}

func newDeviceKeysListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the keys the device trusts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withAccessClient(cmd.Context(), func(c agentpbv2.WendyAccessServiceClient, _ *grpcclient.AgentConnection) error {
				return runDeviceKeysList(cmd.Context(), c, cmd.OutOrStdout(), jsonOutput)
			})
		},
	}
}

func newDeviceKeysAddCmd() *cobra.Command {
	var role, label string
	cmd := &cobra.Command{
		Use:   "add <public-key-file|->",
		Short: "Trust another operator's public key on the device",
		Long: "Trust another operator's public key on the device with the given role.\n" +
			"The file is the output of 'wendy auth local-key show' on the other operator's\n" +
			"computer, or any PEM \"PUBLIC KEY\" block; '-' reads it from stdin.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := parseTrustedKeyRole(role)
			if err != nil {
				return err
			}
			der, err := readPublicKeyArg(args[0], cmd.InOrStdin())
			if err != nil {
				return err
			}
			return withAccessClient(cmd.Context(), func(c agentpbv2.WendyAccessServiceClient, _ *grpcclient.AgentConnection) error {
				return runDeviceKeysAdd(cmd.Context(), c, cmd.OutOrStdout(), der, r, label, jsonOutput)
			})
		},
	}
	cmd.Flags().StringVar(&role, "role", "", "role of the key: owner, operator or viewer (required)")
	cmd.Flags().StringVar(&label, "label", "", "a name for the key, shown by 'wendy device keys list'")
	_ = cmd.MarkFlagRequired("role")
	return cmd
}

func newDeviceKeysRemoveCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "remove <fingerprint>",
		Short: "Stop trusting a key on the device",
		Long:  "Stop trusting a key on the device. The fingerprint is the one 'wendy device keys list' prints.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if ok, err := confirmAccessChange(cmd.OutOrStdout(), yes,
				fmt.Sprintf("Removing key %s stops it reaching the device; if it is this computer's key, this computer loses access", args[0])); !ok {
				return err
			}
			return withAccessClient(cmd.Context(), func(c agentpbv2.WendyAccessServiceClient, _ *grpcclient.AgentConnection) error {
				return runDeviceKeysRemove(cmd.Context(), c, cmd.OutOrStdout(), args[0], jsonOutput)
			})
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Remove without asking for confirmation")
	return cmd
}

// confirmAccessChange asks before a change of who may reach the device, like
// 'wendy device reboot': without a terminal it needs --yes. ok is false when
// the change must not go ahead; err is nil when the user declined.
func confirmAccessChange(out io.Writer, assumeYes bool, what string) (ok bool, err error) {
	if assumeYes {
		return true, nil
	}
	if !isInteractiveTerminal() || jsonOutput {
		return false, fmt.Errorf("%s; pass --yes to go ahead", what)
	}
	fmt.Fprintln(out, what+".")
	if !confirmDefaultNoFn("Continue?") {
		fmt.Fprintln(out, "Aborted; nothing was changed.")
		return false, nil
	}
	return true, nil
}

var trustedKeyRoles = map[string]agentpbv2.TrustedKeyRole{
	"owner":    agentpbv2.TrustedKeyRole_TRUSTED_KEY_ROLE_OWNER,
	"operator": agentpbv2.TrustedKeyRole_TRUSTED_KEY_ROLE_OPERATOR,
	"viewer":   agentpbv2.TrustedKeyRole_TRUSTED_KEY_ROLE_VIEWER,
}

func parseTrustedKeyRole(s string) (agentpbv2.TrustedKeyRole, error) {
	if r, ok := trustedKeyRoles[strings.ToLower(strings.TrimSpace(s))]; ok {
		return r, nil
	}
	return 0, fmt.Errorf("--role must be owner, operator or viewer, not %q", s)
}

func trustedKeyRoleName(r agentpbv2.TrustedKeyRole) string {
	for name, v := range trustedKeyRoles {
		if v == r {
			return name
		}
	}
	return "unknown"
}

// readPublicKeyArg reads a public key file (or stdin for "-") and returns its
// SubjectPublicKeyInfo DER. The first PEM "PUBLIC KEY" block counts, so the
// full output of `wendy auth local-key show` works as is. A private key is
// refused rather than sent anywhere.
func readPublicKeyArg(arg string, stdin io.Reader) ([]byte, error) {
	var data []byte
	var err error
	if arg == "-" {
		data, err = io.ReadAll(io.LimitReader(stdin, 1<<20))
	} else {
		data, err = os.ReadFile(arg)
	}
	if err != nil {
		return nil, fmt.Errorf("reading public key: %w", err)
	}
	for rest := data; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if strings.Contains(block.Type, "PRIVATE KEY") {
			return nil, errors.New("this is a private key; pass the public key ('wendy auth local-key show' prints it)")
		}
		if block.Type == "PUBLIC KEY" {
			if _, err := x509.ParsePKIXPublicKey(block.Bytes); err != nil {
				return nil, fmt.Errorf("parsing public key: %w", err)
			}
			return block.Bytes, nil
		}
	}
	return nil, errors.New("no PEM \"PUBLIC KEY\" block found; pass the output of 'wendy auth local-key show'")
}

// accessError surfaces the device's refusal text as is: it is the device's
// decision ("managed by Wendy Cloud", "last owner", "needs the owner role"),
// not something for the CLI to reword. An agent without the service predates
// access shapes.
func accessError(err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	if st.Code() == codes.Unimplemented {
		return errors.New("this device's agent does not support access shapes; update it with 'wendy device update'")
	}
	return errors.New(st.Message())
}

type trustedKeyJSON struct {
	Fingerprint string `json:"fingerprint"`
	Role        string `json:"role"`
	Label       string `json:"label,omitempty"`
	AddedAt     string `json:"addedAt,omitempty"`
	AddedBy     string `json:"addedBy,omitempty"`
	ThisKey     bool   `json:"thisComputer,omitempty"`
}

func trustedKeyView(k *agentpbv2.TrustedKey, mine string) trustedKeyJSON {
	v := trustedKeyJSON{
		Fingerprint: k.GetFingerprint(),
		Role:        trustedKeyRoleName(k.GetRole()),
		Label:       k.GetLabel(),
		AddedBy:     k.GetAddedBy(),
		ThisKey:     mine != "" && k.GetFingerprint() == mine,
	}
	if k.GetAddedAt() > 0 {
		v.AddedAt = time.Unix(k.GetAddedAt(), 0).UTC().Format(time.RFC3339)
	}
	return v
}

// thisComputerFingerprint is the local key's fingerprint, "" without one.
// Listing needs the owner key, so the key exists whenever this matters.
func thisComputerFingerprint() string {
	key, err := localOperatorKeyFn()
	if err != nil {
		return ""
	}
	fp, _ := localkey.Fingerprint(key)
	return fp
}

func runDeviceKeysList(ctx context.Context, c agentpbv2.WendyAccessServiceClient, out io.Writer, asJSON bool) error {
	resp, err := c.ListTrustedKeys(ctx, &agentpbv2.ListTrustedKeysRequest{})
	if err != nil {
		return accessError(err)
	}
	mine := thisComputerFingerprint()
	views := make([]trustedKeyJSON, 0, len(resp.GetKeys()))
	for _, k := range resp.GetKeys() {
		views = append(views, trustedKeyView(k, mine))
	}
	if asJSON {
		return writeJSON(out, map[string]any{"keys": views})
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "FINGERPRINT\tROLE\tLABEL\tADDED\tADDED BY")
	for _, v := range views {
		// Labels are free text from other owners: control characters would
		// reach the terminal and could forge rows.
		label := tui.StripControl(v.Label)
		if v.ThisKey {
			label = strings.TrimSpace(label + " (this computer)")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", tui.StripControl(v.Fingerprint), v.Role, label, v.AddedAt, tui.StripControl(v.AddedBy))
	}
	return w.Flush()
}

func runDeviceKeysAdd(ctx context.Context, c agentpbv2.WendyAccessServiceClient, out io.Writer, der []byte, role agentpbv2.TrustedKeyRole, label string, asJSON bool) error {
	resp, err := c.AddTrustedKey(ctx, &agentpbv2.AddTrustedKeyRequest{PublicKey: der, Role: role, Label: label})
	if err != nil {
		return accessError(err)
	}
	if asJSON {
		return writeJSON(out, trustedKeyView(resp.GetKey(), ""))
	}
	fmt.Fprintf(out, "Trusted %s as %s.\n", resp.GetKey().GetFingerprint(), trustedKeyRoleName(resp.GetKey().GetRole()))
	return nil
}

func runDeviceKeysRemove(ctx context.Context, c agentpbv2.WendyAccessServiceClient, out io.Writer, fingerprint string, asJSON bool) error {
	fingerprint = strings.TrimSpace(fingerprint)
	if _, err := c.RemoveTrustedKey(ctx, &agentpbv2.RemoveTrustedKeyRequest{Fingerprint: fingerprint}); err != nil {
		return accessError(err)
	}
	if asJSON {
		return writeJSON(out, map[string]any{"removed": fingerprint})
	}
	fmt.Fprintf(out, "Removed %s.\n", fingerprint)
	return nil
}

// newAuthLocalKeyCmd groups the local key: the key this computer
// proves itself with to devices that are not cloud-enrolled.
func newAuthLocalKeyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "local-key",
		Short: "Show this computer's local key",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Print this computer's local public key and fingerprint",
		Long: "Print this computer's local public key and its fingerprint. Give the output to an\n" +
			"owner of a device, who trusts it with 'wendy device keys add <file> --role <role>'.\n" +
			"The key is created on first use; its private half never leaves this computer.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAuthLocalKeyShow(cmd.OutOrStdout(), jsonOutput)
		},
	})
	return cmd
}

func runAuthLocalKeyShow(out io.Writer, asJSON bool) error {
	path, err := localkey.Path()
	if err != nil {
		return err
	}
	key, err := localkey.LoadOrCreate(path)
	if err != nil {
		return err
	}
	der, err := localkey.PublicKeyDER(key)
	if err != nil {
		return err
	}
	fp := trustedkeys.Fingerprint(der)
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	if asJSON {
		return writeJSON(out, map[string]string{"fingerprint": fp, "publicKey": pubPEM, "path": path})
	}
	fmt.Fprintf(out, "Fingerprint: %s\n\n%s", fp, pubPEM)
	return nil
}
