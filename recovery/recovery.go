// Package recovery rebuilds the in-memory index from the append-only log.
//
// This file: scan + validate + damaged-tail handling + sequence checks.
//
//	Recover(f) - used by Open; may cut a torn tail.
//	Verify(f)  - used by `kvdb check`; READ-ONLY, never writes.
//
// Lock + authority rules: open.go. Compaction: compact.go.
//
// Golden rules (from the spec):
//   - A frame (one PUT/DELETE or one whole batch) is applied to the index
//     ONLY after the whole frame is verified. Never entry-by-entry.
//   - Damaged = looks like an interrupted last write  -> may truncate.
//   - Corrupt = checksums pass but structure is wrong,
//     or damage with data after it                    -> refuse, never truncate.
//   - A real I/O error is never treated as end-of-file and never truncates.
//   - Never truncate below snapshot_end.
package recovery

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
)

// ---- Format constants (from the spec; do NOT change without Person 1) ----

const (
	SuperblockSize  = 32
	FrameHeaderSize = 32
	EntryHeaderSize = 11
	MinEntrySize    = EntryHeaderSize + 1 // header + at least a 1-byte key

	FrameMagic uint32 = 0xD8C3B1A5
	FrameType  byte   = 1
	FormatVer  uint16 = 1

	MaxPayload uint64 = 1 << 40
	MaxKey            = 1024
	MaxValue          = 1 << 20

	OpPut    byte = 1
	OpDelete byte = 2

	// Payloads up to this size are read in one go; bigger ones are streamed.
	smallPayload = 1 << 20
)

var (
	superMagic = [8]byte{'K', 'I', 'P', 'D', 'B', 'L', 'O', 'G'}
	castagnoli = crc32.MakeTable(crc32.Castagnoli)
)

// ---- Errors ----

var (
	ErrBadFormat  = errors.New("recovery: bad format") // superblock invalid
	ErrCorruption = errors.New("recovery: corruption") // not an ordinary torn write
	ErrIO         = errors.New("recovery: I/O error")  // disk/OS failure
)

// File is the injectable file abstraction (so tests can inject failures).
type File interface {
	io.ReaderAt
	Size() (int64, error)
	Truncate(size int64) error
	Sync() error
}

// Loc says where a key's latest value lives in the log.
// The index stores locations, never whole values.
type Loc struct {
	Seq         uint64
	ValueOffset int64
	ValueLen    uint32
}

// Result is what a successful recovery gives back.
type Result struct {
	Index         map[string]Loc
	FirstSeq      uint64
	NextSeq       uint64 // seq the next frame must use (invalid if SeqExhausted)
	SeqExhausted  bool
	SnapshotEnd   int64
	ValidEnd      int64 // file size after recovery
	Frames        int   // frames applied
	Truncated     bool
	TruncatedFrom int64 // file size before truncation (if Truncated)
	TornTailAt    int64 // Verify only: offset where a torn tail starts, -1 if none
}

type superblock struct {
	firstSeq    uint64
	snapshotEnd int64
}

type entryOp struct {
	kind     byte
	key      string
	valueOff int64
	valueLen uint32
}

// ---- Public entry point ----

// Recover scans the log in f and rebuilds the index.
// Caller must already hold the process lock and have resolved which file is authoritative.
func Recover(f File) (*Result, error) { return scan(f, false) }

// Verify runs exactly the same checks as Recover but never modifies the file.
// A torn tail is reported in Result.TornTailAt (an ordinary crash state, still OK).
// Corruption and I/O errors are returned as errors, same as Recover.
func Verify(f File) (*Result, error) { return scan(f, true) }

func scan(f File, readOnly bool) (*Result, error) {
	size, err := f.Size()
	if err != nil {
		return nil, fmt.Errorf("%w: size: %v", ErrIO, err)
	}
	sb, err := readSuperblock(f, size)
	if err != nil {
		return nil, err
	}

	s := &scanner{f: f, size: size, buf: make([]byte, smallPayload)}
	res := &Result{
		Index:       make(map[string]Loc),
		FirstSeq:    sb.firstSeq,
		SnapshotEnd: sb.snapshotEnd,
		TornTailAt:  -1,
	}

	off := int64(SuperblockSize)
	expect := sb.firstSeq
	exhausted := false

	for off < size {
		if exhausted {
			return nil, corrupt("frame at %d follows sequence number %d (overflow)", off, uint64(math.MaxUint64))
		}
		ops, end, damaged, err := s.scanFrame(off, expect)
		if err != nil {
			return nil, err
		}
		if damaged {
			// Damage inside the protected snapshot is corruption, not a torn append.
			if off < sb.snapshotEnd {
				return nil, corrupt("damaged frame at %d inside protected snapshot (snapshot_end=%d)", off, sb.snapshotEnd)
			}
			if readOnly {
				res.TornTailAt = off
				break
			}
			if err := f.Truncate(off); err != nil {
				return nil, fmt.Errorf("%w: truncate to %d: %v", ErrIO, off, err)
			}
			if err := f.Sync(); err != nil {
				return nil, fmt.Errorf("%w: sync after truncate: %v", ErrIO, err)
			}
			res.Truncated = true
			res.TruncatedFrom = size
			break
		}
		// snapshot_end must sit exactly on a frame boundary
		if off < sb.snapshotEnd && end > sb.snapshotEnd {
			return nil, corrupt("frame %d..%d straddles snapshot_end=%d", off, end, sb.snapshotEnd)
		}

		// Whole frame verified -> NOW apply to the index.
		for _, o := range ops {
			if o.kind == OpPut {
				res.Index[o.key] = Loc{Seq: expect, ValueOffset: o.valueOff, ValueLen: o.valueLen}
			} else {
				delete(res.Index, o.key)
			}
		}
		res.Frames++
		if expect == math.MaxUint64 {
			exhausted = true
		} else {
			expect++
		}
		off = end
	}

	res.NextSeq = expect
	res.SeqExhausted = exhausted
	res.ValidEnd = off
	return res, nil
}

