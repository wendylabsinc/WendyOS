package data

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/datadoc"
	"gopkg.in/yaml.v3"
)

// WatchVersion is the watch document schema this agent reads.
const WatchVersion = 1

// watchRevisionSchema versions the field list revision covers.
const watchRevisionSchema = 1

// The lease range a watch may ask for (spec §6.1).
const (
	MinWatchLease = 15 * time.Second
	MaxWatchLease = 10 * time.Minute
)

// WatchTarget names the inference whose arrivals a watch receives.
type WatchTarget struct {
	Inference string `json:"inference" yaml:"inference"`
}

// WatchNotify is one destination beyond the journal: `cloud`, or
// `webhook: <url>`.
type WatchNotify struct {
	Cloud   bool   `json:"cloud,omitempty" yaml:"-"`
	Webhook string `json:"webhook,omitempty" yaml:"-"`
}

func (n *WatchNotify) UnmarshalYAML(node *yaml.Node) error {
	switch {
	case node.Kind == yaml.ScalarNode && node.Value == "cloud":
		n.Cloud = true
		return nil
	case node.Kind == yaml.MappingNode && len(node.Content) == 2 && node.Content[0].Value == "webhook":
		if err := node.Content[1].Decode(&n.Webhook); err != nil {
			return err
		}
		if n.Webhook != "" {
			return nil
		}
	}
	return fmt.Errorf("line %d: a notify entry is `cloud` or `webhook: <url>`", node.Line)
}

// Watch delivers one inference's arrivals as notifications (spec §6). A watch
// with a lease is removed, with what it owns, when its lease lapses.
type Watch struct {
	datadoc.HeaderFields `yaml:",inline"`
	Lease                string        `json:"lease,omitempty" yaml:"lease,omitempty"`
	On                   WatchTarget   `json:"on" yaml:"on"`
	Notify               []WatchNotify `json:"notify,omitempty" yaml:"notify,omitempty"`
}

func decodeWatch(_ datadoc.Header, contents []byte) (any, string, []string, error) {
	var watch Watch
	if err := datadoc.DecodeStrict(contents, &watch); err != nil {
		return nil, "", nil, err
	}
	// The Go zero value cannot tell `lease:` from no lease, which would
	// silently make the watch unleased.
	var root yaml.Node
	if err := yaml.Unmarshal(contents, &root); err == nil && len(root.Content) == 1 && root.Content[0].Kind == yaml.MappingNode {
		fields := root.Content[0].Content
		for i := 0; i+1 < len(fields); i += 2 {
			if fields[i].Value == "lease" && watch.Lease == "" {
				return nil, "", nil, errors.New("lease must be a duration from 15s to 10m")
			}
		}
	}
	if err := watch.validate(); err != nil {
		return nil, "", nil, err
	}
	return watch, watch.revision(), nil, nil
}

func (w Watch) validate() error {
	if w.Lease != "" {
		lease, err := time.ParseDuration(w.Lease)
		if err != nil || lease < MinWatchLease || lease > MaxWatchLease {
			return errors.New("lease must be a duration from 15s to 10m")
		}
	}
	if !datadoc.ValidName(w.On.Inference) {
		return errors.New("on.inference must name an inference document")
	}
	clouds, webhooks := 0, 0
	for _, notify := range w.Notify {
		if notify.Cloud {
			clouds++
		}
		if notify.Webhook != "" {
			webhooks++
			if err := validateWebhookURL("notify webhook", notify.Webhook); err != nil {
				return err
			}
		}
	}
	if clouds > 1 || webhooks > 1 {
		return errors.New("notify lists cloud and webhook at most once each")
	}
	return nil
}

func (w Watch) revision() string {
	canonical, _ := json.Marshal(map[string]any{
		"revision_schema": watchRevisionSchema,
		"version":         w.Version,
		"name":            w.Name,
		"owner":           w.Owner,
		"lease":           w.Lease,
		"on_inference":    w.On.Inference,
		"notify":          w.Notify,
	})
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:])
}

// Leased reports whether the device removes the watch when it is not renewed.
func (w Watch) Leased() bool { return w.Lease != "" }

// LeaseDuration is the lease, or zero for a watch without one.
func (w Watch) LeaseDuration() time.Duration {
	lease, _ := time.ParseDuration(w.Lease)
	return lease
}

// NotifiesCloud reports whether the watch delivers through Wendy Cloud.
func (w Watch) NotifiesCloud() bool {
	for _, notify := range w.Notify {
		if notify.Cloud {
			return true
		}
	}
	return false
}

// WebhookURL is the watch's webhook, or empty.
func (w Watch) WebhookURL() string {
	for _, notify := range w.Notify {
		if notify.Webhook != "" {
			return notify.Webhook
		}
	}
	return ""
}
