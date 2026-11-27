package tasking

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/supervision"
)

const maxSupervisionProof = supervision.MaxVerificationProofBytes

var (
	ErrSupervisionUnavailable = errors.New("supervision unavailable for this task or executor")
	ErrSupervisionExpired     = errors.New("supervision challenge expired")
	ErrSupervisionMismatch    = errors.New("supervision acknowledgment does not match an outstanding challenge")
	ErrSupervisionLimit       = errors.New("supervision registry or challenge rate limit reached")
)

type supervisionKey struct{ task, principal string }
type supervisionSlot struct {
	issued     supervision.Challenge
	issuedMono time.Duration
	deadline   time.Duration
	pending    bool
	active     *activeSupervision
}
type activeSupervision struct {
	challenge supervision.Challenge
	deadline  time.Duration
	expires   int64
	proof     json.RawMessage
}

// SpecDigest identifies the exact canonical Spec used in durable admission.
// Invalid JSON values, including nonfinite coordinates, return an empty digest.
func SpecDigest(spec Spec) string {
	d, err := digest(spec)
	if err != nil {
		return ""
	}
	return hex.EncodeToString(d[:])
}
func nonempty(s string, max int) bool {
	if s == "" || len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, c := range s {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}
func requestNonce(s string) bool {
	if len(s) < 16 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func sha256String(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && hex.EncodeToString(b) == s
}
func configureSupervision(c *Config) error {
	if c.ValidateSupervisor == nil {
		if c.ValidateSupervisorsContext != nil || c.Tenant != "" || c.Audience != "" || c.GatewayPrincipal != "" || c.MaxSupervisionLease != 0 || c.MaxSupervisors != 0 || c.MaxPendingSupervision != 0 {
			return errors.New("complete supervision verifier and scope required")
		}
		return nil
	}
	if !nonempty(c.Tenant, 256) || !nonempty(c.Site, 256) || !nonempty(c.Audience, 512) || !nonempty(c.GatewayPrincipal, 512) || !nonempty(c.RunID, 256) || c.Domain != mission.Simulation {
		return errors.New("supervision requires explicit simulation scope")
	}
	if c.MaxSupervisionLease == 0 {
		c.MaxSupervisionLease = min(c.MaxLease, supervision.MaxLease)
	}
	if c.MaxSupervisors == 0 {
		c.MaxSupervisors = 8
	}
	if c.MaxPendingSupervision == 0 {
		c.MaxPendingSupervision = 64
	}
	if c.MaxSupervisionLease < time.Millisecond || c.MaxSupervisionLease > supervision.MaxLease || c.MaxSupervisionLease > c.MaxLease || c.MaxSupervisors < 1 || c.MaxSupervisors > 32 || c.MaxPendingSupervision < 1 || c.MaxPendingSupervision > 256 {
		return errors.New("invalid bounded supervision configuration")
	}
	return nil
}
func (s *Store) supervisionEnabled() bool { return s.cfg.ValidateSupervisor != nil }
func operatorMatches(o supervision.Operator, c supervision.Challenge) bool {
	return o.Principal == c.OperatorPrincipal && o.KeySHA256 == c.OperatorKeySHA256 && o.GrantID == c.OperatorGrantID
}
func validOperator(o supervision.Operator, wall int64) bool {
	return nonempty(o.Principal, 512) && sha256String(o.KeySHA256) && nonempty(o.GrantID, 256) && o.ValidUntilUnixMS > wall
}
func (s *Store) supervisedTask(id string, wall int64) (savedRecord, error) {
	r, ok := s.state.Records[id]
	if !s.supervisionEnabled() || !ok || s.state.ExecutorEpoch == 0 || r.Record.Spec.Mode != mission.Supervised || (r.Record.Status.Phase != mission.Accepted && r.Record.Status.Phase != mission.Running) || r.Record.Spec.NotAfterUnixMS <= wall || r.Record.AuthorizationUntilUnixMS <= wall {
		return savedRecord{}, ErrSupervisionUnavailable
	}
	if _, stopped := s.state.Stops[id]; stopped {
		return savedRecord{}, ErrSupervisionUnavailable
	}
	return r, nil
}
func (s *Store) pruneSupervision(wall int64, mono time.Duration) {
	for key, slot := range s.supervision {
		if _, err := s.supervisedTask(key.task, wall); err != nil {
			delete(s.supervision, key)
			continue
		}
		if slot.active != nil && (mono >= slot.active.deadline || wall >= slot.active.expires) {
			slot.active = nil
		}
		if mono >= slot.deadline || wall >= slot.issued.ExpiresAtUnixMS {
			slot.pending = false
		}
		if !slot.pending && slot.active == nil && mono >= slot.deadline {
			delete(s.supervision, key)
		}
	}
}

// SupervisionChallenge accepts only an Operator produced by a trusted verifier.
// It is not a network input type. Retrying the same request returns the original
// issued challenge while its bounded slot is retained and never moves that
// challenge deadline. After expiry/pruning, a fresh request may reuse its ID
// but receives a new random challenge; the old ACK can never activate it.
func (s *Store) SupervisionChallenge(taskID, taskDigest, requestID string, operator supervision.Operator) (supervision.Challenge, error) {
	return s.SupervisionChallengeContext(context.Background(), taskID, taskDigest, requestID, operator)
}
func (s *Store) SupervisionChallengeContext(ctx context.Context, taskID, taskDigest, requestID string, operator supervision.Operator) (supervision.Challenge, error) {
	hold := s.lockContext(ctx)
	defer s.mu.Unlock()
	defer hold.End()
	wall, mono, err := s.clock()
	if err != nil {
		return supervision.Challenge{}, err
	}
	r, err := s.supervisedTask(taskID, wall)
	if err != nil {
		return supervision.Challenge{}, err
	}
	if taskDigest != hex.EncodeToString(r.Digest[:]) || !sha256String(taskDigest) || !requestNonce(requestID) || !validOperator(operator, wall) {
		return supervision.Challenge{}, ErrSupervisionMismatch
	}
	if s.cfg.ValidateAuthority != nil {
		if err = s.validateAuthority(ctx, copyRecord(r.Record)); err != nil {
			return supervision.Challenge{}, err
		}
	}
	wall, mono, err = s.clock()
	if err != nil {
		return supervision.Challenge{}, err
	}
	if _, err = s.supervisedTask(taskID, wall); err != nil || !validOperator(operator, wall) {
		return supervision.Challenge{}, ErrSupervisionExpired
	}
	if err = s.requireContinuousSupervisionContext(ctx, taskID); err != nil {
		return supervision.Challenge{}, err
	}
	wall, mono, err = s.clock()
	if err != nil {
		return supervision.Challenge{}, err
	}
	if err = s.enforceSupervisionContinuityAtContext(ctx, taskID, wall, mono); err != nil {
		return supervision.Challenge{}, err
	}
	s.pruneSupervision(wall, mono)
	key := supervisionKey{taskID, operator.Principal}
	if slot := s.supervision[key]; slot != nil && slot.issued.RequestID == requestID {
		if !operatorMatches(operator, slot.issued) {
			return supervision.Challenge{}, ErrSupervisionMismatch
		}
		if mono >= slot.deadline || wall >= slot.issued.ExpiresAtUnixMS {
			return supervision.Challenge{}, ErrSupervisionExpired
		}
		return slot.issued, nil
	}
	principals := map[string]bool{}
	perOperator := 0
	interval := min(50*time.Millisecond, s.cfg.MaxSupervisionLease/4)
	for k, slot := range s.supervision {
		principals[k.principal] = true
		if k.principal == operator.Principal {
			perOperator++
			if mono-slot.issuedMono < interval {
				return supervision.Challenge{}, ErrSupervisionLimit
			}
		}
	}
	if s.supervision[key] == nil && (len(s.supervision) >= s.cfg.MaxPendingSupervision || perOperator >= 4 || (!principals[operator.Principal] && len(principals) >= s.cfg.MaxSupervisors)) {
		return supervision.Challenge{}, ErrSupervisionLimit
	}
	expires := min(wall+s.cfg.MaxSupervisionLease.Milliseconds(), r.Record.Spec.NotAfterUnixMS, r.Record.AuthorizationUntilUnixMS, operator.ValidUntilUnixMS)
	until, err := deadline(expires, wall, mono)
	if err != nil {
		return supervision.Challenge{}, ErrSupervisionExpired
	}
	nonce := make([]byte, 32)
	if _, err = rand.Read(nonce); err != nil {
		return supervision.Challenge{}, err
	}
	challenge := supervision.Challenge{Version: supervision.Version, Tenant: s.cfg.Tenant, Site: s.cfg.Site, Domain: string(s.cfg.Domain), RunID: s.cfg.RunID, Audience: s.cfg.Audience, Assignee: s.cfg.Assignee, GatewayPrincipal: s.cfg.GatewayPrincipal, GatewayBootID: s.supervisionBoot, TaskID: taskID, SpecSHA256: taskDigest, ExecutorEpoch: s.state.ExecutorEpoch, OperatorPrincipal: operator.Principal, OperatorKeySHA256: operator.KeySHA256, OperatorGrantID: operator.GrantID, RequestID: requestID, ChallengeID: hex.EncodeToString(nonce), IssuedAtUnixMS: wall, ExpiresAtUnixMS: expires}
	slot := s.supervision[key]
	if slot == nil {
		slot = &supervisionSlot{}
		s.supervision[key] = slot
	}
	slot.issued = challenge
	slot.issuedMono = mono
	slot.deadline = until
	slot.pending = true
	return challenge, nil
}

// AcknowledgeSupervision activates only the exact verified outstanding challenge.
// A successful receipt is volatile presence evidence, never a motion command.
func (s *Store) AcknowledgeSupervision(proof json.RawMessage) (supervision.Receipt, error) {
	return s.AcknowledgeSupervisionContext(context.Background(), proof)
}
func (s *Store) AcknowledgeSupervisionContext(ctx context.Context, proof json.RawMessage) (supervision.Receipt, error) {
	hold := s.lockContext(ctx)
	defer s.mu.Unlock()
	defer hold.End()
	if !s.supervisionEnabled() || len(proof) == 0 || len(proof) > maxSupervisionProof || !json.Valid(proof) {
		return supervision.Receipt{}, ErrSupervisionMismatch
	}
	if _, _, err := s.clock(); err != nil {
		return supervision.Receipt{}, err
	}
	ack, operator, err := s.validateSupervisor(ctx, append(json.RawMessage(nil), proof...))
	if err != nil {
		return supervision.Receipt{}, err
	}
	wall, mono, err := s.clock()
	if err != nil {
		return supervision.Receipt{}, err
	}
	c := ack.Challenge
	r, err := s.supervisedTask(c.TaskID, wall)
	if err != nil {
		return supervision.Receipt{}, err
	}
	if !validOperator(operator, wall) || !operatorMatches(operator, c) || len(operator.Proof) == 0 || len(operator.Proof) > maxSupervisionProof || !json.Valid(operator.Proof) {
		return supervision.Receipt{}, ErrSupervisionMismatch
	}
	key := supervisionKey{c.TaskID, operator.Principal}
	slot := s.supervision[key]
	if slot == nil || !slot.pending || slot.issued != c || c.GatewayBootID != s.supervisionBoot || c.ExecutorEpoch != s.state.ExecutorEpoch {
		return supervision.Receipt{}, ErrSupervisionMismatch
	}
	if s.cfg.ValidateAuthority != nil {
		if err = s.validateAuthority(ctx, copyRecord(r.Record)); err != nil {
			return supervision.Receipt{}, err
		}
	}
	if err = s.requireContinuousSupervisionContext(ctx, c.TaskID); err != nil {
		return supervision.Receipt{}, err
	}
	wall, mono, err = s.clock()
	if err != nil {
		return supervision.Receipt{}, err
	}
	if err = s.enforceSupervisionContinuityAtContext(ctx, c.TaskID, wall, mono); err != nil {
		return supervision.Receipt{}, err
	}
	if _, err = s.supervisedTask(c.TaskID, wall); err != nil {
		return supervision.Receipt{}, err
	}
	if mono >= slot.deadline || wall >= c.ExpiresAtUnixMS || operator.ValidUntilUnixMS <= wall {
		slot.pending = false
		return supervision.Receipt{}, ErrSupervisionExpired
	}
	until, err := deadline(min(c.ExpiresAtUnixMS, operator.ValidUntilUnixMS, r.Record.Spec.NotAfterUnixMS, r.Record.AuthorizationUntilUnixMS), wall, mono)
	if err != nil {
		return supervision.Receipt{}, ErrSupervisionExpired
	}
	until = min(until, slot.deadline)
	slot.pending = false
	slot.active = &activeSupervision{challenge: c, deadline: until, expires: min(c.ExpiresAtUnixMS, operator.ValidUntilUnixMS, r.Record.Spec.NotAfterUnixMS, r.Record.AuthorizationUntilUnixMS), proof: append(json.RawMessage(nil), operator.Proof...)}
	hash := sha256.Sum256(operator.Proof)
	return supervision.Receipt{Challenge: c, AckSHA256: hex.EncodeToString(hash[:])}, nil
}

// supervisionDeadline rechecks current operator authority. It is called with
// s.mu held; failed operator validation removes only that operator's lease.
func (s *Store) supervisionDeadline(taskID string) (time.Duration, error) {
	return s.supervisionDeadlineContext(context.Background(), taskID)
}
func (s *Store) supervisionDeadlineContext(ctx context.Context, taskID string) (time.Duration, error) {
	if s.cfg.ValidateSupervisorsContext != nil {
		return s.supervisionBatchDeadlineContext(ctx, taskID)
	}
	wall, mono, err := s.clock()
	if err != nil {
		return 0, err
	}
	s.pruneSupervision(wall, mono)
	if !s.supervisionEnabled() {
		return 0, nil
	}
	for key, slot := range s.supervision {
		if key.task != taskID || slot.active == nil {
			continue
		}
		active := slot.active
		ack, operator, verifyErr := s.validateSupervisor(ctx, append(json.RawMessage(nil), active.proof...))
		wall, mono, err = s.clock()
		if err != nil {
			return 0, err
		}
		if verifyErr != nil || !validOperator(operator, wall) || ack.Challenge != active.challenge || !operatorMatches(operator, active.challenge) || active.challenge.GatewayBootID != s.supervisionBoot || active.challenge.ExecutorEpoch != s.state.ExecutorEpoch || mono >= active.deadline || wall >= active.expires {
			slot.active = nil
			continue
		}
		active.expires = min(active.expires, operator.ValidUntilUnixMS)
		until, e := deadline(active.expires, wall, mono)
		if e != nil {
			slot.active = nil
			continue
		}
		active.deadline = min(active.deadline, until)
	}
	wall, mono, err = s.clock()
	if err != nil {
		return 0, err
	}
	s.pruneSupervision(wall, mono)
	var until time.Duration
	for key, slot := range s.supervision {
		if key.task == taskID && slot.active != nil {
			until = max(until, slot.active.deadline)
		}
	}
	return until, nil
}

type SupervisorStatus struct {
	Principal       string `json:"principal"`
	KeySHA256       string `json:"key_sha256"`
	GrantID         string `json:"grant_id"`
	RemainingMS     int64  `json:"remaining_ms"`
	ExpiresAtUnixMS int64  `json:"expires_at_unix_ms"`
}
type SupervisionStatus struct {
	TaskID            string             `json:"task_id"`
	SpecSHA256        string             `json:"spec_sha256"`
	GatewayBootID     string             `json:"gateway_boot_id"`
	ExecutorEpoch     uint64             `json:"executor_epoch"`
	ObservedAtUnixMS  int64              `json:"observed_at_unix_ms"`
	PendingChallenges int                `json:"pending_challenges"`
	ActiveOperators   []SupervisorStatus `json:"active_operators"`
}

// SupervisionStatus reports a newly rechecked local observation, not a cached
// authorization capability. Any caller must separately authenticate read access.
func (s *Store) SupervisionStatus(taskID string) (SupervisionStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, _, err := s.clock(); err != nil {
		return SupervisionStatus{}, err
	}
	r, ok := s.state.Records[taskID]
	if !ok {
		return SupervisionStatus{}, errors.New("unknown task")
	}
	if err := s.requireContinuousSupervision(taskID); err != nil && !errors.Is(err, ErrSupervisionUnavailable) {
		return SupervisionStatus{}, err
	}
	if _, err := s.supervisionDeadline(taskID); err != nil {
		return SupervisionStatus{}, err
	}
	wall, mono, err := s.clock()
	if err != nil {
		return SupervisionStatus{}, err
	}
	s.pruneSupervision(wall, mono)
	out := SupervisionStatus{TaskID: taskID, SpecSHA256: hex.EncodeToString(r.Digest[:]), GatewayBootID: s.supervisionBoot, ExecutorEpoch: s.state.ExecutorEpoch, ObservedAtUnixMS: wall, ActiveOperators: []SupervisorStatus{}}
	for key, slot := range s.supervision {
		if key.task != taskID {
			continue
		}
		if slot.pending {
			out.PendingChallenges++
		}
		if slot.active != nil {
			c := slot.active.challenge
			out.ActiveOperators = append(out.ActiveOperators, SupervisorStatus{Principal: c.OperatorPrincipal, KeySHA256: c.OperatorKeySHA256, GrantID: c.OperatorGrantID, RemainingMS: (slot.active.deadline - mono).Milliseconds(), ExpiresAtUnixMS: slot.active.expires})
		}
	}
	sort.Slice(out.ActiveOperators, func(i, j int) bool { return out.ActiveOperators[i].Principal < out.ActiveOperators[j].Principal })
	return out, nil
}

// currentSupervisionBudget uses only leases reverified by the immediately
// preceding evaluation, while s.mu remains held. It never renews a deadline.
func (s *Store) currentSupervisionBudget(taskID string, wall int64, mono time.Duration) *SupervisionBudget {
	var chosen *SupervisionBudget
	for key, slot := range s.supervision {
		if key.task != taskID || slot.active == nil {
			continue
		}
		a := slot.active
		if a.challenge.GatewayBootID != s.supervisionBoot || a.challenge.ExecutorEpoch != s.state.ExecutorEpoch || a.deadline <= mono || a.expires <= wall {
			continue
		}
		remaining := min((a.deadline - mono).Milliseconds(), a.expires-wall)
		if remaining <= 0 || remaining > s.cfg.MaxSupervisionLease.Milliseconds() {
			continue
		}
		if chosen == nil || remaining > chosen.RemainingMS {
			chosen = &SupervisionBudget{RemainingMS: remaining, ExpiresAtUnixMS: a.expires, ExecutorEpoch: s.state.ExecutorEpoch, GatewayBootID: s.supervisionBoot}
		}
	}
	return chosen
}

// requireContinuousSupervision prevents renewal from bridging an expired or
// revoked gap between executor polls. Running task loss is a durable control
// transition; an accepted task is still allowed to acquire its first lease.
func (s *Store) requireContinuousSupervision(taskID string) error {
	return s.requireContinuousSupervisionContext(context.Background(), taskID)
}
func (s *Store) requireContinuousSupervisionContext(ctx context.Context, taskID string) error {
	r, ok := s.state.Records[taskID]
	if !ok || r.Record.Spec.Mode != mission.Supervised || r.Record.Status.Phase != mission.Running {
		return nil
	}
	if _, err := s.supervisionDeadlineContext(ctx, taskID); err != nil {
		return err
	}
	wall, mono, err := s.clock()
	if err != nil {
		return err
	}
	return s.enforceSupervisionContinuityAtContext(ctx, taskID, wall, mono)
}

// The final sampled clock at issue/activation must still be inside existing
// coverage. Do not use the earlier proof-verification sample for this check.
func (s *Store) enforceSupervisionContinuityAt(taskID string, wall int64, mono time.Duration) error {
	return s.enforceSupervisionContinuityAtContext(context.Background(), taskID, wall, mono)
}
func (s *Store) enforceSupervisionContinuityAtContext(ctx context.Context, taskID string, wall int64, mono time.Duration) error {
	r, ok := s.state.Records[taskID]
	if !ok || r.Record.Spec.Mode != mission.Supervised || r.Record.Status.Phase != mission.Running {
		return nil
	}
	if s.currentSupervisionBudget(taskID, wall, mono) != nil {
		return nil
	}
	en := s.engine.Clone()
	decision, err := en.LoseSupervision(taskID)
	if err != nil {
		return err
	}
	st := cloneState(s.state)
	r.Record.Status = decision.Status
	st.Records[taskID] = r
	s.appendAudit(&st, taskID, "system", "supervision continuity lost", decision.Status, nil)
	if err = s.commitContext(ctx, en, st); err != nil {
		return err
	}
	for key := range s.supervision {
		if key.task == taskID {
			delete(s.supervision, key)
		}
	}
	return ErrSupervisionUnavailable
}
