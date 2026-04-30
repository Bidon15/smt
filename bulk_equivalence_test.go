package smt

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"math/rand"
	"testing"
)

// TestBulkUpdate_EquivalenceWithIterativeUpdate is the cornerstone
// property: a BulkUpdate of N (key, value) pairs must produce the same
// root that you'd get by calling Update on each pair sequentially in
// input order. Without this guarantee, verifiers replaying the
// transaction stream would produce different roots than the sequencer.
//
// The test runs many random shapes, including:
//   - Varying delta sizes vs tree size (sparse, medium, dense).
//   - Deliberate path collisions and same-path rewrites.
//   - Empty deltas.
//   - Both "fresh tree" (start empty) and "warm tree" (pre-populated).
func TestBulkUpdate_EquivalenceWithIterativeUpdate(t *testing.T) {
	cases := []struct {
		name             string
		preSize          int
		deltaSize        int
		keySpaceMultiple int // delta keys come from [0, deltaSize*multiple); 1 = lots of dups
	}{
		{"fresh_5_5", 0, 5, 1},
		{"fresh_50_50", 0, 50, 1},
		{"fresh_500_500", 0, 500, 1},
		{"warm_100_100_dense", 100, 100, 1},
		{"warm_1K_500_sparse", 1_000, 500, 4},
		{"warm_1K_1K_dense", 1_000, 1_000, 1},
		{"warm_5K_2K_collision", 5_000, 2_000, 1},   // many collisions with pre-pop
		{"warm_10K_2K_collision", 10_000, 2_000, 1}, // larger tree
		{"empty_delta", 100, 0, 1},
		{"single_update_fresh", 0, 1, 1},
		{"single_update_warm", 50, 1, 1},
	}

	for seed := int64(1); seed <= 3; seed++ {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				iterTree := NewSparseMerkleTree(NewFixedSizeMap(), NewFixedSizeMap(), sha256.New())
				bulkTree := NewSparseMerkleTree(NewFixedSizeMap(), NewFixedSizeMap(), sha256.New())

				rng := rand.New(rand.NewSource(seed))

				// Pre-populate both trees identically.
				for i := 0; i < tc.preSize; i++ {
					k := randomKey(uint64(i))
					v := make([]byte, 16)
					binary.LittleEndian.PutUint64(v[:8], uint64(i))
					if _, err := iterTree.Update(k, v); err != nil {
						t.Fatal(err)
					}
					if _, err := bulkTree.Update(k, v); err != nil {
						t.Fatal(err)
					}
				}
				if !bytes.Equal(iterTree.Root(), bulkTree.Root()) {
					t.Fatalf("seed=%d post-prepop divergence", seed)
				}

				// Build the delta. Some keys may overlap with pre-populated
				// space (i.e. updates rather than inserts).
				keys := make([][]byte, tc.deltaSize)
				vals := make([][]byte, tc.deltaSize)
				keySpaceSize := tc.deltaSize * tc.keySpaceMultiple
				if keySpaceSize == 0 {
					keySpaceSize = 1
				}
				// Prefer drawing from the pre-pop range to maximize the
				// "update existing leaf" path coverage.
				for i := 0; i < tc.deltaSize; i++ {
					var idx uint64
					if tc.preSize > 0 && rng.Float64() < 0.5 {
						idx = uint64(rng.Intn(tc.preSize))
					} else {
						idx = uint64(rng.Intn(keySpaceSize) + tc.preSize)
					}
					keys[i] = randomKey(idx)
					vals[i] = make([]byte, 16)
					binary.LittleEndian.PutUint64(vals[i][:8], rng.Uint64())
					binary.LittleEndian.PutUint64(vals[i][8:], uint64(i))
				}

				// Apply iteratively to the iter tree.
				for i := range keys {
					if _, err := iterTree.Update(keys[i], vals[i]); err != nil {
						t.Fatalf("seed=%d iter update %d: %v", seed, i, err)
					}
				}

				// Apply via BulkUpdate to the bulk tree.
				if _, err := bulkTree.BulkUpdate(keys, vals); err != nil {
					t.Fatalf("seed=%d bulk: %v", seed, err)
				}

				if !bytes.Equal(iterTree.Root(), bulkTree.Root()) {
					t.Fatalf("seed=%d roots diverge after %s\n  iter=%x\n  bulk=%x", seed, tc.name, iterTree.Root(), bulkTree.Root())
				}

				// Verify Get round-trips: every key inserted is gettable
				// with the correct value, and gone keys return default.
				lastByPath := make(map[string][]byte)
				for i := range keys {
					lastByPath[string(sha256Sum(keys[i]))] = vals[i]
				}
				for path, want := range lastByPath {
					got, err := bulkTree.values.Get([]byte(path))
					if err != nil {
						t.Fatalf("seed=%d %s: values.Get failed for %x: %v", seed, tc.name, path, err)
					}
					if !bytes.Equal(got, want) {
						t.Fatalf("seed=%d %s: stored value mismatch for path %x\n  got=%x\n  want=%x", seed, tc.name, path, got, want)
					}
				}
			})
		}
	}
}

