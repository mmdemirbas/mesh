package filesync

import (
	"bytes"
	"io"
	"os"
)

// magicProbeLen is how many leading bytes handleDelta inspects to decide
// whether a file is already compressed. See docs/filesync/DESIGN-v1.md §3.
const magicProbeLen = 4096

// incompressibleMagics lists the leading byte signatures that identify
// already-compressed file formats. Any file whose first magicProbeLen
// bytes begin with one of these sequences is transmitted uncompressed
// (DeltaBlock.raw = true). The list is extended, not configured.
var incompressibleMagics = [][]byte{
	{0x28, 0xb5, 0x2f, 0xfd},                         // .zst
	{0x1f, 0x8b},                                     // .gz
	{0xfd, 0x37, 0x7a, 0x58, 0x5a, 0x00},             // .xz
	{0x42, 0x5a, 0x68},                               // .bz2
	{0x04, 0x22, 0x4d, 0x18},                         // .lz4
	{0xff, 0xd8, 0xff},                               // .jpg / .jpeg
	{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, // .png
	{0x47, 0x49, 0x46, 0x38},                         // .gif (GIF8)
	{0x49, 0x44, 0x33},                               // .mp3 (ID3)
	{0xff, 0xfb},                                     // .mp3 (MPEG frame)
	{0xff, 0xf3},                                     // .mp3 (MPEG frame)
	{0xff, 0xf2},                                     // .mp3 (MPEG frame)
	{0x1a, 0x45, 0xdf, 0xa3},                         // .mkv / .webm (EBML)
	{0x50, 0x4b, 0x03, 0x04},                         // .zip, .docx, .xlsx, .pptx, .jar
	{0x50, 0x4b, 0x05, 0x06},                         // .zip (empty)
	{0x50, 0x4b, 0x07, 0x08},                         // .zip (spanned)
	{0x37, 0x7a, 0xbc, 0xaf, 0x27, 0x1c},             // .7z
	{0x25, 0x50, 0x44, 0x46},                         // .pdf
	{0x52, 0x61, 0x72, 0x21, 0x1a, 0x07},             // .rar
}

// probeIncompressibleRoot reads the first magicProbeLen bytes of
// relPath (through root) and reports whether the file is already in a
// compressed format. See isIncompressible for the probe rules.
func probeIncompressibleRoot(root *os.Root, relPath string) (bool, error) {
	f, err := root.Open(relPath)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, magicProbeLen)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return false, err
	}
	return isIncompressible(head[:n]), nil
}

// isIncompressible reports whether head — the first magicProbeLen bytes
// of a file (or fewer if the file is shorter) — matches any known
// already-compressed format. ISO BMFF containers (mp4, mov, m4a, …)
// carry the "ftyp" atom at byte offset 4..7 per ISO/IEC 14496-12, so
// the probe requires that exact offset rather than scanning — otherwise
// innocuous text containing "ftyp" (e.g. the word "filetype") would be
// treated as incompressible.
func isIncompressible(head []byte) bool {
	for _, m := range incompressibleMagics {
		if bytes.HasPrefix(head, m) {
			return true
		}
	}
	if len(head) >= 8 && bytes.Equal(head[4:8], []byte("ftyp")) {
		return true
	}
	return false
}
