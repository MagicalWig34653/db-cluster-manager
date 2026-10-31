// SPDX-License-Identifier: AGPL-3.0-only

package restic

import (
	"errors"
	"testing"
)

// Lab E17: restic printed a Go stack trace (frames cmd/restic/lock.go and
// OpenRepository) for an unwritable cache directory and the word "lock" made
// the wrapper report a locked repository.
func TestClassifyDoesNotMistakeStackFramesForALock(t *testing.T) {
	stderr := []byte("unable to open cache: mkdir /root/.cache: read-only file system\n" +
		"github.com/restic/restic/internal/global.OpenRepository\n\t/restic/internal/global/global.go:333\n" +
		"main.internalOpenWithLocked\n\t/restic/cmd/restic/lock.go:12\n")
	err := classify(errors.New("exit status 1"), stderr)
	if errors.Is(err, ErrLocked) || errors.Is(err, ErrRepository) || errors.Is(err, ErrAuthentication) {
		t.Fatalf("classified as %v", err)
	}
}

func TestClassifyStillRecognizesRealMessages(t *testing.T) {
	for want, stderr := range map[error]string{
		ErrLocked:         "Fatal: unable to create lock in backend: repository is already locked by PID 8",
		ErrAuthentication: "Fatal: wrong password or no key found",
		ErrRepository:     "Fatal: unable to open config file: Stat: stat /x/config: no such file\nIs there a repository at the following location?",
	} {
		if got := classify(errors.New("exit status 1"), []byte(stderr)); !errors.Is(got, want) {
			t.Errorf("%q: got %v, want %v", stderr, got, want)
		}
	}
}
