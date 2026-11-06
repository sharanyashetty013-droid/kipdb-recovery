package recovery

import (
	"errors"
	"fmt"
	"io"
)

// Handle is an open, recovered, locked database file.
// Person 1's writer appends frames at Result.ValidEnd using Result.NextSeq.
type Handle struct {
	File   WFile
	Result *Result
	Path   string

	fs   FS
	lock io.Closer
}

func compactPath(p string) string { return p + ".compact" }
func oldPath(p string) string     { return p + ".old" }
func lockPath(p string) string    { return p + ".lock" }

// Open = lock -> choose the authoritative file -> Recover -> clean up leftovers.
//
// Authority rules (which file is "the real database" after a crash):
//
//	db exists                 -> db is authoritative. Leftover .compact/.old are deleted
//	                             ONLY after db passes recovery.
//	no db, .old exists        -> crash mid-swap. Roll back: rename .old -> db.
//	no db, no .old            -> nothing acknowledged ever lived elsewhere.
//	                             create=true: make a fresh db. Otherwise ErrNotExist.
//
// .compact is never authoritative: it only becomes the db through a rename,
// and until that rename the old file still holds every acknowledged write.
func Open(fsys FS, path string, create bool) (*Handle, error) {
	lk, err := fsys.Lock(lockPath(path))
	if err != nil {
		if errors.Is(err, ErrLocked) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: lock: %v", ErrIO, err)
	}
	h, err := openLocked(fsys, path, create)
	if err != nil {
		lk.Close()
		return nil, err
	}
	h.lock = lk
	return h, nil
}

func openLocked(fsys FS, path string, create bool) (*Handle, error) {
	hasDB, err := exists(fsys, path)
	if err != nil {
		return nil, err
	}
	hasOld, err := exists(fsys, oldPath(path))
	if err != nil {
		return nil, err
	}

	switch {
	case hasDB:
		// normal case, or crash after the swap finished (db = new compacted file)
	case hasOld:
		// crash between "db -> db.old" and "db.compact -> db"
		if err := fsys.Rename(oldPath(path), path); err != nil {
			return nil, fmt.Errorf("%w: roll back %s: %v", ErrIO, oldPath(path), err)
		}
	case create:
		if err := createFresh(fsys, path); err != nil {
			return nil, err
		}
	default:
		return nil, ErrNotExist
	}

	f, err := fsys.OpenFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: open %s: %v", ErrIO, path, err)
	}
	res, err := Recover(f)
	if err != nil {
		f.Close() // leave every other file alone: one of them may be the only good copy
		return nil, err
	}

	// db is proven good. Only now is it safe to delete leftovers.
	for _, p := range []string{compactPath(path), oldPath(path)} {
		if err := removeIfExists(fsys, p); err != nil {
			f.Close()
			return nil, err
		}
	}
	return &Handle{File: f, Result: res, Path: path, fs: fsys}, nil
}

// createFresh builds an empty db the same way compaction installs one:
// write .compact, sync, then rename. A crash leaves either nothing or a full file.
func createFresh(fsys FS, path string) error {
	f, err := fsys.Create(compactPath(path))
	if err != nil {
		return fmt.Errorf("%w: create: %v", ErrIO, err)
	}
	if err := writeFull(f, EncodeSuperblock(1, SuperblockSize), 0); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("%w: sync new db: %v", ErrIO, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("%w: close new db: %v", ErrIO, err)
	}
	if err := fsys.Rename(compactPath(path), path); err != nil {
		return fmt.Errorf("%w: install new db: %v", ErrIO, err)
	}
	return nil
}

// Close releases the file and the process lock.
func (h *Handle) Close() error {
	var err error
	if h.File != nil {
		err = h.File.Close()
		h.File = nil
	}
	if h.lock != nil {
		if e := h.lock.Close(); err == nil {
			err = e
		}
		h.lock = nil
	}
	return err
}

func exists(fsys FS, p string) (bool, error) {
	ok, err := fsys.Exists(p)
	if err != nil {
		return false, fmt.Errorf("%w: stat %s: %v", ErrIO, p, err)
	}
	return ok, nil
}

func removeIfExists(fsys FS, p string) error {
	ok, err := exists(fsys, p)
	if err != nil || !ok {
		return err
	}
	if err := fsys.Remove(p); err != nil {
		return fmt.Errorf("%w: remove %s: %v", ErrIO, p, err)
	}
	return nil
}

func writeFull(f io.WriterAt, b []byte, off int64) error {
	n, err := f.WriteAt(b, off)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return fmt.Errorf("%w: write %d bytes at %d (wrote %d): %v", ErrIO, len(b), off, n, err)
	}
	return nil
}
