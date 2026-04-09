package gziputil

import (
	"bytes"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	original := []byte("hello world, this is test data for gzip round-trip")
	encoded, err := Encode(original)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(encoded, original) {
		t.Fatal("encoded should differ from original")
	}

	decoded, err := Decode(encoded, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, original) {
		t.Fatalf("round-trip mismatch: got %q", decoded)
	}
}

func TestDecodeExceedsLimit(t *testing.T) {
	data := make([]byte, 1000)
	encoded, err := Encode(data)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Decode(encoded, 500)
	if err == nil {
		t.Fatal("expected error for data exceeding limit")
	}
}

func TestEncodePoolReuse(t *testing.T) {
	// Verify pooled writers produce correct output across multiple calls.
	for i := range 10 {
		data := bytes.Repeat([]byte{byte(i)}, 100)
		encoded, err := Encode(data)
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		decoded, err := Decode(encoded, 1024)
		if err != nil {
			t.Fatalf("iteration %d decode: %v", i, err)
		}
		if !bytes.Equal(decoded, data) {
			t.Fatalf("iteration %d: mismatch", i)
		}
	}
}

func BenchmarkEncode(b *testing.B) {
	data := bytes.Repeat([]byte("benchmark data for gzip encoding "), 1000)
	b.ResetTimer()
	for b.Loop() {
		_, _ = Encode(data)
	}
}

func BenchmarkDecode(b *testing.B) {
	data := bytes.Repeat([]byte("benchmark data for gzip decoding "), 1000)
	encoded, _ := Encode(data)
	b.ResetTimer()
	for b.Loop() {
		_, _ = Decode(encoded, int64(len(data)*2))
	}
}
