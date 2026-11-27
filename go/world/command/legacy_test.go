package command

import (
	"context"
	"io"
)

func runEvidenceMirror(args []string, out io.Writer) error {
	return (&invocation{ctx: context.Background(), stdout: io.Discard, stderr: io.Discard}).runEvidenceMirror(args, out)
}
func runSelectedRead(args []string, output io.Writer) error {
	return (&invocation{ctx: context.Background(), stdout: io.Discard, stderr: io.Discard}).runSelectedRead(args, output)
}
func runEvidenceFetch(args []string, output io.Writer) error {
	return (&invocation{ctx: context.Background(), stdout: io.Discard, stderr: io.Discard}).runEvidenceFetch(args, output)
}
func runEvidenceCache(args []string, output io.Writer) error {
	return (&invocation{ctx: context.Background(), stdout: io.Discard, stderr: io.Discard}).runEvidenceCache(args, output)
}
func runAnnotation(args []string, out io.Writer) error {
	return (&invocation{ctx: context.Background(), stdout: io.Discard, stderr: io.Discard}).runAnnotation(args, out)
}
func runPackage(args []string, output io.Writer) error {
	return (&invocation{ctx: context.Background(), stdout: io.Discard, stderr: io.Discard}).runPackage(args, output)
}
func runFrame(args []string, output io.Writer) error {
	return (&invocation{ctx: context.Background(), stdout: io.Discard, stderr: io.Discard}).runFrame(args, output)
}
func runReplica(args []string, output io.Writer) error {
	return (&invocation{ctx: context.Background(), stdout: io.Discard, stderr: io.Discard}).runReplica(args, output)
}
func runPackageCache(args []string, out io.Writer) error {
	return (&invocation{ctx: context.Background(), stdout: io.Discard, stderr: io.Discard}).runPackageCache(args, out)
}
func runTaskHistory(args []string, out io.Writer) error {
	return (&invocation{ctx: context.Background(), stdout: io.Discard, stderr: io.Discard}).runTaskHistory(args, out)
}
func runZone(args []string, output io.Writer) error {
	return (&invocation{ctx: context.Background(), stdout: io.Discard, stderr: io.Discard}).runZone(args, output)
}
func runSupervise(args []string, output io.Writer) error {
	return (&invocation{ctx: context.Background(), stdout: io.Discard, stderr: io.Discard}).runSupervise(args, output)
}
func runSuperviseContext(ctx context.Context, args []string, output io.Writer) error {
	return (&invocation{ctx: context.Background(), stdout: io.Discard, stderr: io.Discard}).runSuperviseContext(ctx, args, output)
}
func runSite(args []string, output io.Writer) error {
	return (&invocation{ctx: context.Background(), stdout: io.Discard, stderr: io.Discard}).runSite(args, output)
}
func runEvidence(args []string, output io.Writer) error {
	return (&invocation{ctx: context.Background(), stdout: io.Discard, stderr: io.Discard}).runEvidence(args, output)
}
func runEvidenceUsage(args []string, output io.Writer) error {
	return (&invocation{ctx: context.Background(), stdout: io.Discard, stderr: io.Discard}).runEvidenceUsage(args, output)
}
func runArea(args []string, out io.Writer) error {
	return (&invocation{ctx: context.Background(), stdout: io.Discard, stderr: io.Discard}).runArea(args, out)
}
func runProposalCommand(args []string, output io.Writer) error {
	return (&invocation{ctx: context.Background(), stdout: io.Discard, stderr: io.Discard}).runProposalCommand(args, output)
}
func run(args []string) error {
	return (&invocation{ctx: context.Background(), stdout: io.Discard, stderr: io.Discard}).run(args)
}
func runTask(args []string, output io.Writer) error {
	return (&invocation{ctx: context.Background(), stdout: io.Discard, stderr: io.Discard}).runTask(args, output)
}
