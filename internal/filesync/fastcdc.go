package filesync

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
)

// FastCDC parameters from docs/filesync/DESIGN-v1.md §2.
// avg matches the old fixed block size so the block-count distribution
// stays roughly familiar; min and max are standard FastCDC defaults.
// All peers must run identical parameters — determinism of boundaries
// depends on this.
const (
	fastCDCMin = 32 * 1024   // 32 KiB
	fastCDCAvg = 128 * 1024  // 128 KiB
	fastCDCMax = 512 * 1024  // 512 KiB
)

// gearTable holds 256 deterministic 64-bit constants used by the Gear
// hash. The table is generated from a fixed seed so every peer produces
// identical boundaries for identical input — a cross-peer contract.
// Do not replace or reseed; doing so invalidates every persisted index
// and every peer's delta cache.
var gearTable = buildGearTable()

// gearSeed is the fixed input to the deterministic table generator.
// Rotating this constant is a wire-breaking change.
const gearSeed = "mesh.filesync.fastcdc.gear.v1"

// buildGearTable derives 256 uint64 constants from SHA-256 chunks of
// the seed. SHA-256 is used purely as a deterministic PRNG — no
// security property is claimed.
func buildGearTable() [256]uint64 {
	var out [256]uint64
	// Each SHA-256 digest yields 4 uint64 values; 64 digests fill 256 slots.
	for i := 0; i < 64; i++ {
		var counter [8]byte
		binary.BigEndian.PutUint64(counter[:], uint64(i))
		h := sha256.Sum256(append([]byte(gearSeed), counter[:]...))
		for j := 0; j < 4; j++ {
			out[i*4+j] = binary.BigEndian.Uint64(h[j*8 : (j+1)*8])
		}
	}
	return out
}

// fastCDCMasks returns the two boundary masks derived from avg:
// maskS (stricter, fewer cuts) for positions before avg, maskL (looser,
// more cuts) for positions at or past avg. Both masks sit in the low
// bits; the Gear hash's left-shift feeds randomness into the high bits,
// so the low-bit comparison is sound.
func fastCDCMasks(avg int) (maskS, maskL uint64) {
	// log2(avg) rounded down.
	bits := 0
	for v := avg; v > 1; v >>= 1 {
		bits++
	}
	if bits < 2 {
		// Defensive — fastCDCAvg is a large power of two, but if a
		// caller passes a tiny value keep the masks well-formed.
		bits = 2
	}
	maskS = (uint64(1) << uint(bits+1)) - 1
	maskL = (uint64(1) << uint(bits-1)) - 1
	return
}

// Chunk is a FastCDC boundary emitted by Chunker.Next. Data aliases
// the chunker's internal buffer and stays valid only until the next
// call — callers that retain it must copy.
type Chunk struct {
	Offset int64
	Length int
	Data   []byte
}

// Chunker streams FastCDC chunks from an io.Reader. It is not safe for
// concurrent use. Peak memory stays bounded to one max-sized buffer
// plus bookkeeping, regardless of total file size.
type Chunker struct {
	r       io.Reader
	min     int
	avg     int
	max     int
	maskS   uint64
	maskL   uint64
	buf     []byte
	filled  int   // bytes available in buf[0:filled]
	offset  int64 // file offset of buf[0]
	pending int   // bytes to shift off the front on the next Next call
	eof     bool
	closed  bool
}

// NewChunker constructs a Chunker with the given parameters.
// min < avg < max and all positive; otherwise returns an error.
func NewChunker(r io.Reader, min, avg, max int) (*Chunker, error) {
	if min <= 0 || avg <= min || max <= avg {
		return nil, fmt.Errorf("fastcdc: invalid params min=%d avg=%d max=%d", min, avg, max)
	}
	maskS, maskL := fastCDCMasks(avg)
	return &Chunker{
		r:     r,
		min:   min,
		avg:   avg,
		max:   max,
		maskS: maskS,
		maskL: maskL,
		// Buffer sized to max so we can always find a cut point within
		// one refill.
		buf: make([]byte, max),
	}, nil
}

