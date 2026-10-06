package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
	"github.com/wendylabsinc/wendy/go/internal/shared/flock"
)

const gatewayMCPEventTTL = 24 * time.Hour

type gatewayMCPEventError struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data,omitempty"`
}

type gatewayMCPEventDelivery struct {
	Mode   string `json:"mode"`
	URL    string `json:"url"`
	Secret string `json:"secret,omitempty"`
}

type gatewayMCPEventRequest struct {
	Name      string                  `json:"name"`
	Arguments json.RawMessage         `json:"arguments"`
	Delivery  gatewayMCPEventDelivery `json:"delivery"`
	Cursor    *string                 `json:"cursor,omitempty"`
	TTL       json.RawMessage         `json:"ttlMs,omitempty"`
	Meta      json.RawMessage         `json:"_meta,omitempty"`
}

type gatewayMCPEventEnvelope struct {
	ID        string         `json:"eventId"`
	Name      string         `json:"name"`
	Timestamp string         `json:"timestamp"`
	Data      map[string]any `json:"data"`
	Cursor    string         `json:"cursor"`
}

type gatewayMCPEventPending struct {
	ID          string          `json:"id"`
	Cursor      string          `json:"cursor"`
	Body        json.RawMessage `json:"body"`
	Attempts    int             `json:"attempts,omitempty"`
	NextAttempt time.Time       `json:"next_attempt,omitempty"`
}

// Tokens and webhook secrets are retained only in this mode-0600 durable
// record. They never appear in tool results, event payloads, or diagnostics.
type gatewayMCPEventSubscription struct {
	ID          string                   `json:"id"`
	Owner       string                   `json:"owner"`
	Credential  string                   `json:"credential,omitempty"`
	Local       bool                     `json:"local"`
	Name        string                   `json:"name"`
	Arguments   json.RawMessage          `json:"arguments"`
	URL         string                   `json:"url"`
	Secret      string                   `json:"secret"`
	OldSecret   string                   `json:"old_secret,omitempty"`
	RotateUntil time.Time                `json:"rotate_until,omitempty"`
	Expires     time.Time                `json:"expires"`
	Cursor      string                   `json:"cursor"`
	BatchCursor string                   `json:"batch_cursor,omitempty"`
	Pending     []gatewayMCPEventPending `json:"pending,omitempty"`
	Truncated   bool                     `json:"truncated,omitempty"`
	Stopped     string                   `json:"stopped,omitempty"`
}

type gatewayMCPEventEntry struct {
	mu      sync.Mutex
	state   gatewayMCPEventSubscription
	running bool // protected by manager.mu
	wake    chan struct{}
}

type gatewayMCPEventManager struct {
	gateway  *RobotGateway
	path     string
	client   *http.Client
	mu       sync.Mutex
	updates  sync.Mutex
	entries  map[string]*gatewayMCPEventEntry
	verified map[string]time.Time
	ctx      context.Context
	cancel   context.CancelFunc
	release  func()
	workers  sync.WaitGroup
	slots    chan struct{}
	now      func() time.Time
	read     func(context.Context, string, json.RawMessage, string, bool) (gatewayMCPEventBatch, error)
}

