package smt

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"math/rand"
	"testing"
)

// These tests pin down behavioural invariants that every alloc/perf change
// in this branch must respect:
//
//   1. Replacing the SimpleMap backend with FixedSizeMap must produce an
//      identical root for any sequence of Update/Delete operations.
//
//   2. Prove/VerifyProof must round-trip for every key, both members and
//      non-members, on both backends.
//
// Run with -count=N to exercise more random seeds. The default seed list is
// chosen to keep the suite fast while still catching deterministic regressions.

func newSimpleTree(h hash.Hash) *SparseMerkleTree {
	return NewSparseMerkleTree(NewSimpleMap(), NewSimpleMap(), h)
}

func newFixedTree(h hash.Hash) *SparseMerkleTree {
	return NewSparseMerkleTree(NewFixedSizeMap(), NewFixedSizeMap(), h)
}

// randomKey returns the SHA-256 of a uint64 — same shape as the Risotto
// payment workload (20-byte address pre-image, 32-byte key).
func randomKey(seed uint64) []byte {
	var addr [20]byte
	binary.BigEndian.PutUint64(addr[:8], seed)
	h := sha256.Sum256(addr[:])
	return h[:]
}

func TestFixedSizeMap_RootEquivalence_RandomSequence(t *testing.T) {
	const ops = 5000
	const keySpace = 800 // intentional collisions: many ops re-update same keys

	rng := rand.New(rand.NewSource(1))
	simple := newSimpleTree(sha256.New())
	fixed := newFixedTree(sha256.New())

	for i := 0; i < ops; i++ {
		key := randomKey(uint64(rng.Intn(keySpace)))
		var (
			val []byte
			op  string
		)
		switch rng.Intn(10) {
		case 0, 1: // 20% delete
			op = "delete"
			rs, errS := simple.Delete(key)
			rf, errF := fixed.Delete(key)
			if (errS == nil) != (errF == nil) {
				t.Fatalf("op %d %s: error parity mismatch simple=%v fixed=%v", i, op, errS, errF)
			}
			if !bytes.Equal(rs, rf) {
				t.Fatalf("op %d %s: root mismatch\nsimple=%x\nfixed =%x", i, op, rs, rf)
			}
			continue
		default:
			op = "update"
			val = make([]byte, 16)
			binary.LittleEndian.PutUint64(val[:8], rng.Uint64())
			binary.LittleEndian.PutUint64(val[8:], uint64(i))
		}
		rs, errS := simple.Update(key, val)
		rf, errF := fixed.Update(key, val)
		if errS != nil || errF != nil {
			t.Fatalf("op %d %s: errors simple=%v fixed=%v", i, op, errS, errF)
		}
		if !bytes.Equal(rs, rf) {
			t.Fatalf("op %d %s: root mismatch\nsimple=%x\nfixed =%x", i, op, rs, rf)
		}
	}

	if !bytes.Equal(simple.Root(), fixed.Root()) {
		t.Fatalf("final root mismatch\nsimple=%x\nfixed =%x", simple.Root(), fixed.Root())
	}
}

func TestFixedSizeMap_ProofRoundTrip(t *testing.T) {
	const ops = 1000
	const keySpace = 200

	rng := rand.New(rand.NewSource(7))
	tree := newFixedTree(sha256.New())

	keys := make(map[string][]byte)
	for i := 0; i < ops; i++ {
		key := randomKey(uint64(rng.Intn(keySpace)))
		val := make([]byte, 16)
		binary.LittleEndian.PutUint64(val[:8], uint64(i))
		if _, err := tree.Update(key, val); err != nil {
			t.Fatal(err)
		}
		keys[string(key)] = val
	}

	// Membership proofs.
	for k, v := range keys {
		proof, err := tree.Prove([]byte(k))
		if err != nil {
			t.Fatal(err)
		}
		if !VerifyProof(proof, tree.Root(), []byte(k), v, sha256.New()) {
			t.Fatalf("membership proof failed for key %x", k)
		}
		// Compact round-trip.
		cp, err := tree.ProveCompact([]byte(k))
		if err != nil {
			t.Fatal(err)
		}
		if !VerifyCompactProof(cp, tree.Root(), []byte(k), v, sha256.New()) {
			t.Fatalf("compact membership proof failed for key %x", k)
		}
	}

	// Non-membership proofs for fresh keys.
	for i := uint64(keySpace + 1000); i < uint64(keySpace+1100); i++ {
		k := randomKey(i)
		proof, err := tree.Prove(k)
		if err != nil {
			t.Fatal(err)
		}
		if !VerifyProof(proof, tree.Root(), k, defaultValue, sha256.New()) {
			t.Fatalf("non-membership proof failed for key %x", k)
		}
	}
}

// TestRoot_DeterminismAcrossBackends is a defensive: the same ordered insert
// sequence must produce the same root on both backends. This is the core
// invariant that makes the wire format reproducible by verifiers.
func TestRoot_DeterminismAcrossBackends(t *testing.T) {
	cases := [][]struct{ k, v []byte }{
		{
			{[]byte("alpha\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"), []byte("a")},
			{[]byte("bravo\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"), []byte("b")},
			{[]byte("charlie\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"), []byte("c")},
		},
	}
	for ci, seq := range cases {
		s := newSimpleTree(sha256.New())
		f := newFixedTree(sha256.New())
		for _, kv := range seq {
			if _, err := s.Update(kv.k, kv.v); err != nil {
				t.Fatal(err)
			}
			if _, err := f.Update(kv.k, kv.v); err != nil {
				t.Fatal(err)
			}
		}
		if !bytes.Equal(s.Root(), f.Root()) {
			t.Fatalf("case %d roots diverge\nsimple=%x\nfixed =%x", ci, s.Root(), f.Root())
		}
	}
}
