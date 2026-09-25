package models

import (
	"context"
	"sort"
	"strconv"
)

// Paths inside a model host container (design §6.2).
const (
	HostModelDir       = "/run/wendy/model"
	HostModelFile      = HostModelDir + "/model"
	HostLabelsFile     = HostModelDir + "/labels.txt"
	HostEngineCacheDir = HostModelDir + "/engines"
)

// Limits every model host receives.
const (
	HostMaxFPS          = 10
	HostConfidenceFloor = 0.30
)

// HostUID and HostGID are the unprivileged identity model hosts run as.
const (
	HostUID = 65534
	HostGID = 65534
)

// HostSpec is what a Runtime needs to create one model host container.
type HostSpec struct {
	InstanceID   string
	AppID        string // AppIDPrefix + InstanceID: the data socket and cgroup identity
	Image        string // pinned by digest
	Engine       string
	ModelID      string
	VariantID    string
	FileSHA256   string
	ModelFile    string // host path, mounted read-only at HostModelFile
	LabelsFile   string // host path, mounted read-only at HostLabelsFile
	EngineCache  string // host directory mounted read-write at HostEngineCacheDir; TensorRT only
	CameraNode   string // the camera's two-plane node, bound at the same path
	CameraSource string // canonical source id, e.g. "v4l2:/dev/video0"
	LogPath      string // host file that receives the container's stdout and stderr
}

// Env is the environment half of the host contract. The runtime adds
// WENDY_DATA_SOCKET when it mounts the socket.
func (h HostSpec) Env() []string {
	env := []string{
		"WENDY_MODEL_INSTANCE=" + h.InstanceID,
		"WENDY_MODEL_VARIANT=" + h.VariantID,
		"WENDY_MODEL_FILE=" + HostModelFile,
		"WENDY_MODEL_LABELS=" + HostLabelsFile,
		"WENDY_CAMERA_NODE=" + h.CameraNode,
		"WENDY_CAMERA_SOURCE=" + h.CameraSource,
		"WENDY_MODEL_MAX_FPS=" + strconv.Itoa(HostMaxFPS),
		"WENDY_MODEL_CONFIDENCE_FLOOR=" + strconv.FormatFloat(HostConfidenceFloor, 'f', 2, 64),
	}
	if h.EngineCache != "" {
		env = append(env, "WENDY_MODEL_ENGINE_CACHE="+HostEngineCacheDir)
	}
	sort.Strings(env)
	return env
}

// HostExit reports that a model host's process ended.
type HostExit struct {
	Code uint32
	Err  error
}

// Runtime creates and removes model host containers.
type Runtime interface {
	// HasImage reports whether ref is already on the device, so the catalog
	// can say whether a first start downloads it.
	HasImage(ctx context.Context, ref string) bool
	EnsureImage(ctx context.Context, ref string) error
	// StartHost creates and starts a host. The channel receives exactly one
	// value when the host's process ends, for any reason.
	StartHost(ctx context.Context, spec HostSpec) (<-chan HostExit, error)
	// RemoveHost stops and deletes a host; removing a missing host is not an error.
	RemoveHost(ctx context.Context, instanceID string) error
	// ListHosts returns the instance ids of every existing host container.
	ListHosts(ctx context.Context) ([]string, error)
}

// Camera is a local camera a model can watch.
type Camera struct {
	SourceID string // e.g. "v4l2:/dev/video0"
	Name     string
}

// Cameras lists local cameras and pins the nodes models read them from.
type Cameras interface {
	List(ctx context.Context) []Camera
	// Acquire keeps the two-plane path running for owner and returns the node
	// carrying sourceID. Failures wrap ErrCameraNotStreamable.
	Acquire(ctx context.Context, owner, sourceID string) (string, error)
	Release(ctx context.Context, owner string)
}

// Files provides verified model files. *FileCache implements it.
type Files interface {
	Has(sha256 string) bool
	Fetch(ctx context.Context, f File) (string, error)
}
