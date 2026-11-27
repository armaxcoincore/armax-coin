//go:build testhooks

package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/armaxcoincore/armax-coin/core"
	"github.com/armaxcoincore/armax-coin/core/pqsig"
	bolt "go.etcd.io/bbolt"
)

func hexBytes(b []byte) string                 { return hex.EncodeToString(b) }
func mkdir(dir string) error                   { return os.MkdirAll(dir, 0o700) }
func writeFile(path string, data []byte) error { return os.WriteFile(path, data, 0o600) }
func readFile(path string) ([]byte, error)     { return os.ReadFile(path) }

// armx converts whole coins to Armoids.
func armx(n int64) int64 { return n * core.CoinUnit }

// testKey derives a deterministic ML-DSA-65 key from a label.
func testKey(t *testing.T, label string) *pqsig.PrivateKey {
	t.Helper()
	sk, err := pqsig.GenerateFromSeed(sha256.Sum256([]byte("armreconcile-test:" + label)))
	if err != nil {
		t.Fatal(err)
	}
	return sk
}

// addrOn is the address of key on a network ("" = active network).
func addrOn(t *testing.T, key *pqsig.PrivateKey, netID string) string {
	t.Helper()
	a, err := core.PubToAddressForNetwork(pqsig.SigAlgMLDSA65, key.PublicKeyBytes(), netID)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// testChain builds a canonical chain for the active network. Block hashes are synthetic; the
// scanner checks linkage and Merkle roots, not proof of work.
type testChain struct {
	blocks []core.Block
	nonce  int64
}

func newTestChain() *testChain {
	return &testChain{blocks: []core.Block{core.GetGenesisBlock()}}
}

func (c *testChain) height() int { return len(c.blocks) - 1 }

func coinbase(to string, amount int64) core.Transaction {
	return core.Transaction{Sender: "SYSTEM", Recipient: to, Amount: amount, Signature: "REWARD_SIG"}
}

// transfer returns a non-coinbase transaction with a unique nonce, so its hash is unique.
func (c *testChain) transfer(from, to string, amount int64) core.Transaction {
	c.nonce++
	return core.Transaction{Sender: from, Recipient: to, Amount: amount, Fee: 1000, Nonce: c.nonce, SigAlg: pqsig.SigAlgMLDSA65}
}

func (c *testChain) add(txs ...core.Transaction) *core.Block {
	h := len(c.blocks)
	b := core.Block{
		Index:        h,
		Timestamp:    int64(1_700_000_000 + 120*h),
		PrevHash:     c.blocks[h-1].Hash,
		Transactions: txs,
	}
	if b.Transactions == nil {
		b.Transactions = []core.Transaction{}
	}
	for i := range b.Transactions {
		if b.Transactions[i].Sender == "SYSTEM" {
			b.Transactions[i].Nonce = int64(h)
		}
	}
	b.MerkleRoot = b.CalculateMerkleRoot()
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s-block-%d-%s", core.NetworkID(), h, b.MerkleRoot)))
	b.Hash = hex.EncodeToString(sum[:])
	c.blocks = append(c.blocks, b)
	return &c.blocks[h]
}

// padTo appends empty blocks until the tip reaches height.
func (c *testChain) padTo(height int) {
	for c.height() < height {
		c.add()
	}
}

// write stores the chain in a fresh bbolt file laid out like a node database.
func (c *testChain) write(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "blockchain.db")
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	u64 := func(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }
	err = db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucket([]byte("meta"))
		if err != nil {
			return err
		}
		blocks, err := tx.CreateBucket([]byte("blocks"))
		if err != nil {
			return err
		}
		if err := meta.Put([]byte("schema_version"), u64(core.CurrentSchemaVersion)); err != nil {
			return err
		}
		if err := meta.Put([]byte("tip_height"), u64(uint64(c.height()))); err != nil {
			return err
		}
		for i := range c.blocks {
			data, err := json.Marshal(&c.blocks[i])
			if err != nil {
				return err
			}
			if err := blocks.Put(u64(uint64(i)), data); err != nil {
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

// testPolicy returns a normalized policy for the active network.
func testPolicy(t *testing.T, p Policy) *Policy {
	t.Helper()
	p.Version = PolicyVersion
	p.SourceNetworkID = core.NetworkID()
	if p.TargetNetworkID == "" {
		p.TargetNetworkID = "ARMX-MAINNET-1"
	}
	if err := p.normalize(); err != nil {
		t.Fatalf("policy: %v", err)
	}
	return &p
}

func mustClaim(t *testing.T, key *pqsig.PrivateKey, targetNet, target string, cutoff int) *MigrationClaim {
	t.Helper()
	c, err := signMigrationClaim(key, targetNet, target, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustLedger(t *testing.T, key *pqsig.PrivateKey, m PoolLedgerManifest) *SignedPoolLedger {
	t.Helper()
	m.Version = PoolLedgerVersion
	m.Domain = PoolLedgerDomain
	m.NetworkID = core.NetworkID()
	// Copy the payouts so tests that tamper with a signed ledger never alter the fixture.
	m.Payouts = append([]PoolPayout{}, m.Payouts...)
	l, err := signPoolLedger(m, key)
	if err != nil {
		t.Fatal(err)
	}
	return l
}
