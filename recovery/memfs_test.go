package recovery

import (
	"errors"
	"io"
	"io/fs"
	"sort"
)

// memFS is an in-memory FS that can crash at any mutating step.
//
// Crash model:
//   - every Create/WriteAt/Sync/Truncate/Rename/Remove is one "step"
//   - crashAt=k: steps 0..k-1 succeed, step k and everything after fails
//   - reboot(false): unsynced file data is LOST (worst case)
//   - reboot(true):  unsynced file data SURVIVES (the other extreme)
//   - Create/Rename/Remove are durable on return (we fsync the dir / use WRITE_THROUGH)
//   - Rename/Remove of an OPEN file fails, exactly like Windows. This catches
//     ordering bugs on Linux that would only show up on the judges' machines.
type memFS struct {
	files       map[string]*mnode
	locks       map[string]bool
	steps       int
	crashAt     int // -1 = never
	crashed     bool
	writeBudget int64 // -1 = unlimited; models disk full
	failSync    bool
}

type mnode struct {
	cur, durable []byte
	open         int
}

var (
	errCrash    = errors.New("memfs: simulated crash")
	errDiskFull = errors.New("memfs: disk full")
	errInUse    = errors.New("memfs: file is open (Windows would refuse)")
)

func newMemFS() *memFS {
	return &memFS{files: map[string]*mnode{}, locks: map[string]bool{}, crashAt: -1, writeBudget: -1}
}

func (m *memFS) step() error {
	if m.crashed {
		return errCrash
	}
	if m.crashAt >= 0 && m.steps >= m.crashAt {
		m.crashed = true
		return errCrash
	}
	m.steps++
	return nil
}

func (m *memFS) reboot(keepUnsynced bool) *memFS {
	n := newMemFS()
	for name, f := range m.files {
		src := f.durable
		if keepUnsynced {
			src = f.cur
		}
		b := append([]byte(nil), src...)
		n.files[name] = &mnode{cur: b, durable: append([]byte(nil), b...)}
	}
	return n
}

func (m *memFS) put(name string, b []byte) {
	m.files[name] = &mnode{cur: append([]byte(nil), b...), durable: append([]byte(nil), b...)}
}

func (m *memFS) names() []string {
	var out []string
	for k := range m.files {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (m *memFS) Create(name string) (WFile, error) {
	if err := m.step(); err != nil {
		return nil, err
	}
	if f, ok := m.files[name]; ok && f.open > 0 {
		return nil, errInUse
	}
	n := &mnode{open: 1}
	m.files[name] = n
	return &memHandle{fs: m, n: n}, nil
}

func (m *memFS) OpenFile(name string) (WFile, error) {
	if m.crashed {
		return nil, errCrash
	}
	n, ok := m.files[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	n.open++
	return &memHandle{fs: m, n: n}, nil
}

func (m *memFS) Exists(name string) (bool, error) {
	if m.crashed {
		return false, errCrash
	}
	_, ok := m.files[name]
	return ok, nil
}

func (m *memFS) Rename(oldname, newname string) error {
	if err := m.step(); err != nil {
		return err
	}
	src, ok := m.files[oldname]
	if !ok {
		return fs.ErrNotExist
	}
	if dst, ok := m.files[newname]; src.open > 0 || (ok && dst.open > 0) {
		return errInUse
	}
	m.files[newname] = src
	delete(m.files, oldname)
	return nil
}

func (m *memFS) Remove(name string) error {
	if err := m.step(); err != nil {
		return err
	}
	n, ok := m.files[name]
	if !ok {
		return fs.ErrNotExist
	}
	if n.open > 0 {
		return errInUse
	}
	delete(m.files, name)
	return nil
}

func (m *memFS) Lock(name string) (io.Closer, error) {
	if m.locks[name] {
		return nil, ErrLocked
	}
	m.locks[name] = true
	return closerFunc(func() error { delete(m.locks, name); return nil }), nil
}

type closerFunc func() error

func (c closerFunc) Close() error { return c() }

type memHandle struct {
	fs     *memFS
	n      *mnode
	closed bool
}

func (h *memHandle) ReadAt(p []byte, off int64) (int, error) {
	if h.closed {
		return 0, errors.New("memfs: read on closed file")
	}
	if off >= int64(len(h.n.cur)) {
		return 0, io.EOF
	}
	n := copy(p, h.n.cur[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (h *memHandle) Size() (int64, error) { return int64(len(h.n.cur)), nil }

func (h *memHandle) WriteAt(p []byte, off int64) (int, error) {
	if err := h.fs.step(); err != nil {
		return 0, err
	}
	n, err := len(p), error(nil)
	if b := h.fs.writeBudget; b >= 0 {
		if int64(n) > b {
			n, err = int(b), errDiskFull // short write, like a real full disk
		}
		h.fs.writeBudget -= int64(n)
	}
	if end := off + int64(n); end > int64(len(h.n.cur)) {
		h.n.cur = append(h.n.cur, make([]byte, end-int64(len(h.n.cur)))...)
	}
	copy(h.n.cur[off:], p[:n])
	return n, err
}

func (h *memHandle) Sync() error {
	if err := h.fs.step(); err != nil {
		return err
	}
	if h.fs.failSync {
		return errors.New("memfs: injected fsync failure")
	}
	h.n.durable = append([]byte(nil), h.n.cur...)
	return nil
}

func (h *memHandle) Truncate(size int64) error {
	if err := h.fs.step(); err != nil {
		return err
	}
	if size <= int64(len(h.n.cur)) {
		h.n.cur = append([]byte(nil), h.n.cur[:size]...)
	} else {
		h.n.cur = append(h.n.cur, make([]byte, size-int64(len(h.n.cur)))...)
	}
	return nil
}

func (h *memHandle) Close() error {
	if !h.closed {
		h.closed = true
		h.n.open--
	}
	return nil
}
