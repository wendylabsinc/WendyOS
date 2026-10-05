package services

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/acmeenroll"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
)

// ApplyACMEEnrollmentFile consumes the CLI's imaging handoff on first boot.
// Failed or interrupted issuance is never retried on an
// agent restart: the credential and account key remain for coordinated recovery.
// Call after OnProvisioned is installed, as with ApplyEnrollmentFile.
func (s *ProvisioningServiceV2) ApplyACMEEnrollmentFile(ctx context.Context, path string) {
	svc := s.v1
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return
	} else if err != nil {
		svc.logger.Warn("Cannot inspect baked ACME enrollment file")
		return
	}
	if _, err := os.Stat(filepath.Join(svc.configPath, enrollmentFileName)); !errors.Is(err, os.ErrNotExist) {
		svc.logger.Warn("Baked ACME enrollment retained: conflicting legacy enrollment file or unreadable state")
		return
	}
	// Do not read a staged secret or overwrite an existing identity.
	_, _, _, enrolled := svc.ProvisioningInfo()
	if enrolled {
		svc.logger.Warn("Baked ACME enrollment retained: device is already provisioned")
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		svc.logger.Warn("Cannot read baked ACME enrollment file")
		return
	}
	var bake struct {
		acmeenroll.Config
		CloudHost string `json:"cloudHost"`
	}
	// Never log parse errors: encoding/json errors can include secret input.
	if json.Unmarshal(data, &bake) != nil || bake.Config.Validate() != nil || bake.CloudHost == "" {
		svc.logger.Warn("Invalid baked ACME enrollment file retained")
		return
	}
	if err := os.MkdirAll(svc.configPath, 0o700); err != nil {
		svc.logger.Warn("Cannot persist baked ACME enrollment attempt")
		return
	}
	// A durable, exclusive marker prevents reboot/restart from reissuing after
	// an uncertain failure (including a crash after issuance but before state
	// persistence). It contains no credential. Do not automatically remove it.
	marker := filepath.Join(svc.configPath, "acme-first-boot-attempt.json")
	f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		svc.logger.Warn("Baked ACME enrollment retained: previous attempt requires coordinated recovery")
		return
	}
	if err != nil {
		svc.logger.Warn("Cannot persist baked ACME enrollment attempt")
		return
	}
	principal, _ := bake.Config.PrincipalURI()
	metadata, _ := json.Marshal(struct {
		PrincipalURI string `json:"principalURI"`
	}{principal})
	_, writeErr := f.Write(metadata)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		svc.logger.Warn("Cannot persist baked ACME enrollment attempt; enrollment not started")
		return
	}
	// fsync the directory too, so a lost directory entry cannot permit a
	// duplicate automatic issuance after a power interruption.
	dir, err := os.Open(svc.configPath)
	if err != nil {
		svc.logger.Warn("Cannot persist baked ACME enrollment attempt directory")
		return
	}
	syncErr = dir.Sync()
	closeErr = dir.Close()
	if syncErr != nil || closeErr != nil {
		svc.logger.Warn("Cannot persist baked ACME enrollment attempt directory")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	_, err = s.StartACMEProvisioning(ctx, &agentpbv2.StartACMEProvisioningRequest{
		CloudHost: bake.CloudHost, DirectoryUrl: bake.DirectoryURL, DeviceId: bake.DeviceID,
		EabKeyId: bake.EABKeyID, EabHmacKey: bake.EABHMACKey,
	})
	if err != nil {
		// Upstream errors are not guaranteed secret-free. Report the stage,
		// retain all material, and require explicit recovery instead of a loop.
		svc.logger.Warn("Baked ACME enrollment failed; credential retained and automatic retry blocked", zap.String("principalURI", principal))
		return
	}
	if err := os.Remove(path); err != nil {
		svc.logger.Warn("Baked ACME enrollment completed but credential file could not be removed")
		return
	}
	svc.logger.Info("Baked ACME enrollment completed", zap.String("principalURI", principal))
}
