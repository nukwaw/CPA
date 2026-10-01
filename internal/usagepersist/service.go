package usagepersist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Store persists a passive copy of the built-in usage provider's JSON output.
// It neither registers an SDK usage plugin nor changes the provider's gates or queue.
type Store struct {
	store                store
	client               *http.Client
	mu                   sync.Mutex
	closing              bool
	active               sync.WaitGroup
	closeOnce            sync.Once
	closed               chan struct{}
	closeErr             error
	writeFailures        atomic.Int64
	lastWriteFailure     atomic.Int64
	validationFailures   atomic.Int64
	queueOverflows       atomic.Int64
	droppedEvents        atomic.Int64
	lastDrop             atomic.Int64
	pendingEvents        atomic.Int64
	quotaHistoryFailures atomic.Int64

	queue         chan queuedUsage
	workerCtx     context.Context
	cancelWorker  context.CancelFunc
	workerDone    chan struct{}
	workerStopped bool
	accepted      uint64
	completed     uint64
	progress      chan struct{}
}

func Open(ctx context.Context, options Options) (*Store, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var storage store
	var err error
	if options.PostgresDSN != "" {
		storage, err = openUsagePostgres(ctx, options.PostgresDSN, options.Schema)
	} else if options.Database != nil {
		storage, err = openPostgresStore(ctx, options.Database, options.Schema)
	} else {
		if strings.TrimSpace(options.DataDir) == "" {
			return nil, errors.New("usage persistence requires an explicit data directory")
		}
		storage, err = openFileStore(ctx, options.DataDir)
	}
	if err != nil {
		return nil, err
	}
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return errors.New("pricing source redirect refused") }}
	}
	s := &Store{store: storage, client: client, closed: make(chan struct{})}
	s.startWorker(ctx)
	return s, nil
}

func (s *Store) begin() error {
	if s == nil {
		return ErrClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return ErrClosed
	}
	s.active.Add(1)
	return nil
}

// Close seals admission, drains accepted usage and waits for active management
// operations before releasing storage. Detach the passive observer first; records
// still in the upstream SDK queue are outside this Store's lifecycle.
//
// The caller must supply a deadline/cancellation to bound shutdown waiting. If ctx
// expires, Close cancels the independent worker and returns immediately; queued
// records are dropped and counted. An uninterruptible backend operation or live
// core-credential lock may finish later; cleanup then safely releases storage. No
// claim is made that context cancellation interrupts such a lock. Another Close
// can wait for that cleanup. A background context requests an unbounded graceful
// drain. A borrowed SQL pool is never closed.
func (s *Store) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closing = true
		close(s.queue)
		s.mu.Unlock()
		go func() {
			<-s.workerDone
			s.cancelWorker()
			s.active.Wait()
			s.closeErr = s.store.Close()
			close(s.closed)
		}()
	})
	if err := ctx.Err(); err != nil {
		s.cancelWorker()
		return err
	}
	select {
	case <-s.closed:
		return s.closeErr
	case <-ctx.Done():
		s.cancelWorker()
		return ctx.Err()
	}
}

func (s *Store) recordWriteFailure() {
	s.writeFailures.Add(1)
	s.lastWriteFailure.Store(time.Now().UTC().UnixNano())
}

func (s *Store) ListCache(ctx context.Context, namespace string) (map[string]json.RawMessage, error) {
	if err := s.begin(); err != nil {
		return nil, err
	}
	defer s.active.Done()
	return s.store.Cache(ctx, namespace)
}
func (s *Store) Events(ctx context.Context, f Filter, limit, offset int) (EventPage, error) {
	if err := s.begin(); err != nil {
		return EventPage{}, err
	}
	defer s.active.Done()
	if limit < 1 || limit > 500 || offset < 0 {
		return EventPage{}, errors.New("invalid event pagination")
	}
	prices, err := s.prices(ctx)
	if err != nil {
		return EventPage{}, err
	}
	events, total, err := s.store.Events(ctx, f, limit, offset)
	if err != nil {
		return EventPage{}, err
	}
	for i := range events {
		applyPrice(&events[i], prices)
	}
	return EventPage{Events: events, Total: total, GeneratedAt: time.Now().UTC()}, nil
}

func (s *Store) Analyze(ctx context.Context, f Filter, bucket string) (Analysis, error) {
	if err := s.begin(); err != nil {
		return Analysis{}, err
	}
	defer s.active.Done()
	width := time.Hour
	if bucket == "day" {
		width = 24 * time.Hour
	} else if bucket != "" && bucket != "hour" {
		return Analysis{}, errors.New("bucket must be hour or day")
	}
	prices, err := s.prices(ctx)
	if err != nil {
		return Analysis{}, err
	}
	result := Analysis{Series: []SeriesPoint{}, Models: []NamedSummary{}, Providers: []NamedSummary{}, GeneratedAt: time.Now().UTC()}
	series := map[time.Time]*Summary{}
	models := map[string]*Summary{}
	providers := map[string]*Summary{}
	err = s.store.Walk(ctx, f, func(e Event) error {
		applyPrice(&e, prices)
		result.Summary.add(e)
		t := e.RequestedAt.UTC().Truncate(width)
		if series[t] == nil {
			series[t] = &Summary{}
		}
		series[t].add(e)
		if models[e.Model] == nil {
			models[e.Model] = &Summary{}
		}
		models[e.Model].add(e)
		if providers[e.Provider] == nil {
			providers[e.Provider] = &Summary{}
		}
		providers[e.Provider].add(e)
		return nil
	})
	if err != nil {
		return Analysis{}, err
	}
	for t, summary := range series {
		result.Series = append(result.Series, SeriesPoint{Time: t, Summary: *summary})
	}
	sort.Slice(result.Series, func(i, j int) bool { return result.Series[i].Time.Before(result.Series[j].Time) })
	result.Models = namedSummaries(models)
	result.Providers = namedSummaries(providers)
	return result, nil
}
func namedSummaries(values map[string]*Summary) []NamedSummary {
	result := make([]NamedSummary, 0, len(values))
	for name, summary := range values {
		result = append(result, NamedSummary{Name: name, Summary: *summary})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Requests == result[j].Requests {
			return result[i].Name < result[j].Name
		}
		return result[i].Requests > result[j].Requests
	})
	return result
}

type FilterValues struct {
	Models    []string `json:"models"`
	Providers []string `json:"providers"`
	Accounts  []string `json:"accounts"`
	KeyIDs    []string `json:"key_ids"`
}

func (s *Store) Filters(ctx context.Context, f Filter) (FilterValues, error) {
	if err := s.begin(); err != nil {
		return FilterValues{}, err
	}
	defer s.active.Done()
	models, providers, accounts, keys := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	err := s.store.Walk(ctx, f, func(e Event) error {
		models[e.Model] = true
		providers[e.Provider] = true
		if e.Account != "" {
			accounts[e.Account] = true
		}
		if e.KeyID != "" {
			keys[e.KeyID] = true
		}
		return nil
	})
	return FilterValues{Models: sortedKeys(models), Providers: sortedKeys(providers), Accounts: sortedKeys(accounts), KeyIDs: sortedKeys(keys)}, err
}
func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func cacheDecode[T any](values map[string]json.RawMessage) (map[string]T, error) {
	result := make(map[string]T, len(values))
	for key, value := range values {
		var decoded T
		if err := json.Unmarshal(value, &decoded); err != nil {
			return nil, fmt.Errorf("decode usage persistence metadata: %w", err)
		}
		result[key] = decoded
	}
	return result, nil
}
