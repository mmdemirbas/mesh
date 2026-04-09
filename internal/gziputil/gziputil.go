// Package gziputil provides pooled gzip encode/decode with decompression limits.
package gziputil

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"sync"
)

var writerPool = sync.Pool{
	New: func() any { return gzip.NewWriter(nil) },
}

// Encode compresses data using gzip. Writers are pooled to avoid ~300 KB
// allocation per call.
func Encode(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := writerPool.Get().(*gzip.Writer)
	w.Reset(&buf)
	_, err := w.Write(data)
	if err != nil {
		w.Reset(nil)
		writerPool.Put(w)
		return nil, err
	}
	err = w.Close()
	writerPool.Put(w)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Decode decompresses gzip data. maxSize caps the output to prevent zip bombs.
// Returns an error if the decompressed data exceeds maxSize bytes.
func Decode(data []byte, maxSize int64) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	result, err := io.ReadAll(io.LimitReader(r, maxSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(result)) > maxSize {
		return nil, fmt.Errorf("decompressed data exceeds limit (%d bytes)", maxSize)
	}
	return result, nil
}
