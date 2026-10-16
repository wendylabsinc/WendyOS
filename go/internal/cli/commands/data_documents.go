package commands

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/shared/datadoc"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var documentKinds = []string{datadoc.DefaultKind, data.KindInference, data.KindWatch}

func newDataApplyCmd() *cobra.Command {
	var skipCloudRegistration bool
	command := &cobra.Command{Use: "apply [document.yaml]", Short: "Validate and apply a data document (collection, inference or watch) on the connected device",
		Long: "Apply a data document. Without a path, select one from the current directory.\nNon-interactive and --json invocations require an explicit path.",
		Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			path, err := documentFilePicker.resolve(args, !jsonOutput && isInteractiveTerminal(), pickFromItems)
			if err != nil {
				return err
			}
			contents, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("reading %q: %w", path, err)
			}
			document, err := data.ParseDocument(contents)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			conn, err := connectToAgent(cmd.Context())
			if err != nil {
				return err
			}
			defer conn.Close()
			if apps := documentCloudApps(document); len(apps) > 0 {
				if err := registerCloudApps(cmd.Context(), conn, apps, skipCloudRegistration); err != nil {
					return err
				}
			}
			response, err := conn.DataService.DocumentApply(cmd.Context(), &agentpbv2.DataDocumentApplyRequest{DocumentYaml: contents})
			if status.Code(err) == codes.Unimplemented && document.Header.Kind == datadoc.DefaultKind {
				// An agent from before data documents still deploys campaigns.
				campaign, deployErr := conn.DataService.CampaignDeploy(cmd.Context(), &agentpbv2.DataCampaignDeployRequest{CampaignYaml: contents})
				if deployErr != nil {
					return campaignDeployError(deployErr)
				}
				response, err = &agentpbv2.DataDocumentApplyResponse{Document: &agentpbv2.DataDocument{Kind: datadoc.DefaultKind, Version: uint32(document.Header.Version), Name: campaign.GetName(),
					Revision: campaign.GetRevision(), State: campaign.GetState(), DeployedUnixNanos: campaign.GetDeployedUnixNanos(), DocumentJson: campaign.GetPlanJson(), Warnings: campaign.GetWarnings()}}, nil
			}
			if err != nil {
				return documentCallError(err)
			}
			if jsonOutput {
				return encodeProtoJSON(cmd.OutOrStdout(), response.GetDocument())
			}
			printDocumentSummary(cmd.OutOrStdout(), response.GetDocument())
			return nil
		}}
	command.Flags().BoolVar(&skipCloudRegistration, "skip-cloud-registration", false, "Apply without registering the document's Cloud app (offline use)")
	return command
}

func newDataDocumentsCmd() *cobra.Command {
	return &cobra.Command{Use: "documents [<kind>[/<name>]]", Aliases: []string{"docs"}, Short: "List data documents on the connected device, or inspect one",
		Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			kind, name, err := parseDocumentFilter(args)
			if err != nil {
				return err
			}
			return withDataClient(cmd.Context(), func(client agentpbv2.DataServiceClient) error {
				response, err := client.Documents(cmd.Context(), &agentpbv2.DataDocumentsRequest{Kind: kind, Name: name})
				if err != nil {
					return documentCallError(err)
				}
				if jsonOutput {
					return encodeProtoJSON(cmd.OutOrStdout(), response)
				}
				out := cmd.OutOrStdout()
				if name != "" && len(response.GetDocuments()) == 1 {
					document := response.GetDocuments()[0]
					printDocumentSummary(out, document)
					fmt.Fprintf(out, "%s\n", document.GetDocumentJson())
					if len(document.GetStatusJson()) > 0 {
						fmt.Fprintf(out, "status: %s\n", document.GetStatusJson())
					}
					return nil
				}
				fmt.Fprintln(out, "KIND\tNAME\tSTATE\tOWNER\tEXPIRES\tREVISION")
				for _, document := range response.GetDocuments() {
					fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\t%s\n", document.GetKind(), document.GetName(), document.GetState(), dashIfEmpty(document.GetOwner()),
						expiresIn(document.GetExpiresUnixNanos()), shortRevision(document.GetRevision()))
				}
				return nil
			})
		}}
}

