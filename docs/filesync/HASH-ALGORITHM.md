# Hash Algorithm — Decision and Open Ideas

> Why filesync uses SHA-256 today, and what it would take to change.
>
> Cross-reference: `PLAN.md` item **D2**, `DESIGN-v1.md`.
> Last updated: 2026-04-19.

## Current decision

**SHA-256** is the hash for file-level and block-level integrity in
filesync v1. Implemented via Go's stdlib `crypto/sha256`. No new
dependency, uses hardware acceleration where the platform provides
it (ARMv8 crypto extensions on Apple Silicon, SHA-NI on recent x86).

D2 (switch to BLAKE3) is **deferred**, not cancelled. The analysis
below is the reason and captures the conditions to reopen.

---

## Why BLAKE3 was proposed

1. **Fewer rounds.** BLAKE3 runs 7 rounds of its compression function
   against a 16-word state; SHA-256 runs 64 rounds against an 8-word
   state. In a pure-software comparison, BLAKE3 is ~4–5× faster per
   byte before SIMD enters the picture.
2. **SIMD-friendly core.** BLAKE3's G-function is add / xor / rotate
   on 32-bit words — a direct match for NEON, SSE2, AVX2, and
   AVX-512 lane ops. SHA-256's Σ/σ/Maj/Ch mixers are not
   SIMD-friendly by design.
3. **Intra-file parallelism.** BLAKE3 splits input into independent
   1 KiB chunks arranged in a Merkle tree. Multiple chunks can be
   compressed in parallel (4-way with NEON/SSE2, 8-way with AVX2,
   16-way with AVX-512). SHA-256 has no intra-file parallelism.
4. **Published performance.** BLAKE3 reference implementations
   reach ~3 GB/s single-thread on Apple M1 (NEON) and ~6 GB/s on
   recent x86 (AVX2). That's well above typical silicon-accelerated
   SHA-256 throughput.

None of this is wrong. The reason we are not shipping BLAKE3 is
not an algorithm problem — it is an implementation ecosystem
problem in the Go world.

---

## What we measured

Throwaway benchmark against `crypto/sha256`, `github.com/zeebo/blake3`,
and `lukechampine.com/blake3`.

- **Hardware**: Apple M1 Max, `darwin/arm64`.
- **Sizes**: 4 KiB, 32 KiB, 128 KiB, 512 KiB, 1 MiB, 10 MiB, 100 MiB.
- **Two modes**: one-shot `Sum256(data)`, and streaming writes of
  64 KiB chunks (matches how filesync actually hashes files).
- **`-benchtime=2s`**, single goroutine.

### Throughput (MB/s, higher is better)

One-shot `Sum256`:

| Size   | SHA-256 (stdlib) | zeebo/blake3 | luke/blake3 |
|--------|------------------|--------------|-------------|
| 4 KiB  | **2,286**        |   618        |   343       |
| 32 KiB | **2,273**        |   660        |   656       |
| 128 KiB| **2,372**        |   654        | 1,224       |
| 512 KiB| **2,400**        |   658        | 2,343       |
| 1 MiB  |   2,404          |   632        | **2,996**   |
| 10 MiB |   2,290          |   642        | **3,896**   |
| 100 MiB|   2,380          |   663        | **4,089**   |