func (g *RobotGateway) initMCPEvents() error {
	base := g.cfg.StateDirectory
	if base == "" {
		directory, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		base = filepath.Join(directory, "wendy", "robot-gateway")
	}
	routes := []string{}
	for _, robot := range g.cfg.Robots {
		routes = append(routes, robot.ID+"="+robot.Device)
	}
	for _, source := range g.cfg.CloudSources {
		routes = append(routes, fmt.Sprintf("%s=%s/%d/%s", source.ID, source.Endpoint, source.OrganizationID, source.TenantUUID))
	}
	sort.Strings(routes)
	routeJSON, _ := json.Marshal(routes)
	namespace := sha256.Sum256(routeJSON)
	m := &gatewayMCPEventManager{gateway: g, path: filepath.Join(base, "mcp-events", fmt.Sprintf("%x", namespace[:16])), client: gatewayMCPWebhookClient(), entries: map[string]*gatewayMCPEventEntry{}, verified: map[string]time.Time{}, slots: make(chan struct{}, 4), now: time.Now, read: g.readMCPEventSource}
	files, err := os.ReadDir(m.path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read MCP event storage: %w", err)
	}
	for _, file := range files {
		if !strings.HasPrefix(file.Name(), "sub_") || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		if len(m.entries) >= 128 {
			return fmt.Errorf("MCP event subscription storage exceeds limit")
		}
		fullPath := filepath.Join(m.path, file.Name())
		info, err := os.Lstat(fullPath)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4<<20 {
			return fmt.Errorf("MCP event storage must contain private regular files")
		}
		raw, err := os.ReadFile(fullPath)
		if err != nil {
			return fmt.Errorf("read MCP event subscription")
		}
		var subscription gatewayMCPEventSubscription
		if json.Unmarshal(raw, &subscription) != nil || subscription.ID+".json" != file.Name() || !gatewayMCPStoredSubscriptionValid(subscription) {
			return fmt.Errorf("invalid stored MCP event subscription")
		}
		m.entries[subscription.ID] = &gatewayMCPEventEntry{state: subscription, wake: make(chan struct{}, 1)}
	}
	g.mcpEvents = m
	return nil
}

func gatewayMCPStoredSubscriptionValid(subscription gatewayMCPEventSubscription) bool {
	if subscription.Owner == "" || subscription.Expires.IsZero() || len(subscription.Pending) > 512 || (subscription.Local && subscription.Credential != "") || (!subscription.Local && subscription.Credential == "") {
		return false
	}
	if _, err := gatewayMCPParseArguments(subscription.Name, subscription.Arguments); err != nil {
		return false
	}
	if url, err := gatewayMCPCallbackURL(subscription.URL); err != nil || url != subscription.URL {
		return false
	}
	if _, err := gatewayMCPWebhookKey(subscription.Secret); err != nil {
		return false
	}
	if subscription.OldSecret != "" {
		if _, err := gatewayMCPWebhookKey(subscription.OldSecret); err != nil {
			return false
		}
	}
	return subscription.ID == gatewayMCPSubscriptionID(subscription.Owner, subscription.URL, subscription.Name, subscription.Arguments)
}

func gatewayMCPSubscriptionID(owner, callback, name string, args json.RawMessage) string {
	identity, _ := json.Marshal([]any{owner, callback, name, args})
	digest := sha256.Sum256(identity)
	return fmt.Sprintf("sub_%x", digest[:])
}

func (m *gatewayMCPEventManager) start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx != nil {
		return nil
	}
	if len(m.entries) > 0 {
		if err := m.lockLocked(); err != nil {
			return err
		}
	}
	m.ctx, m.cancel = context.WithCancel(ctx)
	for _, entry := range m.entries {
		m.startEntryLocked(entry)
	}
	return nil
}

func (m *gatewayMCPEventManager) lockLocked() error {
	if m.release != nil {
		return nil
	}
	if err := os.MkdirAll(m.path, 0700); err != nil {
		return err
	}
	release, err := flock.Acquire(filepath.Join(m.path, ".lock"), 0)
	if err != nil {
		return fmt.Errorf("MCP event storage is already in use")
	}
	m.release = release
	return nil
}

func (m *gatewayMCPEventManager) close() {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()
	m.workers.Wait()
	m.mu.Lock()
	if m.release != nil {
		m.release()
		m.release = nil
	}
	m.mu.Unlock()
}

func (m *gatewayMCPEventManager) startEntryLocked(entry *gatewayMCPEventEntry) {
	if m.ctx == nil || entry.running {
		return
	}
	entry.running = true
	m.workers.Add(1)
	go func() {
		defer m.workers.Done()
		defer func() { m.mu.Lock(); entry.running = false; m.mu.Unlock() }()
		for {
			entry.mu.Lock()
			unsubscribed := entry.state.Stopped == "unsubscribed"
			idle := entry.state.Stopped != "" || !m.now().Before(entry.state.Expires)
			entry.mu.Unlock()
			if unsubscribed {
				return
			}
			if idle {
				select {
				case <-m.ctx.Done():
					return
				case <-entry.wake:
					continue
				case <-time.After(time.Hour):
					m.expireRetained(entry)
					continue
				}
			}
			select {
			case <-m.ctx.Done():
				return
			case m.slots <- struct{}{}:
			}
			m.step(m.ctx, entry)
			<-m.slots
			select {
			case <-m.ctx.Done():
				return
			case <-time.After(2 * time.Second):
			case <-entry.wake:
			}
		}
	}()
}

