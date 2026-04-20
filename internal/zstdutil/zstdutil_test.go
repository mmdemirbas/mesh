package zstdutil

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"short", []byte("hello world")},
		{"repeating", bytes.Repeat([]byte("mesh "), 1000)},
		{"binary", func() []byte {
			b := make([]byte, 4096)
			for i := range b {
				b[i] = byte(i ^ 0x5a)
			}
			return b
		}()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			enc := Encode(c.in)
			dec, err := Decode(enc, 1<<20)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !bytes.Equal(dec, c.in) {
				t.Fatalf("round-trip mismatch: got %d bytes, want %d", len(dec), len(c.in))
			}
		})
	}
}

func TestDecodeRejectsOverLimit(t *testing.T) {
	data := bytes.Repeat([]byte("a"), 10000)
	enc := Encode(data)
	if _, err := Decode(enc, 100); err == nil {
		t.Fatal("expected error for payload over maxSize")
	}
}

func TestDecodeRejectsCorrupt(t *testing.T) {
	if _, err := Decode([]byte("not zstd"), 1<<20); err == nil {
		t.Fatal("expected error decoding garbage")
	}
}

func TestEncodeIsConcurrencySafe(t *testing.T) {
	data := bytes.Repeat([]byte("filesync"), 500)
	want := Encode(data)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := Encode(data)
			if !bytes.Equal(got, want) {
				t.Errorf("concurrent encode diverged: got %d bytes, want %d", len(got), len(want))
			}
		}()
	}
	wg.Wait()
}

func TestStreamReaderWriter(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	if _, err := io.Copy(w, strings.NewReader("zstd streaming test payload")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	r, err := NewReader(&buf)
	if err != nil {
		t.Fatalf("new reader: %v", err)
	}
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "zstd streaming test payload" {
		t.Fatalf("stream round-trip mismatch: %q", got)
	}
}
