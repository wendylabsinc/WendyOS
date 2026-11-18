package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/inference"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// workerImageClient is a device's DataService as campaign deploy sees it.
type workerImageClient struct {
	agentpbv2.DataServiceClient
	image    *agentpbv2.DataInferenceWorkerImageResponse
	imageErr error
	calls    []string
}

func (c *workerImageClient) InferenceWorkerImage(context.Context, *agentpbv2.DataInferenceWorkerImageRequest, ...grpc.CallOption) (*agentpbv2.DataInferenceWorkerImageResponse, error) {
	c.calls = append(c.calls, "image?")
	return c.image, c.imageErr
}

const deployedPlanJSON = `{"name":"people","inference_status":{"state":"loading"}}`

func (c *workerImageClient) CampaignDeploy(context.Context, *agentpbv2.DataCampaignDeployRequest, ...grpc.CallOption) (*agentpbv2.DataCampaign, error) {
	c.calls = append(c.calls, "deploy")
	return &agentpbv2.DataCampaign{Name: "people", State: "armed", Revision: "0123456789abcdef", PlanJson: []byte(deployedPlanJSON)}, nil
}

func missingWorkerImage() *agentpbv2.DataInferenceWorkerImageResponse {
	return &agentpbv2.DataInferenceWorkerImageResponse{Repository: "wendy-inference-worker", DepsHash: "62dfd59926b6a643",
		BuildContext: map[string][]byte{"Dockerfile": []byte("FROM scratch\n"), "uv.lock": []byte("lock\n")}}
}

// deployWith runs a campaign deploy against client, with Docker running or
// not and a build that returns buildErr, and reports what happened.
func deployWith(t *testing.T, client *workerImageClient, conn *grpcclient.AgentConnection, asJSON, dockerRunning bool, buildErr error) (stdout, stderr string, err error) {
	t.Helper()
	oldDocker, oldBuild, oldInteractive := dockerVersionOKFn, buildInferenceWorkerImageFn, isInteractiveTerminalFn
	t.Cleanup(func() {
		dockerVersionOKFn, buildInferenceWorkerImageFn, isInteractiveTerminalFn = oldDocker, oldBuild, oldInteractive
	})
	dockerVersionOKFn = func(context.Context) bool { return dockerRunning }
	isInteractiveTerminalFn = func() bool { return false }
	buildInferenceWorkerImageFn = func(ctx context.Context, _ *grpcclient.AgentConnection, info *agentpbv2.DataInferenceWorkerImageResponse) error {
		client.calls = append(client.calls, "build "+info.GetRepository()+" "+info.GetDepsHash())
		if !detachedJSONRun(ctx) {
			t.Error("build progress would go to stdout")
		}
		return buildErr
	}
	if conn == nil {
		conn = &grpcclient.AgentConnection{}
	}
	conn.DataService = client
	var out, notices bytes.Buffer
	plan := data.Campaign{Name: "people", Inference: &data.CampaignInference{Model: "owner/model"}}
	err = deployCampaign(context.Background(), conn, plan, []byte("name: people\n"), asJSON, &out, &notices)
	return out.String(), notices.String(), err
}

