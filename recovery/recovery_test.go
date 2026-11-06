package recovery

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"testing"
)

// ---------- test helpers: build files exactly as the spec describes ----------

type tOp struct {
	op       byte
	key, val string
}

func put(k, v string) tOp { return tOp{OpPut, k, v} }
func del(k string) tOp    { return tOp{OpDelete, k, ""} }

func encEntry(o tOp) []byte {
	b := make([]byte, EntryHeaderSize, EntryHeaderSize+len(o.key)+len(o.val))
	b[0] = o.op
	binary.LittleEndian.PutUint16(b[1:], uint16(len(o.key)))
	binary.LittleEndian.PutUint32(b[3:], uint32(len(o.val)))
	c := crc32.Update(0, castagnoli, b[0:7])
	c = crc32.Update(c, castagnoli, []byte(o.key))
	c = crc32.Update(c, castagnoli, []byte(o.val))
	binary.LittleEndian.PutUint32(b[7:], c)
	b = append(b, o.key...)
	return append(b, o.val...)
}

func encFrameRaw(typ, reserved byte, opCount uint16, seq, plen uint64, payload []byte) []byte {
	h := make([]byte, FrameHeaderSize)
	binary.LittleEndian.PutUint32(h[0:], FrameMagic)
	h[4], h[5] = typ, reserved
	binary.LittleEndian.PutUint16(h[6:], opCount)
	binary.LittleEndian.PutUint64(h[8:], seq)
	binary.LittleEndian.PutUint64(h[16:], plen)
	binary.LittleEndian.PutUint32(h[24:], crc32.Checksum(payload, castagnoli))
	binary.LittleEndian.PutUint32(h[28:], crc32.Checksum(h[0:28], castagnoli))
	return append(h, payload...)
}

func encFrame(seq uint64, ops ...tOp) []byte {
	var p []byte
	for _, o := range ops {
		p = append(p, encEntry(o)...)
	}
	return encFrameRaw(FrameType, 0, uint16(len(ops)), seq, uint64(len(p)), p)
}

func encSuper(firstSeq, snapEnd uint64) []byte {
	b := make([]byte, SuperblockSize)
	copy(b, superMagic[:])
	binary.LittleEndian.PutUint16(b[8:], FormatVer)
	binary.LittleEndian.PutUint64(b[12:], firstSeq)
	binary.LittleEndian.PutUint64(b[20:], snapEnd)
	binary.LittleEndian.PutUint32(b[28:], crc32.Checksum(b[0:28], castagnoli))
	return b
}

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// memFile is an in-memory File with fault injection.
type memFile struct {
	data         []byte
	failReadAt   int64 // -1 = never
	failTruncate bool
	truncCalls   int
}

func newMem(b []byte) *memFile { return &memFile{data: append([]byte(nil), b...), failReadAt: -1} }

