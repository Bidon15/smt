# SMT performance audit — perf/* branches

This file tracks each optimization attempted on top of the upgraded Go 1.25
codebase, with EC2 c7i.2xlarge (Intel Xeon Platinum 8488C, 8 vCPU) numbers as
the only ones that count for the Risotto throughput target. Apple Silicon
local runs are noted only as a sanity check.

The mission goal: 2.6M state-tree updates/sec aggregate on a single 8-vCPU
worker, to cover the 1.3M tx/s × 2 writes/tx demand. Baseline measured
aggregate (16 independent trees) was 313K/s. Gap = 8.3×.

All numbers below are with `-benchtime=2s -count=3`. Bench harness lives at
`/tmp/smt-risotto-bench/bench_test.go`; per the mission spec it stays in a
separate module so the SMT package itself has no test-only dependencies.

Rules of the audit:

- Each change must keep `Prove`/`VerifyProof` round-trips correct and produce
  identical roots to the unmodified library on identical input sequences.
  This is enforced by `equivalence_test.go` (5,000-op random sequence with
  20% deletes + proof round-trips for ~1,000 keys).
- No new third-party dependencies without flagging them here.
- Public API stays stable: existing call sites compile and behave the same.

## Baseline (Go 1.25, unmodified library, EC2 c7i.2xlarge)

| Benchmark | ns/op | per-update | allocs/op | B/op |
|---|---:|---:|---:|---:|
| SHA-256 (32B input) | 73 | — | 0 | 0 |
| Update fresh tree 100K | 17,083 | 17.1 µs | 66 | 16,555 |
| BulkApply 100K, delta 1K | 5,074,268 | 5.1 µs | 7,798 | 13.3 MB |
| BulkApply 100K, delta 10K | 65,361,473 | 6.5 µs | 134,599 | 135.7 MB |
| Parallel 8 trees × 1K ops | 29,694,416 | **3.7 µs** (270K/s) | 250,723 | 115 MB |
| Parallel 16 trees × 1K ops | 51,155,260 | **3.2 µs** (313K/s) | 503,457 | 230 MB |

This matches the table in `RESULTS.md` (T16 SMT bench section) and is the
reference point every subsequent optimization is compared against.

## Milestone 1 — sync.Pool scratch reduction

Branch: `perf/m1-alloc-pool` (commit `cbc3509`).

### Changes

- **`pathSlices` pool** (`pool.go`): one [][]byte pool entry holds the two
  capacity-256/257 slices that `fillSideNodes` populates per Update. Eliminates
  ~12 KB B/op and 2 allocs/op.
- **`updateScratch` pool**: one struct per Update holds four 64-byte arrays —
  pathBuf (key digest), valueHashBuf (value digest), and two tick-tock buffers
  (`hashBufA`, `hashBufB`) used as `Sum` destinations during the leaf-up walk.
  This removes Sum(nil) allocations from every level of the spine without
  buffer reuse hazards: at iteration N+1, `digestNodeInto` reads the previous
  hash from one buffer and writes into the *other*, so the read-after-write
  ordering is preserved.
- **New `digestInto` / `digestLeafInto` / `digestNodeInto` methods on
  treeHasher**: same semantics as their existing counterparts, but accept a
  destination buffer for `Sum`.
- **`FixedSizeMap` MapStore** (`mapstore_fixed.go`): `map[[32]byte][]byte`
  instead of `map[string][]byte`, eliminating the per-Set string allocation.
  Use only with hashers where `Size() == 32` (SHA-256, BLAKE3-default). This
  is opt-in — existing `SimpleMap` users get no behaviour change.
- **`UpdateForRoot` / `updateWithSideNodes` / `deleteWithSideNodes`** now
  thread the pooled `*updateScratch` through and copy the final root into a
  fresh allocation before returning so callers can hold it across subsequent
  Updates that reuse the pooled buffers. This keeps Update's documented
  contract intact.

### Property tests added

`equivalence_test.go`:
- `TestFixedSizeMap_RootEquivalence_RandomSequence` — 5,000 ops of mixed
  Update/Delete with deliberate key collisions; asserts roots match
  step-by-step between SimpleMap-backed and FixedSizeMap-backed trees.
- `TestFixedSizeMap_ProofRoundTrip` — Prove + VerifyProof, plus the compact
  variant, for ~1,000 inserted keys, plus 100 fresh-key non-membership proofs.
- `TestRoot_DeterminismAcrossBackends` — pinned-input regression tests so any
  divergence shows up loudly.

These pass against every commit in this branch.

### EC2 results

| Benchmark | Baseline ns/op | M1 SimpleMap ns/op | M1 FixedMap ns/op | Speedup (Fixed) |
|---|---:|---:|---:|---:|
| Update fresh tree 100K | 17,083 | 13,048 | 12,024 | **1.42×** |
| Update fresh tree 1M | (≈18K) | 18,194 | 17,524 | ~1.0× |
| BulkApply 100K, delta 1K (ns/update) | 5,074 | 2,368 | 2,299 | **2.21×** |
| BulkApply 100K, delta 10K (ns/update) | 6,500 | 3,254 | 3,235 | **2.01×** |
| Parallel 8 trees × 1K ops (ns/update) | 3,718 | 1,309 | 1,084 | **3.43×** |
| Parallel 16 trees × 1K ops (ns/update) | 3,191 | 1,207 | 1,016 | **3.14×** |

