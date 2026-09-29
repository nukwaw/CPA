//go:build linux || darwin || windows

package usagepersist

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const fileStoreHelperEnv = "CPA_TEST_JOURNAL_LOCK_HELPER"
const fileStoreHelperReplyPrefix = "JOURNAL_LOCK_REPLY "

type fileStoreHelperCommand struct {
	Op          string
	Dir         string
	ID          string
	CancelCheck int
}

type fileStoreHelperReply struct {
	Error    string
	Locked   bool
	Canceled bool
	Events   []string
}

// TestFileStoreLockHelperProcess is a real second process, not another store in
// the test process. Commands/replies synchronize all ownership transitions.
func TestFileStoreLockHelperProcess(t *testing.T) {
	if os.Getenv(fileStoreHelperEnv) != "1" {
		return
	}
	var storage *fileStore
	defer func() {
		if storage != nil {
			if err := storage.Close(); err != nil {
				t.Errorf("helper cleanup: %v", err)
			}
		}
	}()
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var command fileStoreHelperCommand
		if err := json.Unmarshal(scanner.Bytes(), &command); err != nil {
			t.Fatal(err)
		}
		var reply fileStoreHelperReply
		var err error
		switch command.Op {
		case "lock-only":
			var lock *journalLock
			lock, err = acquireJournalLock(filepath.Join(command.Dir, journalFilename+".lock"))
			if err == nil {
				storage = &fileStore{lock: lock}
			}
		case "open":
			if storage != nil {
				t.Fatal("helper already owns a store")
			}
			ctx, cancel := context.WithCancel(context.Background())
			var openContext context.Context = ctx
			if command.CancelCheck > 0 {
				openContext = &fileStoreCancelCheckContext{Context: ctx, cancel: cancel, remaining: command.CancelCheck}
			}
			storage, err = openFileStore(openContext, command.Dir)
			cancel()
		case "insert":
			_, err = storage.Insert(context.Background(), Event{ID: command.ID, RequestedAt: time.Unix(1700000000, 0).UTC()})
		case "partial":
			// Simulate a crash interrupting the current owner's next append.
			_, err = storage.file.WriteString(`{"event":`)
			if err == nil {
				err = storage.file.Sync()
			}
		case "events":
			var events []Event
			events, _, err = storage.Events(context.Background(), Filter{}, 100, 0)
			for _, event := range events {
				reply.Events = append(reply.Events, event.ID)
			}
			sort.Strings(reply.Events)
		case "close", "quit":
			if storage != nil {
				err = storage.Close()
				storage = nil
			}
		case "close-broken-file":
			if err = storage.file.Close(); err != nil {
				t.Fatal(err)
			}
			err = storage.Close()
			storage = nil
		default:
			t.Fatalf("unknown helper command %q", command.Op)
		}
		if err != nil {
			reply.Error = err.Error()
			reply.Locked = errors.Is(err, ErrLocked)
			reply.Canceled = errors.Is(err, context.Canceled)
		}
		data, errMarshal := json.Marshal(reply)
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		if _, errWrite := fmt.Fprintln(os.Stdout, fileStoreHelperReplyPrefix+string(data)); errWrite != nil {
			t.Fatal(errWrite)
		}
		if command.Op == "quit" {
			return
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}

// Cancellation is triggered by a context check, never by a sleep or scheduler
// race. This lets startup tests cancel after ownership and replay have begun.
type fileStoreCancelCheckContext struct {
	context.Context
	cancel    context.CancelFunc
	remaining int
}

func (ctx *fileStoreCancelCheckContext) Err() error {
	ctx.remaining--
	if ctx.remaining <= 0 {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

type fileStoreProcess struct {
	cmd    *exec.Cmd
	input  io.WriteCloser
	output *bufio.Reader
	stderr bytes.Buffer
	waited bool
}

func newFileStoreProcess(t *testing.T) *fileStoreProcess {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// This is only a deadlock watchdog; pipes synchronize the actual test.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	process := &fileStoreProcess{cmd: exec.CommandContext(ctx, executable, "-test.run=^TestFileStoreLockHelperProcess$")}
	process.cmd.Env = append(os.Environ(), fileStoreHelperEnv+"=1")
	process.cmd.Stderr = &process.stderr
	process.input, err = process.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := process.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	process.output = bufio.NewReader(output)
	if err = process.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !process.waited {
			if errKill := process.cmd.Process.Kill(); errKill != nil && !errors.Is(errKill, os.ErrProcessDone) {
				t.Errorf("kill helper during cleanup: %v", errKill)
			}
			_ = process.cmd.Wait()
			process.waited = true
		}
		_ = process.input.Close()
	})
	return process
}

func (process *fileStoreProcess) command(t *testing.T, command fileStoreHelperCommand) fileStoreHelperReply {
	t.Helper()
	data, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fmt.Fprintln(process.input, string(data)); err != nil {
		t.Fatalf("send helper command: %v", err)
	}
	for {
		line, errRead := process.output.ReadString('\n')
		if errRead != nil {
			// Wait before inspecting stderr, which os/exec fills concurrently.
			errWait := process.cmd.Wait()
			process.waited = true
			t.Fatalf("read helper response: %v; exit: %v; stderr: %s", errRead, errWait, process.stderr.String())
		}
		if !strings.HasPrefix(line, fileStoreHelperReplyPrefix) {
			continue
		}
		var reply fileStoreHelperReply
		if err = json.Unmarshal([]byte(strings.TrimPrefix(line, fileStoreHelperReplyPrefix)), &reply); err != nil {
			t.Fatal(err)
		}
		return reply
	}
}

func (process *fileStoreProcess) ok(t *testing.T, command fileStoreHelperCommand) fileStoreHelperReply {
	t.Helper()
	reply := process.command(t, command)
	if reply.Error != "" {
		t.Fatalf("helper %s failed: %s", command.Op, reply.Error)
	}
	return reply
}

func (process *fileStoreProcess) quit(t *testing.T) {
	t.Helper()
	process.ok(t, fileStoreHelperCommand{Op: "quit"})
	err := process.cmd.Wait()
	process.waited = true
	if err != nil {
		t.Fatalf("helper exit: %v; stderr: %s", err, process.stderr.String())
	}
}

func (process *fileStoreProcess) kill(t *testing.T) {
	t.Helper()
	if err := process.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err := process.cmd.Wait()
	process.waited = true
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		t.Fatalf("killed helper exit: %v", err)
	}
}

