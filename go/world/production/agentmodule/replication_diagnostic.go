package agentmodule

import (
	"context"
	"errors"
	"log"
	"strings"

	"github.com/wendylabsinc/wendy/go/world/production/replicastore"
	"github.com/wendylabsinc/wendy/go/world/production/replication"
	"github.com/wendylabsinc/wendy/go/world/production/servicejournal"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/sourceauthority"
)

// Diagnostics retain only fixed classes, never an error's free-form text,
// credentials, source identity or observation bytes. They change no outcome.
func replicaFailureClass(err error) string {
	if err == nil {
		return "none"
	}
	var classes []string
	for _, c := range []struct {
		err  error
		name string
	}{
		{context.Canceled, "canceled"},
		{context.DeadlineExceeded, "deadline"},
		{ErrDenied, "manager_denied"},
		{replication.ErrDenied, "peer_denied"},
		{replication.ErrUncertain, "uncertain"},
		{replication.ErrRecordWindowDeferred, "record_window_deferred"},
		{replicastore.ErrFaulted, "replica_store_faulted"},
		{replicastore.ErrDenied, "replica_store_denied"},
		{servicejournal.ErrFaulted, "journal_faulted"},
		{servicejournal.ErrEpoch, "journal_epoch"},
		{serviceorigin.ErrExpired, "record_expired"},
		{serviceorigin.ErrDenied, "record_denied"},
		{sourceauthority.ErrFaulted, "source_authority_faulted"},
		{sourceauthority.ErrExpired, "source_authority_expired"},
		{sourceauthority.ErrReplay, "source_authority_replay"},
		{sourceauthority.ErrClock, "source_authority_clock"},
		{sourceauthority.ErrDenied, "source_authority_denied"},
	} {
		if errors.Is(err, c.err) {
			classes = append(classes, c.name)
		}
	}
	if len(classes) == 0 {
		return "unclassified"
	}
	return strings.Join(classes, ",")
}

func observeReplicaAdmissionFailure(kind replication.Kind, finalChecks int, err error) {
	log.Printf("World replica admission failed: kind=%d final_checks=%d failure_class=%s", kind, finalChecks, replicaFailureClass(err))
}
