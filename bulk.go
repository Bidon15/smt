package smt

import (
	"bytes"
	"errors"
	"sort"
)

// ErrBulkInputMismatch is returned when BulkUpdate is called with
// keys and values slices of different lengths.
var ErrBulkInputMismatch = errors.New("bulk update: len(keys) != len(values)")

// bulkKV is the working representation of a single update during
// BulkUpdate. The path is the hashed key (one digest per input rather
// than one per Update call).
type bulkKV struct {
	path, value []byte
}

// BulkUpdate applies a batch of updates to the tree in a single
// recursive walk. Keys whose paths share a prefix amortize the spine
// hashing across the shared portion: instead of N independent
// top-down + bottom-up walks (the cost of iterating Update), one
// top-down DFS visits each tree node at most once per batch.
//
// Semantics:
//   - len(keys) must equal len(values).
//   - Empty values are treated as deletes; for now they are dispatched
//     via per-key Delete BEFORE the bulk insert/update walk runs. The
//     amortization win lives on the insert/update path; deletes are
//     rare in payment workloads.
//   - When the same path appears multiple times, the LAST (key, value)
//     wins, matching the iterative-Update semantics.
//
// The returned root is a fresh allocation, like Update's.
func (smt *SparseMerkleTree) BulkUpdate(keys, values [][]byte) ([]byte, error) {
	if len(keys) != len(values) {
		return nil, ErrBulkInputMismatch
	}
	if len(keys) == 0 {
		return smt.Root(), nil
	}

	// Process deletes via the existing single-key path. These are
	// uncommon in our workload; keeping them out of the bulk walk
	// avoids special-casing the recursive merge logic.
	for i := range keys {
		if bytes.Equal(values[i], defaultValue) {
			if _, err := smt.Delete(keys[i]); err != nil {
				return nil, err
			}
		}
	}

	// Build the non-delete kv list, computing each path once.
	kvs := make([]bulkKV, 0, len(keys))
	for i := range keys {
		if bytes.Equal(values[i], defaultValue) {
			continue
		}
		kvs = append(kvs, bulkKV{
			path:  smt.th.path(keys[i]),
			value: values[i],
		})
	}

	if len(kvs) == 0 {
		return smt.Root(), nil
	}

	// Sort by path. Stable sort preserves later-wins semantics during
	// the dedup pass below.
	sort.SliceStable(kvs, func(i, j int) bool {
		return bytes.Compare(kvs[i].path, kvs[j].path) < 0
	})

	// Dedup adjacent same-path entries: keep the LAST occurrence.
	w := 0
	for r := 0; r < len(kvs); r++ {
		if r+1 < len(kvs) && bytes.Equal(kvs[r].path, kvs[r+1].path) {
			continue
		}
		if w != r {
			kvs[w] = kvs[r]
		}
		w++
	}
	kvs = kvs[:w]

	sc := getUpdateScratch()
	defer putUpdateScratch(sc)

	newRoot, err := smt.bulkApplyAtRoot(sc, smt.Root(), 0, kvs)
	if err != nil {
		return nil, err
	}
	smt.SetRoot(newRoot)

	out := make([]byte, len(newRoot))
	copy(out, newRoot)
	return out, nil
}

// bulkApplyAtRoot recursively descends from currentHash applying the
// sorted kvs whose paths fall under that subtree. It returns the new
// hash for that subtree, allocating only the internal-node and leaf
// value buffers it actually needs to update.
//
// Each recursion level allocates a fresh hash buffer for the new
// internal-node digest; we can't tick-tock here because the two
// sibling recursive calls each return a hash that must coexist while
// we compute their parent.
func (smt *SparseMerkleTree) bulkApplyAtRoot(sc *updateScratch, currentHash []byte, depth int, kvs []bulkKV) ([]byte, error) {
	if len(kvs) == 0 {
		return currentHash, nil
	}

	if bytes.Equal(currentHash, smt.th.placeholder()) {
		return smt.buildSubtree(sc, depth, kvs)
	}

	nodeData, err := smt.nodes.Get(currentHash)
	if err != nil {
		return nil, err
	}

	if smt.th.isLeaf(nodeData) {
		return smt.mergeLeafWithKVs(sc, currentHash, nodeData, depth, kvs)
	}

	leftHash, rightHash := smt.th.parseNode(nodeData)
	splitIdx := partitionByBit(kvs, depth)
	leftKVs := kvs[:splitIdx]
	rightKVs := kvs[splitIdx:]

	newLeft, err := smt.bulkApplyAtRoot(sc, leftHash, depth+1, leftKVs)
	if err != nil {
		return nil, err
	}
	newRight, err := smt.bulkApplyAtRoot(sc, rightHash, depth+1, rightKVs)
	if err != nil {
		return nil, err
	}

	if bytes.Equal(newLeft, leftHash) && bytes.Equal(newRight, rightHash) {
		return currentHash, nil
	}

	if err := smt.nodes.Delete(currentHash); err != nil {
		return nil, err
	}
	newHash, newData := smt.th.digestNode(newLeft, newRight)
	if err := smt.nodes.Set(newHash, newData); err != nil {
		return nil, err
	}
	return newHash, nil
}