func readFileStoreJournal(t *testing.T, dir string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, journalFilename))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestFileStoreProcessLockRejectsCompetitorBeforeRecovery(t *testing.T) {
	dir := t.TempDir()
	owner, contender := newFileStoreProcess(t), newFileStoreProcess(t)
	owner.ok(t, fileStoreHelperCommand{Op: "lock-only", Dir: dir})
	if reply := contender.command(t, fileStoreHelperCommand{Op: "open", Dir: dir}); !reply.Locked {
		t.Fatalf("competing process bypassed startup lock: %+v", reply)
	}
	if _, err := os.Stat(filepath.Join(dir, journalFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("contender opened the data file before acquiring ownership: %v", err)
	}
	owner.ok(t, fileStoreHelperCommand{Op: "close"})
	owner.ok(t, fileStoreHelperCommand{Op: "open", Dir: dir})
	owner.ok(t, fileStoreHelperCommand{Op: "insert", ID: "committed"})
	owner.ok(t, fileStoreHelperCommand{Op: "partial"})
	before := readFileStoreJournal(t, dir)

	reply := contender.command(t, fileStoreHelperCommand{Op: "open", Dir: dir})
	if !reply.Locked {
		t.Fatalf("competing process did not return ErrLocked: %+v", reply)
	}
	if after := readFileStoreJournal(t, dir); !bytes.Equal(before, after) {
		t.Fatal("competing process changed the owned journal, including its incomplete tail")
	}
	// A rejected contender can use a different directory without waiting for
	// the first writer; there is no cross-store global mutex.
	contender.ok(t, fileStoreHelperCommand{Op: "open", Dir: t.TempDir()})
	contender.ok(t, fileStoreHelperCommand{Op: "insert", ID: "independent"})
	contender.quit(t)
	owner.quit(t)
}

func TestFileStoreProcessCrashUnlocksAndRecoversCommittedEvents(t *testing.T) {
	dir := t.TempDir()
	owner, next := newFileStoreProcess(t), newFileStoreProcess(t)
	owner.ok(t, fileStoreHelperCommand{Op: "open", Dir: dir})
	owner.ok(t, fileStoreHelperCommand{Op: "insert", ID: "before-crash"})
	committed := readFileStoreJournal(t, dir)
	owner.ok(t, fileStoreHelperCommand{Op: "partial"})
	owner.kill(t)

	// The lock file deliberately survives the crashed owner.
	if _, err := os.Stat(filepath.Join(dir, journalFilename+".lock")); err != nil {
		t.Fatal(err)
	}
	next.ok(t, fileStoreHelperCommand{Op: "open", Dir: dir})
	if after := readFileStoreJournal(t, dir); !bytes.Equal(committed, after) {
		t.Fatal("recovery did not preserve exactly the committed prefix")
	}
	if reply := next.ok(t, fileStoreHelperCommand{Op: "events"}); !reflect.DeepEqual(reply.Events, []string{"before-crash"}) {
		t.Fatalf("lost committed events: %+v", reply)
	}
	next.ok(t, fileStoreHelperCommand{Op: "insert", ID: "after-recovery"})
	next.ok(t, fileStoreHelperCommand{Op: "close"})
	next.ok(t, fileStoreHelperCommand{Op: "open", Dir: dir})
	if reply := next.ok(t, fileStoreHelperCommand{Op: "events"}); !reflect.DeepEqual(reply.Events, []string{"after-recovery", "before-crash"}) {
		t.Fatalf("recovered journal did not remain appendable: %+v", reply)
	}
	next.quit(t)
}

func TestFileStoreProcessCleanClosePreservesLockInode(t *testing.T) {
	dir := t.TempDir()
	first, second := newFileStoreProcess(t), newFileStoreProcess(t)
	first.ok(t, fileStoreHelperCommand{Op: "open", Dir: dir})
	first.ok(t, fileStoreHelperCommand{Op: "insert", ID: "retained"})
	lockPath := filepath.Join(dir, journalFilename+".lock")
	before, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	first.ok(t, fileStoreHelperCommand{Op: "close"})
	second.ok(t, fileStoreHelperCommand{Op: "open", Dir: dir})
	after, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("unlock replaced the shared lock inode")
	}
	if reply := first.command(t, fileStoreHelperCommand{Op: "open", Dir: dir}); !reply.Locked {
		t.Fatalf("cleanly closed owner bypassed successor's lock: %+v", reply)
	}
	if reply := second.ok(t, fileStoreHelperCommand{Op: "events"}); !reflect.DeepEqual(reply.Events, []string{"retained"}) {
		t.Fatalf("clean close lost history: %+v", reply)
	}
	second.ok(t, fileStoreHelperCommand{Op: "close"})
	first.ok(t, fileStoreHelperCommand{Op: "open", Dir: dir})
	first.quit(t)
	second.quit(t)
}

