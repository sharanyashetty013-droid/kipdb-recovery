//go:build unix

package recovery

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func renameDurable(oldname, newname string) error {
	if err := os.Rename(oldname, newname); err != nil {
		return err
	}
	return syncDir(filepath.Dir(newname))
}

// The lock file is never deleted: deleting lock files creates races.
func lockFile(name string) (io.Closer, error) {
	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return f, nil // closing the file releases the lock
}
