package usagepersist

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func requireLocalCapacityPlatform(t testing.TB) {
	t.Helper()
	switch runtime.GOOS {
	case "linux", "darwin", "windows":
	default:
		t.Skip("local process locking is intentionally unsupported on this platform")
	}
}

func openLocalCapacityFixture(t testing.TB, dir string, limits localCapacityLimits) *fileStore {
	t.Helper()
	requireLocalCapacityPlatform(t)
	storage, err := openFileStoreWithLimits(context.Background(), dir, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if errClose := storage.Close(); errClose != nil {
			t.Error(errClose)
		}
	})
	return storage
}

func localCapacityRecord(t testing.TB, entry journalEntry) []byte {
	t.Helper()
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func localCapacityJournal(t testing.TB, dir string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, journalFilename))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func localCapacitySnapshot(t testing.TB, storage *fileStore) LocalCapacityStatus {
	t.Helper()
	status, ok := (&Store{store: storage}).LocalCapacity()
	if !ok {
		t.Fatal("local capacity snapshot is unavailable")
	}
	return status
}

func assertLocalCapacityUnlocked(t *testing.T, dir string) {
	t.Helper()
	lock, err := acquireJournalLock(filepath.Join(dir, journalFilename+".lock"))
	if err != nil {
		t.Fatalf("capacity failure leaked the lifetime lock: %v", err)
	}
	if errClose := lock.Close(); errClose != nil {
		t.Fatal(errClose)
	}
}

func TestLocalCapacitySupportedLimitsAndUnsupportedPlatform(t *testing.T) {
	limits := supportedLocalCapacity()
	if limits.journalBytes != 67_108_864 || limits.retainedEvents != 100_000 {
		t.Fatalf("supported limits changed without updating their contract: %+v", limits)
	}
	dir := t.TempDir()
	storage, err := openFileStore(context.Background(), dir)
	switch runtime.GOOS {
	case "linux", "darwin", "windows":
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if errClose := storage.Close(); errClose != nil {
				t.Error(errClose)
			}
		}()
		status := localCapacitySnapshot(t, storage)
		if status.JournalBytes != 0 || status.RetainedEvents != 0 || status.JournalByteLimit != limits.journalBytes || status.RetainedEventLimit != limits.retainedEvents || status.RejectedWrites != 0 || status.LastRejectedLimit != "" {
			t.Fatalf("incorrect initial capacity snapshot: %+v", status)
		}
	default:
		if err == nil || storage != nil {
			t.Fatal("unsupported platform opened an unlocked journal")
		}
		if _, errStat := os.Stat(filepath.Join(dir, journalFilename)); !errors.Is(errStat, os.ErrNotExist) {
			t.Fatalf("unsupported lock fallback touched the data file: %v", errStat)
		}
	}
	for _, service := range []*Store{nil, {}, {store: (*fileStore)(nil)}} {
		if status, ok := service.LocalCapacity(); ok || status != (LocalCapacityStatus{}) {
			t.Fatalf("non-local store reported capacity: %+v, %v", status, ok)
		}
	}
}

