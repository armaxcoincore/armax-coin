package main

import "crypto/sha256"

// merkleRoot computes the RFC 6962 Merkle tree hash over leaf data: leaves are hashed as
// SHA-256(0x00 || leaf) and interior nodes as SHA-256(0x01 || left || right), with the split at
// the largest power of two below the leaf count. The prefixes stop a leaf from posing as an
// interior node, and the split rule means an odd leaf is never duplicated.
func merkleRoot(leaves [][]byte) [32]byte {
	if len(leaves) == 0 {
		return sha256.Sum256(nil)
	}
	if len(leaves) == 1 {
		return sha256.Sum256(append([]byte{0x00}, leaves[0]...))
	}
	k := 1
	for k*2 < len(leaves) {
		k *= 2
	}
	left := merkleRoot(leaves[:k])
	right := merkleRoot(leaves[k:])
	buf := make([]byte, 0, 65)
	buf = append(buf, 0x01)
	buf = append(buf, left[:]...)
	buf = append(buf, right[:]...)
	return sha256.Sum256(buf)
}