// Retain an expired outbox for a bounded replay window, then remove its bearer
// token and signing keys. Unacknowledged notifications can still be replayed
// from the device's retained journal using the client's last cursor.
func (m *gatewayMCPEventManager) expireRetained(entry *gatewayMCPEventEntry) {
	m.updates.Lock()
	defer m.updates.Unlock()
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.state.ID == "" || m.now().Before(entry.state.Expires.Add(7*24*time.Hour)) {
		return
	}
	if err := os.Remove(filepath.Join(m.path, entry.state.ID+".json")); err != nil && !os.IsNotExist(err) {
		return
	}
	entry.state.Stopped = "unsubscribed"
	entry.state.Credential = ""
	entry.state.Secret = ""
	entry.state.OldSecret = ""
	entry.state.Pending = nil
	m.mu.Lock()
	delete(m.entries, entry.state.ID)
	m.mu.Unlock()
}

func (m *gatewayMCPEventManager) save(subscription gatewayMCPEventSubscription) error {
	if err := os.MkdirAll(m.path, 0700); err != nil {
		return err
	}
	if info, err := os.Lstat(m.path); err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("MCP event directory must be private")
	}
	raw, err := json.Marshal(subscription)
	if err != nil || len(raw) > 4<<20 {
		return fmt.Errorf("MCP event storage exceeds limit")
	}
	return atomicfile.Write(filepath.Join(m.path, subscription.ID+".json"), raw, 0600)
}

func (g *RobotGateway) handleMCPEventRequest(ctx context.Context, method string, raw json.RawMessage) (any, *gatewayMCPEventError) {
	fail := func(code int, message string) (any, *gatewayMCPEventError) {
		return nil, &gatewayMCPEventError{Code: code, Message: message}
	}
	if !g.hasScope(ctx, RobotEventsScope) {
		return fail(-32001, "Device event access is not authorized")
	}
	if g.mcpEvents == nil {
		return fail(-32601, "MCP events are unavailable")
	}
	if method == "events/list" {
		if len(raw) == 0 {
			raw = json.RawMessage(`{}`)
		}
		var params struct {
			Cursor string          `json:"cursor"`
			Meta   json.RawMessage `json:"_meta,omitempty"`
		}
		if gatewayMCPDecode(raw, &params) != nil || params.Cursor != "" {
			return fail(-32602, "Invalid events catalog cursor")
		}
		return map[string]any{"events": g.mcpEventDefinitions(ctx)}, nil
	}
	if method != "events/subscribe" && method != "events/unsubscribe" {
		return fail(-32601, "Unknown event method")
	}
	var request gatewayMCPEventRequest
	if gatewayMCPDecode(raw, &request) != nil || request.Delivery.Mode != "webhook" {
		return fail(-32602, "Invalid webhook subscription")
	}
	args, err := gatewayMCPParseArguments(request.Name, request.Arguments)
	if err != nil {
		return fail(-32602, err.Error())
	}
	request.Arguments, _ = json.Marshal(args)
	callback, err := gatewayMCPCallbackURL(request.Delivery.URL)
	if err != nil {
		return nil, &gatewayMCPEventError{Code: -32015, Message: "Invalid callback endpoint", Data: map[string]any{"reason": "invalid_url"}}
	}
	request.Delivery.URL = callback
	if err = g.authorizeMCPEvent(ctx, request.Name, request.Arguments); err != nil {
		return fail(-32001, "Device event access is not authorized")
	}
	_, principal := g.grant(ctx)
	id := gatewayMCPSubscriptionID(principal.Subject, callback, request.Name, request.Arguments)
	if method == "events/unsubscribe" {
		return g.mcpEvents.unsubscribe(id)
	}
	if _, err = gatewayMCPWebhookKey(request.Delivery.Secret); err != nil {
		return fail(-32602, err.Error())
	}
	if request.Cursor != nil && len(*request.Cursor) > 100 {
		return fail(-32602, "Invalid event cursor")
	}
	ttl := gatewayMCPEventTTL
	if len(request.TTL) > 0 && string(request.TTL) != "null" {
		var milliseconds int64
		if json.Unmarshal(request.TTL, &milliseconds) != nil || milliseconds <= 0 {
			return fail(-32602, "ttlMs must be a positive integer or null")
		}
		if milliseconds < int64(ttl/time.Millisecond) {
			ttl = time.Duration(milliseconds) * time.Millisecond
		}
	}
	return g.mcpEvents.subscribe(ctx, id, principal.Subject, request, ttl)
}