func TestLocalCapacityStartupRejectsBeforeReplayAndLeavesJournal(t *testing.T) {
	requireLocalCapacityPlatform(t)
	first, second, third := Event{ID: "first"}, Event{ID: "second"}, Event{ID: "third"}
	metadata := []cacheUpdate{{Namespace: "pricing", Key: "model", Value: json.RawMessage(`{"rate":1}`)}}
	events := localCapacityRecord(t, journalEntry{Event: &first, Updates: metadata})
	events = append(events, localCapacityRecord(t, journalEntry{Event: &second})...)
	events = append(events, localCapacityRecord(t, journalEntry{Event: &third})...)
	events = append(events, []byte(`{"event":`)...)
	for _, test := range []struct {
		name     string
		data     []byte
		limits   localCapacityLimits
		resource string
	}{
		{"bytes-before-json-decode", []byte("not-json\nunfinished-secret-tail"), localCapacityLimits{journalBytes: 16, retainedEvents: 2}, localCapacityJournalBytes},
		{"events-before-tail-repair", events, localCapacityLimits{journalBytes: int64(len(events)), retainedEvents: 2}, localCapacityRetainedEvents},
		{"oversize-incomplete-tail", events[:len(events)-1], localCapacityLimits{journalBytes: int64(len(events) - 2), retainedEvents: 3}, localCapacityJournalBytes},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, journalFilename)
			if err := os.WriteFile(path, test.data, 0600); err != nil {
				t.Fatal(err)
			}
			// Repeated failed opens must neither mutate history nor keep the lock.
			for range 2 {
				storage, err := openFileStoreWithLimits(context.Background(), dir, test.limits)
				if storage != nil {
					_ = storage.Close()
					t.Fatal("over-limit startup returned a live store")
				}
				if !errors.Is(err, ErrCapacity) || !strings.Contains(err.Error(), test.resource) || errors.Is(err, ErrLocked) {
					t.Fatalf("startup did not expose distinct capacity error: %v", err)
				}
				if strings.Contains(err.Error(), "secret-tail") {
					t.Fatal("capacity error disclosed journal contents")
				}
				if after := localCapacityJournal(t, dir); !bytes.Equal(after, test.data) {
					t.Fatal("capacity rejection modified completed history or an incomplete tail")
				}
				assertLocalCapacityUnlocked(t, dir)
			}

			// Verify the guard itself retains no decoded Events or cache values.
			lock, err := acquireJournalLock(filepath.Join(dir, journalFilename+".lock"))
			if err != nil {
				t.Fatal(err)
			}
			guard := &fileStore{lock: lock, limits: test.limits, events: map[string]Event{}, cache: map[string]map[string]json.RawMessage{}}
			t.Cleanup(func() {
				if errClose := guard.Close(); errClose != nil {
					t.Error(errClose)
				}
			})
			guard.file, err = os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if err = guard.preflightCapacity(context.Background()); !errors.Is(err, ErrCapacity) {
				t.Fatalf("preflight: %v", err)
			}
			if len(guard.events) != 0 || len(guard.cache) != 0 || guard.capacity.Load() != nil {
				t.Fatal("preflight partially loaded history before rejecting capacity")
			}
		})
	}
}

