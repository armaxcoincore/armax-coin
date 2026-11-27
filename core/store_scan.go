package core

import (
	"errors"
	"fmt"
	"os"
	"time"

	bolt "go.etcd.io/bbolt"
)

// CanonicalChainInfo describes the canonical chain read by ScanCanonicalBlocks.
type CanonicalChainInfo struct {
	TipHeight int
	TipHash   string
}

// ScanCanonicalBlocks opens the block database at path strictly read-only and calls fn for every
// canonical block from Genesis to the stored tip, in height order.
//
// It is meant for offline tooling (migration reconciliation, treasury tranche planning) and never
// writes to the file. bbolt holds an exclusive lock while a node runs, so the open fails after
// timeout instead of waiting forever; stop the node or scan a copy of the database.
//
// Before any block reaches fn it checks the schema version, that the stored Genesis matches the
// active network profile, that every height is present with a matching index, that each block
// links to its parent, and that each block's Merkle root matches its transactions. It does not
// re-verify proof of work or signatures: the database is expected to come from a node that already
// validated the chain, and tools should publish TipHash so others can compare it with their own
// node.
func ScanCanonicalBlocks(path string, timeout time.Duration, fn func(b *Block) error) (CanonicalChainInfo, error) {
	var info CanonicalChainInfo
	if fn == nil {
		return info, errors.New("scan callback is nil")
	}
	if st, err := os.Stat(path); err != nil || st.IsDir() {
		// The path is not echoed back: it usually sits under the user's home directory.
		return info, errors.New("block database file not found")
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{ReadOnly: true, Timeout: timeout})
	if err != nil {
		return info, fmt.Errorf("open block database read-only (stop the node or use a copy): %w", err)
	}
	defer func() { _ = db.Close() }()

	err = db.View(func(tx *bolt.Tx) error {
		bktM := tx.Bucket(bktMeta)
		bktB := tx.Bucket(bktBlocks)
		if bktM == nil || bktB == nil {
			return errors.New("database is missing the meta or blocks bucket")
		}
		verBytes := bktM.Get(keySchemaVersion)
		if len(verBytes) != 8 || bytesToUint64(verBytes) != CurrentSchemaVersion {
			return fmt.Errorf("incompatible database schema version (required v%d)", CurrentSchemaVersion)
		}
		tipBytes := bktM.Get(keyTipHeight)
		if len(tipBytes) != 8 {
			return errors.New("tip_height is missing or malformed")
		}
		tip := bytesToUint64(tipBytes)
		if tip > uint64(^uint32(0)) {
			return fmt.Errorf("tip_height %d is out of range", tip)
		}

		genesis := GetGenesisBlock()
		prevHash := ""
		for h := uint64(0); h <= tip; h++ {
			raw := bktB.Get(uint64ToBytes(h))
			if raw == nil {
				return fmt.Errorf("missing block at height %d (tip %d)", h, tip)
			}
			b := new(Block)
			if err := unmarshalRecord(raw, b); err != nil {
				return fmt.Errorf("decode block at height %d: %w", h, err)
			}
			if b.Index != int(h) {
				return fmt.Errorf("block at height %d declares index %d", h, b.Index)
			}
			if h == 0 {
				if b.Hash != genesis.Hash {
					return fmt.Errorf("database Genesis %s does not match the active network Genesis %s", b.Hash, genesis.Hash)
				}
			} else if b.PrevHash != prevHash {
				return fmt.Errorf("block %d does not link to its parent", h)
			}
			if h > 0 && b.MerkleRoot != b.CalculateMerkleRoot() {
				return fmt.Errorf("block %d Merkle root does not match its transactions", h)
			}
			prevHash = b.Hash
			if err := fn(b); err != nil {
				return err
			}
		}
		info.TipHeight = int(tip)
		info.TipHash = prevHash
		return nil
	})
	if err != nil {
		return CanonicalChainInfo{}, err
	}
	return info, nil
}