func (m *memFile) ReadAt(p []byte, off int64) (int, error) {
	if m.failReadAt >= 0 && off <= m.failReadAt && m.failReadAt < off+int64(len(p)) {
		return 0, errors.New("injected disk read error")
	}
	if off >= int64(len(m.data)) {
		return 0, io.EOF
	}
	n := copy(p, m.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
func (m *memFile) Size() (int64, error) { return int64(len(m.data)), nil }
func (m *memFile) Sync() error          { return nil }
func (m *memFile) Truncate(n int64) error {
	m.truncCalls++
	if m.failTruncate {
		return errors.New("injected truncate error")
	}
	m.data = m.data[:n]
	return nil
}

// state returns key->value for everything in the index, reading values back from the file.
func state(t *testing.T, m *memFile, r *Result) map[string]string {
	t.Helper()
	out := map[string]string{}
	for k, l := range r.Index {
		out[k] = string(m.data[l.ValueOffset : l.ValueOffset+int64(l.ValueLen)])
	}
	return out
}

func eq(t *testing.T, got, want map[string]string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) { // fmt prints maps sorted by key
		t.Fatalf("state mismatch\n got:  %v\n want: %v", got, want)
	}
}

func mustRecover(t *testing.T, data []byte) (*memFile, *Result) {
	t.Helper()
	m := newMem(data)
	r, err := Recover(m)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	return m, r
}

// ---------- basic recovery ----------

func TestEmptyDatabase(t *testing.T) {
	_, r := mustRecover(t, encSuper(1, 32))
	if len(r.Index) != 0 || r.NextSeq != 1 || r.Truncated {
		t.Fatalf("unexpected: %+v", r)
	}
}

func TestOverwriteAndDeleteOrder(t *testing.T) { // the spec's apple/banana example
	data := cat(encSuper(1, 32),
		encFrame(1, put("apple", "100")),
		encFrame(2, put("banana", "200")),
		encFrame(3, put("apple", "300")),
		encFrame(4, del("banana")))
	m, r := mustRecover(t, data)
	eq(t, state(t, m, r), map[string]string{"apple": "300"})
	if r.NextSeq != 5 || r.Frames != 4 {
		t.Fatalf("NextSeq=%d Frames=%d", r.NextSeq, r.Frames)
	}
}

func TestSameKeyInsideOneBatchLastWins(t *testing.T) {
	data := cat(encSuper(1, 32), encFrame(1, put("a", "1"), put("a", "2"), put("b", "x"), del("b")))
	m, r := mustRecover(t, data)
	eq(t, state(t, m, r), map[string]string{"a": "2"})
}

// ---------- the big one: crash at EVERY byte of the last frame ----------

func TestTornTailAtEveryByte(t *testing.T) {
	super := encSuper(1, 32)
	fA := encFrame(1, put("a", "1"))
	fB := encFrame(2, put("b", "2"), put("c", "3"), del("a")) // a batch
	full := cat(super, fA, fB)
	boundA := len(super)
	boundB := len(super) + len(fA)

	for cut := boundA; cut <= len(full); cut++ {
		m, r := mustRecover(t, full[:cut])
		var want map[string]string
		var wantLen int
		switch {
		case cut == len(full):
			want, wantLen = map[string]string{"b": "2", "c": "3"}, len(full)
		case cut >= boundB:
			want, wantLen = map[string]string{"a": "1"}, boundB // batch NEVER partly applied
		default:
			want, wantLen = map[string]string{}, boundA
		}
		eq(t, state(t, m, r), want)
		if len(m.data) != wantLen {
			t.Fatalf("cut=%d: file len %d, want %d", cut, len(m.data), wantLen)
		}
		// Truncation should only happen when we cut mid-frame.
		clean := cut == boundA || cut == boundB || cut == len(full)
		if r.Truncated == clean {
			t.Fatalf("cut=%d: Truncated=%v but clean=%v", cut, r.Truncated, clean)
		}
		// Recovery must be idempotent: running it again changes nothing.
		r2, err := Recover(m)
		if err != nil || r2.Truncated || fmt.Sprint(state(t, m, r2)) != fmt.Sprint(want) {
			t.Fatalf("cut=%d: second recovery differs (err=%v)", cut, err)
		}
	}
}

// ---------- damaged-tail distinctions ----------

func TestBadPayloadCRCOnLastFrameIsTruncated(t *testing.T) {
	fA, fB := encFrame(1, put("a", "1")), encFrame(2, put("b", "2"))
	data := cat(encSuper(1, 32), fA, fB)
	data[len(data)-1] ^= 0xFF // flip a byte in the last frame's payload
	m, r := mustRecover(t, data)
	eq(t, state(t, m, r), map[string]string{"a": "1"})
	if !r.Truncated || len(m.data) != 32+len(fA) {
		t.Fatalf("expected truncation to frame boundary, got len=%d", len(m.data))
	}
}

func TestBadPayloadCRCWithDataAfterIsCorruption(t *testing.T) {
	fA, fB := encFrame(1, put("a", "1")), encFrame(2, put("b", "2"))
	data := cat(encSuper(1, 32), fA, fB)
	data[32+FrameHeaderSize+2] ^= 0xFF // damage frame A's payload; frame B follows
	m := newMem(data)
	before := append([]byte(nil), m.data...)
	_, err := Recover(m)
	if !errors.Is(err, ErrCorruption) {
		t.Fatalf("want ErrCorruption, got %v", err)
	}
	if m.truncCalls != 0 || !bytes.Equal(m.data, before) {
		t.Fatal("must NOT truncate when valid data follows a damaged frame")
	}
}

func TestDamagedHeaderIsTreatedAsTornTail(t *testing.T) {
	fA, fB := encFrame(1, put("a", "1")), encFrame(2, put("b", "2"))
	data := cat(encSuper(1, 32), fA, fB)
	data[32+len(fA)+9] ^= 0x01 // flip a bit in frame B's header
	m, r := mustRecover(t, data)
	eq(t, state(t, m, r), map[string]string{"a": "1"})
	if len(m.data) != 32+len(fA) {
		t.Fatalf("want truncate to %d, got %d", 32+len(fA), len(m.data))
	}
}

func TestFakeFrameInsideValueIsNeverParsed(t *testing.T) {
	// A user's value that is itself a perfectly valid frame (seq 2). A crash mid-write
	// must not make recovery "find" it and treat it as a real frame.
	inner := encFrame(2, put("evil", "x"))
	fA := encFrame(1, put("a", "1"))
	fB := encFrame(2, put("blob", string(inner)))
	full := cat(encSuper(1, 32), fA, fB)

	m, r := mustRecover(t, full) // intact file: blob holds the inner bytes
	eq(t, state(t, m, r), map[string]string{"a": "1", "blob": string(inner)})

	for cut := 32 + len(fA); cut < len(full); cut++ {
		m, r := mustRecover(t, full[:cut])
		if _, bad := r.Index["evil"]; bad {
			t.Fatalf("cut=%d: parsed data from inside a value", cut)
		}
		eq(t, state(t, m, r), map[string]string{"a": "1"})
	}
}

// ---------- corruption (must refuse, must not touch the file) ----------

func expectCorrupt(t *testing.T, data []byte) {
	t.Helper()
	m := newMem(data)
	_, err := Recover(m)
	if !errors.Is(err, ErrCorruption) {
		t.Fatalf("want ErrCorruption, got %v", err)
	}
	if m.truncCalls != 0 {
		t.Fatal("corruption must never truncate")
	}
}

func TestSequenceGapAndDuplicate(t *testing.T) {
	expectCorrupt(t, cat(encSuper(1, 32), encFrame(1, put("a", "1")), encFrame(3, put("b", "2")))) // gap
	expectCorrupt(t, cat(encSuper(1, 32), encFrame(1, put("a", "1")), encFrame(1, put("b", "2")))) // dup
	expectCorrupt(t, cat(encSuper(5, 32), encFrame(4, put("a", "1"))))                             // wrong first
}

func TestSequenceOverflow(t *testing.T) {
	ok := cat(encSuper(math.MaxUint64, 32), encFrame(math.MaxUint64, put("a", "1")))
	_, r := mustRecover(t, ok)
	if !r.SeqExhausted {
		t.Fatal("expected SeqExhausted")
	}
	expectCorrupt(t, cat(ok, encFrame(0, put("b", "2"))))
}

func TestStructurallyInvalidButChecksumsPass(t *testing.T) {
	badOp := encEntry(put("k", "v"))
	badOp[0] = 9 // invalid op, then re-seal entry CRC so checksums pass
	c := crc32.Update(0, castagnoli, badOp[0:7])
	c = crc32.Update(c, castagnoli, badOp[11:])
	binary.LittleEndian.PutUint32(badOp[7:], c)
	expectCorrupt(t, cat(encSuper(1, 32), encFrameRaw(FrameType, 0, 1, 1, uint64(len(badOp)), badOp)))

	e := encEntry(put("k", "v"))
	expectCorrupt(t, cat(encSuper(1, 32), encFrameRaw(FrameType, 0, 2, 1, uint64(len(e)), e)))                        // count > entries (2 won't fit)
	expectCorrupt(t, cat(encSuper(1, 32), encFrameRaw(FrameType, 0, 1, 1, uint64(len(e)+3), append(e, 0, 0, 0))))     // trailing bytes
	expectCorrupt(t, cat(encSuper(1, 32), encFrameRaw(FrameType, 7, 1, 1, uint64(len(e)), e)))                        // reserved != 0
	expectCorrupt(t, cat(encSuper(1, 32), encFrameRaw(2, 0, 1, 1, uint64(len(e)), e)))                                // bad type
	expectCorrupt(t, cat(encSuper(1, 32), encFrameRaw(FrameType, 0, 1, 1, 1<<41, nil)))                               // absurd length
	expectCorrupt(t, cat(encSuper(1, 32), encFrameRaw(FrameType, 0, 1, 1, 5, make([]byte, 5))))                       // payload < 12
	expectCorrupt(t, cat(encSuper(1, 32), encFrame(1, del("k")), encFrameRaw(FrameType, 0, 0, 2, uint64(len(e)), e))) // 0 ops
}

func TestEntryCRCMismatchInsideValidPayload(t *testing.T) {
	e := encEntry(put("k", "v"))
	e[len(e)-1] ^= 1 // break the entry, but frame CRC is computed over the broken bytes
	expectCorrupt(t, cat(encSuper(1, 32), encFrameRaw(FrameType, 0, 1, 1, uint64(len(e)), e)))
}

// ---------- superblock ----------

func TestBadSuperblock(t *testing.T) {
	good := cat(encSuper(1, 32), encFrame(1, put("a", "1")))
	for i := 0; i < SuperblockSize; i++ {
		bad := append([]byte(nil), good...)
		bad[i] ^= 0x01
		if _, err := Recover(newMem(bad)); !errors.Is(err, ErrBadFormat) {
			t.Fatalf("flip byte %d: want ErrBadFormat, got %v", i, err)
		}
	}
	if _, err := Recover(newMem(good[:10])); !errors.Is(err, ErrBadFormat) {
		t.Fatal("short file must be ErrBadFormat")
	}
	if _, err := Recover(newMem(cat(encSuper(0, 32)))); !errors.Is(err, ErrBadFormat) {
		t.Fatal("first_seq=0 must be ErrBadFormat")
	}
	if _, err := Recover(newMem(encSuper(1, 9999))); !errors.Is(err, ErrBadFormat) {
		t.Fatal("snapshot_end beyond file must be ErrBadFormat")
	}
}

// ---------- snapshot protection ----------

func TestNeverTruncateBelowSnapshotEnd(t *testing.T) {
	fA := encFrame(1, put("a", "1"))
	snapEnd := uint64(32 + len(fA))
	data := cat(encSuper(1, snapEnd), fA)
	data[len(data)-1] ^= 0xFF // damage inside the protected snapshot
	m := newMem(data)
	if _, err := Recover(m); !errors.Is(err, ErrCorruption) || m.truncCalls != 0 {
		t.Fatalf("want ErrCorruption and no truncate, got %v (truncs=%d)", err, m.truncCalls)
	}

	// Damage AFTER the snapshot is an ordinary torn tail: allowed to truncate, but only to snapshot_end.
	fB := encFrame(2, put("b", "2"))
	data = cat(encSuper(1, snapEnd), fA, fB[:len(fB)-3])
	m, r := mustRecover(t, data)
	eq(t, state(t, m, r), map[string]string{"a": "1"})
	if int64(len(m.data)) != int64(snapEnd) {
		t.Fatalf("want len %d got %d", snapEnd, len(m.data))
	}

	// snapshot_end in the middle of a frame is corruption.
	expectCorrupt(t, cat(encSuper(1, snapEnd-4), fA))
}

// ---------- real I/O errors ----------

func TestRealReadErrorIsNotEOFAndNeverTruncates(t *testing.T) {
	fA, fB := encFrame(1, put("a", "1")), encFrame(2, put("b", "2"))
	data := cat(encSuper(1, 32), fA, fB)
	for _, at := range []int64{40, int64(32 + len(fA) + 5), int64(len(data) - 2)} { // header, next header, last payload
		m := newMem(data)
		m.failReadAt = at
		_, err := Recover(m)
		if !errors.Is(err, ErrIO) {
			t.Fatalf("read fail at %d: want ErrIO, got %v", at, err)
		}
		if m.truncCalls != 0 || len(m.data) != len(data) {
			t.Fatalf("read fail at %d: file was modified", at)
		}
	}
}

func TestTruncateFailureIsReported(t *testing.T) {
	fA := encFrame(1, put("a", "1"))
	m := newMem(cat(encSuper(1, 32), fA, []byte{1, 2, 3}))
	m.failTruncate = true
	if _, err := Recover(m); !errors.Is(err, ErrIO) {
		t.Fatalf("want ErrIO, got %v", err)
	}
}

// ---------- large payload (streaming path) ----------

func TestLargeBatchStreaming(t *testing.T) {
	big1 := string(bytes.Repeat([]byte{'x'}, MaxValue))
	big2 := string(bytes.Repeat([]byte{'y'}, MaxValue))
	fA := encFrame(1, put("a", "1"))
	fB := encFrame(2, put("big1", big1), put("big2", big2)) // > 2 MB, takes the streaming path
	full := cat(encSuper(1, 32), fA, fB)

	m, r := mustRecover(t, full)
	if got := state(t, m, r); got["big1"] != big1 || got["big2"] != big2 {
		t.Fatal("large values not recovered")
	}
	// torn in the middle of the big payload -> whole batch gone
	m, r = mustRecover(t, full[:len(full)-MaxValue/2])
	eq(t, state(t, m, r), map[string]string{"a": "1"})
	// bad CRC on the last big frame -> truncated
	bad := append([]byte(nil), full...)
	bad[len(bad)-10] ^= 0xFF
	m, r = mustRecover(t, bad)
	eq(t, state(t, m, r), map[string]string{"a": "1"})
}

// ---------- performance (informational) ----------

func BenchmarkRecoverOneMillionKeys(b *testing.B) {
	parts := [][]byte{encSuper(1, 32)}
	for i := 0; i < 1_000_000; i++ {
		parts = append(parts, encFrame(uint64(i+1), put(fmt.Sprintf("key%07d", i), "value-value-value")))
	}
	data := cat(parts...)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m := newMem(data)
		if _, err := Recover(m); err != nil {
			b.Fatal(err)
		}
	}
}

