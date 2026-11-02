package services

import (
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/gpudiscovery"
	"github.com/wendylabsinc/wendy/go/internal/agent/inference"
)

// InferenceHardware is the accelerator evidence campaign inference chooses an
// engine from: the same probe, and so the same values, as device info's
// gpuVendor, gpuCapabilities[].computeBackends, cudaVersion and gpuArch.
func InferenceHardware() inference.Hardware {
	info := detectGPUInfo()
	_, backends := gpudiscovery.Summary(info.devices)
	return inference.Hardware{GPUVendor: info.vendor, ComputeBackends: backends, CUDAVersion: info.cudaVersion, GPUArch: info.gpuArch}
}

// acceleratorState records the model process's report of what it runs on.
// A fallback to the CPU on a device that could do better is logged once per
// change, so it is never silent.
func (j *campaignInferenceJob) acceleratorState(accelerator *data.AcceleratorStatus, stats *data.InferenceStats) {
	if accelerator == nil {
		return
	}
	j.mu.Lock()
	previous := j.status.Accelerator
	j.status.Accelerator = accelerator
	if stats != nil {
		j.status.Stats = stats
	}
	j.mu.Unlock()
	if previous != nil && previous.Active == accelerator.Active && previous.Degraded == accelerator.Degraded && previous.Reason == accelerator.Reason {
		return
	}
	if accelerator.Degraded {
		active := accelerator.Active
		if active == "" {
			active = "nothing"
		}
		j.owner.service.manager.Warnf("campaign %q inference runs on %s, not tensorrt: %s", j.campaign.Name, active, accelerator.Reason)
	}
}

// predictionProvenance adds the engine, precision, inference time and model
// hash to a prediction record's attributes, when the process reported them.
func (j *campaignInferenceJob) predictionProvenance(attributes map[string]any, result inference.Result) {
	if result.Engine != "" {
		attributes["accelerator"] = result.Engine
	}
	if result.Precision != "" {
		attributes["precision"] = result.Precision
	}
	if result.InferMs > 0 {
		attributes["infer_ms"] = result.InferMs
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if accelerator := j.status.Accelerator; accelerator != nil && accelerator.ModelSHA256 != "" {
		attributes["model_sha256"] = accelerator.ModelSHA256
	}
}

// acceleratorSnapshot copies a status for inspect. The worker reports when a
// build started, or a wait for memory or idle CPUs began; its age is computed
// here, so it is current between reports.
func acceleratorSnapshot(status *data.AcceleratorStatus, now time.Time) *data.AcceleratorStatus {
	if status == nil {
		return nil
	}
	copied := *status
	copied.Candidates = append([]string(nil), status.Candidates...)
	if status.Parity != nil {
		parity := *status.Parity
		copied.Parity = &parity
	}
	if status.Engine != nil {
		engine := *status.Engine
		engine.Warnings = append([]string(nil), status.Engine.Warnings...)
		since := ""
		switch engine.State {
		case "building":
			since = engine.StartedAt
		case "waiting_for_memory", "waiting_for_cpu":
			since = engine.WaitingSince
		}
		if started, err := time.Parse(time.RFC3339, since); err == nil {
			engine.ElapsedSeconds = math.Round(now.Sub(started).Seconds())
		}
		copied.Engine = &engine
	}
	return &copied
}