// ---- Superblock ----

func readSuperblock(f File, size int64) (superblock, error) {
	var sb superblock
	if size < SuperblockSize {
		return sb, fmt.Errorf("%w: file is %d bytes, smaller than superblock", ErrBadFormat, size)
	}
	var b [SuperblockSize]byte
	if err := readAt(f, b[:], 0); err != nil {
		return sb, err
	}
	if !bytes.Equal(b[0:8], superMagic[:]) {
		return sb, fmt.Errorf("%w: bad magic", ErrBadFormat)
	}
	if crc32.Checksum(b[0:28], castagnoli) != binary.LittleEndian.Uint32(b[28:32]) {
		return sb, fmt.Errorf("%w: superblock checksum mismatch", ErrBadFormat)
	}
	if v := binary.LittleEndian.Uint16(b[8:10]); v != FormatVer {
		return sb, fmt.Errorf("%w: unsupported version %d", ErrBadFormat, v)
	}
	if fl := binary.LittleEndian.Uint16(b[10:12]); fl != 0 {
		return sb, fmt.Errorf("%w: unexpected flags %#x", ErrBadFormat, fl)
	}
	sb.firstSeq = binary.LittleEndian.Uint64(b[12:20])
	snap := binary.LittleEndian.Uint64(b[20:28])
	if sb.firstSeq < 1 {
		return sb, fmt.Errorf("%w: first_seq must be >= 1", ErrBadFormat)
	}
	if snap < SuperblockSize || snap > uint64(size) {
		return sb, fmt.Errorf("%w: snapshot_end %d outside [%d,%d]", ErrBadFormat, snap, SuperblockSize, size)
	}
	sb.snapshotEnd = int64(snap)
	return sb, nil
}

// ---- Frame scanning ----

type scanner struct {
	f    File
	size int64
	buf  []byte // reused for small payloads
}

// scanFrame validates the frame at off.
//
//	damaged=true  -> looks like a torn final write (caller decides if truncation is allowed)
//	err != nil    -> corruption or I/O error (caller must NOT truncate)
func (s *scanner) scanFrame(off int64, expectSeq uint64) (ops []entryOp, end int64, damaged bool, err error) {
	// 1. Header must fit in the file.
	if s.size-off < FrameHeaderSize {
		return nil, 0, true, nil
	}
	var h [FrameHeaderSize]byte
	if err := readAt(s.f, h[:], off); err != nil {
		return nil, 0, false, err
	}
	// 2. Header magic + CRC BEFORE trusting any field.
	//    A damaged header is the torn last frame. We never search ahead for another header.
	if binary.LittleEndian.Uint32(h[0:4]) != FrameMagic ||
		crc32.Checksum(h[0:28], castagnoli) != binary.LittleEndian.Uint32(h[28:32]) {
		return nil, 0, true, nil
	}

	// 3. Header checksum passed, so structural problems are corruption, not a torn write.
	typ, reserved := h[4], h[5]
	opCount := binary.LittleEndian.Uint16(h[6:8])
	seq := binary.LittleEndian.Uint64(h[8:16])
	plen := binary.LittleEndian.Uint64(h[16:24])
	payloadCRC := binary.LittleEndian.Uint32(h[24:28])

	switch {
	case typ != FrameType:
		return nil, 0, false, corrupt("frame at %d: bad type %d", off, typ)
	case reserved != 0:
		return nil, 0, false, corrupt("frame at %d: reserved byte is %d", off, reserved)
	case opCount < 1:
		return nil, 0, false, corrupt("frame at %d: zero operations", off)
	case plen < MinEntrySize:
		return nil, 0, false, corrupt("frame at %d: payload length %d too small", off, plen)
	case plen > MaxPayload:
		return nil, 0, false, corrupt("frame at %d: payload length %d exceeds maximum", off, plen)
	case uint64(opCount) > plen/MinEntrySize:
		return nil, 0, false, corrupt("frame at %d: %d ops cannot fit in %d bytes", off, opCount, plen)
	case seq != expectSeq:
		return nil, 0, false, corrupt("frame at %d: sequence %d, expected %d", off, seq, expectSeq)
	}

	// 4. Declared frame runs past end of file -> torn tail.
	payloadStart := off + FrameHeaderSize
	end = payloadStart + int64(plen) // plen <= 2^40, no overflow
	if end > s.size {
		return nil, 0, true, nil
	}

	// 5. Payload checksum.
	r, ok, err := s.openPayload(payloadStart, plen, payloadCRC)
	if err != nil {
		return nil, 0, false, err
	}
	if !ok {
		if end == s.size {
			return nil, 0, true, nil // bad CRC on the very last frame: interrupted write
		}
		// More data follows a damaged frame: NOT an ordinary crash. Never truncate.
		return nil, 0, false, corrupt("frame at %d: payload checksum mismatch but %d bytes follow", off, s.size-end)
	}

	// 6. Entries (payload CRC is good, so any problem here is real corruption).
	ops, err = parseEntries(r, payloadStart, plen, opCount)
	if err != nil {
		return nil, 0, false, fmt.Errorf("frame at %d: %w", off, err)
	}
	return ops, end, false, nil
}

