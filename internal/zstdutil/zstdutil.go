// Package zstdutil provides pooled zstd encode/decode with decompression limits.
package zstdutil

import (
	"bytes"
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
)

var (
	encoderOnce sync.Once
	encoder     *zstd.Encoder
)

func getEncoder() *zstd.Encoder {
	encoderOnce.Do(func() {
		encoder, _ = zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstd.SpeedDefault),
			zstd.WithEncoderConcurrency(1),
		)
	})
	return encoder
}

// Encode compresses data using zstd at the default level. The package
// uses a shared encoder; klauspost's EncodeAll is goroutine-safe.
func Encode(data []byte) []byte {
	return getEncoder().EncodeAll(data, nil)
}

// Decode decompresses zstd data. maxSize caps the output to prevent
// zip bombs — it is enforced while streaming so a frame that inflates
// past the cap never allocates the full output. Returns an error if
// the decompressed data exceeds maxSize.
func Decode(data []byte, maxSize int64) ([]byte, error) {
	if maxSize <= 0 {
		return nil, fmt.Errorf("zstdutil: maxSize must be positive, got %d", maxSize)
	}
	r, err := zstd.NewReader(bytes.NewReader(data),
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(uint64(maxSize)),
	)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(r, maxSize+1))
	if err != nil {
		return nil, err
	}
	if n > maxSize {
		return nil, fmt.Errorf("decompressed data exceeds limit (%d bytes)", maxSize)
	}
	return buf.Bytes(), nil
}

// NewReader wraps r in a zstd stream reader. Caller must call Close on
// the returned ReadCloser when done.
func NewReader(r io.Reader) (io.ReadCloser, error) {
	dec, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	return &streamReader{dec: dec}, nil
}

type streamReader struct {
	dec *zstd.Decoder
}

func (s *streamReader) Read(p []byte) (int, error) { return s.dec.Read(p) }

func (s *streamReader) Close() error {
	s.dec.Close()
	return nil
}

// NewWriter returns a zstd stream writer at the default level. Caller
// must call Close to flush the final frame.
func NewWriter(w io.Writer) (io.WriteCloser, error) {
	return zstd.NewWriter(w,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderConcurrency(1),
	)
}
