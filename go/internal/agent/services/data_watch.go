package services

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
)

// documentLeases holds leased watches' deadlines. They live in memory only:
// a leased watch without a deadline counts as expired, and the boot sweep
// removes every leased watch of a previous run (spec §6.3). The zero value is
// ready to use.
type documentLeases struct {
	mu sync.Mutex
	// now is the clock; nil means time.Now. Tests set a fake one before
	// StartCampaignInference.
	now       func() time.Time
	deadlines map[string]time.Time
}

func (l *documentLeases) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// extend sets name's deadline to now + lease and returns it.
func (l *documentLeases) extend(name string, lease time.Duration) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.deadlines == nil {
		l.deadlines = map[string]time.Time{}
	}
	deadline := l.clock().Add(lease)
	l.deadlines[name] = deadline
	return deadline
}

// renew extends a lease that has not lapsed. It refuses a lapsed one, even in
// the seconds before the expiry pass removes its watch: renewing would revive
// a watch whose client has already lost it.
func (l *documentLeases) renew(name string, lease time.Duration) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	if deadline, ok := l.deadlines[name]; !ok || !now.Before(deadline) {
		return time.Time{}, false
	}
	l.deadlines[name] = now.Add(lease)
	return l.deadlines[name], true
}

func (l *documentLeases) forget(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.deadlines, name)
}

// expired reports whether name's lease has lapsed. A name without a deadline
// has lapsed.
func (l *documentLeases) expired(name string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	deadline, ok := l.deadlines[name]
	return !ok || !l.clock().Before(deadline)
}

// lapsed reports whether name has a deadline that has passed. Unlike expired,
// a name without a deadline has not lapsed: an unleased owner has none.
func (l *documentLeases) lapsed(name string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	deadline, ok := l.deadlines[name]
	return ok && !l.clock().Before(deadline)
}

func (l *documentLeases) deadline(name string) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	deadline, ok := l.deadlines[name]
	return deadline, ok
}

// watchDelivery is one Cloud (empty endpoint) or webhook delivery.
type watchDelivery struct {
	watch        string
	endpoint     string
	notification DetectionNotification
}

// watchWorker delivers one watch's notifications, so a slow or failing
// destination delays and drops only its own watch's deliveries.
type watchWorker struct {
	queue  chan watchDelivery
	ctx    context.Context
	cancel context.CancelFunc
}

// watchQueueSize matches a campaign job's notification queue.
const watchQueueSize = 16

// watchRunner turns inference arrivals into watch notifications (spec §6).
type watchRunner struct {
	service *DataService
	sender  CampaignNotificationSender
	ctx     context.Context
	mu      sync.Mutex
	// workers is guarded by mu, as are stopped and errors. Workers are only
	// added while stopped is false, which the supervisor sets before it waits.
	workers map[string]*watchWorker
	stopped bool
	wg      sync.WaitGroup
	errors  map[string]map[string]string // watch -> destination -> last error
}

// startWatches removes what a previous agent run leased, then runs the watch
// runner until ctx ends: deliveries, and lease expiry every 5 s. It returns a
// channel closed once the expiry loop and every delivery worker have stopped.
func (s *DataService) startWatches(ctx context.Context, sender CampaignNotificationSender) <-chan struct{} {
	s.sweepLeasedDocuments()
	runner := &watchRunner{service: s, sender: sender, ctx: ctx, workers: map[string]*watchWorker{}, errors: map[string]map[string]string{}}
	s.watches = runner
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
	loop:
		for {
			select {
			case <-ctx.Done():
				break loop
			case <-ticker.C:
				s.expireLeases()
			}
		}
		runner.mu.Lock()
		runner.stopped = true
		runner.mu.Unlock()
		runner.wg.Wait()
	}()
	return done
}