func TestCampaignDeployPutsTheWorkerImageOnTheDevice(t *testing.T) {
	const deployed = "Campaign people: armed (revision 0123456789ab)\n"
	const build = "build wendy-inference-worker 62dfd59926b6a643"
	cloud := func() *grpcclient.AgentConnection {
		return &grpcclient.AgentConnection{Reconnect: func(context.Context) (*grpcclient.AgentConnection, error) { return nil, nil }}
	}
	for _, test := range []struct {
		name     string
		client   *workerImageClient
		conn     *grpcclient.AgentConnection
		noDocker bool
		buildErr error
		calls    string
		notice   string // what stderr must say; "" means nothing at all
	}{
		{name: "the device has the image", client: &workerImageClient{image: &agentpbv2.DataInferenceWorkerImageResponse{Repository: "wendy-inference-worker", DepsHash: "62dfd59926b6a643", Present: true}},
			calls: "image?,deploy"},
		{name: "the device lacks the image", client: &workerImageClient{image: missingWorkerImage()},
			calls: "image?," + build + ",deploy", notice: "The model worker image is on the device."},
		// An agent that predates the image gets exactly the deploy it got before.
		{name: "an older agent", client: &workerImageClient{imageErr: status.Error(codes.Unimplemented, "unknown method InferenceWorkerImage")},
			calls: "image?,deploy"},
		{name: "an agent that runs no containers", client: &workerImageClient{image: &agentpbv2.DataInferenceWorkerImageResponse{}},
			calls: "image?,deploy"},
		{name: "no Docker on this machine", client: &workerImageClient{image: missingWorkerImage()}, noDocker: true,
			calls: "image?,deploy", notice: "not in a container: Docker is not running on this machine. The campaign is deployed either way."},
		{name: "the build fails", client: &workerImageClient{image: missingWorkerImage()}, buildErr: errors.New("step 5/7: uv sync: exit status 2"),
			calls: "image?," + build + ",deploy", notice: "building or pushing the image failed (step 5/7: uv sync: exit status 2). The campaign is deployed either way."},
		{name: "the push fails", client: &workerImageClient{image: missingWorkerImage()}, buildErr: errors.New("failed to push: connection reset by peer"),
			calls: "image?," + build + ",deploy", notice: "building or pushing the image failed (failed to push: connection reset by peer)"},
		{name: "the device cannot say", client: &workerImageClient{imageErr: status.Error(codes.Internal, "containerd is down")},
			calls: "image?,deploy", notice: "the device did not say which image it needs"},
		{name: "through Wendy Cloud", client: &workerImageClient{image: missingWorkerImage()}, conn: cloud(),
			calls: "image?,deploy", notice: "this connection goes through Wendy Cloud"},
	} {
		for _, asJSON := range []bool{false, true} {
			name := test.name
			if asJSON {
				name += " with --json"
			}
			t.Run(name, func(t *testing.T) {
				test.client.calls = nil
				stdout, stderr, err := deployWith(t, test.client, test.conn, asJSON, !test.noDocker, test.buildErr)
				if err != nil {
					t.Fatalf("the image kept the campaign from deploying: %v", err)
				}
				if calls := strings.Join(test.client.calls, ","); calls != test.calls {
					t.Fatalf("calls %q, want %q", calls, test.calls)
				}
				// Stdout is the deploy's result and nothing else.
				if want := map[bool]string{false: deployed, true: deployedPlanJSON}[asJSON]; stdout != want {
					t.Fatalf("stdout %q, want %q", stdout, want)
				}
				if (test.notice == "") != (stderr == "") || !strings.Contains(stderr, test.notice) || strings.Count(stderr, "\n") > 1 {
					t.Fatalf("stderr %q, want one line with %q", stderr, test.notice)
				}
				t.Logf("what the operator sees:\n%s%s", stderr, stdout)
			})
		}
	}
}