// buildSubtree constructs a fresh subtree at depth from sortedKVs. It
// matches the SMT's lazy structuring: a single-key subtree IS a leaf
// (no wrapping), multi-key subtrees wrap with placeholder siblings as
// needed. Every inner node and leaf gets stored in the MapStore.
//
// kvs must be sorted by path and de-duplicated.
func (smt *SparseMerkleTree) buildSubtree(sc *updateScratch, depth int, kvs []bulkKV) ([]byte, error) {
	if len(kvs) == 0 {
		return smt.th.placeholder(), nil
	}

	if len(kvs) == 1 {
		kv := kvs[0]
		valueHash := smt.th.digestInto(sc.valueHashBuf[:], kv.value)
		leafHash, leafData := smt.th.digestLeaf(kv.path, valueHash)
		if err := smt.nodes.Set(leafHash, leafData); err != nil {
			return nil, err
		}
		if err := smt.values.Set(kv.path, kv.value); err != nil {
			return nil, err
		}
		return leafHash, nil
	}

	splitIdx := partitionByBit(kvs, depth)
	leftKVs := kvs[:splitIdx]
	rightKVs := kvs[splitIdx:]

	var leftHash, rightHash []byte
	var err error
	if len(leftKVs) == 0 {
		leftHash = smt.th.placeholder()
	} else {
		leftHash, err = smt.buildSubtree(sc, depth+1, leftKVs)
		if err != nil {
			return nil, err
		}
	}
	if len(rightKVs) == 0 {
		rightHash = smt.th.placeholder()
	} else {
		rightHash, err = smt.buildSubtree(sc, depth+1, rightKVs)
		if err != nil {
			return nil, err
		}
	}

	nodeHash, nodeData := smt.th.digestNode(leftHash, rightHash)
	if err := smt.nodes.Set(nodeHash, nodeData); err != nil {
		return nil, err
	}
	return nodeHash, nil
}

// mergeLeafWithKVs handles the case where bulkApplyAtRoot encounters
// an existing leaf during descent. The leaf is treated as a synthetic
// kv (unless one of the incoming kvs has the same path, which
// overrides it) and the merged set is fed through buildSubtree.
//
// The existing leaf node is deleted from the MapStore; buildSubtree
// re-Sets it under its (potentially new) parent shape.
func (smt *SparseMerkleTree) mergeLeafWithKVs(sc *updateScratch, currentHash, leafData []byte, depth int, kvs []bulkKV) ([]byte, error) {
	leafPath, _ := smt.th.parseLeaf(leafData)

	shadowed := false
	for i := range kvs {
		if bytes.Equal(kvs[i].path, leafPath) {
			shadowed = true
			break
		}
	}

	if err := smt.nodes.Delete(currentHash); err != nil {
		return nil, err
	}

	merged := kvs
	if !shadowed {
		oldValue, err := smt.values.Get(leafPath)
		if err != nil {
			return nil, err
		}
		insertIdx := sort.Search(len(kvs), func(i int) bool {
			return bytes.Compare(kvs[i].path, leafPath) >= 0
		})
		merged = make([]bulkKV, 0, len(kvs)+1)
		merged = append(merged, kvs[:insertIdx]...)
		merged = append(merged, bulkKV{path: leafPath, value: oldValue})
		merged = append(merged, kvs[insertIdx:]...)
	}

	return smt.buildSubtree(sc, depth, merged)
}

// partitionByBit splits sorted kvs into [path-bit-at-depth == 0] and
// [path-bit-at-depth == 1] portions. Since kvs is sorted lexically by
// path, the bit at any fixed depth is monotonic non-decreasing across
// the slice. The returned index is the start of the bit==1 portion.
func partitionByBit(kvs []bulkKV, depth int) int {
	if len(kvs) == 0 {
		return 0
	}
	return sort.Search(len(kvs), func(i int) bool {
		return getBitAtFromMSB(kvs[i].path, depth) == right
	})
}