func TestLocalCapacityDefaultRejectsSparseOversizeJournal(t *testing.T) {
	requireLocalCapacityPlatform(t)
	dir := t.TempDir()
	path := filepath.Join(dir, journalFilename)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	// A sparse fixture checks the real production limit without allocating or
	// writing a 64 MiB buffer (or creating 100,001 real event records).
	marker := []byte("untouched complete history\n")
	if _, err = file.Write(marker); err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(localJournalByteLimit + 1); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	storage, err := openFileStore(context.Background(), dir)
	if storage != nil {
		_ = storage.Close()
		t.Fatal("oversize sparse journal opened")
	}
	if !errors.Is(err, ErrCapacity) {
		t.Fatalf("expected byte capacity rejection before parsing marker: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != localJournalByteLimit+1 {
		t.Fatalf("oversize journal size changed: %v, %v", info, err)
	}
	file, err = os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	actual := make([]byte, len(marker))
	_, errRead := file.ReadAt(actual, 0)
	errClose := file.Close()
	if errRead != nil || errClose != nil || !bytes.Equal(actual, marker) {
		t.Fatalf("oversize journal prefix changed: %q, %v, %v", actual, errRead, errClose)
	}
	assertLocalCapacityUnlocked(t, dir)
}

func TestLocalCapacityEventLimitPreservesReadsDedupAndMetadata(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	limits := localCapacityLimits{journalBytes: 8192, retainedEvents: 3}
	storage := openLocalCapacityFixture(t, dir, limits)
	for _, id := range []string{"one", "two", "three"} {
		if inserted, err := storage.Insert(ctx, Event{ID: id}); err != nil || !inserted {
			t.Fatalf("insert %q: %v, %v", id, inserted, err)
		}
	}
	before := localCapacityJournal(t, dir)
	if inserted, err := storage.Insert(ctx, Event{ID: "four"}); inserted || !errors.Is(err, ErrCapacity) {
		t.Fatalf("event over capacity was accepted: %v, %v", inserted, err)
	}
	if inserted, err := storage.Insert(ctx, Event{ID: "one", Model: "must-not-replace"}); inserted || err != nil {
		t.Fatalf("idempotent replay failed at capacity: %v, %v", inserted, err)
	}
	if after := localCapacityJournal(t, dir); !bytes.Equal(before, after) {
		t.Fatal("rejected or duplicate event changed journal")
	}
	if len(storage.events) != 3 || storage.events["one"].Model != "" {
		t.Fatal("capacity rejection mutated event history")
	}
	updates := []cacheUpdate{{Namespace: "quota", Key: "account", Value: json.RawMessage(`{"remaining":7}`)}}
	if err := storage.UpdateCache(ctx, updates); err != nil {
		t.Fatalf("safe metadata update blocked by event limit: %v", err)
	}
	if err := storage.MutateCache(ctx, "pricing", "model", func(json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"rate":2}`), nil
	}); err != nil {
		t.Fatalf("safe metadata mutation blocked by event limit: %v", err)
	}
	status := localCapacitySnapshot(t, storage)
	if status.RetainedEvents != 3 || status.RejectedWrites != 1 || status.LastRejectedLimit != localCapacityRetainedEvents || status.JournalBytes != int64(len(localCapacityJournal(t, dir))) {
		t.Fatalf("incorrect event-limit snapshot: %+v", status)
	}
	page, total, err := storage.Events(ctx, Filter{}, 2, 0)
	if err != nil || total != 3 || len(page) != 2 {
		t.Fatalf("retained events unreadable after rejection: %v, %d, %v", page, total, err)
	}
	var walked int
	if err = storage.Walk(ctx, Filter{}, func(Event) error { walked++; return nil }); err != nil || walked != 3 {
		t.Fatalf("analysis scan unreadable at capacity: %d, %v", walked, err)
	}
	if err = storage.Close(); err != nil {
		t.Fatal(err)
	}
	storage = openLocalCapacityFixture(t, dir, limits)
	status = localCapacitySnapshot(t, storage)
	if status.RetainedEvents != 3 || status.RejectedWrites != 0 || status.LastRejectedLimit != "" {
		t.Fatalf("exact-limit history failed to reopen cleanly: %+v", status)
	}
	values, err := storage.Cache(ctx, "quota")
	if err != nil || string(values["account"]) != `{"remaining":7}` {
		t.Fatalf("metadata lost on reopen: %v, %v", values, err)
	}
	if _, err = storage.Insert(ctx, Event{ID: "still-full"}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("reopen bypassed event capacity: %v", err)
	}
}

func TestLocalCapacityByteLimitRejectsAtomicallyAndAllowsSmallerWrites(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first := Event{ID: "one"}
	initial := []cacheUpdate{{Namespace: "quota", Key: "account", Value: json.RawMessage(`{"remaining":7}`)}}
	replacement := []cacheUpdate{{Namespace: "quota", Key: "account", Value: json.RawMessage(`{"remaining":6}`)}}
	budget := len(localCapacityRecord(t, journalEntry{Event: &first})) + len(localCapacityRecord(t, journalEntry{Updates: initial})) + len(localCapacityRecord(t, journalEntry{Updates: replacement}))
	limits := localCapacityLimits{journalBytes: int64(budget), retainedEvents: 3}
	storage := openLocalCapacityFixture(t, dir, limits)
	if _, err := storage.Insert(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := storage.UpdateCache(ctx, initial); err != nil {
		t.Fatal(err)
	}
	before := localCapacityJournal(t, dir)
	if _, err := storage.Insert(ctx, Event{ID: "two"}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("event exceeded byte budget: %v", err)
	}
	largeValue := json.RawMessage(`"` + strings.Repeat("x", budget) + `"`)
	batch := append(append([]cacheUpdate{}, replacement...), cacheUpdate{Namespace: "quota", Key: "too-large", Value: largeValue})
	if err := storage.UpdateCache(ctx, batch); !errors.Is(err, ErrCapacity) {
		t.Fatalf("metadata batch exceeded byte budget: %v", err)
	}
	if err := storage.MutateCache(ctx, "quota", "account", func(current json.RawMessage) (json.RawMessage, error) {
		current[1] = 'x' // Even callback mutation of its input must be isolated.
		return largeValue, nil
	}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("metadata mutation exceeded byte budget: %v", err)
	}
	if after := localCapacityJournal(t, dir); !bytes.Equal(after, before) {
		t.Fatal("rejected event/metadata writes changed journal bytes")
	}
	values, err := storage.Cache(ctx, "quota")
	if err != nil || len(values) != 1 || !bytes.Equal(values["account"], initial[0].Value) {
		t.Fatalf("rejected metadata was partially applied: %v, %v", values, err)
	}
	status := localCapacitySnapshot(t, storage)
	if status.RetainedEvents != 1 || status.JournalBytes != int64(len(before)) || status.RejectedWrites != 3 || status.LastRejectedLimit != localCapacityJournalBytes {
		t.Fatalf("incorrect byte-limit rejection snapshot: %+v", status)
	}
	// A previously rejected large record must not close or poison the store.
	if err = storage.UpdateCache(ctx, replacement); err != nil {
		t.Fatalf("smaller metadata write at exact byte boundary: %v", err)
	}
	status = localCapacitySnapshot(t, storage)
	if status.JournalBytes != limits.journalBytes || status.RejectedWrites != 3 {
		t.Fatalf("successful smaller write lost the sticky rejection or byte count: %+v", status)
	}
	full := localCapacityJournal(t, dir)
	if len(full) != budget {
		t.Fatalf("newline omitted from byte accounting: %d != %d", len(full), budget)
	}
	if _, err = storage.Insert(ctx, Event{ID: "two"}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("insert into full journal: %v", err)
	}
	if err = storage.UpdateCache(ctx, []cacheUpdate{{Namespace: "quota", Key: "account", Delete: true}}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("deletion must not bypass append budget: %v", err)
	}
	if err = storage.MutateCache(ctx, "quota", "account", func(json.RawMessage) (json.RawMessage, error) {
		t.Error("full journal invoked a mutation callback")
		return nil, nil
	}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("mutation into full journal: %v", err)
	}
	if inserted, errDuplicate := storage.Insert(ctx, first); inserted || errDuplicate != nil {
		t.Fatalf("duplicate in full byte journal: %v, %v", inserted, errDuplicate)
	}
	if after := localCapacityJournal(t, dir); !bytes.Equal(after, full) {
		t.Fatal("writes into full journal changed committed history")
	}
	if err = storage.Close(); err != nil {
		t.Fatal(err)
	}
	storage = openLocalCapacityFixture(t, dir, limits)
	page, total, err := storage.Events(ctx, Filter{}, 5, 0)
	if err != nil || total != 1 || len(page) != 1 || page[0].ID != "one" {
		t.Fatalf("exact byte-limit history unreadable after reopen: %v, %d, %v", page, total, err)
	}
	values, err = storage.Cache(ctx, "quota")
	if err != nil || !bytes.Equal(values["account"], replacement[0].Value) {
		t.Fatalf("metadata lost on full journal reopen: %v, %v", values, err)
	}
	if _, err = storage.Insert(ctx, Event{ID: "two"}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("reopen bypassed byte capacity: %v", err)
	}
}

func TestLocalCapacityByteLimitIncludesRecordNewline(t *testing.T) {
	event := Event{ID: "boundary"}
	size := int64(len(localCapacityRecord(t, journalEntry{Event: &event})))
	for _, budget := range []int64{size - 1, size} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			dir := t.TempDir()
			storage := openLocalCapacityFixture(t, dir, localCapacityLimits{journalBytes: budget, retainedEvents: 1})
			inserted, err := storage.Insert(context.Background(), event)
			if budget < size {
				if inserted || !errors.Is(err, ErrCapacity) || len(localCapacityJournal(t, dir)) != 0 || len(storage.events) != 0 {
					t.Fatalf("record newline bypassed capacity: %v, %v", inserted, err)
				}
			} else if !inserted || err != nil || int64(len(localCapacityJournal(t, dir))) != size {
				t.Fatalf("exact byte boundary rejected: %v, %v", inserted, err)
			}
		})
	}
}

func TestLocalCapacityReplayCountsUniqueIDsAndRepairsOnlyTail(t *testing.T) {
	requireLocalCapacityPlatform(t)
	dir := t.TempDir()
	first := Event{ID: "same", Model: "first-wins"}
	duplicate := Event{ID: "same", Model: "must-not-replace"}
	second := Event{ID: "second"}
	committed := localCapacityRecord(t, journalEntry{Event: &first})
	committed = append(committed, localCapacityRecord(t, journalEntry{Event: &duplicate})...)
	committed = append(committed, localCapacityRecord(t, journalEntry{Event: &second})...)
	withTail := append(append([]byte{}, committed...), []byte(`{"event":`)...)
	if err := os.WriteFile(filepath.Join(dir, journalFilename), withTail, 0600); err != nil {
		t.Fatal(err)
	}
	storage := openLocalCapacityFixture(t, dir, localCapacityLimits{journalBytes: int64(len(withTail)), retainedEvents: 2})
	if after := localCapacityJournal(t, dir); !bytes.Equal(after, committed) {
		t.Fatal("tail repair did not preserve every completed record, including duplicates")
	}
	status := localCapacitySnapshot(t, storage)
	if status.RetainedEvents != 2 || status.JournalBytes != int64(len(committed)) || storage.events["same"].Model != "first-wins" {
		t.Fatalf("replay capacity/dedup accounting: %+v", status)
	}
}

// Cancel based on replay progress, not a fragile count of context checks (the
// capacity preflight is a separate pass) or scheduler timing.
type localCapacityReplayCancelContext struct {
	context.Context
	storage *fileStore
	cancel  context.CancelFunc
}

func (ctx *localCapacityReplayCancelContext) Err() error {
	if len(ctx.storage.events) > 0 {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func TestLocalCapacityReplayCancellationAfterPreflightDoesNotRepair(t *testing.T) {
	requireLocalCapacityPlatform(t)
	dir := t.TempDir()
	event := Event{ID: "committed"}
	committed := localCapacityRecord(t, journalEntry{Event: &event})
	withTail := append(append([]byte{}, committed...), []byte(`{"event":`)...)
	path := filepath.Join(dir, journalFilename)
	if err := os.WriteFile(path, withTail, 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireJournalLock(filepath.Join(dir, journalFilename+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	limits := localCapacityLimits{journalBytes: 4096, retainedEvents: 2}
	storage := &fileStore{lock: lock, limits: limits, events: map[string]Event{}, cache: map[string]map[string]json.RawMessage{}}
	t.Cleanup(func() {
		if errClose := storage.Close(); errClose != nil {
			t.Error(errClose)
		}
	})
	storage.file, err = os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = storage.preflightCapacity(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err = storage.replay(&localCapacityReplayCancelContext{Context: ctx, storage: storage, cancel: cancel}); !errors.Is(err, context.Canceled) {
		t.Fatalf("replay after preflight ignored cancellation: %v", err)
	}
	if after := localCapacityJournal(t, dir); !bytes.Equal(after, withTail) {
		t.Fatal("canceled replay repaired the tail")
	}
	if err = storage.Close(); err != nil {
		t.Fatal(err)
	}
	assertLocalCapacityUnlocked(t, dir)
	reopened := openLocalCapacityFixture(t, dir, limits)
	if after := localCapacityJournal(t, dir); !bytes.Equal(after, committed) || len(reopened.events) != 1 {
		t.Fatal("reopen after replay cancellation did not safely recover committed history")
	}
}

func TestLocalCapacityWorkerReportsWriteFailure(t *testing.T) {
	storage := openLocalCapacityFixture(t, t.TempDir(), localCapacityLimits{journalBytes: 4096, retainedEvents: 1})
	service := &Store{store: storage, closed: make(chan struct{})}
	service.startWorker(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := service.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, id := range []string{"retained", "rejected"} {
		service.Consume(context.Background(), fixturePayload(fixtureRecord(id, at)))
	}
	if err := service.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if service.writeFailures.Load() != 1 || service.pendingEvents.Load() != 0 {
		t.Fatalf("capacity did not flow through existing backend failure health: failures %d, pending %d", service.writeFailures.Load(), service.pendingEvents.Load())
	}
	page, err := service.Events(context.Background(), Filter{}, 10, 0)
	if err != nil || page.Total != 1 || len(page.Events) != 1 || page.Events[0].ID != "retained" {
		t.Fatalf("worker capacity failure made earlier history unreadable: %+v, %v", page, err)
	}
	if status, ok := service.LocalCapacity(); !ok || status.RejectedWrites != 1 {
		t.Fatalf("worker rejection missing from capacity snapshot: %+v, %v", status, ok)
	}
}

func TestLocalCapacityCancellationAndCloseTakePrecedence(t *testing.T) {
	storage := openLocalCapacityFixture(t, t.TempDir(), localCapacityLimits{journalBytes: 4096, retainedEvents: 1})
	if _, err := storage.Insert(context.Background(), Event{ID: "full"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := storage.Insert(ctx, Event{ID: "too-many"}); !errors.Is(err, context.Canceled) || errors.Is(err, ErrCapacity) {
		t.Fatalf("capacity hid cancellation: %v", err)
	}
	if status := localCapacitySnapshot(t, storage); status.RejectedWrites != 0 {
		t.Fatalf("canceled write counted as capacity rejection: %+v", status)
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Insert(context.Background(), Event{ID: "too-many"}); !errors.Is(err, ErrClosed) || errors.Is(err, ErrCapacity) {
		t.Fatalf("capacity hid closed store: %v", err)
	}
	if status := localCapacitySnapshot(t, storage); status.RetainedEvents != 1 {
		t.Fatalf("close discarded nonblocking historical snapshot: %+v", status)
	}
}

func TestLocalCapacitySnapshotNeverWaitsForStoreLocks(t *testing.T) {
	storage := openLocalCapacityFixture(t, t.TempDir(), localCapacityLimits{journalBytes: 4096, retainedEvents: 3})
	service := &Store{store: storage}
	want := localCapacitySnapshot(t, storage)
	// fileStore.mu remains held through fsync; holding both mutexes demonstrates
	// that status does not wait for fsync or service lifecycle bookkeeping.
	storage.mu.Lock()
	defer storage.mu.Unlock()
	service.mu.Lock()
	defer service.mu.Unlock()
	result := make(chan LocalCapacityStatus, 1)
	go func() {
		status, _ := service.LocalCapacity()
		result <- status
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second) // Deadlock watchdog only.
	defer cancel()
	select {
	case got := <-result:
		if got != want {
			t.Fatalf("incoherent capacity snapshot: got %+v, want %+v", got, want)
		}
		got.JournalBytes++
		if next, _ := service.LocalCapacity(); next != want {
			t.Fatal("mutating a returned status modified the stored snapshot")
		}
	case <-ctx.Done():
		t.Fatal("capacity status blocked on a store mutex")
	}
}

func TestLocalCapacitySnapshotConcurrentWrites(t *testing.T) {
	storage := openLocalCapacityFixture(t, t.TempDir(), localCapacityLimits{journalBytes: 16384, retainedEvents: 8})
	service := &Store{store: storage}
	start := make(chan struct{})
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			<-start
			var previous LocalCapacityStatus
			for range 1000 {
				status, ok := service.LocalCapacity()
				if !ok || status.JournalBytes < previous.JournalBytes || status.RetainedEvents < previous.RetainedEvents || status.RejectedWrites < previous.RejectedWrites || status.JournalBytes > status.JournalByteLimit || status.RetainedEvents > status.RetainedEventLimit {
					t.Errorf("inconsistent concurrent snapshot: previous %+v, current %+v", previous, status)
					return
				}
				previous = status
			}
		}()
	}
	close(start)
	for i := range 16 {
		inserted, err := storage.Insert(context.Background(), Event{ID: fmt.Sprint(i)})
		if i < 8 && (!inserted || err != nil) || i >= 8 && (inserted || !errors.Is(err, ErrCapacity)) {
			t.Errorf("write %d: %v, %v", i, inserted, err)
		}
	}
	readers.Wait()
	if status := localCapacitySnapshot(t, storage); status.RetainedEvents != 8 || status.RejectedWrites != 8 {
		t.Fatalf("lost concurrent status updates: %+v", status)
	}
}

func localCapacityPageFixture(t testing.TB, count int) (*fileStore, []Event) {
	t.Helper()
	requireLocalCapacityPlatform(t)
	dir := t.TempDir()
	events := make([]Event, count)
	var journal []byte
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range events {
		events[i] = Event{ID: fmt.Sprintf("event-%04d", i), RequestedAt: base.Add(time.Duration(i%11) * time.Second), Provider: fmt.Sprintf("provider-%d", i%3), Model: fmt.Sprintf("model-%d", i%2), Failed: i%5 == 0}
		journal = append(journal, localCapacityRecord(t, journalEntry{Event: &events[i]})...)
	}
	if err := os.WriteFile(filepath.Join(dir, journalFilename), journal, 0600); err != nil {
		t.Fatal(err)
	}
	return openLocalCapacityFixture(t, dir, localCapacityLimits{journalBytes: int64(len(journal) + 1024), retainedEvents: count}), events
}

func TestLocalCapacityEventsPageMatchesTimestampIDOrder(t *testing.T) {
	storage, events := localCapacityPageFixture(t, 64)
	for _, filter := range []Filter{
		{}, {Provider: "provider-1"}, {Model: "model-1", Status: "failed"}, {Status: "success"},
		{From: events[3].RequestedAt, To: events[8].RequestedAt}, {Provider: "absent"},
	} {
		var expected []Event
		for _, event := range events {
			if filter.matches(event) {
				expected = append(expected, event)
			}
		}
		sort.Slice(expected, func(i, j int) bool {
			if expected[i].RequestedAt.Equal(expected[j].RequestedAt) {
				return expected[i].ID > expected[j].ID
			}
			return expected[i].RequestedAt.After(expected[j].RequestedAt)
		})
		for _, offset := range []int{0, 1, 5, 32, 63, 64, 100, math.MaxInt} {
			for _, limit := range []int{0, 1, 5, 64, math.MaxInt} {
				page, total, err := storage.Events(context.Background(), filter, limit, offset)
				start := min(offset, len(expected))
				end := start + min(limit, len(expected)-start)
				want := append([]Event{}, expected[start:end]...)
				if err != nil || total != int64(len(expected)) || !reflect.DeepEqual(page, want) {
					t.Fatalf("filter %+v limit %d offset %d: got %v / %d / %v, want %v / %d", filter, limit, offset, page, total, err, want, len(expected))
				}
				if cap(page) != len(page) {
					t.Fatalf("bounded page retains a larger backing array: len %d cap %d", len(page), cap(page))
				}
			}
		}
	}
	page, _, err := storage.Events(context.Background(), Filter{}, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	id := page[0].ID
	page[0].Model = "caller-modified"
	if storage.events[id].Model == "caller-modified" {
		t.Fatal("mutating a returned event changed retained history")
	}
	for _, pagination := range [][2]int{{-1, 0}, {1, -1}} {
		if _, _, err = storage.Events(context.Background(), Filter{}, pagination[0], pagination[1]); err == nil {
			t.Fatalf("negative pagination accepted: %v", pagination)
		}
	}
}

func BenchmarkLocalCapacityEventsSmallPage(b *testing.B) {
	storage, _ := localCapacityPageFixture(b, 2048)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		page, total, err := storage.Events(ctx, Filter{}, 5, 0)
		if err != nil || len(page) != 5 || total != 2048 {
			b.Fatalf("page: %d, %d, %v", len(page), total, err)
		}
	}
}