func TestFileStoreProcessOpenFailureUnlocks(t *testing.T) {
	for _, failure := range []string{"data-open", "replay"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, journalFilename)
			badRecord := []byte("{\"event\":{\"requested_at\":\"secret-journal-canary\"}}\n")
			if failure == "data-open" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, badRecord, 0600); err != nil {
				t.Fatal(err)
			}
			first, second := newFileStoreProcess(t), newFileStoreProcess(t)
			for _, process := range []*fileStoreProcess{first, second} {
				reply := process.command(t, fileStoreHelperCommand{Op: "open", Dir: dir})
				if reply.Error == "" || reply.Locked {
					t.Fatalf("failed open leaked a lock or accepted invalid data: %+v", reply)
				}
				if strings.Contains(reply.Error, "secret-journal-canary") {
					t.Fatal("replay error disclosed journal contents")
				}
			}
			if failure == "data-open" {
				// Remove only this test's deliberately created empty directory.
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else if after := readFileStoreJournal(t, dir); !bytes.Equal(after, badRecord) {
				t.Fatal("failed replay changed a corrupt complete record")
			}
			if err := os.WriteFile(path, nil, 0600); err != nil {
				t.Fatal(err)
			}
			second.ok(t, fileStoreHelperCommand{Op: "open", Dir: dir})
			second.ok(t, fileStoreHelperCommand{Op: "insert", ID: "after-failure"})
			first.quit(t)
			second.quit(t)
		})
	}
}

