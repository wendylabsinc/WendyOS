// Package modelhub reads public Hugging Face metadata. Model weights are never
// downloaded to the desktop; the selected device's runtime downloads them.
package modelhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Repository struct {
	ID, Name, Description, License string
	Downloads                      int64
}
type File struct {
	Name, Quantization string
	UnavailableReason  string
	Bytes              int64
}
type Details struct {
	Repository
	Revision string
	Files    []File
}
type Client struct {
	HTTP    *http.Client
	BaseURL string
}

func New() *Client {
	return &Client{HTTP: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, BaseURL: "https://huggingface.co"}
}

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)
var filePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*\.gguf$`)
var quantPattern = regexp.MustCompile(`(?i)(?:^|[-_.])(IQ[1-4]_[A-Z0-9_]+|Q[2-8]_[A-Z0-9_]+|BF16|F16|F32)(?:\.|$)`)
var splitPattern = regexp.MustCompile(`-\d{5}-of-\d{5}\.gguf$`)
var splitPartsPattern = regexp.MustCompile(`^(.*)-(\d{5})-of-(\d{5})\.gguf$`)

func ValidRepository(id string) bool {
	return len(id) <= 200 && repoPattern.MatchString(id) && !strings.Contains(id, "..")
}
func ModelName(repo, file string) (string, error) {
	if !ValidRepository(repo) || !filePattern.MatchString(file) || len(file) > 240 || splitPattern.MatchString(file) || strings.Contains(file, "..") || modelSidecar(file) {
		return "", errors.New("choose a single-file GGUF model from the library")
	}
	return "hf.co/" + repo + ":" + file, nil
}
func modelSidecar(file string) bool {
	lower := strings.ToLower(file)
	return strings.Contains(lower, "mmproj") || strings.HasPrefix(lower, "mtp-") || strings.HasPrefix(lower, "dflash-") || strings.Contains(lower, "imatrix")
}
func Recommended() []Repository {
	return []Repository{
		{ID: "ggml-org/Qwen3.8-Flash-Next-GGUF", Name: "Qwen3.8 Flash Next", Description: "Qwen’s Flash model. Large split weights; check device capacity and runtime support."},
		{ID: "Qwen/Qwen3-0.6B-GGUF", Name: "Qwen3 · 0.6B", Description: "Smallest download. A starting point for CPU devices."},
		{ID: "Qwen/Qwen3-1.7B-GGUF", Name: "Qwen3 · 1.7B", Description: "A small general-purpose model for local conversation."},
		{ID: "Qwen/Qwen3-4B-GGUF", Name: "Qwen3 · 4B", Description: "A larger Qwen model, with a compact Q4 option."},
		{ID: "nvidia/NVIDIA-Nemotron-3-Nano-4B-GGUF", Name: "NVIDIA Nemotron 3 Nano · 4B", Description: "NVIDIA's compact reasoning model. Runs on CPU or supported GPU hardware."},
	}
}

type metadata struct {
	Pipeline  string          `json:"pipeline_tag"`
	ID        string          `json:"id"`
	SHA       string          `json:"sha"`
	Downloads int64           `json:"downloads"`
	Tags      []string        `json:"tags"`
	Gated     json.RawMessage `json:"gated"`
	Siblings  []struct {
		Name string `json:"rfilename"`
		Size int64  `json:"size"`
	} `json:"siblings"`
}

func (m metadata) repository() Repository {
	r := Repository{ID: m.ID, Name: m.ID, Downloads: m.Downloads}
	for _, tag := range m.Tags {
		if strings.HasPrefix(tag, "license:") {
			r.License = strings.TrimPrefix(tag, "license:")
			break
		}
	}
	return r
}
func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("reading Hugging Face: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Hugging Face returned HTTP %d; only public, ungated models are supported", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 4<<20 {
		return errors.New("model metadata is too large")
	}
	return json.Unmarshal(data, out)
}
func (c *Client) Search(ctx context.Context, query string) ([]Repository, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return Recommended(), nil
	}
	if len(query) > 200 {
		return nil, errors.New("search is too long")
	}
	var rows []metadata
	v := url.Values{"search": {query}, "filter": {"gguf"}, "pipeline_tag": {"text-generation"}, "sort": {"downloads"}, "direction": {"-1"}, "limit": {"24"}}
	if err := c.get(ctx, "/api/models?"+v.Encode(), &rows); err != nil {
		return nil, err
	}
	out := make([]Repository, 0, len(rows))
	for _, m := range rows {
		if ValidRepository(m.ID) {
			out = append(out, m.repository())
		}
	}
	return out, nil
}
func (c *Client) Details(ctx context.Context, repo string) (Details, error) {
	if !ValidRepository(repo) {
		return Details{}, errors.New("invalid Hugging Face repository")
	}
	var m metadata
	if err := c.get(ctx, "/api/models/"+repo+"?blobs=true", &m); err != nil {
		return Details{}, err
	}
	if len(m.Gated) > 0 && string(m.Gated) != "false" && string(m.Gated) != "null" {
		return Details{}, errors.New("this model requires Hugging Face approval; choose a public, ungated model")
	}
	d := Details{Repository: m.repository(), Revision: m.SHA}
	type splitGroup struct {
		first string
		total int
		parts map[int]int64
	}
	groups := map[string]*splitGroup{}
	for _, s := range m.Siblings {
		match := splitPartsPattern.FindStringSubmatch(s.Name)
		if len(match) != 4 || !filePattern.MatchString(s.Name) || modelSidecar(s.Name) || s.Size <= 0 || s.Size > 1<<40 {
			continue
		}
		index, _ := strconv.Atoi(match[2])
		total, _ := strconv.Atoi(match[3])
		if total < 2 || total > 999 || index < 1 || index > total {
			continue
		}
		key := match[1] + "/" + match[3]
		group := groups[key]
		if group == nil {
			group = &splitGroup{total: total, parts: map[int]int64{}}
			groups[key] = group
		}
		group.parts[index] = s.Size
		if index == 1 {
			group.first = s.Name
		}
	}
	for _, group := range groups {
		if group.first == "" || len(group.parts) != group.total {
			continue
		}
		var size int64
		for _, n := range group.parts {
			size += n
		}
		base := splitPartsPattern.FindStringSubmatch(group.first)[1] + ".gguf"
		quant := "GGUF"
		if match := quantPattern.FindStringSubmatch(base); len(match) > 1 {
			quant = strings.ToUpper(match[1])
		}
		d.Files = append(d.Files, File{Name: group.first, Quantization: quant, Bytes: size, UnavailableReason: "This model uses split GGUF weights. The managed Ollama downloader supports single-file models. Use a compatible llama.cpp server, then connect Chat to that server."})
	}
	for _, s := range m.Siblings {
		if _, err := ModelName(repo, s.Name); err != nil || s.Size <= 0 || s.Size > 1<<40 {
			continue
		}
		q := "GGUF"
		if match := quantPattern.FindStringSubmatch(s.Name); len(match) > 1 {
			q = strings.ToUpper(match[1])
		}
		d.Files = append(d.Files, File{Name: s.Name, Quantization: q, Bytes: s.Size})
	}
	sort.SliceStable(d.Files, func(i, j int) bool {
		a, b := d.Files[i], d.Files[j]
		if (a.Quantization == "Q4_K_M") != (b.Quantization == "Q4_K_M") {
			return a.Quantization == "Q4_K_M"
		}
		if a.Bytes != b.Bytes {
			return a.Bytes < b.Bytes
		}
		return a.Name < b.Name
	})
	if len(d.Files) == 0 {
		return d, errors.New("no supported single-file GGUF weights in this repository")
	}
	return d, nil
}