// openPayload verifies the payload CRC and returns a reader positioned at the payload start.
// Small payloads are read once; large ones are streamed (CRC pass, then parse pass).
func (s *scanner) openPayload(start int64, plen uint64, want uint32) (io.Reader, bool, error) {
	if plen <= uint64(len(s.buf)) {
		p := s.buf[:plen]
		if err := readAt(s.f, p, start); err != nil {
			return nil, false, err
		}
		if crc32.Checksum(p, castagnoli) != want {
			return nil, false, nil
		}
		return bytes.NewReader(p), true, nil
	}
	h := crc32.New(castagnoli)
	n, err := io.CopyBuffer(h, io.NewSectionReader(s.f, start, int64(plen)), make([]byte, 64<<10))
	if err != nil || n != int64(plen) {
		return nil, false, fmt.Errorf("%w: streaming payload at %d: copied %d of %d: %v", ErrIO, start, n, plen, err)
	}
	if h.Sum32() != want {
		return nil, false, nil
	}
	return bufio.NewReaderSize(io.NewSectionReader(s.f, start, int64(plen)), 64<<10), true, nil
}

func parseEntries(r io.Reader, payloadStart int64, plen uint64, count uint16) ([]entryOp, error) {
	ops := make([]entryOp, 0, count)
	var pos uint64
	var h [EntryHeaderSize]byte

	for i := 0; i < int(count); i++ {
		if plen-pos < MinEntrySize {
			return nil, corrupt("entry %d: header does not fit in payload", i)
		}
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return nil, fmt.Errorf("%w: entry %d header: %v", ErrIO, i, err)
		}
		kind := h[0]
		klen := binary.LittleEndian.Uint16(h[1:3])
		vlen := binary.LittleEndian.Uint32(h[3:7])
		want := binary.LittleEndian.Uint32(h[7:11])

		// Check lengths BEFORE allocating anything.
		switch {
		case kind != OpPut && kind != OpDelete:
			return nil, corrupt("entry %d: invalid operation %d", i, kind)
		case klen < 1 || klen > MaxKey:
			return nil, corrupt("entry %d: key length %d out of range", i, klen)
		case vlen > MaxValue:
			return nil, corrupt("entry %d: value length %d too large", i, vlen)
		case kind == OpDelete && vlen != 0:
			return nil, corrupt("entry %d: DELETE with value length %d", i, vlen)
		}
		total := uint64(EntryHeaderSize) + uint64(klen) + uint64(vlen)
		if total > plen-pos {
			return nil, corrupt("entry %d: length %d exceeds remaining payload %d", i, total, plen-pos)
		}

		body := make([]byte, int(klen)+int(vlen))
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, fmt.Errorf("%w: entry %d body: %v", ErrIO, i, err)
		}
		// ASSUMPTION (confirm with Person 1 / DESIGN.md): entry CRC covers header bytes 0..6 + key + value.
		c := crc32.Update(0, castagnoli, h[0:7])
		c = crc32.Update(c, castagnoli, body)
		if c != want {
			return nil, corrupt("entry %d: checksum mismatch inside checksum-valid payload", i)
		}
		ops = append(ops, entryOp{
			kind:     kind,
			key:      string(body[:klen]),
			valueOff: payloadStart + int64(pos) + EntryHeaderSize + int64(klen),
			valueLen: vlen,
		})
		pos += total
	}
	if pos != plen {
		return nil, corrupt("entries use %d bytes but payload is %d", pos, plen)
	}
	return ops, nil
}

// ---- helpers ----

// readAt reads exactly len(buf) bytes. Because we always check bounds against the
// file size first, a short read here is a real I/O problem, never "end of file".
func readAt(f File, buf []byte, off int64) error {
	n, err := f.ReadAt(buf, off)
	if n == len(buf) && (err == nil || err == io.EOF) {
		return nil
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return fmt.Errorf("%w: read %d bytes at offset %d: %v", ErrIO, len(buf), off, err)
}

func corrupt(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrCorruption, fmt.Sprintf(format, a...))
}
