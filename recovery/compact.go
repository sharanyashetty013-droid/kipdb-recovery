package recovery

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"sort"
)

// ErrCompactAborted: compaction failed but the database is unchanged and the Handle still works.
var ErrCompactAborted = errors.New("recovery: compaction aborted, database unchanged")

const (
	compactFrameOps     = 1024
	compactFramePayload = 4 << 20
)

// Compact rewrites only the live keys into a fresh file and swaps it in.
//
// The caller MUST hold the write lock: no frame may be appended during compaction.
//
// Protocol (every step is a crash point; see docs/RECOVERY.md):
//
//  1. write db.compact: snapshot frames, then superblock (first_seq = NextSeq, snapshot_end = end)
//  2. fsync db.compact, close it
//  3. reopen db.compact and run Recover on it + compare every value (verify)
//  4. close db                        (Windows can't rename open files)
//  5. rename db -> db.old             (durable)
//  6. rename db.compact -> db         (durable)
//  7. openLocked: recover db, then delete db.old
//
// Errors in 1-3: ErrCompactAborted, Handle still valid.
// Errors in 4-7: we reopen (which rolls back if needed). If that works, ErrCompactAborted.
// If even the reopen fails, the Handle is dead and the caller must call Open again.
func (h *Handle) Compact() error {
	if h.File == nil {
		return errors.New("recovery: compact on closed handle")
	}
	if h.Result.SeqExhausted {
		return fmt.Errorf("%w: sequence numbers exhausted", ErrCompactAborted)
	}
	if ok, err := exists(h.fs, oldPath(h.Path)); err != nil || ok {
		return fmt.Errorf("%w: stale %s present (err=%v)", ErrCompactAborted, oldPath(h.Path), err)
	}

	if err := h.buildCompact(); err != nil {
		_ = removeIfExists(h.fs, compactPath(h.Path)) // best effort; Open also cleans it
		return fmt.Errorf("%w: %v", ErrCompactAborted, err)
	}

	// ---- install ----
	closeErr := h.File.Close()
	h.File = nil
	if closeErr != nil {
		return h.reopenAfter(fmt.Errorf("close db: %v", closeErr))
	}
	if err := h.fs.Rename(h.Path, oldPath(h.Path)); err != nil {
		return h.reopenAfter(fmt.Errorf("rename db -> old: %v", err))
	}
	if err := h.fs.Rename(compactPath(h.Path), h.Path); err != nil {
		return h.reopenAfter(fmt.Errorf("rename compact -> db: %v", err))
	}
	nh, err := openLocked(h.fs, h.Path, false)
	if err != nil {
		return fmt.Errorf("compaction installed but reopen failed, call Open again: %w", err)
	}
	h.File, h.Result = nh.File, nh.Result
	return nil
}

func (h *Handle) reopenAfter(cause error) error {
	nh, err := openLocked(h.fs, h.Path, false) // rolls back from .old if needed
	if err != nil {
		return fmt.Errorf("compaction install failed (%v) and reopen failed, call Open again: %w", cause, err)
	}
	h.File, h.Result = nh.File, nh.Result
	return fmt.Errorf("%w: %v", ErrCompactAborted, cause)
}

func (h *Handle) buildCompact() (err error) {
	r := h.Result
	keys := make([]string, 0, len(r.Index))
	for k := range r.Index {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic output

	cf, err := h.fs.Create(compactPath(h.Path))
	if err != nil {
		return fmt.Errorf("create compact: %v", err)
	}
	closed := false
	defer func() {
		if !closed {
			cf.Close()
		}
	}()

	off := int64(SuperblockSize)
	seq, exhausted := r.NextSeq, false
	var ops []Op
	payload := 0
	flush := func() error {
		if len(ops) == 0 {
			return nil
		}
		if exhausted {
			return errors.New("sequence numbers exhausted during compaction")
		}
		b, err := EncodeFrame(seq, ops)
		if err != nil {
			return err
		}
		if err := writeFull(cf, b, off); err != nil {
			return err
		}
		off += int64(len(b))
		if seq == math.MaxUint64 {
			exhausted = true
		} else {
			seq++
		}
		ops, payload = ops[:0], 0
		return nil
	}

	for _, k := range keys {
		loc := r.Index[k]
		v := make([]byte, loc.ValueLen)
		if err := readAt(h.File, v, loc.ValueOffset); err != nil {
			return err
		}
		sz := EntryHeaderSize + len(k) + len(v)
		if len(ops) == compactFrameOps || (len(ops) > 0 && payload+sz > compactFramePayload) {
			if err := flush(); err != nil {
				return err
			}
		}
		ops = append(ops, Op{Kind: OpPut, Key: []byte(k), Value: v})
		payload += sz
	}
	if err := flush(); err != nil {
		return err
	}
	// Superblock last: snapshot_end is only known now.
	if err := writeFull(cf, EncodeSuperblock(r.NextSeq, off), 0); err != nil {
		return err
	}
	if err := cf.Sync(); err != nil {
		return fmt.Errorf("sync compact: %v", err)
	}
	closed = true
	if err := cf.Close(); err != nil {
		return fmt.Errorf("close compact: %v", err)
	}
	return h.verifyCompact(off)
}

// verifyCompact re-reads the new file with the real recovery code and checks every value.
func (h *Handle) verifyCompact(wantEnd int64) error {
	vf, err := h.fs.OpenFile(compactPath(h.Path))
	if err != nil {
		return fmt.Errorf("verify open: %v", err)
	}
	defer vf.Close()
	vr, err := Recover(vf)
	if err != nil {
		return fmt.Errorf("verify: %v", err)
	}
	if vr.Truncated || vr.ValidEnd != wantEnd || vr.SnapshotEnd != wantEnd || len(vr.Index) != len(h.Result.Index) {
		return fmt.Errorf("verify: compacted file does not match (keys %d vs %d, end %d vs %d)",
			len(vr.Index), len(h.Result.Index), vr.ValidEnd, wantEnd)
	}
	for k, oldLoc := range h.Result.Index {
		newLoc, ok := vr.Index[k]
		if !ok || newLoc.ValueLen != oldLoc.ValueLen {
			return fmt.Errorf("verify: key %q missing or wrong length", k)
		}
		a, b := make([]byte, oldLoc.ValueLen), make([]byte, newLoc.ValueLen)
		if err := readAt(h.File, a, oldLoc.ValueOffset); err != nil {
			return err
		}
		if err := readAt(vf, b, newLoc.ValueOffset); err != nil {
			return err
		}
		if !bytes.Equal(a, b) {
			return fmt.Errorf("verify: key %q value differs", k)
		}
	}
	return nil
}