// deliver writes one journal entry for each watch on inference and queues its
// Cloud and webhook deliveries. The inference runner calls it for every
// arrival; it never waits on Cloud or a webhook.
func (r *watchRunner) deliver(inference string, base DetectionNotification) {
	s := r.service
	watches, err := s.manager.Documents(data.KindWatch)
	if err != nil {
		s.manager.Warnf("reading watches for inference %q: %v", inference, err)
		return
	}
	for _, stored := range watches {
		watch, err := stored.Watch()
		if err != nil || watch.On.Inference != inference {
			continue
		}
		// The deadline decides, not the 5 s expiry pass: a lapsed lease writes
		// nothing even before its watch is removed.
		if watch.Leased() && s.leases.expired(watch.Name) {
			continue
		}
		notification := base
		notification.ID = uuid.NewString()
		notification.Watch = watch.Name
		notification.Leased = watch.Leased()
		if err := s.manager.RecordNotification(notification); err != nil {
			s.manager.Warnf("watch %q notification: %v", watch.Name, err)
			continue
		}
		if watch.NotifiesCloud() {
			r.enqueue(watchDelivery{watch: watch.Name, notification: notification})
		}
		if endpoint := watch.WebhookURL(); endpoint != "" {
			r.enqueue(watchDelivery{watch: watch.Name, endpoint: endpoint, notification: notification})
		}
	}
}

// enqueue hands a delivery to its watch's worker, starting the worker on the
// watch's first delivery. It never blocks.
func (r *watchRunner) enqueue(delivery watchDelivery) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	worker := r.workers[delivery.watch]
	if worker == nil {
		ctx, cancel := context.WithCancel(r.ctx)
		worker = &watchWorker{queue: make(chan watchDelivery, watchQueueSize), ctx: ctx, cancel: cancel}
		r.workers[delivery.watch] = worker
		r.wg.Add(1)
		go func() { defer r.wg.Done(); r.work(worker) }()
	}
	select {
	case worker.queue <- delivery:
	default:
		r.setErrorLocked(delivery, errors.New("external notification queue full; notification retained in Wendy Data"))
	}
}

func (r *watchRunner) work(worker *watchWorker) {
	for {
		select {
		case <-worker.ctx.Done():
			return
		case delivery := <-worker.queue:
			err := deliverNotification(worker.ctx, r.sender, delivery.endpoint, delivery.notification)
			if worker.ctx.Err() != nil {
				return
			}
			r.mu.Lock()
			// A retired watch's worker records nothing.
			if worker.ctx.Err() == nil {
				r.setErrorLocked(delivery, err)
			}
			r.mu.Unlock()
			if err != nil {
				r.service.manager.Warnf("watch %q notification: %v", delivery.watch, err)
			}
		}
	}
}

// forget stops a removed watch's worker, dropping its pending deliveries, and
// clears its delivery errors so a watch re-applied under the same name starts
// clean.
func (r *watchRunner) forget(watch string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if worker := r.workers[watch]; worker != nil {
		worker.cancel()
		delete(r.workers, watch)
	}
	delete(r.errors, watch)
}

func destination(delivery watchDelivery) string {
	if delivery.endpoint == "" {
		return "cloud"
	}
	return "webhook"
}

// setErrorLocked records a delivery's outcome for its own destination: a success
// clears only that destination's error.
func (r *watchRunner) setErrorLocked(delivery watchDelivery, err error) {
	dest := destination(delivery)
	if err == nil {
		delete(r.errors[delivery.watch], dest)
		if len(r.errors[delivery.watch]) == 0 {
			delete(r.errors, delivery.watch)
		}
		return
	}
	if r.errors[delivery.watch] == nil {
		r.errors[delivery.watch] = map[string]string{}
	}
	r.errors[delivery.watch][dest] = err.Error()
}

// deliveryError is the watch's last Cloud and webhook failures ("cloud: ...;
// webhook: ..."), or empty.
func (r *watchRunner) deliveryError(watch string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var parts []string
	for _, dest := range []string{"cloud", "webhook"} {
		if message := r.errors[watch][dest]; message != "" {
			parts = append(parts, dest+": "+message)
		}
	}
	return strings.Join(parts, "; ")
}

// removeDocumentTree deletes a document and everything it owns, owned
// documents first, and returns their references in the order removed. The
// caller waits for inference jobs afterwards, without deploymentMu.
func (s *DataService) removeDocumentTree(kind, name string) ([]string, error) {
	s.deploymentMu.Lock()
	defer s.deploymentMu.Unlock()
	return s.removeDocumentTreeLocked(kind, name)
}

