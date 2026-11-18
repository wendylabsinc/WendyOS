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

// missingWorkerImage is the answer of an agent of this CLI's version on a
// device without the image.
func missingWorkerImage() *agentpbv2.DataInferenceWorkerImageResponse {
	return &agentpbv2.DataInferenceWorkerImageResponse{Repository: inference.WorkerRepository, DepsHash: inference.DepsHash()}
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
	buildInferenceWorkerImageFn = func(ctx context.Context, _ *grpcclient.AgentConnection, platform string) error {
		client.calls = append(client.calls, "build "+platform)
		if !detachedJSONRun(ctx) {
			t.Error("build progress would go to stdout")
		}
		return buildErr
	}
	if conn == nil {
		conn = linuxDevice("arm64")
	}
	conn.DataService = client
	var out, notices bytes.Buffer
	plan := data.Campaign{Name: "people", Inference: &data.CampaignInference{Model: "owner/model"}}
	err = deployCampaign(context.Background(), conn, plan, []byte("name: people\n"), asJSON, &out, &notices)
	return out.String(), notices.String(), err
}

// linuxDevice is a connection to a device whose agent reports Linux on the
// given architecture.
func linuxDevice(architecture string) *grpcclient.AgentConnection {
	conn := &grpcclient.AgentConnection{}
	conn.CacheAgentVersion(&agentpb.GetAgentVersionResponse{Os: "linux", CpuArchitecture: architecture})
	return conn
}

func TestCampaignDeployPutsTheWorkerImageOnTheDevice(t *testing.T) {
	const deployed = "Campaign people: armed (revision 0123456789ab)\n"
	const build = "build linux/arm64"
	cloud := func() *grpcclient.AgentConnection {
		conn := linuxDevice("arm64")
		conn.Reconnect = func(context.Context) (*grpcclient.AgentConnection, error) { return nil, nil }
		return conn
	}
	otherPlatform := func(os, architecture string) *grpcclient.AgentConnection {
		conn := &grpcclient.AgentConnection{}
		conn.CacheAgentVersion(&agentpb.GetAgentVersionResponse{Os: os, CpuArchitecture: architecture})
		return conn
	}
	answer := func(repository, depsHash string) *workerImageClient {
		return &workerImageClient{image: &agentpbv2.DataInferenceWorkerImageResponse{Repository: repository, DepsHash: depsHash}}
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
		{name: "the device has the image", client: &workerImageClient{image: &agentpbv2.DataInferenceWorkerImageResponse{Repository: inference.WorkerRepository, DepsHash: inference.DepsHash(), Present: true}},
			calls: "image?,deploy"},
		{name: "an amd64 device", client: &workerImageClient{image: missingWorkerImage()}, conn: linuxDevice("amd64"),
			calls: "image?,build linux/amd64,deploy", notice: "The model worker image is on the device."},
		// The device's answer is compared with what this CLI builds, and is
		// never an input to a build: on any difference nothing is built.
		{name: "an agent with other worker dependencies", client: answer(inference.WorkerRepository, "0000000000000000"),
			calls: "image?,deploy", notice: "this CLI and the device's agent ship different worker dependencies, so this CLI cannot build the image that agent runs. The campaign is deployed either way. To run it in a container, bring the CLI and the device's agent to matching versions"},
		{name: "a hash that is no hash", client: answer(inference.WorkerRepository, inference.DepsHash()+" --build-arg HTTP_PROXY=http://evil.example"),
			calls: "image?,deploy", notice: "ship different worker dependencies"},
		{name: "another repository", client: answer("someone-elses-app", inference.DepsHash()),
			calls: "image?,deploy", notice: "the device asks for an image this CLI does not build"},
		{name: "a repository on another registry", client: answer("evil.example/"+inference.WorkerRepository, inference.DepsHash()),
			calls: "image?,deploy", notice: "the device asks for an image this CLI does not build"},
		{name: "a repository that is a path", client: answer("../../"+inference.WorkerRepository, inference.DepsHash()),
			calls: "image?,deploy", notice: "the device asks for an image this CLI does not build"},
		{name: "a device that reports another OS", client: &workerImageClient{image: missingWorkerImage()}, conn: otherPlatform("darwin", "arm64"),
			calls: "image?,deploy", notice: "there is no worker image for the platform the device reports"},
		{name: "a device that reports no real architecture", client: &workerImageClient{image: missingWorkerImage()}, conn: otherPlatform("linux", "arm64 --output type=local,dest=/"),
			calls: "image?,deploy", notice: "there is no worker image for the platform the device reports"},
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
// of the files compiled into this CLI, to a throwaway registry that stands in
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
	// Progress on stderr, as campaign deploy asks for with --json.
	if err := buildInferenceWorkerImage(context.WithValue(ctx, detachedJSONRunKey{}, true), conn, inferenceWorkerPlatforms[runtime.GOARCH]); err != nil {
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