Streaming (64 KiB writes, matches filesync's full-file hash):

| Size   | SHA-256   | zeebo | luke    |
|--------|-----------|-------|---------|
| 128 KiB| **2,274** |  664  |  811    |
| 1 MiB  | **2,333** |  657  |  943    |
| 10 MiB | **2,340** |  664  |  925    |
| 100 MiB| **2,282** |  658  |  925    |

### Why the BLAKE3 libraries are slow on M1

- **`zeebo/blake3`** has hand-written Plan 9 asm for `amd64`
  (AVX2 and AVX-512 code paths) but no asm for `arm64`. On M1
  it runs a generic-Go scalar implementation. Hence the flat
  ~650 MB/s across all sizes.
- **`lukechampine.com/blake3`** has portable-Go optimizations and
  reaches ~4 GB/s on one-shot large inputs via better unrolling,
  but it still has no NEON path. Streaming mode stays under
  1 GB/s because chunk-boundary overhead is not hidden by SIMD.
- **`crypto/sha256`** on `arm64` uses the `sha256h` / `sha256h2`
  ARMv8 crypto-extension opcodes. That is hardware silicon, one
  round per instruction. That is why stdlib SHA-256 outpaces
  software BLAKE3 here.

The comparison is not BLAKE3-vs-SHA-256. It is
*silicon-accelerated SHA-256* vs *unaccelerated-Go BLAKE3*.
Algorithmically BLAKE3 is still faster; the Go ecosystem has
not yet written the ARM64 asm that would let it beat silicon
SHA-256.

### x86 is different

Neither library was measured on x86 in this investigation, but
published benchmarks on the libraries' own repos put
`zeebo/blake3` at ~3 GB/s AVX2 and ~4 GB/s AVX-512 on recent
Intel and AMD cores. SHA-NI on the same cores is ~1.5–2 GB/s.
**On x86, off-the-shelf `zeebo/blake3` would beat stdlib
SHA-256 by roughly 2×.** Shipping it would be a win on the
x86 peers in the deployment and a ~3× loss on the M1 peer.

---

## Why we did not ship the asymmetric path

Accepting a 3× regression on one of three devices is not
tolerable for a feature whose only motivation is CPU reduction.
The M1 peer is a development driver, runs continuous scans
during normal use, and is battery-sensitive. Shipping a hash
that hurts it to help the other two is the wrong trade.

A per-architecture hash choice (SHA-256 on arm64, BLAKE3 on x86)
was considered and rejected. It doubles the wire surface, forces
an algorithm discriminator per block hash, and breaks the
"one algorithm per protocol version" simplification that keeps
v1 clean. Cross-hash verification across peers becomes impossible.

---

## What it would take to flip

The blocker is a single missing piece: **pure-Go BLAKE3 with
ARM64 NEON Plan 9 asm**. Once that exists, D2 becomes a drop-in
swap.

Two ways to get there:

### 1. Contribute NEON to `zeebo` or `luke`

Both library authors have expressed interest in broader arch
coverage; neither has landed it. A NEON path for `zeebo/blake3`
that mirrors its existing AVX2 structure is concrete work —
~300–500 lines of Plan 9 asm, plus a dispatcher tweak. Benefits
the whole Go community.

### 2. In-tree minimal BLAKE3 (`blake3min`)

Filesync uses only the hash mode of BLAKE3. Keyed mode, KDF
mode, extendable output, and incremental verification are all
unused. A stripped-down implementation scoped to our actual
needs is bounded:

| Piece                              | Estimate     |
|-----------------------------------|--------------|
| Scalar Go (compression + tree)    | ~250 lines   |
| ARM64 NEON 4-way compression      | ~350 lines asm |
| AMD64 AVX2 8-way compression      | ~400 lines asm |
| Scalar fallback for other arches  | free         |
| Tests (BLAKE3 spec vectors + diff vs zeebo) | ~200 lines |

Effort: 1–3 weeks elapsed for someone comfortable with Plan 9
asm. The BLAKE3 spec provides deterministic test vectors
(hashes of 0, 1, 2, …, 102400-byte inputs derived from a known
IV) which anchor correctness without guesswork.

Scalar-only is not worth doing on its own — it loses to
hardware SHA-256 on both target architectures. The asm paths
are load-bearing.

### The chosen path

Neither is in scope for the v1 landing. D2 is deferred until
one of these becomes true:

- An upstream pure-Go BLAKE3 library lands an ARM64 asm path
  and publishes benchmarks over 2 GB/s on Apple Silicon.
- We decide to invest 1–3 weeks in an in-tree `blake3min`.
- A measured workload shows SHA-256 hashing as the dominant
  cost of a user-visible operation (for example, >40% of wall
  time on a realistic full-scan), making the investment
  unambiguously worth it.

Until then, SHA-256 is the right answer. It is fast on every
target, has zero new dependencies, and the stdlib
implementation is audited and production-grade.

---

## Bench reproduction

The bench lives outside the repo to avoid polluting `go.mod`
with unused deps. To reproduce:

```shell
BENCH_DIR=$(mktemp -d)
cd "$BENCH_DIR"
go mod init blake3bench
go get github.com/zeebo/blake3@latest lukechampine.com/blake3@latest
# paste the bench file (see below)
go test -c -o bench.test .
./bench.test -test.bench=. -test.benchtime=2s -test.run=^$
```

Bench source (save as `bench_test.go` in the temp dir):

```go
package blake3bench

import (
    "crypto/rand"
    "crypto/sha256"
    "io"
    "testing"

    luke "lukechampine.com/blake3"
    zeebo "github.com/zeebo/blake3"
)

var sizes = []struct {
    name string
    n    int
}{
    {"4KB", 4 * 1024},
    {"32KB", 32 * 1024},
    {"128KB", 128 * 1024},
    {"512KB", 512 * 1024},
    {"1MB", 1 << 20},
    {"10MB", 10 << 20},
    {"100MB", 100 << 20},
}

func makeBuf(n int) []byte {
    b := make([]byte, n)
    if _, err := rand.Read(b); err != nil {
        panic(err)
    }
    return b
}

func BenchmarkSum_SHA256(b *testing.B) {
    for _, s := range sizes {
        data := makeBuf(s.n)
        b.Run(s.name, func(b *testing.B) {
            b.SetBytes(int64(s.n))
            b.ResetTimer()
            for i := 0; i < b.N; i++ {
                _ = sha256.Sum256(data)
            }
        })
    }
}

func BenchmarkSum_Zeebo(b *testing.B) {
    for _, s := range sizes {
        data := makeBuf(s.n)
        b.Run(s.name, func(b *testing.B) {
            b.SetBytes(int64(s.n))
            b.ResetTimer()
            for i := 0; i < b.N; i++ {
                _ = zeebo.Sum256(data)
            }
        })
    }
}

func BenchmarkSum_Luke(b *testing.B) {
    for _, s := range sizes {
        data := makeBuf(s.n)
        b.Run(s.name, func(b *testing.B) {
            b.SetBytes(int64(s.n))
            b.ResetTimer()
            for i := 0; i < b.N; i++ {
                _ = luke.Sum256(data)
            }
        })
    }
}

const streamChunk = 64 * 1024

func streamSHA256(data []byte) [32]byte {
    h := sha256.New()
    for off := 0; off < len(data); off += streamChunk {
        end := off + streamChunk
        if end > len(data) {
            end = len(data)
        }
        h.Write(data[off:end])
    }
    var out [32]byte
    copy(out[:], h.Sum(nil))
    return out
}

func streamZeebo(data []byte) [32]byte {
    h := zeebo.New()
    for off := 0; off < len(data); off += streamChunk {
        end := off + streamChunk
        if end > len(data) {
            end = len(data)
        }
        h.Write(data[off:end])
    }
    var out [32]byte
    d := h.Digest()
    _, _ = io.ReadFull(d, out[:])
    return out
}

func streamLuke(data []byte) [32]byte {
    h := luke.New(32, nil)
    for off := 0; off < len(data); off += streamChunk {
        end := off + streamChunk
        if end > len(data) {
            end = len(data)
        }
        h.Write(data[off:end])
    }
    var out [32]byte
    copy(out[:], h.Sum(nil))
    return out
}

func BenchmarkStream_SHA256(b *testing.B) {
    for _, s := range sizes {
        data := makeBuf(s.n)
        b.Run(s.name, func(b *testing.B) {
            b.SetBytes(int64(s.n))
            b.ResetTimer()
            for i := 0; i < b.N; i++ {
                _ = streamSHA256(data)
            }
        })
    }
}

func BenchmarkStream_Zeebo(b *testing.B) {
    for _, s := range sizes {
        data := makeBuf(s.n)
        b.Run(s.name, func(b *testing.B) {
            b.SetBytes(int64(s.n))
            b.ResetTimer()
            for i := 0; i < b.N; i++ {
                _ = streamZeebo(data)
            }
        })
    }
}

func BenchmarkStream_Luke(b *testing.B) {
    for _, s := range sizes {
        data := makeBuf(s.n)
        b.Run(s.name, func(b *testing.B) {
            b.SetBytes(int64(s.n))
            b.ResetTimer()
            for i := 0; i < b.N; i++ {
                _ = streamLuke(data)
            }
        })
    }
}
```

To extend: re-run on x86 (Lenovo, HW) to confirm the expected
BLAKE3 win there, and add a pool-vs-fresh variant to measure
allocation overhead at small sizes.

---

## References

- BLAKE3 spec and test vectors:
  `https://github.com/BLAKE3-team/BLAKE3-specs`
- `zeebo/blake3`: `https://github.com/zeebo/blake3`
- `lukechampine.com/blake3`: `https://github.com/lukechampine/blake3`
- Go stdlib SHA-256 ARM64 path: `src/crypto/sha256/sha256block_arm64.s`
  in the Go source tree.
