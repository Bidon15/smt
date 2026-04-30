package smt

import (
	"bytes"
	"hash"
)

var leafPrefix = []byte{0}
var nodePrefix = []byte{1}

type treeHasher struct {
	hasher    hash.Hash
	zeroValue []byte
}

func newTreeHasher(hasher hash.Hash) *treeHasher {
	th := treeHasher{hasher: hasher}
	th.zeroValue = make([]byte, th.pathSize())

	return &th
}

func (th *treeHasher) digest(data []byte) []byte {
	th.hasher.Write(data)
	sum := th.hasher.Sum(nil)
	th.hasher.Reset()
	return sum
}

// digestInto writes the digest of data into dst[:0] and returns the resulting
// slice. The caller is responsible for ensuring dst has at least Size bytes
// of capacity. Used on the hot path with pool-managed buffers to avoid the
// per-call allocation that hash.Hash.Sum(nil) incurs.
func (th *treeHasher) digestInto(dst, data []byte) []byte {
	th.hasher.Write(data)
	sum := th.hasher.Sum(dst[:0])
	th.hasher.Reset()
	return sum
}

func (th *treeHasher) path(key []byte) []byte {
	return th.digest(key)
}

// pathInto computes the SHA-256 path of key into dst[:0]. See digestInto.
func (th *treeHasher) pathInto(dst, key []byte) []byte {
	return th.digestInto(dst, key)
}

func (th *treeHasher) digestLeaf(path []byte, leafData []byte) ([]byte, []byte) {
	value := make([]byte, 0, len(leafPrefix)+len(path)+len(leafData))
	value = append(value, leafPrefix...)
	value = append(value, path...)
	value = append(value, leafData...)

	th.hasher.Write(value)
	sum := th.hasher.Sum(nil)
	th.hasher.Reset()

	return sum, value
}

// digestLeafInto writes the leaf hash into hashDst[:0] and returns it
// alongside the leaf-data value buffer (which still escapes to the
// MapStore). hashDst must have at least Size bytes of capacity.
func (th *treeHasher) digestLeafInto(hashDst, path, leafData []byte) ([]byte, []byte) {
	value := make([]byte, 0, len(leafPrefix)+len(path)+len(leafData))
	value = append(value, leafPrefix...)
	value = append(value, path...)
	value = append(value, leafData...)

	th.hasher.Write(value)
	hash := th.hasher.Sum(hashDst[:0])
	th.hasher.Reset()

	return hash, value
}

func (th *treeHasher) parseLeaf(data []byte) ([]byte, []byte) {
	return data[len(leafPrefix) : th.pathSize()+len(leafPrefix)], data[len(leafPrefix)+th.pathSize():]
}

func (th *treeHasher) isLeaf(data []byte) bool {
	return bytes.Equal(data[:len(leafPrefix)], leafPrefix)
}

func (th *treeHasher) digestNode(leftData []byte, rightData []byte) ([]byte, []byte) {
	value := make([]byte, 0, len(nodePrefix)+len(leftData)+len(rightData))
	value = append(value, nodePrefix...)
	value = append(value, leftData...)
	value = append(value, rightData...)

	th.hasher.Write(value)
	sum := th.hasher.Sum(nil)
	th.hasher.Reset()

	return sum, value
}

// digestNodeInto writes the node hash into hashDst[:0] and returns it
// alongside the freshly allocated node-data value buffer (which still
// escapes to the MapStore). hashDst must have at least Size bytes of
// capacity.
func (th *treeHasher) digestNodeInto(hashDst, leftData, rightData []byte) ([]byte, []byte) {
	value := make([]byte, 0, len(nodePrefix)+len(leftData)+len(rightData))
	value = append(value, nodePrefix...)
	value = append(value, leftData...)
	value = append(value, rightData...)

	th.hasher.Write(value)
	hash := th.hasher.Sum(hashDst[:0])
	th.hasher.Reset()

	return hash, value
}

func (th *treeHasher) parseNode(data []byte) ([]byte, []byte) {
	return data[len(nodePrefix) : th.pathSize()+len(nodePrefix)], data[len(nodePrefix)+th.pathSize():]
}

func (th *treeHasher) pathSize() int {
	return th.hasher.Size()
}

func (th *treeHasher) placeholder() []byte {
	return th.zeroValue
}