// newDefaultChunker wraps NewChunker with filesync's standard
// parameters. Production callers should prefer this so all peers agree
// on boundary locations.
func newDefaultChunker(r io.Reader) *Chunker {
	c, _ := NewChunker(r, fastCDCMin, fastCDCAvg, fastCDCMax)
	return c
}

// Next returns the next chunk. On the final chunk Data is non-nil and
// err is nil; the *following* call returns io.EOF with a zero Chunk.
func (c *Chunker) Next() (Chunk, error) {
	if c.closed {
		return Chunk{}, io.EOF
	}
	// Consume the previously emitted chunk now, not before returning it
	// last time — the caller's Data alias stays valid until this call.
	if c.pending > 0 {
		if c.pending < c.filled {
			copy(c.buf, c.buf[c.pending:c.filled])
		}
		c.filled -= c.pending
		c.offset += int64(c.pending)
		c.pending = 0
	}
	if err := c.refill(); err != nil && c.filled == 0 {
		if err == io.EOF {
			c.closed = true
			return Chunk{}, io.EOF
		}
		return Chunk{}, err
	}
	cutLen := c.findCut()
	if cutLen == 0 {
		// Only happens when buffer is empty at EOF.
		c.closed = true
		return Chunk{}, io.EOF
	}
	c.pending = cutLen
	return Chunk{
		Offset: c.offset,
		Length: cutLen,
		Data:   c.buf[:cutLen],
	}, nil
}

// refill reads from r until buf is full or EOF. Returns io.EOF when no
// more data is available; callers may still have buffered bytes to
// emit when this returns io.EOF.
func (c *Chunker) refill() error {
	if c.eof {
		return io.EOF
	}
	for c.filled < len(c.buf) {
		n, err := c.r.Read(c.buf[c.filled:])
		c.filled += n
		if err == io.EOF {
			c.eof = true
			return io.EOF
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// findCut returns the length of the next chunk given the currently
// buffered bytes. Never returns 0 unless filled is 0. Returns filled
// when the buffer is at EOF and shorter than min.
func (c *Chunker) findCut() int {
	n := c.filled
	if n == 0 {
		return 0
	}
	// Below min: no boundary possible. Emit whatever is left only at EOF.
	if n <= c.min {
		if c.eof {
			return n
		}
		// Should not happen — buffer is always sized to max and we only
		// come here after refill. Safety: emit what we have.
		return n
	}

	i := c.min
	// Phase 1: stricter mask until we hit avg.
	limit1 := c.avg
	if limit1 > n {
		limit1 = n
	}
	var fp uint64
	for i < limit1 {
		fp = (fp << 1) + gearTable[c.buf[i]]
		if (fp & c.maskS) == 0 {
			return i + 1
		}
		i++
	}
	// Phase 2: looser mask until max or EOF.
	limit2 := c.max
	if limit2 > n {
		limit2 = n
	}
	for i < limit2 {
		fp = (fp << 1) + gearTable[c.buf[i]]
		if (fp & c.maskL) == 0 {
			return i + 1
		}
		i++
	}
	return i
}

// ChunkFile streams chunks from r and returns their (offset, length, sha256)
// tuples. Reserved for tests and small files — production callers stream
// via Next so peak memory stays bounded.
func ChunkFile(r io.Reader) ([]Block, error) {
	c := newDefaultChunker(r)
	var out []Block
	for {
		ch, err := c.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		h := sha256.Sum256(ch.Data)
		out = append(out, Block{
			Offset: ch.Offset,
			Length: ch.Length,
			Hash:   h,
		})
	}
}

// Block is a single FastCDC chunk's identity — offset and length within
// the file and the content hash. Used by D1 delta protocol signatures
// (receiver → sender). See docs/filesync/DESIGN-v1.md §2.
type Block struct {
	Offset int64
	Length int
	Hash   Hash256
}
