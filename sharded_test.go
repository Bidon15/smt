package smt

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"math/rand"
	"testing"
)

func newSharded(t *testing.T, n int) *ShardedSMT {
	t.Helper()
	s, err := NewShardedSMT(
		n,
		func(_ int) (MapStore, MapStore) { return NewFixedSizeMap(), NewFixedSizeMap() },
		sha256.New,
	)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestShardedSMT_DeterministicRoot — same input sequence on two
// independent ShardedSMTs (same shard count) must produce the same
// combined root. This is the wire-reproducibility property: any
// verifier replaying the sequenced txs derives the same root.
func TestShardedSMT_DeterministicRoot(t *testing.T) {
	for _, n := range []int{1, 2, 4, 8, 16, 32, 64} {
		t.Run("", func(t *testing.T) {
			a := newSharded(t, n)
			b := newSharded(t, n)

			rng := rand.New(rand.NewSource(int64(n)))
			for i := 0; i < 2000; i++ {
				k := randomKey(uint64(rng.Intn(500)))
				v := make([]byte, 16)
				binary.LittleEndian.PutUint64(v, uint64(i))
				if _, err := a.Update(k, v); err != nil {
					t.Fatal(err)
				}
				if _, err := b.Update(k, v); err != nil {
					t.Fatal(err)
				}
			}

			if !bytes.Equal(a.Root(), b.Root()) {
				t.Fatalf("n=%d combined-root divergence\n  a=%x\n  b=%x", n, a.Root(), b.Root())
			}
		})
	}
}

// TestShardedSMT_BulkEquivalentToSingleApply — applying a batch via
// BulkUpdate yields the same combined root as applying each (k,v)
// via Update one at a time, on the same ShardedSMT shape. This is the
// core safety property of the parallel fan-out.
func TestShardedSMT_BulkEquivalentToSingleApply(t *testing.T) {
	for _, n := range []int{1, 4, 16, 64} {
		t.Run("", func(t *testing.T) {
			seq := newSharded(t, n)
			bulk := newSharded(t, n)

			// Pre-populate both with 500 keys.
			rng := rand.New(rand.NewSource(int64(n)))
			for i := 0; i < 500; i++ {
				k := randomKey(uint64(i))
				v := make([]byte, 16)
				binary.LittleEndian.PutUint64(v, uint64(i))
				if _, err := seq.Update(k, v); err != nil {
					t.Fatal(err)
				}
				if _, err := bulk.Update(k, v); err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Equal(seq.Root(), bulk.Root()) {
				t.Fatalf("n=%d post-prepop divergence", n)
			}

			// Build a delta of 1500 ops: mix of inserts and updates.
			keys := make([][]byte, 1500)
			vals := make([][]byte, 1500)
			for i := range keys {
				idx := uint64(rng.Intn(800))
				keys[i] = randomKey(idx)
				vals[i] = make([]byte, 16)
				binary.LittleEndian.PutUint64(vals[i], rng.Uint64())
			}

			// seq tree: apply iteratively.
			for i := range keys {
				if _, err := seq.Update(keys[i], vals[i]); err != nil {
					t.Fatal(err)
				}
			}

			// bulk tree: BulkUpdate.
			if _, err := bulk.BulkUpdate(keys, vals); err != nil {
				t.Fatal(err)
			}

			if !bytes.Equal(seq.Root(), bulk.Root()) {
				t.Fatalf("n=%d combined-root divergence after BulkUpdate", n)
			}
		})
	}
}

// TestShardedSMT_GetAfterBulk — every (k,v) inserted via BulkUpdate
// must be retrievable via Get from the same ShardedSMT. Sanity for
// the routing logic.
func TestShardedSMT_GetAfterBulk(t *testing.T) {
	s := newSharded(t, 16)

	keys := make([][]byte, 1000)
	vals := make([][]byte, 1000)
	for i := range keys {
		keys[i] = randomKey(uint64(i))
		vals[i] = make([]byte, 16)
		binary.LittleEndian.PutUint64(vals[i], uint64(i*31))
	}
	if _, err := s.BulkUpdate(keys, vals); err != nil {
		t.Fatal(err)
	}

	for i := range keys {
		got, err := s.Get(keys[i])
		if err != nil {
			t.Fatalf("Get(%d): %v", i, err)
		}
		if !bytes.Equal(got, vals[i]) {
			t.Fatalf("Get(%d): %x vs %x", i, got, vals[i])
		}
	}
}

// TestShardedSMT_ShardCountValidation — only powers of two between
// 1 and 64 are accepted.
func TestShardedSMT_ShardCountValidation(t *testing.T) {
	mapFn := func(_ int) (MapStore, MapStore) { return NewSimpleMap(), NewSimpleMap() }
	good := []int{1, 2, 4, 8, 16, 32, 64}
	bad := []int{0, 3, 5, 7, 9, 65, 100, 128, -1}
	for _, n := range good {
		if _, err := NewShardedSMT(n, mapFn, sha256.New); err != nil {
			t.Errorf("n=%d should be valid, got %v", n, err)
		}
	}
	for _, n := range bad {
		if _, err := NewShardedSMT(n, mapFn, sha256.New); err == nil {
			t.Errorf("n=%d should be invalid", n)
		}
	}
}
