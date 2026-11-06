//go:build windows

package recovery

import (
	"errors"
	"io"
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32        = syscall.NewLazyDLL("kernel32.dll")
	procMoveFileExW = kernel32.NewProc("MoveFileExW")
	procLockFileEx  = kernel32.NewProc("LockFileEx")
)

const (
	moveFileReplaceExisting = 0x1
	moveFileWriteThrough    = 0x8 // don't return until the rename is on disk
	lockfileFailImmediately = 0x1
	lockfileExclusiveLock   = 0x2

	errorLockViolation syscall.Errno = 33
)

// Windows has no directory fsync. Rename durability comes from MOVEFILE_WRITE_THROUGH.
func syncDir(string) error { return nil }

func renameDurable(oldname, newname string) error {
	from, err := syscall.UTF16PtrFromString(oldname)
	if err != nil {
		return err
	}
	to, err := syscall.UTF16PtrFromString(newname)
	if err != nil {
		return err
	}
	r, _, e := procMoveFileExW.Call(
		uintptr(unsafe.Pointer(from)),
		uintptr(unsafe.Pointer(to)),
		moveFileReplaceExisting|moveFileWriteThrough,
	)
	if r == 0 {
		return &os.LinkError{Op: "MoveFileEx", Old: oldname, New: newname, Err: e}
	}
	return nil
}

// The lock file is never deleted. Closing the handle (or the process dying) releases the lock.
func lockFile(name string) (io.Closer, error) {
	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	var ol syscall.Overlapped
	r, _, e := procLockFileEx.Call(f.Fd(), lockfileExclusiveLock|lockfileFailImmediately,
		0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	if r == 0 {
		f.Close()
		if errors.Is(e, errorLockViolation) {
			return nil, ErrLocked
		}
		return nil, e
	}
	return f, nil
}
