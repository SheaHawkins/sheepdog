package run

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// lock makes sure only one supervisor drives a run. The lock file holds the
// supervisor's process ID. A lock whose process is gone was left by a crash,
// and the next supervisor takes it over.
type lock struct{ path string }

func acquire(path string) (*lock, error) {
	for tries := 0; tries < 3; tries++ {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			_, err = fmt.Fprintf(f, "%d\n", os.Getpid())
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				os.Remove(path)
				return nil, err
			}
			return &lock{path}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			continue // removed between our attempts; try again
		}
		pid, perr := strconv.Atoi(strings.TrimSpace(string(raw)))
		if perr == nil && pid != os.Getpid() && alive(pid) {
			return nil, fmt.Errorf("run is already supervised by process %d", pid)
		}
		// Stale. Two supervisors taking over at once could both get here;
		// O_EXCL on the retry lets only one of them win.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return nil, errors.New("could not take the run lock")
}

func (l *lock) release() { os.Remove(l.path) }