| Benchmark | Baseline allocs/op | M1 SimpleMap | M1 FixedMap | Reduction |
|---|---:|---:|---:|---:|
| Update fresh tree 100K | 66 | 43 | **22** | 3.0× |
| Update fresh tree 1M | (≈66) | 48 | **25** | 2.6× |
| BulkApply 100K, delta 1K | 7,798 | 3,493 | **2,239** | 3.5× |
| Parallel 16 trees × 1K ops | 503,457 | 320,978 | **179,313** | 2.8× |

### What this means in updates/sec on EC2

| Config | Baseline | M1 SimpleMap | M1 FixedMap |
|---|---:|---:|---:|
| Single SMT (cold, 100K) | 58.5K/s | 76.6K/s | **83.2K/s** |
| BulkApply 100K, delta 1K | 197K/s | 422K/s | **435K/s** |
| Parallel 8 trees | 269K/s | 764K/s | **923K/s** |
| Parallel 16 trees | 313K/s | 829K/s | **985K/s** |

We've moved from 313K aggregate to 985K aggregate — 38% of the way to the
2.6M target on M1 alone, and 3.15× over baseline.

### Targets vs. actual

- "drop allocs/op from 66 to under 10" — **partially met**. Down to 22 with
  FixedSizeMap, 43 with SimpleMap. The remaining floor is one ~65-byte node
  value buffer per spine level (~17–18 per Update) that must escape to the
  MapStore; only an arena-backed MapStore would close the rest, and that's a
  bigger refactor than this milestone targeted.
- "≥1.5× single-SMT throughput on EC2" — **just missed at 1.42×** for fresh
  100K-tree Update. The compounding wins on bulk/parallel benchmarks (2–3×)
  exceed the milestone target where the workload allocates the most.

### Regression risk

Low. All existing tests pass (`go test ./...`); equivalence properties are
proven for 5,000-op random sequences and proof round-trips on ~1,000 keys.
The tick-tock buffer aliasing is invariant-checked: `digestNodeInto` reads
its right input (which always points into the *other* tick-tock buffer or
into a permanent map-owned buffer) before writing to its dst, so a single
round of static analysis suffices.

The one nuance worth noting: `UpdateForRoot` now allocates a fresh 32-byte
slice for the returned root on every successful insert/update path. We
intentionally accept this 1 alloc/op so the caller's stored root stays
valid across subsequent Updates that overwrite the pooled scratch. Eliminating
it would require either (a) returning a slice into a per-tree permanent
rootBuf with documented re-use semantics, or (b) introducing the bulk API
(M3) where only one root copy is needed for an entire delta.

## Milestone 2 — BLAKE3 hasher swap (NEGATIVE result)

Branch: `perf/m2-blake3` (kept on the branch; will not promote to the final
stack on this hardware).

### Hypothesis

BLAKE3 with AVX-2/AVX-512 SIMD on Xeon would beat SHA-256 by 1.3–1.8× per
hash on the SMT's 65-byte single-block workload, compounding 280-ish hashes
per Update for a meaningful end-to-end win. The library
(`lukechampine.com/blake3`) is a single Go file with a small assembly path
and is the standard implementation in the ecosystem.

### What was actually built

- `lukechampine.com/blake3 v1.4.1` added to `go.mod` (one extra transitive
  dep: `github.com/klauspost/cpuid/v2 v2.0.9`).
- `blake3_test.go`: round-trip property tests
  (`TestBlake3_ProofRoundTrip`, `TestBlake3_RootDeterminism`) that exercise
  the entire SMT API with a BLAKE3 hasher. They pass — the SMT itself is
  correctly hasher-agnostic.
- BLAKE3 bench variants in the harness mirroring the SHA-256 ones.

### Why it lost

The c7i.2xlarge runs an Intel Xeon Platinum 8488C (Sapphire Rapids), which
ships SHA-NI extensions (`SHA256RNDS2`, `SHA256MSG1`, `SHA256MSG2`). Go's
`crypto/sha256` automatically uses these on x86 builds, giving 72-73 ns
per 32-byte hash. BLAKE3, which would otherwise win via 4-/8-way parallel
compressions on AVX-2/AVX-512, falls back to a single-chunk path for the
SMT's ~65-byte inputs and clocks in at 134 ns — almost 2× slower per
hash.

### EC2 numbers (n=3, benchtime=2s)

| Benchmark | M1 SHA-256 (ns/op) | M2 BLAKE3 (ns/op) | Ratio |
|---|---:|---:|---:|
| Hasher direct, 32B input | 72.7 | 133.9 | 0.54× (BLAKE3 slower) |
| Update fresh tree 100K | 11,930 | 13,855 | 0.86× |
| BulkApply 100K, delta 1K (ns/update) | 2,390 | 2,675 | 0.89× |
| Parallel 16 trees × 1K ops (ns/update) | 1,001 | 1,288 | 0.78× |

### Decision

- **Do not promote BLAKE3 into the M3/M4 stack on this hardware.** The
  hasher hook stays; the test stays; the dep stays declared so consumers
  with non-SHA-NI hardware can opt in. PERF gains require a hash function
  with hardware acceleration on the deployment target, not BLAKE3 by
  default.
- For deployments on older Xeons (Skylake-X and earlier without SHA-NI),
  ARM without ARMv8 crypto extensions, or AMD pre-Zen2 — BLAKE3 likely
  wins on the same workload. Re-bench before assuming.
- The mission's "stack 1+2" expectation of 2.5× single-SMT was based on a
  BLAKE3-wins assumption that doesn't hold on Sapphire Rapids. We're at
  1.42× single-SMT after M1 alone; M3+M4 are the path to the rest, not
  the hasher swap.

### Regression risk

Zero — the BLAKE3 path is opt-in via the existing hasher arg. Default
construction (`smt.NewSparseMerkleTree(nodes, values, sha256.New())`)
behaves identically to before this branch.