// ---------- read-only verify (kvdb check) ----------

func TestVerifyNeverWrites(t *testing.T) {
	fA, fB := encFrame(1, put("a", "1")), encFrame(2, put("b", "2"))
	full := cat(encSuper(1, 32), fA, fB)

	// clean file: OK, no torn tail
	m := newMem(full)
	r, err := Verify(m)
	if err != nil || r.TornTailAt != -1 || len(r.Index) != 2 {
		t.Fatalf("clean: err=%v torn=%d keys=%d", err, r.TornTailAt, len(r.Index))
	}

	// torn tail at every byte: reported, file untouched, same index Recover would build
	for cut := 32 + len(fA) + 1; cut < len(full); cut++ {
		m := newMem(full[:cut])
		r, err := Verify(m)
		if err != nil {
			t.Fatalf("cut=%d: %v", cut, err)
		}
		if r.TornTailAt != int64(32+len(fA)) || m.truncCalls != 0 || len(m.data) != cut {
			t.Fatalf("cut=%d: torn=%d truncs=%d len=%d", cut, r.TornTailAt, m.truncCalls, len(m.data))
		}
		eq(t, state(t, m, r), map[string]string{"a": "1"})
	}

	// corruption: error, file untouched
	bad := append([]byte(nil), full...)
	bad[32+FrameHeaderSize+2] ^= 0xFF
	m = newMem(bad)
	if _, err := Verify(m); !errors.Is(err, ErrCorruption) || m.truncCalls != 0 {
		t.Fatalf("want ErrCorruption and no writes, got %v", err)
	}
}
