// world-agent-vm-fixture is a bounded test issuer, never a production CA.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/internal/agentvmfixture"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "VM fixture refused:", err)
		os.Exit(1)
	}
}
func printPublic(v any) {
	b, err := agentvmfixture.JSONReceipt(v)
	if err == nil {
		fmt.Println(string(b))
	}
}
func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: world-agent-vm-fixture init|init-enrollment|commission|serve|verify|journal [flags]")
	}
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	directory := f.String("directory", "", "fresh generated fixture directory (never real credentials)")
	state := f.String("state-directory", "", "isolated World journal/socket directory for init")
	commissionSpec := f.String("commission-spec", "", "protected observed-scene commissioning specification")
	listen := f.String("listen", "127.0.0.1:0", "numeric loopback ACME listen address")
	handoff := f.String("handoff", "/config/acme-enrollment.json", "exclusive normal first-boot enrollment handoff")
	duration := f.Duration("duration", 5*time.Minute, "bounded ACME listener lifetime, at most ten minutes")
	proof := f.String("proof", "", "exact signed_record_json saved by app")
	request := f.String("request", "", "original PublishRequest protojson saved by app")
	service := f.String("service", "", "gas or temperature")
	generation := f.Uint64("generation", 0, "expected deployment generation from host lifecycle")
	epoch := f.Uint64("epoch", 0, "expected durably reserved source epoch")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 || *directory == "" {
		return fmt.Errorf("explicit directory and no positional arguments required")
	}
	switch args[0] {
	case "init":
		p, err := agentvmfixture.Initialize(*directory, *state)
		if err == nil {
			printPublic(p)
		}
		return err
	case "init-enrollment":
		p, err := agentvmfixture.InitializeEnrollment(*directory, *state)
		if err == nil {
			printPublic(p)
		}
		return err
	case "commission":
		p, err := agentvmfixture.CommissionMultimodal(*directory, *state, *commissionSpec)
		if err == nil {
			printPublic(p)
		}
		return err
	case "serve":
		return agentvmfixture.Serve(ctx, *directory, *listen, *handoff, *duration, func(r agentvmfixture.ListenerReceipt) { printPublic(r) })
	case "verify":
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		p, err := agentvmfixture.Verify(ctx, *directory, *proof, *request, *service, *generation, *epoch)
		if err == nil {
			printPublic(p)
		}
		return err
	case "journal":
		p, err := agentvmfixture.CompareJournal(*directory, *proof, *service, *generation, *epoch)
		if err == nil {
			printPublic(p)
		}
		return err
	default:
		return fmt.Errorf("unknown fixture command")
	}
}
