package usagepersist

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrCapacity means the local journal cannot accept the requested history within
// its supported bounds. It is distinct from I/O failure and never permits pruning
// committed history. Callers should use errors.Is; reads remain available after a
// runtime rejection. PostgreSQL does not use these local limits.
var ErrCapacity = errors.New("local usage persistence capacity exceeded")

const (
	// These are supported local-backend limits, not runtime settings. The byte
	// bound includes event and metadata records, duplicates and incomplete tails;
	// it bounds replay input and retained string/cache data. The independent event
	// bound limits map overhead and scan work even for unusually small records.
	// Neither is an exact Go heap/RSS budget: decoding and queries need headroom.
	localJournalByteLimit       int64 = 64 << 20
	localRetainedEventLimit           = 100_000
	localCapacityJournalBytes         = "journal_bytes"
	localCapacityRetainedEvents       = "retained_events"
)

// localCapacityLimits is private so tests can exercise boundaries with tiny
// fixtures, without adding a setting or mutable process-global test override.
type localCapacityLimits struct {
	journalBytes   int64
	retainedEvents int
}

func supportedLocalCapacity() localCapacityLimits {
	return localCapacityLimits{journalBytes: localJournalByteLimit, retainedEvents: localRetainedEventLimit}
}

// LocalCapacityStatus is an immutable snapshot of the last completed local-store
// operation. JournalBytes includes metadata as well as events. The counters equal
// their limits at a full boundary; a record can also be rejected before that point
// if it would not fit. RejectedWrites counts only ErrCapacity write rejections in
// this open lifetime, and LastRejectedLimit is "journal_bytes" or
// "retained_events" (empty before any rejection). Both remain sticky after a
// later successful smaller write, and reset on reopen. They do not mean all
// writes are blocked: metadata may still fit after the event limit is reached.
type LocalCapacityStatus struct {
	JournalBytes       int64  `json:"journal_bytes"`
	JournalByteLimit   int64  `json:"journal_byte_limit"`
	RetainedEvents     int    `json:"retained_events"`
	RetainedEventLimit int    `json:"retained_event_limit"`
	RejectedWrites     uint64 `json:"rejected_writes"`
	LastRejectedLimit  string `json:"last_rejected_limit,omitempty"`
}

// LocalCapacity never takes a mutex or performs I/O, including while an append
// is stuck in fsync. The bool is false for nil or non-file stores. A closed file
// store still returns its last snapshot; this method is not a liveness check.
func (s *Store) LocalCapacity() (LocalCapacityStatus, bool) {
	if s == nil {
		return LocalCapacityStatus{}, false
	}
	storage, ok := s.store.(*fileStore)
	if !ok || storage == nil {
		return LocalCapacityStatus{}, false
	}
	status := storage.capacity.Load()
	if status == nil {
		return LocalCapacityStatus{}, false
	}
	return *status, true
}

// publishCapacity runs only before publication or while holding s.mu. Readers
// use one atomic load, so byte/event counts always come from the same operation.
func (s *fileStore) publishCapacity(rejectedLimit string) {
	status := LocalCapacityStatus{
		JournalBytes: s.journalBytes, JournalByteLimit: s.limits.journalBytes,
		RetainedEvents: len(s.events), RetainedEventLimit: s.limits.retainedEvents,
	}
	if previous := s.capacity.Load(); previous != nil {
		status.RejectedWrites = previous.RejectedWrites
		status.LastRejectedLimit = previous.LastRejectedLimit
	}
	if rejectedLimit != "" {
		status.RejectedWrites++
		status.LastRejectedLimit = rejectedLimit
	}
	s.capacity.Store(&status)
}

func localCapacityError(resource string, limit int64) error {
	return fmt.Errorf("%w: %s limit %d", ErrCapacity, resource, limit)
}

func (s *fileStore) rejectCapacity(resource string, limit int64) error {
	s.publishCapacity(resource)
	return localCapacityError(resource, limit)
}

// preflightCapacity runs under the lifetime process lock, before replay loads
// any Events/cache values or repairs a tail. Oversize journals are rejected by
// stat before any read. The bounded first pass retains only unique event IDs,
// so an over-count journal also fails without partially loading its history.
// Duplicate IDs retain the existing first-event-wins semantics. A second pass
// is deliberate: no repair/mutation is allowed before both limits are known.
func (s *fileStore) preflightCapacity(ctx context.Context) error {
	info, err := s.file.Stat()
	if err != nil {
		return fmt.Errorf("stat usage journal before replay: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if info.Size() > s.limits.journalBytes {
		return localCapacityError(localCapacityJournalBytes, s.limits.journalBytes)
	}
	reader := bufio.NewReader(io.LimitReader(s.file, s.limits.journalBytes+1))
	ids := make(map[string]struct{})
	var offset int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, errRead := reader.ReadBytes('\n')
		if err := ctx.Err(); err != nil {
			return err
		}
		if int64(len(line)) > s.limits.journalBytes-offset {
			return localCapacityError(localCapacityJournalBytes, s.limits.journalBytes)
		}
		if errors.Is(errRead, io.EOF) {
			// Replay alone may repair an incomplete final record, after this
			// pass has verified all completed history fits the supported limits.
			if _, errSeek := s.file.Seek(0, io.SeekStart); errSeek != nil {
				return fmt.Errorf("rewind usage journal after capacity check: %w", errSeek)
			}
			return ctx.Err()
		}
		if errRead != nil {
			return fmt.Errorf("read usage journal for capacity check: %w", errRead)
		}
		var entry struct {
			Event *struct {
				ID string `json:"id"`
			} `json:"event"`
		}
		if errDecode := json.Unmarshal(line, &entry); errDecode != nil {
			return fmt.Errorf("invalid usage journal record at byte %d", offset)
		}
		if entry.Event != nil {
			if _, exists := ids[entry.Event.ID]; !exists {
				if len(ids) >= s.limits.retainedEvents {
					return localCapacityError(localCapacityRetainedEvents, int64(s.limits.retainedEvents))
				}
				ids[entry.Event.ID] = struct{}{}
			}
		}
		offset += int64(len(line))
	}
}