func (m *gatewayMCPEventManager) unsubscribe(id string) (any, *gatewayMCPEventError) {
	m.updates.Lock()
	defer m.updates.Unlock()
	m.mu.Lock()
	entry := m.entries[id]
	m.mu.Unlock()
	if entry == nil {
		return map[string]any{}, nil
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	// Wait for any in-flight delivery to finish before acknowledging stop.
	entry.state.Stopped = "unsubscribed"
	entry.state.Pending = nil
	entry.state.Secret = ""
	entry.state.OldSecret = ""
	entry.state.Credential = ""
	select {
	case entry.wake <- struct{}{}:
	default:
	}
	if err := os.Remove(filepath.Join(m.path, id+".json")); err != nil && !os.IsNotExist(err) {
		return nil, &gatewayMCPEventError{Code: -32603, Message: "Could not remove subscription"}
	}
	m.mu.Lock()
	delete(m.entries, id)
	m.mu.Unlock()
	return map[string]any{}, nil
}

func (m *gatewayMCPEventManager) subscribe(ctx context.Context, id, owner string, request gatewayMCPEventRequest, ttl time.Duration) (any, *gatewayMCPEventError) {
	m.updates.Lock()
	defer m.updates.Unlock()
	fail := func(message string) (any, *gatewayMCPEventError) {
		return nil, &gatewayMCPEventError{Code: -32000, Message: message}
	}
	local := ctx.Value(gatewayLocalContextKey{}) == true
	credential, _ := ctx.Value(gatewayEventCredentialKey{}).(string)
	if (!local && credential == "") || (local && owner != m.gateway.cfg.LocalSubject) {
		return fail("Subscription has no renewable account authorization")
	}
	m.mu.Lock()
	if err := m.lockLocked(); err != nil {
		m.mu.Unlock()
		return fail(err.Error())
	}
	entry := m.entries[id]
	if entry == nil {
		if len(m.entries) >= 128 {
			// Expired empty subscriptions have no undelivered data to retain.
			for oldID, old := range m.entries {
				old.mu.Lock()
				if !m.now().Before(old.state.Expires) && (len(old.state.Pending) == 0 || !m.now().Before(old.state.Expires.Add(7*24*time.Hour))) {
					if err := os.Remove(filepath.Join(m.path, oldID+".json")); err == nil || os.IsNotExist(err) {
						old.state.Stopped = "unsubscribed"
						select {
						case old.wake <- struct{}{}:
						default:
						}
						delete(m.entries, oldID)
					}
				}
				old.mu.Unlock()
			}
			if len(m.entries) >= 128 {
				m.mu.Unlock()
				return fail("Subscription limit reached")
			}
		}
		entry = &gatewayMCPEventEntry{wake: make(chan struct{}, 1)}
		m.entries[id] = entry
	}
	m.mu.Unlock()
	entry.mu.Lock()
	defer entry.mu.Unlock()
	accepted := false
	defer func() {
		if !accepted && entry.state.ID == "" {
			m.mu.Lock()
			delete(m.entries, id)
			m.mu.Unlock()
		}
	}()
	now := m.now()
	verifyKey := gatewayMCPSubscriptionID(owner, request.Delivery.URL, request.Delivery.Secret, nil)
	m.mu.Lock()
	verifiedUntil := m.verified[verifyKey]
	m.mu.Unlock()
	if !now.Before(verifiedUntil) {
		if err := gatewayMCPVerifyCallback(ctx, m.client, request.Delivery.URL, id, request.Delivery.Secret, now); err != nil {
			reason := "challenge_failed"
			var callbackErr *gatewayMCPWebhookFailure
			if errors.As(err, &callbackErr) {
				reason = callbackErr.reason
			}
			return nil, &gatewayMCPEventError{Code: -32015, Message: "Callback verification failed", Data: map[string]any{"reason": reason}}
		}
		m.mu.Lock()
		for key, until := range m.verified {
			if !now.Before(until) {
				delete(m.verified, key)
			}
		}
		m.verified[verifyKey] = now.Add(5 * time.Minute)
		m.mu.Unlock()
	}
	next := entry.state
	next.Pending = append([]gatewayMCPEventPending(nil), next.Pending...)
	if next.ID == "" {
		next = gatewayMCPEventSubscription{ID: id, Owner: owner, Name: request.Name, Arguments: request.Arguments, URL: request.Delivery.URL}
		cursor := ""
		if request.Cursor != nil {
			cursor = *request.Cursor
		}
		batch, err := m.read(ctx, request.Name, request.Arguments, cursor, cursor != "")
		if err != nil {
			return fail(err.Error())
		}
		next.Cursor = cursor
		if err = m.enqueue(&next, batch); err != nil {
			return fail("Invalid notification batch")
		}
	} else if !now.Before(next.Expires) && len(next.Pending) == 0 && request.Cursor != nil {
		batch, err := m.read(ctx, request.Name, request.Arguments, *request.Cursor, true)
		if err != nil {
			return fail(err.Error())
		}
		next.Cursor = *request.Cursor
		if err = m.enqueue(&next, batch); err != nil {
			return fail("Invalid notification batch")
		}
	}
	if next.Secret != "" && next.Secret != request.Delivery.Secret {
		next.OldSecret = next.Secret
		next.RotateUntil = now.Add(5 * time.Minute)
	}
	next.Secret = request.Delivery.Secret
	next.Local, next.Credential = local, credential
	next.Expires = now.Add(ttl)
	next.Stopped = ""
	for i := range next.Pending {
		next.Pending[i].Attempts = 0
		next.Pending[i].NextAttempt = time.Time{}
	}
	if err := m.save(next); err != nil {
		return fail("Could not persist subscription")
	}
	entry.state = next
	accepted = true
	m.mu.Lock()
	m.startEntryLocked(entry)
	m.mu.Unlock()
	select {
	case entry.wake <- struct{}{}:
	default:
	}
	return map[string]any{"id": id, "refreshBefore": next.Expires.UTC().Format(time.RFC3339Nano), "cursor": next.Cursor, "truncated": next.Truncated}, nil
}

func (m *gatewayMCPEventManager) authorization(ctx context.Context, subscription gatewayMCPEventSubscription) (context.Context, error) {
	var principal gatewayPrincipal
	if subscription.Local {
		if subscription.Owner != m.gateway.cfg.LocalSubject || subscription.Credential != "" {
			return nil, fmt.Errorf("local event authorization revoked")
		}
		principal = gatewayPrincipal{Subject: subscription.Owner, Scopes: robotGatewayScopes}
		ctx = context.WithValue(ctx, gatewayLocalContextKey{}, true)
	} else {
		if m.gateway.eventAuthenticate == nil {
			return nil, fmt.Errorf("event account authorization unavailable")
		}
		var err error
		principal, err = m.gateway.eventAuthenticate(ctx, subscription.Credential)
		if err != nil || principal.Subject != subscription.Owner {
			return nil, fmt.Errorf("event account authorization revoked")
		}
	}
	ctx = context.WithValue(ctx, gatewayPrincipalKey{}, principal)
	if err := m.gateway.authorizeMCPEvent(ctx, subscription.Name, subscription.Arguments); err != nil {
		return nil, err
	}
	return ctx, nil
}

func (m *gatewayMCPEventManager) enqueue(subscription *gatewayMCPEventSubscription, batch gatewayMCPEventBatch) error {
	if len(batch.Events) > 512 {
		return fmt.Errorf("notification batch exceeds limit")
	}
	for _, event := range batch.Events {
		body, err := json.Marshal(gatewayMCPEventEnvelope{ID: event.ID, Name: subscription.Name, Timestamp: event.Timestamp, Data: event.Data, Cursor: event.Cursor})
		if err != nil || len(body) > gatewayMCPEventBodyLimit || event.ID == "" || event.Cursor == "" {
			return fmt.Errorf("invalid notification payload")
		}
		subscription.Pending = append(subscription.Pending, gatewayMCPEventPending{ID: event.ID, Cursor: event.Cursor, Body: body})
	}
	subscription.BatchCursor = batch.Cursor
	subscription.Truncated = subscription.Truncated || batch.Truncated
	if len(subscription.Pending) == 0 {
		subscription.Cursor = batch.Cursor
		subscription.BatchCursor = ""
	}
	return nil
}

// step commits an outbox before sending, then advances only acknowledged
// cursors. A crash after a 2xx but before the commit repeats the same event ID.
func (m *gatewayMCPEventManager) step(parent context.Context, entry *gatewayMCPEventEntry) {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	subscription := &entry.state
	now := m.now()
	if subscription.ID == "" || subscription.Stopped != "" || !now.Before(subscription.Expires) {
		return
	}
	deadline := now.Add(30 * time.Second)
	if subscription.Expires.Before(deadline) {
		deadline = subscription.Expires
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	authorized, err := m.authorization(ctx, *subscription)
	if err != nil {
		// Retry authorization later without reading or delivering. A temporary
		// identity-provider outage must not lose the durable subscription.
		return
	}
	if len(subscription.Pending) == 0 {
		batch, err := m.read(authorized, subscription.Name, subscription.Arguments, subscription.Cursor, true)
		if err != nil {
			return
		}
		if err = m.enqueue(subscription, batch); err != nil {
			subscription.Stopped = "invalid_source"
			_ = m.save(*subscription)
			return
		}
		if err = m.save(*subscription); err != nil {
			subscription.Stopped = "storage_unavailable"
			return
		}
	}
	if len(subscription.Pending) == 0 {
		return
	}
	head := &subscription.Pending[0]
	if now.Before(head.NextAttempt) {
		return
	}
	// A source read may take seconds. Recheck access immediately before sending.
	if _, err = m.authorization(ctx, *subscription); err != nil {
		return
	}
	if !m.now().Before(subscription.Expires) || ctx.Err() != nil {
		return
	}
	oldSecret := subscription.OldSecret
	if !now.Before(subscription.RotateUntil) {
		oldSecret = ""
		subscription.OldSecret = ""
	}
	response, err := gatewayMCPPost(ctx, m.client, subscription.URL, subscription.ID, subscription.Secret, oldSecret, head.ID, head.Body, m.now())
	status := 0
	if response != nil {
		status = response.StatusCode
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		response.Body.Close()
	}
	if err == nil && status >= 200 && status < 300 {
		subscription.Cursor = head.Cursor
		subscription.Pending = subscription.Pending[1:]
		if len(subscription.Pending) == 0 {
			subscription.Cursor = subscription.BatchCursor
			subscription.BatchCursor = ""
		}
	} else {
		head.Attempts++
		if status == 410 || status == 413 || (status >= 300 && status < 500 && status != 408 && status != 429) || head.Attempts >= 8 {
			subscription.Stopped = "delivery_rejected"
		} else {
			head.NextAttempt = m.now().Add(time.Duration(1<<min(head.Attempts, 8)) * time.Second)
		}
	}
	if err = m.save(*subscription); err != nil {
		subscription.Stopped = "storage_unavailable"
	}
}
