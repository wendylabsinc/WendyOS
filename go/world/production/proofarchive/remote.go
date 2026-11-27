package proofarchive

import (
	"context"

	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

// RememberRemoteAccepted retains an original foreign typed capture selected
// by the host from its admitted remote journal. It is not an app import API.
// The optional remote verifier and host callback run before, during and after
// storage, under the caller's existing identity/remote-operation fence. A
// postcommit refusal remains uncertain, just as for RememberAccepted.
func (a *Archive) RememberRemoteAccepted(ctx context.Context, envelope []byte) (Reference, error) {
	return a.rememberAccepted(ctx, envelope, true)
}

// This immutable schema check is also used on reopen; no current authority or
// time is inferred from retained bytes. Camera/audio media references and native
// SensorLink bindings are retained, but no media bytes are read or stored here.
// Recursive results remain outside this path. Original sample, capture time,
// identity and TTL remain untouched. The internal name is retained for the
// existing storage decoder; it now includes media-referencing envelopes.
func remoteInline(record serviceorigin.Record) bool {
	p := record.Publication
	if p.ComponentType != sensorrecord.ComponentType || p.Tombstone || record.Scope.ExecutionDomain != "simulation" {
		return false
	}
	component := (serviceorigin.VerifiedRecord{Record: record}).Component()
	c, err := sensorrecord.FromComponent(component)
	if err != nil {
		return false
	}
	s, err := c.Sample()
	if err != nil || (s.Kind != "gas" && s.Kind != "lidar" && s.Kind != "camera" && s.Kind != "audio") {
		return false
	}
	if _, err := c.Evidence(); err != nil {
		return false
	}
	_, _, err = sensorrecord.LiveDeadlines(component)
	return err == nil
}
