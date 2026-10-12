package commands

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

func newVMSerialCmd() *cobra.Command {
	var port string
	var baud uint32
	var clear bool
	cmd := &cobra.Command{
		Use: "serial <name>", Short: "Show or configure Windows host serial passthrough for a stopped VM",
		Long: "Persist one Windows COM port and exact baud for foreground VM starts. Uses 8N1, no handshake, DTR/RTS disabled. Guest baud changes do not alter the host. Stop the VM before changing its mapping.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := vm.NewStore()
			if err != nil {
				return err
			}
			if err := vm.ValidName(args[0]); err != nil {
				return err
			}
			st, err := s.Status(args[0])
			if err != nil {
				return err
			}
			if !st.Exists {
				return fmt.Errorf("no VM named %q", args[0])
			}
			changed := cmd.Flags().Changed("port") || cmd.Flags().Changed("baud")
			if clear && changed {
				return fmt.Errorf("use either --clear or --port and --baud")
			}
			if clear {
				return s.ConfigureSerial(args[0], nil)
			}
			if changed {
				if runtime.GOOS != "windows" {
					return fmt.Errorf("configuring VM serial passthrough requires a Windows host")
				}
				if !cmd.Flags().Changed("port") || !cmd.Flags().Changed("baud") {
					return fmt.Errorf("specify both --port COM<number> and --baud <exact rate>")
				}
				return s.ConfigureSerial(args[0], &vm.SerialConfig{Port: strings.ToUpper(strings.TrimSpace(port)), Baud: baud})
			}
			c, err := s.ReadSerial(args[0])
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(c)
		},
	}
	cmd.Flags().StringVar(&port, "port", "", "Windows COM port (for example COM12)")
	cmd.Flags().Uint32Var(&baud, "baud", 0, "Exact baud supported by the Windows driver; no rounding")
	cmd.Flags().BoolVar(&clear, "clear", false, "Remove the persisted mapping")
	return cmd
}
