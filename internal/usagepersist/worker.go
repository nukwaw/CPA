package usagepersist

import (
	"context"
	"errors"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist/quota"
	log "github.com/sirupsen/logrus"
)

// The worker retains at most this many queued records plus one in-flight
// record, shared by usage, quota-only observations and reset barriers. Admission
// never waits for capacity or creates per-event goroutines. Overflow drops the
// incoming record (including resets) and increments queue_overflows/dropped_events;
// resets have no unbounded priority path. Health's event counters include all work.
// This fixed bound is deliberately not another runtime setting.
const usageQueueCapacity = 256

type queuedWorkKind uint8

const (
	queuedUsageEvent queuedWorkKind = iota // Keep existing Consume admission unchanged.
	queuedQuotaObservation
	queuedQuotaResetBarrier
)

// queuedUsage is the entire queued representation. Do not add raw JSON, headers,
// credentials, callbacks or request contexts here. Only usage work inserts Event;
// quota-only observations and resets never produce dummy accounting events.
type queuedUsage struct {
	Event Event             `json:"event"`
	Quota *quota.Snapshot   `json:"quota,omitempty"`
	Kind  queuedWorkKind    `json:"kind,omitempty"`
	Reset *queuedQuotaReset `json:"reset,omitempty"`
}

// queuedQuotaReset carries only the account a reset applies to and when it was
// received. There is no credential fence to retain.
type queuedQuotaReset struct {
	Provider   string    `json:"provider"`
	Account    string    `json:"account"`
	ObservedAt time.Time `json:"observed_at"`
}

// enqueueManagement uses the same admission/bookkeeping contract as Consume.
// Normalization must finish before calling it. This lock never covers backend I/O
// and a full queue never blocks the original management handler's response.
func (s *Store) enqueueManagement(item queuedUsage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.workerStopped || s.workerCtx.Err() != nil {
		s.recordDrop(1)
		return
	}
	select {
	case s.queue <- item:
		s.accepted++
		s.pendingEvents.Add(1)
	default:
		s.queueOverflows.Add(1)
		s.recordDrop(1)
	}
}

func (s *Store) startWorker(lifecycle context.Context) {
	s.workerCtx, s.cancelWorker = context.WithCancel(lifecycle)
	s.queue = make(chan queuedUsage, usageQueueCapacity)
	s.workerDone = make(chan struct{})
	s.progress = make(chan struct{})
	go s.runWorker()
}

func (s *Store) runWorker() {
	defer func() {
		// Stop admission before discarding so a concurrent callback cannot put
		// another snapshot behind the final drain after lifecycle cancellation.
		s.mu.Lock()
		s.workerStopped = true
		s.mu.Unlock()
		for {
			select {
			case _, ok := <-s.queue:
				if !ok {
					close(s.workerDone)
					return
				}
				s.pendingEvents.Add(-1)
				s.recordDrop(1)
			default:
				close(s.workerDone)
				return
			}
		}
	}()
	for {
		if s.workerCtx.Err() != nil {
			return
		}
		select {
		case <-s.workerCtx.Done():
			return
		case item, ok := <-s.queue:
			if !ok {
				return
			}
			if s.workerCtx.Err() != nil {
				s.mu.Lock()
				s.pendingEvents.Add(-1)
				s.mu.Unlock()
				s.recordDrop(1)
				return
			}
			if err := s.persistUsage(s.workerCtx, item); err != nil {
				s.recordWriteFailure()
				// Backend errors can contain credentials or raw upstream data.
				log.Error("usage persistence: failed to store queued observation")
			}
			s.mu.Lock()
			s.pendingEvents.Add(-1)
			s.completed++
			close(s.progress)
			s.progress = make(chan struct{})
			s.mu.Unlock()
		}
	}
}

func (s *Store) persistUsage(ctx context.Context, item queuedUsage) error {
	switch item.Kind {
	case queuedUsageEvent:
		if _, err := s.store.Insert(ctx, item.Event); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	case queuedQuotaObservation:
		// Quota-only management observations never enter usage statistics.
	case queuedQuotaResetBarrier:
		if item.Reset == nil {
			return errors.New("missing queued quota reset")
		}
		return s.resetBoundQuotaAt(ctx, item.Reset)
	default:
		return errors.New("unknown queued observation kind")
	}
	if item.Quota != nil {
		return s.mergeQuota(ctx, *item.Quota)
	}
	return nil
}

func (s *Store) recordDrop(count int64) {
	s.droppedEvents.Add(count)
	s.lastDrop.Store(time.Now().UTC().UnixNano())
}

// Flush waits until every record admitted before this call has finished its
// persistence attempt, including quota-only writes and reset barriers. Later
// admissions do not extend the barrier. Flush does not consume queue capacity,
// change the SDK dispatcher or disable collection. Write errors use health counters,
// not as the result of this synchronization barrier. A canceled context ends only
// this wait; unlike Close it does not cancel the worker. Abandoned admitted work
// returns ErrClosed once the worker stops. Detach the observer or synchronize the
// upstream producer first if the caller needs a barrier for its last publication.
func (s *Store) Flush(ctx context.Context) error {
	if s == nil {
		return ErrClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	target := s.accepted
	for s.completed < target {
		progress := s.progress
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.workerDone:
			s.mu.Lock()
			completed := s.completed
			s.mu.Unlock()
			if completed < target {
				return ErrClosed
			}
			return nil
		case <-progress:
		}
		s.mu.Lock()
	}
	s.mu.Unlock()
	return nil
}