func TestFileStoreProcessCloseErrorStillUnlocks(t *testing.T) {
	dir := t.TempDir()
	first, second := newFileStoreProcess(t), newFileStoreProcess(t)
	first.ok(t, fileStoreHelperCommand{Op: "open", Dir: dir})
	if reply := first.command(t, fileStoreHelperCommand{Op: "close-broken-file"}); reply.Error == "" {
		t.Fatal("expected a data-file close error")
	}
	second.ok(t, fileStoreHelperCommand{Op: "open", Dir: dir})
	first.quit(t)
	second.quit(t)
}

func TestFileStoreProcessCanceledStartupAndReplayUnlock(t *testing.T) {
	first, second := newFileStoreProcess(t), newFileStoreProcess(t)
	dir := filepath.Join(t.TempDir(), "not-created")
	if reply := first.command(t, fileStoreHelperCommand{Op: "open", Dir: dir, CancelCheck: 1}); !reply.Canceled {
		t.Fatalf("pre-canceled open: %+v", reply)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pre-canceled open created storage: %v", err)
	}
	if reply := first.command(t, fileStoreHelperCommand{Op: "open", Dir: dir, CancelCheck: 2}); !reply.Canceled {
		t.Fatalf("cancellation immediately after acquiring ownership: %+v", reply)
	}
	if _, err := os.Stat(filepath.Join(dir, journalFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("open canceled after locking created the journal: %v", err)
	}
	second.ok(t, fileStoreHelperCommand{Op: "open", Dir: dir})
	second.ok(t, fileStoreHelperCommand{Op: "insert", ID: "before-cancel"})
	committed := readFileStoreJournal(t, dir)
	second.ok(t, fileStoreHelperCommand{Op: "partial"})
	second.ok(t, fileStoreHelperCommand{Op: "close"})
	before := readFileStoreJournal(t, dir)
	// Checks 1/2 precede data open; checks 3/4 surround the first replay
	// record; check 5 cancels before reading/repairing the incomplete tail.
	if reply := first.command(t, fileStoreHelperCommand{Op: "open", Dir: dir, CancelCheck: 5}); !reply.Canceled {
		t.Fatalf("replay cancellation: %+v", reply)
	}
	if after := readFileStoreJournal(t, dir); !bytes.Equal(before, after) {
		t.Fatal("canceled replay truncated the journal")
	}
	second.ok(t, fileStoreHelperCommand{Op: "open", Dir: dir})
	if after := readFileStoreJournal(t, dir); !bytes.Equal(committed, after) {
		t.Fatal("reopen after cancellation did not safely repair the tail")
	}
	first.quit(t)
	second.quit(t)
}

func TestFileStoreCanonicalJournalDoesNotTouchPrototype(t *testing.T) {
	dir := t.TempDir()
	prototypePath := filepath.Join(dir, "telemetry.jsonl")
	prototype := []byte("old user data that must not be replayed or removed\n")
	if err := os.WriteFile(prototypePath, prototype, 0600); err != nil {
		t.Fatal(err)
	}
	process := newFileStoreProcess(t)
	process.ok(t, fileStoreHelperCommand{Op: "open", Dir: dir})
	if reply := process.ok(t, fileStoreHelperCommand{Op: "events"}); len(reply.Events) != 0 {
		t.Fatalf("loaded prototype journal: %+v", reply)
	}
	process.ok(t, fileStoreHelperCommand{Op: "insert", ID: "canonical"})
	process.quit(t)
	if len(readFileStoreJournal(t, dir)) == 0 {
		t.Fatal("canonical usage journal is empty")
	}
	after, err := os.ReadFile(prototypePath)
	if err != nil || !bytes.Equal(after, prototype) {
		t.Fatalf("prototype user data changed: %q, %v", after, err)
	}
}

type fileStoreOperation struct {
	name string
	run  func(context.Context, *fileStore) error
}

func fileStoreOperations(t *testing.T) []fileStoreOperation {
	t.Helper()
	return []fileStoreOperation{
		{"insert", func(ctx context.Context, s *fileStore) error {
			_, err := s.Insert(ctx, Event{ID: "unexpected"})
			return err
		}},
		{"duplicate-insert", func(ctx context.Context, s *fileStore) error {
			_, err := s.Insert(ctx, Event{ID: "committed"})
			return err
		}},
		{"walk", func(ctx context.Context, s *fileStore) error {
			return s.Walk(ctx, Filter{}, func(Event) error { t.Error("unexpected walk callback"); return nil })
		}},
		{"events", func(ctx context.Context, s *fileStore) error {
			_, _, err := s.Events(ctx, Filter{}, 10, 0)
			return err
		}},
		{"cache", func(ctx context.Context, s *fileStore) error {
			_, err := s.Cache(ctx, "test")
			return err
		}},
		{"update-cache", func(ctx context.Context, s *fileStore) error {
			return s.UpdateCache(ctx, []cacheUpdate{{Namespace: "test", Key: "key", Value: json.RawMessage(`true`)}})
		}},
		{"mutate-cache", func(ctx context.Context, s *fileStore) error {
			return s.MutateCache(ctx, "test", "key", func(json.RawMessage) (json.RawMessage, error) {
				t.Error("unexpected mutation callback")
				return json.RawMessage(`true`), nil
			})
		}},
	}
}

type fileStoreEnteredContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (ctx *fileStoreEnteredContext) Err() error {
	// Capture the initial live context before signaling. The caller cancels
	// only after this first check, so a post-mutex check is required to pass.
	err := ctx.Context.Err()
	ctx.once.Do(func() { close(ctx.entered) })
	return err
}

func TestFileStoreOperationsHonorCancellationAndClose(t *testing.T) {
	dir := t.TempDir()
	storage, err := openFileStore(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if errClose := storage.Close(); errClose != nil {
			t.Error(errClose)
		}
	})
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, operation := range fileStoreOperations(t) {
		t.Run("empty-canceled/"+operation.name, func(t *testing.T) {
			if err := operation.run(canceled, storage); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled empty operation: %v", err)
			}
		})
	}
	if _, err = storage.Insert(context.Background(), Event{ID: "committed"}); err != nil {
		t.Fatal(err)
	}
	before := readFileStoreJournal(t, dir)
	for _, operation := range fileStoreOperations(t) {
		t.Run("waiting-canceled/"+operation.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered := &fileStoreEnteredContext{Context: ctx, entered: make(chan struct{})}
			storage.mu.Lock()
			result := make(chan error, 1)
			go func() { result <- operation.run(entered, storage) }()
			<-entered.entered
			cancel()
			storage.mu.Unlock()
			if err := <-result; !errors.Is(err, context.Canceled) {
				t.Fatalf("operation canceled while waiting for the instance mutex: %v", err)
			}
		})
	}
	ctx, cancelMutation := context.WithCancel(context.Background())
	err = storage.MutateCache(ctx, "test", "key", func(json.RawMessage) (json.RawMessage, error) {
		cancelMutation()
		return json.RawMessage(`true`), nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("mutation callback cancellation: %v", err)
	}
	if after := readFileStoreJournal(t, dir); !bytes.Equal(before, after) {
		t.Fatal("a canceled operation wrote to the journal")
	}
	if err = storage.Close(); err != nil {
		t.Fatal(err)
	}
	for _, operation := range fileStoreOperations(t) {
		t.Run("closed/"+operation.name, func(t *testing.T) {
			if err := operation.run(context.Background(), storage); !errors.Is(err, ErrClosed) {
				t.Fatalf("closed operation: %v", err)
			}
		})
	}
}
