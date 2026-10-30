package data

import (
	"fmt"
	"math"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

// CampaignInference is executed by the agent, not by a user application.
// Empty Backend uses Transformers; yolo_onnx runs a pinned ONNX detection export.
// Accelerator chooses the hardware that runs the model, which Backend does not:
// Backend is the kind of model, Accelerator the engine that scores it.
type CampaignInference struct {
	Task        string   `json:"task,omitempty" yaml:"task,omitempty"`
	Backend     string   `json:"backend,omitempty" yaml:"backend,omitempty"`
	ModelFile   string   `json:"model_file,omitempty" yaml:"model_file,omitempty"`
	Accelerator string   `json:"accelerator,omitempty" yaml:"accelerator,omitempty"`
	Model       string   `json:"model" yaml:"model"`
	Revision    string   `json:"revision" yaml:"revision"`
	Labels      []string `json:"labels" yaml:"labels"`
	Threshold   float64  `json:"threshold" yaml:"threshold"`
	Rate        float64  `json:"rate" yaml:"rate"`
	Event       string   `json:"event" yaml:"event"`
	ClearAfter  string   `json:"clear_after" yaml:"clear_after"`
	Cooldown    string   `json:"cooldown" yaml:"cooldown"`
	Enabled     *bool    `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

// The accelerators a campaign may name. The set is closed: anything else is
// refused at deploy.
const (
	// AcceleratorAuto uses TensorRT on an NVIDIA GPU with CUDA and the TensorRT
	// libraries, and the CPU otherwise. A fallback to the CPU on such a device is
	// reported as degraded, with its reason.
	AcceleratorAuto = "auto"
	// AcceleratorCPU never uses an accelerator: the behaviour before accelerators.
	AcceleratorCPU = "cpu"
	// AcceleratorTensorRT requires TensorRT and never scores on the CPU.
	AcceleratorTensorRT = "tensorrt"
)

func (i *CampaignInference) IsEnabled() bool { return i != nil && (i.Enabled == nil || *i.Enabled) }

// EffectiveAccelerator is Accelerator, with auto when the campaign names none.
func (i *CampaignInference) EffectiveAccelerator() string {
	if i == nil || i.Accelerator == "" {
		return AcceleratorAuto
	}
	return i.Accelerator
}
func (i *CampaignInference) ClearDuration() time.Duration {
	d, _ := time.ParseDuration(i.ClearAfter)
	return d
}
func (i *CampaignInference) CooldownDuration() time.Duration {
	d, _ := time.ParseDuration(i.Cooldown)
	return d
}

var modelRepositoryRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)
var modelRevisionRE = regexp.MustCompile(`^[a-f0-9]{40}$`)
var inferenceEventRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

func (i *CampaignInference) validate() error {
	if i == nil {
		return nil
	}
	if i.Task != "" && i.Task != "object-detection" && i.Task != "image-classification" {
		return fmt.Errorf("inference.task must be object-detection or image-classification")
	}
	if i.Task == "image-classification" && i.Backend == "yolo_onnx" {
		return fmt.Errorf("image classification requires the transformers backend")
	}
	if strings.HasPrefix(i.Model, "https://huggingface.co/") {
		u, err := url.Parse(i.Model)
		if err != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("inference.model must be a Hugging Face repository URL without a query or fragment")
		}
		i.Model = strings.Trim(u.Path, "/")
	}
	if !modelRepositoryRE.MatchString(i.Model) {
		return fmt.Errorf("inference.model must be owner/repository or https://huggingface.co/owner/repository")
	}
	if !modelRevisionRE.MatchString(i.Revision) {
		return fmt.Errorf("inference.revision must pin a 40-character Hugging Face commit SHA")
	}
	switch i.Backend {
	case "", "transformers":
		if i.ModelFile != "" {
			return fmt.Errorf("inference.model_file requires backend: yolo_onnx")
		}
	case "yolo_onnx":
		if len(i.ModelFile) > 256 || !strings.HasSuffix(i.ModelFile, ".onnx") || path.Clean(i.ModelFile) != i.ModelFile || strings.HasPrefix(i.ModelFile, "/") || strings.HasPrefix(i.ModelFile, "../") || strings.ContainsAny(i.ModelFile, "\\\x00\r\n") {
			return fmt.Errorf("inference.model_file must be a relative .onnx file inside the Hugging Face repository")
		}
	default:
		return fmt.Errorf("inference.backend must be transformers or yolo_onnx")
	}
	switch i.Accelerator {
	case "", AcceleratorAuto, AcceleratorCPU:
	case AcceleratorTensorRT:
		// Only the YOLO ONNX backend has a TensorRT engine. Auto on a
		// Transformers model runs on the CPU and says why.
		if i.Backend != "yolo_onnx" {
			return fmt.Errorf("inference.accelerator: tensorrt requires backend: yolo_onnx")
		}
	default:
		return fmt.Errorf("inference.accelerator must be auto, cpu or tensorrt")
	}
	if len(i.Labels) == 0 || len(i.Labels) > 32 {
		return fmt.Errorf("inference.labels must contain 1..32 labels")
	}
	seen := map[string]bool{}
	for _, label := range i.Labels {
		if len(label) == 0 || len(label) > 128 || strings.TrimSpace(label) != label || seen[label] {
			return fmt.Errorf("inference.labels must be unique, nonempty labels of at most 128 bytes")
		}
		seen[label] = true
	}
	if math.IsNaN(i.Threshold) || math.IsInf(i.Threshold, 0) || i.Threshold <= 0 || i.Threshold > 1 {
		return fmt.Errorf("inference.threshold must be in (0, 1]")
	}
	// Two records per result leaves room below the app record rate limit.
	if math.IsNaN(i.Rate) || math.IsInf(i.Rate, 0) || i.Rate <= 0 || i.Rate > 30 {
		return fmt.Errorf("inference.rate must be in (0, 30] frames per second per camera")
	}
	if !inferenceEventRE.MatchString(i.Event) {
		return fmt.Errorf("inference.event must use 1..128 letters, numbers, '.', '-' or '_'")
	}
	for name, raw := range map[string]string{"clear_after": i.ClearAfter, "cooldown": i.Cooldown} {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 || d > 24*time.Hour {
			return fmt.Errorf("inference.%s must be a positive duration of at most 24h", name)
		}
	}
	return nil
}

// InferenceStatus is live state, never part of the campaign revision or plan.
type InferenceStatus struct {
	State             string            `json:"state"`
	Error             string            `json:"error,omitempty"`
	Sources           map[string]string `json:"sources,omitempty"`
	NotificationError string            `json:"notification_error,omitempty"`
	// Accelerator is what the model process reports it is scoring on.
	Accelerator *AcceleratorStatus `json:"accelerator,omitempty"`
	// Stats are the model process's recent throughput and latency.
	Stats *InferenceStats `json:"stats,omitempty"`
}

// AcceleratorStatus is the engine a campaign's model actually runs on, as the
// model process reports it, never as the agent planned it.
type AcceleratorStatus struct {
	// Requested is the campaign's accelerator: auto, cpu or tensorrt.
	Requested string `json:"requested"`
	// Active is the engine scoring frames now: cpu or tensorrt. It is empty
	// while a tensorrt campaign waits for its engine, because that campaign
	// never scores on the CPU.
	Active string `json:"active"`
	// Candidates are the engines the agent offered, best first.
	Candidates []string `json:"candidates,omitempty"`
	// Degraded is true when the device has an accelerator the campaign could
	// use and the model runs on the CPU anyway. Reason says why.
	Degraded bool `json:"degraded,omitempty"`
	// Reason says why the model is not on an accelerator, or why it left one.
	Reason string `json:"reason,omitempty"`
	// Runtime and Precision describe Active, such as "TensorRT 10.16.2.10"
	// and fp16.
	Runtime   string `json:"runtime,omitempty"`
	Precision string `json:"precision,omitempty"`
	// GPUArch is the GPU's compute capability, such as sm_87.
	GPUArch string `json:"gpu_arch,omitempty"`
	// ModelSHA256 is the hash of the model bytes the engine was built from.
	ModelSHA256 string `json:"model_sha256,omitempty"`
	// GPUMemoryMiB is what loading the TensorRT engine took, measured.
	GPUMemoryMiB float64 `json:"gpu_memory_mib,omitempty"`
	// Engine is the TensorRT engine's cache and build state.
	Engine *EngineStatus `json:"engine,omitempty"`
	// Parity is the CPU and TensorRT comparison that preceded the switch.
	Parity *ParityStatus `json:"parity,omitempty"`
}

// EngineStatus is a TensorRT engine's build or cache state.
type EngineStatus struct {
	// State is checking, scheduled, waiting_for_memory, waiting_for_cpu,
	// queued, building, verifying, ready or failed.
	State string `json:"state"`
	// Key is the engine cache key: a hash over the model, TensorRT version,
	// GPU, precision, input shape and builder settings.
	Key string `json:"key,omitempty"`
	// Cached is true when the engine came from the cache, with no build.
	Cached bool `json:"cached,omitempty"`
	// StartedAt is when the current build started, and WaitingSince when
	// the current wait for memory or idle CPUs began. ElapsedSeconds is how
	// long the build has run, or the wait lasted, as of this status.
	StartedAt      string  `json:"started_at,omitempty"`
	WaitingSince   string  `json:"waiting_since,omitempty"`
	ElapsedSeconds float64 `json:"elapsed_seconds,omitempty"`
	BuildSeconds   float64 `json:"build_seconds,omitempty"`
	PlanBytes      int64   `json:"plan_bytes,omitempty"`
	Error          string  `json:"error,omitempty"`
	// NotBefore is when a scheduled build may start: an auto campaign's
	// first build waits a random delay, so a fleet does not build at once.
	NotBefore string `json:"not_before,omitempty"`
	// BuildBy is when a build waiting for idle CPUs starts anyway, at the
	// lowest priority; LowPriority marks such a build while it runs.
	BuildBy     string `json:"build_by,omitempty"`
	LowPriority bool   `json:"low_priority,omitempty"`
	// Warnings are what TensorRT warned about while loading the engine.
	Warnings []string `json:"warnings,omitempty"`
}

// ParityStatus compares CPU and TensorRT detections on the same frames.
type ParityStatus struct {
	Passed        bool    `json:"passed"`
	Frames        int     `json:"frames"`
	MaxScoreDelta float64 `json:"max_score_delta"`
	MinIoU        float64 `json:"min_iou,omitempty"`
}

// InferenceStats summarise the model process's recent frames.
type InferenceStats struct {
	FPS          float64 `json:"fps"`
	InferMsP50   float64 `json:"infer_ms_p50"`
	InferMsP95   float64 `json:"infer_ms_p95"`
	FramesScored uint64  `json:"frames_scored"`
}

// InferenceDirectory is outside the episode quota and upload tree. It is
// beside the store, or where an earlier agent downloaded the runtime when the
// store moved to the data disk; see StoreLocation.InferenceDir.
func (m *Manager) InferenceDirectory() string {
	return m.location.InferenceDir
}
