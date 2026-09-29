//go:build !linux && !darwin && !windows

package usagepersist

import (
	"errors"
	"os"
)

func lockJournalFile(_ *os.File) error {
	return errors.New("usage journal process locking is unsupported on this platform")
}

func unlockJournalFile(_ *os.File) error {
	return nil
}
