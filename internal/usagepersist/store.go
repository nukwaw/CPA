package usagepersist

import (
	"bufio"
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var ErrClosed = errors.New("usage persistence is closed")

const journalFilename = "usage.jsonl"

type store interface {
	Insert(context.Context, Event) (bool, error)
	Walk(context.Context, Filter, func(Event) error) error
	Events(context.Context, Filter, int, int) ([]Event, int64, error)
	Cache(context.Context, string) (map[string]json.RawMessage, error)
	UpdateCache(context.Context, []cacheUpdate) error
	MutateCache(context.Context, string, string, func(json.RawMessage) (json.RawMessage, error)) error
	Close() error
}

// fileStore holds an exclusive process lock for the journal's entire lifetime.
// Its mutex serializes operations within this instance, not across stores.
type fileStore struct {
	mu     sync.RWMutex
	file   *os.File
	lock   *journalLock
	events map[string]Event
	cache  map[string]map[string]json.RawMessage

	limits       localCapacityLimits
	journalBytes int64 // Protected by mu, unlike the immutable published snapshot.
	capacity     atomic.Pointer[LocalCapacityStatus]
}
type journalEntry struct {
	Event   *Event        `json:"event,omitempty"`
	Updates []cacheUpdate `json:"updates,omitempty"`
}

func openFileStore(ctx context.Context, dir string) (*fileStore, error) {
	return openFileStoreWithLimits(ctx, dir, supportedLocalCapacity())
}

func openFileStoreWithLimits(ctx context.Context, dir string, limits localCapacityLimits) (_ *fileStore, err error) {
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if limits.journalBytes <= 0 || limits.journalBytes > localJournalByteLimit ||
		limits.retainedEvents <= 0 || limits.retainedEvents > localRetainedEventLimit {
		return nil, errors.New("invalid private local capacity limits")
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create usage directory: %w", err)
	}
	lock, err := acquireJournalLock(filepath.Join(dir, journalFilename+".lock"))
	if err != nil {
		return nil, err
	}
	s := &fileStore{lock: lock, events: map[string]Event{}, cache: map[string]map[string]json.RawMessage{}, limits: limits}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.Close())
		}
	}()
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	// Do not even open the data file until this process owns the lock.
	s.file, err = os.OpenFile(filepath.Join(dir, journalFilename), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		return nil, fmt.Errorf("open usage journal: %w", err)
	}
	if err = s.preflightCapacity(ctx); err != nil {
		return nil, err
	}
	if err = s.replay(ctx); err != nil {
		return nil, err
	}
	s.publishCapacity("")
	return s, nil
}

func (s *fileStore) replay(ctx context.Context) error {
	reader := bufio.NewReader(io.LimitReader(s.file, s.limits.journalBytes+1))
	var offset int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := reader.ReadBytes('\n')
		if errContext := ctx.Err(); errContext != nil {
			return errContext
		}
		if int64(len(line)) > s.limits.journalBytes-offset {
			return localCapacityError(localCapacityJournalBytes, s.limits.journalBytes)
		}
		if errors.Is(err, io.EOF) {
			// Only an incomplete final write is repairable, and only under the
			// process lock. Corrupt complete records must fail without data loss.
			if len(line) > 0 {
				if errTruncate := s.file.Truncate(offset); errTruncate != nil {
					return fmt.Errorf("repair incomplete usage journal record: %w", errTruncate)
				}
				if errSync := s.file.Sync(); errSync != nil {
					return fmt.Errorf("sync repaired usage journal: %w", errSync)
				}
			}
			s.journalBytes = offset
			return ctx.Err()
		}
		if err != nil {
			return fmt.Errorf("read usage journal: %w", err)
		}
		var entry journalEntry
		if errDecode := json.Unmarshal(line, &entry); errDecode != nil {
			// Decoder errors can include record contents; report only the location.
			return fmt.Errorf("invalid usage journal record at byte %d", offset)
		}
		if entry.Event != nil {
			if _, exists := s.events[entry.Event.ID]; !exists && len(s.events) >= s.limits.retainedEvents {
				return localCapacityError(localCapacityRetainedEvents, int64(s.limits.retainedEvents))
			}
		}
		s.apply(entry)
		offset += int64(len(line))
	}
}

func (s *fileStore) apply(entry journalEntry) {
	if entry.Event != nil {
		if _, exists := s.events[entry.Event.ID]; !exists {
			s.events[entry.Event.ID] = *entry.Event
		}
	}
	for _, update := range entry.Updates {
		if s.cache[update.Namespace] == nil {
			s.cache[update.Namespace] = map[string]json.RawMessage{}
		}
		if update.Delete {
			delete(s.cache[update.Namespace], update.Key)
		} else {
			s.cache[update.Namespace][update.Key] = append(json.RawMessage(nil), update.Value...)
		}
	}
}

