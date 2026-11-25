package modelhub

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

type VisionModel struct {
	Repository, Revision, Task, License string
	Labels                              []string
	WeightsBytes                        int64
}

var revisionPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

// VisionDetails resolves a concrete revision and reads model-declared labels.
// The desktop never downloads or executes repository code.
func (c *Client) VisionDetails(ctx context.Context, repo, task string) (VisionModel, error) {
	if !ValidRepository(repo) {
		return VisionModel{}, errors.New("enter a Hugging Face owner/repository")
	}
	if task != "object-detection" && task != "image-classification" {
		return VisionModel{}, errors.New("unsupported vision task")
	}
	var m metadata
	if err := c.get(ctx, "/api/models/"+repo+"?blobs=true", &m); err != nil {
		return VisionModel{}, err
	}
	if m.Pipeline != task {
		return VisionModel{}, errors.New("this repository is not tagged for the selected vision task")
	}
	if !revisionPattern.MatchString(m.SHA) {
		return VisionModel{}, errors.New("model does not have a valid immutable revision")
	}
	v := VisionModel{Repository: repo, Revision: m.SHA, Task: task, License: m.repository().License}
	for _, file := range m.Siblings {
		if strings.HasSuffix(file.Name, ".safetensors") {
			if file.Size <= 0 || file.Size > 1<<40 {
				return v, errors.New("model weight size is unavailable")
			}
			v.WeightsBytes += file.Size
		}
	}
	if v.WeightsBytes == 0 {
		return v, errors.New("choose a model with safetensors weights; pickle weights are not supported")
	}
	var cfg struct {
		ModelType string            `json:"model_type"`
		Labels    map[string]string `json:"id2label"`
	}
	// HF's small JSON files redirect to its same-origin resolve-cache endpoint.
	client := *c.HTTP
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 4 || req.URL.Scheme != "https" || req.URL.Host != "huggingface.co" {
			return http.ErrUseLastResponse
		}
		return nil
	}
	reader := &Client{HTTP: &client, BaseURL: c.BaseURL}
	if err := reader.get(ctx, "/"+repo+"/resolve/"+m.SHA+"/config.json", &cfg); err != nil {
		return v, err
	}
	supported := map[string]bool{"detr": true, "yolos": true, "vit": true, "mobilenet_v2": true, "resnet": true, "convnext": true, "swin": true, "deit": true, "beit": true, "rt_detr": true}
	if !supported[cfg.ModelType] {
		return v, errors.New("this model architecture is not supported by the campaign model picker")
	}
	seen := map[string]bool{}
	for _, label := range cfg.Labels {
		if label != "N/A" && label != "" && len(label) <= 128 && !seen[label] {
			v.Labels = append(v.Labels, label)
			seen[label] = true
		}
	}
	if len(v.Labels) == 0 || len(v.Labels) > 10000 {
		return v, errors.New("model does not declare a supported label vocabulary")
	}
	sort.Strings(v.Labels)
	return v, nil
}