func TestCampaignWithoutInferenceAsksForNoWorkerImage(t *testing.T) {
	client := &workerImageClient{image: missingWorkerImage()}
	conn := &grpcclient.AgentConnection{DataService: client}
	var stdout, stderr bytes.Buffer
	if err := deployCampaign(context.Background(), conn, data.Campaign{Name: "people"}, nil, false, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if calls := strings.Join(client.calls, ","); calls != "deploy" || stderr.Len() != 0 {
		t.Fatalf("calls %q, stderr %q", calls, stderr.String())
	}
}

// Stopping the build is the one thing that stops the deploy.
func TestCancellingTheWorkerImageBuildCancelsTheDeploy(t *testing.T) {
	client := &workerImageClient{image: missingWorkerImage()}
	_, _, err := deployWith(t, client, nil, false, true, ErrUserCancelled)
	if !errors.Is(err, ErrUserCancelled) || slicesContain(client.calls, "deploy") {
		t.Fatalf("error %v, calls %q", err, client.calls)
	}
}

func slicesContain(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type registryOnlyDevice struct {
	agentpb.WendyProvisioningServiceClient
}

func (registryOnlyDevice) IsProvisioned(context.Context, *agentpb.IsProvisionedRequest, ...grpc.CallOption) (*agentpb.IsProvisionedResponse, error) {
	return nil, status.Error(codes.Unavailable, "no agent in this test")
}

// Opt-in integration: the real build and push, the way `wendy run` does them,
// of exactly what an agent serves, to a throwaway registry that stands in
// for a device's. Set WENDY_INFERENCE_PUSH_REGISTRY to its host:port (plain
// HTTP, such as a local `registry:2` container). Needs Docker.
func TestWorkerImageBuildsAndPushesToARegistry(t *testing.T) {
	registry := os.Getenv("WENDY_INFERENCE_PUSH_REGISTRY")
	if registry == "" {
		t.Skip("set WENDY_INFERENCE_PUSH_REGISTRY to a throwaway registry's host:port; needs docker and builds the worker image")
	}
	// This package's tests run with an empty HOME. Docker's plugins, context
	// and build cache are in the real one, as they are for `wendy run`.
	if account, err := user.Current(); err == nil {
		t.Setenv("HOME", account.HomeDir)
	}
	ctx, cancel := context.WithTimeout(context.Background(), inferenceWorkerImageTimeout)
	defer cancel()
	// As through a tunnel: the registry is reached by a dialer, not by the
	// device's fixed registry port.
	conn := &grpcclient.AgentConnection{Host: "throwaway-registry", ProvisioningService: registryOnlyDevice{},
		RegistryDialer: func(ctx context.Context, _ int) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", registry)
		}}
	conn.CacheAgentVersion(&agentpb.GetAgentVersionResponse{Os: "linux", CpuArchitecture: runtime.GOARCH})
	info := &agentpbv2.DataInferenceWorkerImageResponse{Repository: inference.WorkerRepository, DepsHash: inference.DepsHash(), BuildContext: inference.WorkerBuildContext()}
	// Progress on stderr, as campaign deploy asks for with --json.
	if err := buildInferenceWorkerImage(context.WithValue(ctx, detachedJSONRunKey{}, true), conn, info); err != nil {
		t.Fatal(err)
	}

	// The registry now holds what the agent looks for: the repository at tag
	// latest, labelled with the agent's hash.
	fetch := func(path, accept string, into any) {
		t.Helper()
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+registry+"/v2/"+inference.WorkerRepository+path, nil)
		request.Header.Set("Accept", accept)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %s", path, response.Status)
		}
		if err := json.NewDecoder(response.Body).Decode(into); err != nil {
			t.Fatal(err)
		}
	}
	const manifestTypes = "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"
	var manifest struct {
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Size int64 `json:"size"`
		} `json:"layers"`
	}
	fetch("/manifests/latest", manifestTypes, &manifest)
	if len(manifest.Manifests) > 0 {
		fetch("/manifests/"+manifest.Manifests[0].Digest, manifestTypes, &manifest)
	}
	var config struct {
		Architecture string `json:"architecture"`
		Config       struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	fetch("/blobs/"+manifest.Config.Digest, "application/vnd.oci.image.config.v1+json", &config)
	if config.Config.Labels[inference.WorkerImageDepsLabel] != inference.DepsHash() || config.Architecture != runtime.GOARCH {
		t.Fatalf("pushed image: architecture %s, labels %v, want %s labelled %s", config.Architecture, config.Config.Labels, runtime.GOARCH, inference.DepsHash())
	}
	var pushed int64
	for _, layer := range manifest.Layers {
		pushed += layer.Size
	}
	t.Logf("pushed %s/%s:latest for linux/%s: %d layers of %d MiB together, label %s=%s",
		registry, inference.WorkerRepository, runtime.GOARCH, len(manifest.Layers), pushed>>20, inference.WorkerImageDepsLabel, inference.DepsHash())
}

// The device names the repository, the build argument and the files; none of
// them may reach outside the build directory or the registry path.
func TestWorkerImageFromTheDeviceIsHeldToPlainNames(t *testing.T) {
	dir := t.TempDir()
	if err := writeInferenceBuildContext(dir, missingWorkerImage().GetBuildContext()); err != nil {
		t.Fatal(err)
	}
	if written, err := os.ReadFile(filepath.Join(dir, "Dockerfile")); err != nil || string(written) != "FROM scratch\n" {
		t.Fatalf("Dockerfile %q: %v", written, err)
	}
	for name, files := range map[string]map[string][]byte{
		"no Dockerfile":  {"uv.lock": nil},
		"a path":         {"Dockerfile": nil, "../../.ssh/authorized_keys": []byte("key")},
		"a subdirectory": {"Dockerfile": nil, "a/b": nil},
		"a hidden file":  {"Dockerfile": nil, ".dockerignore": nil},
		"too large":      {"Dockerfile": make([]byte, inferenceContextMaxBytes+1)},
	} {
		parent := t.TempDir()
		target := filepath.Join(parent, "context")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeInferenceBuildContext(target, files); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if entries, _ := os.ReadDir(parent); len(entries) != 1 {
			t.Errorf("%s: wrote outside the build directory: %v", name, entries)
		}
	}
	for name, info := range map[string]*agentpbv2.DataInferenceWorkerImageResponse{
		"a repository path": {Repository: "../../etc", DepsHash: "62dfd59926b6a643"},
		"a registry host":   {Repository: "evil.example/worker", DepsHash: "62dfd59926b6a643"},
		"a build argument":  {Repository: "wendy-inference-worker", DepsHash: "x --build-arg HTTP_PROXY=evil"},
	} {
		info.BuildContext = missingWorkerImage().GetBuildContext()
		if err := buildInferenceWorkerImage(context.Background(), &grpcclient.AgentConnection{}, info); !errors.Is(err, errInvalidInferenceImage) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