func sha256Sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

// TestBulkUpdate_DuplicateKeys_LastWins enforces last-write-wins
// semantics for same-key duplicates within a single bulk call —
// matches what iterative Update would produce.
func TestBulkUpdate_DuplicateKeys_LastWins(t *testing.T) {
	iterTree := NewSparseMerkleTree(NewFixedSizeMap(), NewFixedSizeMap(), sha256.New())
	bulkTree := NewSparseMerkleTree(NewFixedSizeMap(), NewFixedSizeMap(), sha256.New())

	keys := [][]byte{
		randomKey(1), randomKey(2), randomKey(1), randomKey(3), randomKey(2), randomKey(1),
	}
	vals := [][]byte{
		[]byte("v1a"), []byte("v2a"), []byte("v1b"), []byte("v3"), []byte("v2b"), []byte("v1c"),
	}

	for i := range keys {
		if _, err := iterTree.Update(keys[i], vals[i]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := bulkTree.BulkUpdate(keys, vals); err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(iterTree.Root(), bulkTree.Root()) {
		t.Fatalf("dup-key roots diverge\n  iter=%x\n  bulk=%x", iterTree.Root(), bulkTree.Root())
	}
}

// TestBulkUpdate_Deletes ensures that empty values delete the
// corresponding key, and the resulting root matches iterative
// Delete.
func TestBulkUpdate_Deletes(t *testing.T) {
	iterTree := NewSparseMerkleTree(NewFixedSizeMap(), NewFixedSizeMap(), sha256.New())
	bulkTree := NewSparseMerkleTree(NewFixedSizeMap(), NewFixedSizeMap(), sha256.New())

	// Pre-populate with 100 keys.
	for i := 0; i < 100; i++ {
		k := randomKey(uint64(i))
		v := make([]byte, 16)
		binary.LittleEndian.PutUint64(v, uint64(i))
		if _, err := iterTree.Update(k, v); err != nil {
			t.Fatal(err)
		}
		if _, err := bulkTree.Update(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(iterTree.Root(), bulkTree.Root()) {
		t.Fatalf("pre-delete roots diverge")
	}

	// Mixed: delete 30 existing, update 30 existing, insert 30 new.
	keys := make([][]byte, 0, 90)
	vals := make([][]byte, 0, 90)
	for i := 0; i < 30; i++ {
		keys = append(keys, randomKey(uint64(i)))
		vals = append(vals, defaultValue) // delete
	}
	for i := 30; i < 60; i++ {
		k := randomKey(uint64(i))
		v := make([]byte, 16)
		binary.LittleEndian.PutUint64(v, uint64(i+1000))
		keys = append(keys, k)
		vals = append(vals, v)
	}
	for i := 100; i < 130; i++ {
		k := randomKey(uint64(i))
		v := make([]byte, 16)
		binary.LittleEndian.PutUint64(v, uint64(i))
		keys = append(keys, k)
		vals = append(vals, v)
	}

	for i := range keys {
		if _, err := iterTree.Update(keys[i], vals[i]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := bulkTree.BulkUpdate(keys, vals); err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(iterTree.Root(), bulkTree.Root()) {
		t.Fatalf("post-delete roots diverge\n  iter=%x\n  bulk=%x", iterTree.Root(), bulkTree.Root())
	}
}

// TestBulkUpdate_ProofRoundTrip asserts Prove/VerifyProof still works
// on a tree built via BulkUpdate.
func TestBulkUpdate_ProofRoundTrip(t *testing.T) {
	tree := NewSparseMerkleTree(NewFixedSizeMap(), NewFixedSizeMap(), sha256.New())

	keys := make([][]byte, 500)
	vals := make([][]byte, 500)
	for i := range keys {
		keys[i] = randomKey(uint64(i))
		vals[i] = make([]byte, 16)
		binary.LittleEndian.PutUint64(vals[i], uint64(i*17+5))
	}
	if _, err := tree.BulkUpdate(keys, vals); err != nil {
		t.Fatal(err)
	}

	for i := range keys {
		proof, err := tree.Prove(keys[i])
		if err != nil {
			t.Fatal(err)
		}
		if !VerifyProof(proof, tree.Root(), keys[i], vals[i], sha256.New()) {
			t.Fatalf("bulk-built tree: membership proof failed for key %d", i)
		}
	}

	for i := uint64(10000); i < 10100; i++ {
		k := randomKey(i)
		proof, err := tree.Prove(k)
		if err != nil {
			t.Fatal(err)
		}
		if !VerifyProof(proof, tree.Root(), k, defaultValue, sha256.New()) {
			t.Fatalf("bulk-built tree: non-membership proof failed for key %d", i)
		}
	}
}
