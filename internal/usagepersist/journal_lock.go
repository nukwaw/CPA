package usagepersist

import (
	"errors"
	"fmt"
	"os"
)

// ErrLocked means another writer already owns the local usage journal.
var ErrLocked = errors.New("usage journal is locked by another writer")

// journalLock uses a separate, persistent inode so opening the data file (and
// especially repairing its tail) happens only after exclusive ownership is held.
// Never remove the lock file: waiters may already have the same inode open.
type journalLock struct {
	file *os.File
}

func acquireJournalLock(path string) (*journalLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open usage journal lock: %w", err)
	}
	if err = lockJournalFile(file); err != nil {
		err = fmt.Errorf("lock usage journal: %w", err)
		if errClose := file.Close(); errClose != nil {
			err = errors.Join(err, fmt.Errorf("close unacquired usage journal lock: %w", errClose))
		}
		return nil, err
	}
	return &journalLock{file: file}, nil
}

func (lock *journalLock) Close() error {
	if lock.file == nil {
		return nil
	}
	var err error
	if errUnlock := unlockJournalFile(lock.file); errUnlock != nil {
		err = fmt.Errorf("unlock usage journal: %w", errUnlock)
	}
	// Closing the handle also releases the OS lock, including after an unlock
	// error. The OS performs this cleanup automatically if the process crashes.
	if errClose := lock.file.Close(); errClose != nil {
		err = errors.Join(err, fmt.Errorf("close usage journal lock: %w", errClose))
	}
	lock.file = nil
	return err
}
