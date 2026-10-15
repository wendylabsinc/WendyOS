package containerd

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/hostnetwork"
	"go.uber.org/zap"
)

// This is actual container inventory, including stopped containers with retained
// namespaces. No task/PID absence alone proves that a mesh app released its IP.
func (c *Client) reconcileLANReplyGuards(ctx context.Context) {
	ctx, cancel := context.WithTimeout(c.withNamespace(ctx), 5*time.Second)
	defer cancel()
	err := hostnetwork.ReconcileLANReplyGuards(func() ([]string, error) {
		ctrs, err := c.client.Containers(ctx)
		if err != nil {
			return nil, err
		}
		raw, err := os.ReadFile(cniSubnetRegistryPath)
		if err != nil {
			return nil, err
		}
		registry := map[string]string{}
		if err = json.Unmarshal(raw, &registry); err != nil {
			return nil, err
		}
		inventory := make(map[string]map[string]string, len(ctrs))
		for _, ctr := range ctrs {
			labels, err := ctr.Labels(ctx)
			if err != nil {
				return nil, err
			}
			inventory[ctr.ID()] = labels
		}
		return lanGuardNetworks(inventory, registry)
	})
	if err != nil {
		c.logger.Warn("LAN reply guard inventory cleanup deferred", zap.Error(err))
	}
}

// Cleanup must not use the forgiving legacy decoder: corrupt or ambiguous
// network metadata cannot prove that a persisted container relinquished its IP.
func lanGuardNetworks(inventory map[string]map[string]string, registry map[string]string) ([]string, error) {
	var networks []string
	for id, labels := range inventory {
		mesh := false
		for key, raw := range labels {
			prefix := appconfig.EntitlementAnnotationKeyPrefix + "network"
			if key != prefix && !strings.HasPrefix(key, prefix+".") {
				continue
			}
			if key != prefix {
				n, e := strconv.Atoi(strings.TrimPrefix(key, prefix+"."))
				if e != nil || n < 0 {
					return nil, fmt.Errorf("ambiguous network label on %s", id)
				}
			}
			var ent appconfig.Entitlement
			value := strings.TrimSpace(raw)
			if strings.HasPrefix(value, "{") {
				// Reject duplicate keys, which otherwise silently use the last JSON value.
				dec := json.NewDecoder(strings.NewReader(value))
				if _, e := dec.Token(); e != nil {
					return nil, e
				}
				keys := map[string]bool{}
				for dec.More() {
					token, e := dec.Token()
					if e != nil {
						return nil, e
					}
					key, ok := token.(string)
					key = strings.ToLower(key)
					if !ok || keys[key] {
						return nil, fmt.Errorf("ambiguous network JSON on %s", id)
					}
					keys[key] = true
					var v json.RawMessage
					if e = dec.Decode(&v); e != nil {
						return nil, e
					}
				}
				if _, e := dec.Token(); e != nil {
					return nil, e
				}
				if _, e := dec.Token(); e != io.EOF {
					return nil, fmt.Errorf("trailing network JSON on %s", id)
				}
				dec = json.NewDecoder(strings.NewReader(value))
				dec.DisallowUnknownFields()
				if e := dec.Decode(&ent); e != nil {
					return nil, e
				}
			} else {
				ent = appconfig.ParseEntitlementAnnotation(appconfig.EntitlementNetwork, value)
				if appconfig.EntitlementAnnotationValue(ent) != value {
					return nil, fmt.Errorf("ambiguous network annotation on %s", id)
				}
			}
			switch ent.Mode {
			case "mesh":
				mesh = true
			case "", "host", "host-admin", "bridge", "none":
			default:
				return nil, fmt.Errorf("unknown network mode on %s", id)
			}
			if ent.ServiceCIDR != "" && ent.Mode != "mesh" {
				return nil, fmt.Errorf("ambiguous service CIDR on %s", id)
			}
		}
		if !mesh {
			continue
		}
		appID, _, err := ParseContainerName(id)
		if err != nil {
			return nil, err
		}
		subnet, exists := registry[appID]
		if !exists {
			return nil, fmt.Errorf("live mesh app %s has no trusted subnet allocation", appID)
		}
		networks = append(networks, subnet)
	}
	return networks, nil
}
