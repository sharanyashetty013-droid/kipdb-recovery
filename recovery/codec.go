package recovery

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
)

// Op is one operation inside a frame. A frame with several Ops is an atomic batch.
type Op struct {
	Kind  byte // OpPut or OpDelete
	Key   []byte
	Value []byte // must be empty for OpDelete
}

// EncodeSuperblock builds the 32-byte file header.
func EncodeSuperblock(firstSeq uint64, snapshotEnd int64) []byte {
	b := make([]byte, SuperblockSize)
	copy(b, superMagic[:])
	binary.LittleEndian.PutUint16(b[8:], FormatVer)
	binary.LittleEndian.PutUint64(b[12:], firstSeq)
	binary.LittleEndian.PutUint64(b[20:], uint64(snapshotEnd))
	binary.LittleEndian.PutUint32(b[28:], crc32.Checksum(b[0:28], castagnoli))
	return b
}

// EncodeFrame builds one complete frame (header + payload).
// Compaction uses it. Person 1 can diff their writer's bytes against it.
func EncodeFrame(seq uint64, ops []Op) ([]byte, error) {
	if len(ops) == 0 || len(ops) > math.MaxUint16 {
		return nil, fmt.Errorf("encode: %d ops (need 1..%d)", len(ops), math.MaxUint16)
	}
	size := FrameHeaderSize
	for i, o := range ops {
		switch {
		case o.Kind != OpPut && o.Kind != OpDelete:
			return nil, fmt.Errorf("encode: op %d: bad kind %d", i, o.Kind)
		case len(o.Key) < 1 || len(o.Key) > MaxKey:
			return nil, fmt.Errorf("encode: op %d: key length %d", i, len(o.Key))
		case len(o.Value) > MaxValue:
			return nil, fmt.Errorf("encode: op %d: value length %d", i, len(o.Value))
		case o.Kind == OpDelete && len(o.Value) != 0:
			return nil, fmt.Errorf("encode: op %d: delete with value", i)
		}
		size += EntryHeaderSize + len(o.Key) + len(o.Value)
	}
	if uint64(size-FrameHeaderSize) > MaxPayload {
		return nil, fmt.Errorf("encode: payload too large")
	}
	b := make([]byte, FrameHeaderSize, size)
	for _, o := range ops {
		b = appendEntry(b, o)
	}
	h, payload := b[:FrameHeaderSize], b[FrameHeaderSize:]
	binary.LittleEndian.PutUint32(h[0:], FrameMagic)
	h[4], h[5] = FrameType, 0
	binary.LittleEndian.PutUint16(h[6:], uint16(len(ops)))
	binary.LittleEndian.PutUint64(h[8:], seq)
	binary.LittleEndian.PutUint64(h[16:], uint64(len(payload)))
	binary.LittleEndian.PutUint32(h[24:], crc32.Checksum(payload, castagnoli))
	binary.LittleEndian.PutUint32(h[28:], crc32.Checksum(h[0:28], castagnoli))
	return b, nil
}

func appendEntry(b []byte, o Op) []byte {
	var h [EntryHeaderSize]byte
	h[0] = o.Kind
	binary.LittleEndian.PutUint16(h[1:], uint16(len(o.Key)))
	binary.LittleEndian.PutUint32(h[3:], uint32(len(o.Value)))
	// ASSUMPTION (same as recovery.go): entry CRC = header bytes 0..6 + key + value.
	c := crc32.Update(0, castagnoli, h[0:7])
	c = crc32.Update(c, castagnoli, o.Key)
	c = crc32.Update(c, castagnoli, o.Value)
	binary.LittleEndian.PutUint32(h[7:], c)
	b = append(b, h[:]...)
	b = append(b, o.Key...)
	return append(b, o.Value...)
}
