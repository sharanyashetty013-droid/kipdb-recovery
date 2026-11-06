package recovery

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

var (
	ErrLocked   = errors.New("recovery: database is locked by another process")
	ErrNotExist = errors.New("recovery: database does not exist")
)

// WFile is a database file we can read, append to, truncate and sync.
type WFile interface {
	File
	io.WriterAt
	io.Closer
}

// FS is every filesystem operation recovery and compaction use.
// Tests swap in an in-memory FS that can "crash" at any step.
//
// Contract:
//   - Rename atomically replaces newname and is DURABLE when it returns
//     (Unix: rename + fsync parent dir; Windows: MoveFileEx with WRITE_THROUGH).
//   - Remove and Create are durable when they return.
//   - Rename/Remove are only ever called on CLOSED files (Windows refuses otherwise).
type FS interface {
	Create(name string) (WFile, error) // create or truncate
	OpenFile(name string) (WFile, error)
	Exists(name string) (bool, error)
	Rename(oldname, newname string) error
	Remove(name string) error
	Lock(name string) (io.Closer, error) // exclusive, non-blocking; ErrLocked if held
}

// OSFS is the real filesystem.
type OSFS struct{}

type osFile struct{ *os.File }

func (f osFile) Size() (int64, error) {
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

func (OSFS) Create(name string) (WFile, error) {
	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syncDir(filepath.Dir(name)); err != nil {
		f.Close()
		return nil, err
	}
	return osFile{f}, nil
}

func (OSFS) OpenFile(name string) (WFile, error) {
	f, err := os.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	return osFile{f}, nil
}

func (OSFS) Exists(name string) (bool, error) {
	_, err := os.Stat(name)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err // a real error is NOT "file missing"
}

func (OSFS) Rename(oldname, newname string) error { return renameDurable(oldname, newname) }

func (OSFS) Remove(name string) error {
	if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncDir(filepath.Dir(name))
}

func (OSFS) Lock(name string) (io.Closer, error) { return lockFile(name) }
