package smt

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"math/rand"
	"testing"
)

// TestArenaFixedSizeMap_RootEquivalence — same operations on
// FixedSizeMap-backed and ArenaFixedSizeMap-backed trees must produce
// identical roots after every Update / Delete. This is the wire-format
// reproducibility property: any MapStore swap must be invisible to the
// root that lands on Celestia DA.
func TestArenaFixedSizeMap_RootEquivalence(t *testing.T) {
	const ops = 5000
	const keySpace = 800

	rng := rand.New(rand.NewSource(11))
	fixed := NewSparseMerkleTree(NewFixedSizeMap(), NewFixedSizeMap(), sha256.New())
	arena := NewSparseMerkleTree(NewArenaFixedSizeMap(), NewArenaFixedSizeMap(), sha256.New())

	for i := 0; i < ops; i++ {
		key := randomKey(uint64(rng.Intn(keySpace)))
		switch rng.Intn(10) {
		case 0, 1: // 20% delete
			rf, errF := fixed.Delete(key)
			ra, errA := arena.Delete(key)
			if (errF == nil) != (errA == nil) {
				t.Fatalf("op %d delete: error parity mismatch fixed=%v arena=%v", i, errF, errA)
			}
			if !bytes.Equal(rf, ra) {
				t.Fatalf("op %d delete: root mismatch\nfixed=%x\narena=%x", i, rf, ra)
			}
		default:
			val := make([]byte, 16)
			binary.LittleEndian.PutUint64(val[:8], rng.Uint64())
			binary.LittleEndian.PutUint64(val[8:], uint64(i))
			rf, errF := fixed.Update(key, val)
			ra, errA := arena.Update(key, val)
			if errF != nil || errA != nil {
				t.Fatalf("op %d update: errors fixed=%v arena=%v", i, errF, errA)
			}
			if !bytes.Equal(rf, ra) {
				t.Fatalf("op %d update: root mismatch\nfixed=%x\narena=%x", i, rf, ra)
			}
		}
	}

	if !bytes.Equal(fixed.Root(), arena.Root()) {
		t.Fatalf("final root mismatch")
	}
}

// TestArenaFixedSizeMap_BulkRootEquivalence — same property under
// BulkUpdate, including the merge / placeholder / leaf-shadow paths
// in bulk.go.
func TestArenaFixedSizeMap_BulkRootEquivalence(t *testing.T) {
	const preSize = 1000
	const deltaSize = 1500
	rng := rand.New(rand.NewSource(23))

	fixed := NewSparseMerkleTree(NewFixedSizeMap(), NewFixedSizeMap(), sha256.New())
	arena := NewSparseMerkleTree(NewArenaFixedSizeMap(), NewArenaFixedSizeMap(), sha256.New())

	// Pre-pop both identically.
	for i := 0; i < preSize; i++ {
		k := randomKey(uint64(i))
		v := make([]byte, 16)
		binary.LittleEndian.PutUint64(v[:8], uint64(i))
		if _, err := fixed.Update(k, v); err != nil {
			t.Fatal(err)
		}
		if _, err := arena.Update(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(fixed.Root(), arena.Root()) {
		t.Fatalf("post-prepop divergence")
	}

	// Bulk delta with overlap with pre-pop range and fresh keys.
	keys := make([][]byte, deltaSize)
	vals := make([][]byte, deltaSize)
	for i := range keys {
		var idx uint64
		if rng.Float64() < 0.6 {
			idx = uint64(rng.Intn(preSize)) // updates
		} else {
			idx = uint64(rng.Intn(preSize) + preSize) // inserts
		}
		keys[i] = randomKey(idx)
		vals[i] = make([]byte, 16)
		binary.LittleEndian.PutUint64(vals[i][:8], rng.Uint64())
	}

	// One does iterative Update, the other does BulkUpdate. Both must
	// agree (this is the existing bulk equivalence property — checked
	// on a different MapStore here to exercise the arena path).
	for i := range keys {
		if _, err := fixed.Update(keys[i], vals[i]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := arena.BulkUpdate(keys, vals); err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(fixed.Root(), arena.Root()) {
		t.Fatalf("post-bulk divergence\nfixed=%x\narena=%x", fixed.Root(), arena.Root())
	}
}

// TestArenaFixedSizeMap_ProofRoundTrip — Prove/VerifyProof must work
// against an arena-backed tree. Sub-slices into slabs must be valid
// for the proof's siblingData and other byte references.
func TestArenaFixedSizeMap_ProofRoundTrip(t *testing.T) {
	tree := NewSparseMerkleTree(NewArenaFixedSizeMap(), NewArenaFixedSizeMap(), sha256.New())

	const n = 500
	keys := make([][]byte, n)
	vals := make([][]byte, n)
	for i := range keys {
		keys[i] = randomKey(uint64(i))
		vals[i] = make([]byte, 16)
		binary.LittleEndian.PutUint64(vals[i], uint64(i*97+3))
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
			t.Fatalf("arena membership proof failed for key %d", i)
		}
		cp, err := tree.ProveCompact(keys[i])
		if err != nil {
			t.Fatal(err)
		}
		if !VerifyCompactProof(cp, tree.Root(), keys[i], vals[i], sha256.New()) {
			t.Fatalf("arena compact proof failed for key %d", i)
		}
	}

	// Non-membership.
	for i := uint64(n + 100); i < uint64(n+200); i++ {
		k := randomKey(i)
		proof, err := tree.Prove(k)
		if err != nil {
			t.Fatal(err)
		}
		if !VerifyProof(proof, tree.Root(), k, defaultValue, sha256.New()) {
			t.Fatalf("arena non-membership proof failed for key %d", i)
		}
	}
}

// TestArenaFixedSizeMap_Reset clears both slabs and map.
func TestArenaFixedSizeMap_Reset(t *testing.T) {
	am := NewArenaFixedSizeMap()
	for i := 0; i < 100; i++ {
		k := randomKey(uint64(i))
		v := make([]byte, 16)
		binary.LittleEndian.PutUint64(v, uint64(i))
		if err := am.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if am.Len() != 100 {
		t.Fatalf("Len=%d want 100", am.Len())
	}
	if am.SlabBytes() == 0 {
		t.Fatalf("SlabBytes=0 after 100 Sets")
	}

	am.Reset()
	if am.Len() != 0 {
		t.Fatalf("Len=%d after Reset, want 0", am.Len())
	}
	if am.SlabBytes() != 0 {
		t.Fatalf("SlabBytes=%d after Reset, want 0", am.SlabBytes())
	}

	// Reuse after Reset.
	for i := 0; i < 50; i++ {
		k := randomKey(uint64(i))
		v := make([]byte, 16)
		binary.LittleEndian.PutUint64(v, uint64(i))
		if err := am.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if am.Len() != 50 {
		t.Fatalf("Len=%d after 50 Sets, want 50", am.Len())
	}
}