func newDataRemoveCmd() *cobra.Command {
	return &cobra.Command{Use: "remove <kind>/<name>", Aliases: []string{"rm"}, Short: "Remove an inference or watch document, and what it owns, from the connected device",
		Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			kind, name, err := parseDocumentRef(args[0])
			if err != nil {
				return err
			}
			return withDataClient(cmd.Context(), func(client agentpbv2.DataServiceClient) error {
				response, err := client.DocumentRemove(cmd.Context(), &agentpbv2.DataDocumentRemoveRequest{Kind: kind, Name: name})
				if err != nil {
					return documentCallError(err)
				}
				if jsonOutput {
					return encodeProtoJSON(cmd.OutOrStdout(), response)
				}
				for _, removed := range response.GetRemoved() {
					fmt.Fprintf(cmd.OutOrStdout(), "Removed %s\n", removed)
				}
				return nil
			})
		}}
}

// parseDocumentFilter reads `documents`' optional "<kind>" or "<kind>/<name>".
func parseDocumentFilter(args []string) (kind, name string, err error) {
	if len(args) == 0 {
		return "", "", nil
	}
	kind, name, hasName := strings.Cut(args[0], "/")
	if !isDocumentKind(kind) {
		return "", "", fmt.Errorf("unknown document kind %q; use %s", kind, strings.Join(documentKinds, ", "))
	}
	if hasName && !datadoc.ValidName(name) {
		return "", "", fmt.Errorf("invalid document name %q", name)
	}
	return kind, name, nil
}

// parseDocumentRef reads a required "<kind>/<name>".
func parseDocumentRef(arg string) (kind, name string, err error) {
	kind, name, err = parseDocumentFilter([]string{arg})
	if err == nil && name == "" {
		err = errors.New("name a document as <kind>/<name>, for example watch/front-door")
	}
	return kind, name, err
}

func isDocumentKind(kind string) bool {
	for _, known := range documentKinds {
		if kind == known {
			return true
		}
	}
	return false
}

// documentCloudApps names the Cloud apps a document needs before it is
// applied: a collection's campaign app, and a watch's when it notifies Cloud.
func documentCloudApps(document datadoc.Document) []string {
	switch body := document.Body.(type) {
	case data.Campaign:
		return []string{"campaign:" + body.Name}
	case data.Watch:
		if body.NotifiesCloud() {
			return []string{"watch:" + body.Name}
		}
	}
	return nil
}

// documentCallError explains an agent from before data documents.
func documentCallError(err error) error {
	if status.Code(err) == codes.Unimplemented {
		return status.Error(codes.Unimplemented, "This device's agent is too old for data documents; update it with `wendy device update`.")
	}
	return err
}

func printDocumentSummary(out io.Writer, document *agentpbv2.DataDocument) {
	fmt.Fprintf(out, "%s %s: %s (revision %s)", document.GetKind(), document.GetName(), document.GetState(), shortRevision(document.GetRevision()))
	if detail := document.GetStateDetail(); detail != "" {
		fmt.Fprintf(out, " — %s", detail)
	}
	if expires := document.GetExpiresUnixNanos(); expires != 0 {
		fmt.Fprintf(out, ", lease expires %s", expiresIn(expires))
	}
	fmt.Fprintln(out)
	for _, warning := range document.GetWarnings() {
		fmt.Fprintf(out, "Warning: %s\n", warning)
	}
}

func expiresIn(unixNanos int64) string {
	if unixNanos == 0 {
		return "-"
	}
	return "in " + time.Until(time.Unix(0, unixNanos)).Round(time.Second).String()
}

func dashIfEmpty(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
