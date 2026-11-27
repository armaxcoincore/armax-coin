package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// writeScanTestDB writes a minimal canonical chain database: Genesis plus n linked blocks with one
// coinbase each. mutate may alter blocks before they are stored.
func writeScanTestDB(t *testing.T, n int, schema uint64, mutate func(blocks []Block)) string {
	t.Helper()
	blocks := []Block{GetGenesisBlock()}
	for h := 1; h <= n; h++ {
		b := Block{
			Index:        h,
			Timestamp:    int64(1_700_000_000 + h),
			PrevHash:     blocks[h-1].Hash,
			Transactions: []Transaction{{Sender: "SYSTEM", Recipient: GenesisMinerForNetwork(""), Amount: CoinUnit, Nonce: int64(h), Signature: "REWARD_SIG"}},
		}
		b.MerkleRoot = b.CalculateMerkleRoot()
		sum := sha256.Sum256([]byte(fmt.Sprintf("scan-test-block-%d", h)))
		b.Hash = hex.EncodeToString(sum[:])
		blocks = append(blocks, b)
	}
	if mutate != nil {
		mutate(blocks)
	}
	path := filepath.Join(t.TempDir(), "blockchain.db")
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	err = db.Update(func(tx *bolt.Tx) error {
		bm, err := tx.CreateBucket(bktMeta)
		if err != nil {
			return err
		}
		bb, err := tx.CreateBucket(bktBlocks)
		if err != nil {
			return err
		}
		if err := bm.Put(keySchemaVersion, uint64ToBytes(schema)); err != nil {
			return err
		}
		if err := bm.Put(keyTipHeight, uint64ToBytes(uint64(n))); err != nil {
			return err
		}
		for i := range blocks {
			data, err := json.Marshal(&blocks[i])
			if err != nil {
				return err
			}
			if err := bb.Put(uint64ToBytes(uint64(i)), encodeRecord(data)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestScanCanonicalBlocks_ReadsChainInOrderWithoutWriting(t *testing.T) {
	path := writeScanTestDB(t, 5, CurrentSchemaVersion, nil)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var heights []int
	info, err := ScanCanonicalBlocks(path, time.Second, func(b *Block) error {
		heights = append(heights, b.Index)
		return nil
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if info.TipHeight != 5 || len(heights) != 6 || heights[0] != 0 || heights[5] != 5 {
		t.Fatalf("unexpected scan: info=%+v heights=%v", info, heights)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("read-only scan modified the database file")
	}
}

func TestScanCanonicalBlocks_ReadsCompressedRecords(t *testing.T) {
	prev := storeCompression.Load()
	storeCompression.Store(true)
	t.Cleanup(func() { storeCompression.Store(prev) })
	path := writeScanTestDB(t, 3, CurrentSchemaVersion, nil)
	if _, err := ScanCanonicalBlocks(path, time.Second, func(*Block) error { return nil }); err != nil {
		t.Fatalf("compressed scan: %v", err)
	}
}

func TestScanCanonicalBlocks_FailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		schema uint64
		mutate func([]Block)
		want   string
	}{
		{"legacy schema", 2, nil, "schema version"},
		{"foreign genesis", CurrentSchemaVersion, func(b []Block) { b[0].Hash = strings.Repeat("ab", 32) }, "Genesis"},
		{"broken link", CurrentSchemaVersion, func(b []Block) { b[3].PrevHash = strings.Repeat("cd", 32) }, "does not link"},
		{"index mismatch", CurrentSchemaVersion, func(b []Block) { b[2].Index = 7 }, "declares index"},
		{"tampered transactions", CurrentSchemaVersion, func(b []Block) { b[4].Transactions[0].Amount++ }, "Merkle root"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeScanTestDB(t, 5, tc.schema, tc.mutate)
			_, err := ScanCanonicalBlocks(path, time.Second, func(*Block) error { return nil })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}

	if _, err := ScanCanonicalBlocks(filepath.Join(t.TempDir(), "missing.db"), time.Second, func(*Block) error { return nil }); err == nil || strings.Contains(err.Error(), "missing.db") {
		t.Fatalf("missing file must fail without echoing the path, got %v", err)
	}
}

func TestScanCanonicalBlocks_TimesOutWhileNodeHoldsTheLock(t *testing.T) {
	path := writeScanTestDB(t, 2, CurrentSchemaVersion, nil)
	writer, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	start := time.Now()
	_, err = ScanCanonicalBlocks(path, 200*time.Millisecond, func(*Block) error { return nil })
	if err == nil {
		t.Fatal("scan must fail while another process holds the database lock")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("scan did not honour its timeout")
	}
}