func (s *DataService) removeDocumentTreeLocked(kind, name string) ([]string, error) {
	if _, err := s.manager.Document(kind, name); err != nil {
		return nil, err
	}
	ref := kind + "/" + name
	var removed []string
	for _, ownedKind := range []string{data.KindInference, data.KindWatch} {
		documents, err := s.manager.Documents(ownedKind)
		if err != nil {
			return removed, err
		}
		for _, document := range documents {
			if document.Owner != ref {
				continue
			}
			if err := s.manager.RemoveDocument(document.Kind, document.Name); err != nil && !errors.Is(err, os.ErrNotExist) {
				return removed, err
			}
			if document.Kind == data.KindWatch {
				s.forgetWatch(document.Name)
			}
			removed = append(removed, document.Ref())
		}
	}
	if err := s.manager.RemoveDocument(kind, name); err != nil {
		return removed, err
	}
	if kind == data.KindWatch {
		s.forgetWatch(name)
	}
	return append(removed, ref), nil
}

// forgetWatch drops a removed watch's lease and delivery state.
func (s *DataService) forgetWatch(name string) {
	s.leases.forget(name)
	if s.watches != nil {
		s.watches.forget(name)
	}
}

// expireLeases removes every leased watch whose lease lapsed, with what it
// owns, through DocumentRemove's path, then asks the inference loop to retire
// the jobs.
func (s *DataService) expireLeases() {
	watches, err := s.manager.Documents(data.KindWatch)
	if err != nil {
		s.manager.Warnf("reading watches to expire leases: %v", err)
		return
	}
	removedAny := false
	for _, stored := range watches {
		watch, err := stored.Watch()
		if err != nil || !watch.Leased() || !s.leases.expired(watch.Name) {
			continue
		}
		removed, err := s.removeExpiredWatch(watch.Name)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			s.manager.Warnf("removing watch %q after its lease lapsed: %v", watch.Name, err)
			continue
		}
		if len(removed) > 0 {
			removedAny = true
			s.manager.Warnf("watch %q was removed because its lease lapsed: %s", watch.Name, strings.Join(removed, ", "))
		}
	}
	if removedAny && s.inference != nil {
		s.inference.wakeNow()
	}
}

// sweepLeasedDocuments runs once at boot, before any runner starts. Deadlines
// live in memory, so every leased watch belongs to a previous run: it goes,
// with what it owns, and so does every document whose owner is missing.
func (s *DataService) sweepLeasedDocuments() {
	s.deploymentMu.Lock()
	defer s.deploymentMu.Unlock()
	watches, err := s.manager.Documents(data.KindWatch)
	if err != nil {
		s.manager.Warnf("reading watches for the boot sweep: %v", err)
		return
	}
	for _, stored := range watches {
		if watch, err := stored.Watch(); err == nil && watch.Leased() {
			if _, err := s.removeDocumentTreeLocked(data.KindWatch, watch.Name); err != nil && !errors.Is(err, os.ErrNotExist) {
				s.manager.Warnf("removing leased watch %q at boot: %v", watch.Name, err)
			}
		}
	}
	for _, kind := range []string{data.KindInference, data.KindWatch} {
		documents, err := s.manager.Documents(kind)
		if err != nil {
			s.manager.Warnf("reading %s documents for the boot sweep: %v", kind, err)
			continue
		}
		for _, document := range documents {
			if document.Owner == "" {
				continue
			}
			ownerKind, ownerName, _ := strings.Cut(document.Owner, "/")
			if _, err := s.manager.Document(ownerKind, ownerName); !errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err := s.manager.RemoveDocument(document.Kind, document.Name); err != nil && !errors.Is(err, os.ErrNotExist) {
				s.manager.Warnf("removing %s, whose owner %s is gone: %v", document.Ref(), document.Owner, err)
			}
		}
	}
}

// removeExpiredWatch decides expiry under deploymentMu, so an apply or redeploy
// that renewed the lease first wins. It returns nothing if the watch no longer
// qualifies.
func (s *DataService) removeExpiredWatch(name string) ([]string, error) {
	s.deploymentMu.Lock()
	defer s.deploymentMu.Unlock()
	stored, err := s.manager.Document(data.KindWatch, name)
	if err != nil {
		return nil, err
	}
	watch, err := stored.Watch()
	if err != nil || !watch.Leased() || !s.leases.expired(name) {
		return nil, nil
	}
	return s.removeDocumentTreeLocked(data.KindWatch, name)
}