func (s *fileStore) append(ctx context.Context, entry journalEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.file == nil {
		return ErrClosed
	}
	if entry.Event != nil {
		if _, exists := s.events[entry.Event.ID]; !exists && len(s.events) >= s.limits.retainedEvents {
			return s.rejectCapacity(localCapacityRetainedEvents, int64(s.limits.retainedEvents))
		}
	}
	info, err := s.file.Stat()
	if err != nil {
		return fmt.Errorf("stat usage journal before write: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	s.journalBytes = info.Size()
	if s.journalBytes >= s.limits.journalBytes {
		return s.rejectCapacity(localCapacityJournalBytes, s.limits.journalBytes)
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	// Include the newline, and subtract before comparing to avoid overflow.
	if int64(len(data)) >= s.limits.journalBytes-s.journalBytes {
		return s.rejectCapacity(localCapacityJournalBytes, s.limits.journalBytes)
	}
	data = append(data, '\n')
	// O_APPEND selects the write offset; the pre-write size is only for rollback.
	n, err := s.file.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = s.file.Sync()
	}
	if err != nil {
		errWrite := fmt.Errorf("write usage journal: %w", err)
		errRollback := s.file.Truncate(info.Size())
		if errRollback == nil {
			errRollback = s.file.Sync()
		}
		if errRollback != nil {
			// The journal is no longer safe to append to. Close it before unlocking.
			return errors.Join(errWrite, fmt.Errorf("rollback usage journal: %w", errRollback), s.closeLocked())
		}
		return errWrite
	}
	s.apply(entry)
	s.journalBytes += int64(len(data))
	s.publishCapacity("")
	return nil
}

func (s *fileStore) Insert(ctx context.Context, e Event) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if s.file == nil {
		return false, ErrClosed
	}
	if _, exists := s.events[e.ID]; exists {
		return false, nil
	}
	if err := s.append(ctx, journalEntry{Event: &e}); err != nil {
		return false, err
	}
	return true, nil
}

func (s *fileStore) matching(ctx context.Context, f Filter) ([]Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.file == nil {
		return nil, ErrClosed
	}
	events := make([]Event, 0)
	for _, e := range s.events {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if f.matches(e) {
			events = append(events, e)
		}
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].RequestedAt.Equal(events[j].RequestedAt) {
			return events[i].ID > events[j].ID
		}
		return events[i].RequestedAt.After(events[j].RequestedAt)
	})
	return events, ctx.Err()
}
func (s *fileStore) Walk(ctx context.Context, f Filter, fn func(Event) error) error {
	events, err := s.matching(ctx, f)
	if err != nil {
		return err
	}
	for _, e := range events {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = fn(e); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// eventOrder contains only sort keys, never a full Event. The heap's root is the
// oldest selected key, so a scan retains at most offset+limit newest keys. Only
// the returned page copies Events; the response cannot retain a full-history
// backing array. Deep offsets still require more keys, bounded by local capacity.
type eventOrder struct {
	id string
	at time.Time
}

func (a eventOrder) newer(b eventOrder) bool {
	if a.at.Equal(b.at) {
		return a.id > b.id
	}
	return a.at.After(b.at)
}

type eventPageHeap []eventOrder

func (h eventPageHeap) Len() int           { return len(h) }
func (h eventPageHeap) Less(i, j int) bool { return h[j].newer(h[i]) }
func (h eventPageHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *eventPageHeap) Push(value any)    { *h = append(*h, value.(eventOrder)) }
func (h *eventPageHeap) Pop() any {
	last := len(*h) - 1
	value := (*h)[last]
	(*h)[last] = eventOrder{}
	*h = (*h)[:last]
	return value
}

func (s *fileStore) Events(ctx context.Context, f Filter, limit, offset int) ([]Event, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if s.file == nil {
		return nil, 0, ErrClosed
	}
	if limit < 0 || offset < 0 {
		return nil, 0, errors.New("invalid event pagination")
	}
	// Clamp before adding so an arbitrarily large offset/limit cannot overflow
	// or cause allocation proportional to an untrusted pagination parameter.
	var k int
	if limit > 0 && offset < len(s.events) {
		k = offset + min(limit, len(s.events)-offset)
	}
	selected := make(eventPageHeap, 0, min(k, 32))
	var total int64
	for _, e := range s.events {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if !f.matches(e) {
			continue
		}
		total++
		if k == 0 {
			continue
		}
		key := eventOrder{id: e.ID, at: e.RequestedAt}
		if len(selected) < k {
			heap.Push(&selected, key)
		} else if key.newer(selected[0]) {
			selected[0] = key
			heap.Fix(&selected, 0)
		}
	}
	events := make([]Event, 0)
	if offset < len(selected) {
		sort.Slice(selected, func(i, j int) bool { return selected[i].newer(selected[j]) })
		events = make([]Event, len(selected)-offset)
		for i, key := range selected[offset:] {
			events[i] = s.events[key.id]
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	return events, total, nil
}
func (s *fileStore) Cache(ctx context.Context, namespace string) (map[string]json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.file == nil {
		return nil, ErrClosed
	}
	result := map[string]json.RawMessage{}
	for k, v := range s.cache[namespace] {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result[k] = append(json.RawMessage(nil), v...)
	}
	return result, ctx.Err()
}
func (s *fileStore) UpdateCache(ctx context.Context, updates []cacheUpdate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.append(ctx, journalEntry{Updates: updates})
}
func (s *fileStore) MutateCache(ctx context.Context, namespace, key string, fn func(json.RawMessage) (json.RawMessage, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.file == nil {
		return ErrClosed
	}
	if s.journalBytes >= s.limits.journalBytes {
		return s.rejectCapacity(localCapacityJournalBytes, s.limits.journalBytes)
	}
	// The callback receives a copy. Its result is not applied unless the entire
	// record fits the byte budget and has been written and synced successfully.
	value, err := fn(append(json.RawMessage(nil), s.cache[namespace][key]...))
	if err != nil {
		return err
	}
	return s.append(ctx, journalEntry{Updates: []cacheUpdate{{Namespace: namespace, Key: key, Value: value, Delete: value == nil}}})
}
func (s *fileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeLocked()
}

func (s *fileStore) closeLocked() error {
	var err error
	if s.file != nil {
		if errClose := s.file.Close(); errClose != nil {
			err = fmt.Errorf("close usage journal: %w", errClose)
		}
		s.file = nil
	}
	// Even a data-file close error must not leak the separate process lock.
	if s.lock != nil {
		err = errors.Join(err, s.lock.Close())
		s.lock = nil
	}
	return err
}
