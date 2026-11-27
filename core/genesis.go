package core

import (
	"fmt"
	"strings"
	"sync"
)

const (
	// GenesisTimestamp is the deterministic immutable timestamp for the Armaxcoin Mainnet Genesis block
	GenesisTimestamp = int64(1710000000) // 2024-03-09T18:40:00Z

	// MainnetGenesisMiner is the Mainnet burn address (32-byte 0x00 payload, HRP "ax") receiving the
	// initial Genesis reward [P2-2].
	MainnetGenesisMiner = "ax1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqyum580"
	// TestnetGenesisMiner is the Testnet burn address (32-byte 0x00 payload, HRP "armx"); frozen with Testnet Genesis.
	TestnetGenesisMiner = "armx1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqplwcdd"
	// RegtestGenesisMiner is the Regtest burn address (32-byte 0x00 payload, HRP "axrt").
	RegtestGenesisMiner = "axrt1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqa862k7"
	// SoakGenesisMiner is the Soak burn address (32-byte 0x00 payload, HRP "axsk").
	SoakGenesisMiner = "axsk1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqpactgh"

	// GenesisMiner is the Testnet burn address, kept for callers that predate per-network prefixes.
	// Use GenesisMinerForNetwork for network-aware code.
	GenesisMiner = TestnetGenesisMiner

	// GenesisNonce is the Proof of Work nonce solving the Mainnet Genesis block
	// (first nonce from 0 meeting GenesisTargetHex after the burn address moved to the "ax" prefix).
	GenesisNonce = int64(12)

	// GenesisTargetHex is the Proof of Work target ceiling for the Mainnet Genesis block (Difficulty 1) [C-31]
	GenesisTargetHex = "0fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"

	// GenesisMerkleRoot is the Merkle root of the Mainnet Genesis coinbase transaction [C-07]
	GenesisMerkleRoot = "d4043c6881da932c78f0571da6bcd55d313c05eaeb0d1f790da5d9bec5e0b81f"

	// GenesisHash is the canonical double-SHA256 (SHA-256d) hash of the Mainnet Genesis block
	GenesisHash = "0ffbf057398af25d6c1ba605c48cf344c8631440778c31bbe7629cdd36342f72"

	// GenesisPrevHash is the 64-zero parent hash of block 0
	GenesisPrevHash = "0000000000000000000000000000000000000000000000000000000000000000"

	// Testnet Genesis Constants [P2-6, L1-1, L1-2, L1-6, L4-8]
	TestnetGenesisTimestamp  = int64(1710000001)
	TestnetGenesisNonce      = int64(40272237)
	TestnetGenesisTargetHex  = "0000000fffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	TestnetGenesisMerkleRoot = "808a4007681dd123133bfe106b101d251ff0d2d023f600384394673bfc03768d"
	TestnetGenesisHash       = "0000000dc84202e63a340a2439625fbf9cea5cb7b84119f93d52cff39f63a68b"
)

// GenesisMinerForNetwork returns the zero-payload burn address of a network's Genesis block,
// encoded with that network's address prefix. An empty or unknown ID means the active network;
// an unknown active network falls back to the Testnet burn address.
func GenesisMinerForNetwork(netID string) string {
	if miner, ok := genesisMinerByID(netID); ok {
		return miner
	}
	if miner, ok := genesisMinerByID(GetActiveNetwork().NetworkID); ok {
		return miner
	}
	return TestnetGenesisMiner
}

func genesisMinerByID(netID string) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(netID)) {
	case "ARMX-MAINNET-1", "MAINNET":
		return MainnetGenesisMiner, true
	case "ARMX-TESTNET-1", "TESTNET":
		return TestnetGenesisMiner, true
	case "ARMX-REGTEST-1", "REGTEST":
		return RegtestGenesisMiner, true
	case "ARMX-SOAK-1", "SOAK":
		return SoakGenesisMiner, true
	}
	return "", false
}

// GetMainnetGenesisBlock returns the hardcoded Mainnet Genesis block [P2-6]
func GetMainnetGenesisBlock() Block {
	coinbase := Transaction{
		Sender:    "SYSTEM",
		SenderPub: "",
		Recipient: MainnetGenesisMiner,
		Amount:    InitialReward,
		Fee:       0,
		Nonce:     0,
		Signature: "REWARD_SIG",
	}

	return Block{
		Index:        0,
		Timestamp:    GenesisTimestamp,
		Transactions: []Transaction{coinbase},
		PrevHash:     GenesisPrevHash,
		MerkleRoot:   GenesisMerkleRoot,
		Hash:         GenesisHash,
		Nonce:        GenesisNonce,
		Miner:        MainnetGenesisMiner,
		Difficulty:   1,
		Target:       GenesisTargetHex,
	}
}

// GetTestnetGenesisBlock returns the hardcoded Testnet Genesis block [P2-6, L1-1, L1-6]
func GetTestnetGenesisBlock() Block {
	coinbase := Transaction{
		Sender:    "SYSTEM",
		SenderPub: "",
		Recipient: TestnetGenesisMiner,
		Amount:    InitialReward,
		Fee:       0,
		Nonce:     0,
		Signature: "REWARD_SIG",
	}

	return Block{
		Index:        0,
		Timestamp:    TestnetGenesisTimestamp,
		Transactions: []Transaction{coinbase},
		PrevHash:     GenesisPrevHash,
		MerkleRoot:   TestnetGenesisMerkleRoot,
		Hash:         TestnetGenesisHash,
		Nonce:        TestnetGenesisNonce,
		Miner:        TestnetGenesisMiner,
		Difficulty:   7,
		Target:       TestnetGenesisTargetHex,
	}
}

var (
	soakGenesisOnce    sync.Once
	soakGenesisBlock   Block
	regtestGenesisOnce sync.Once
	regtestGenesisBlk  Block
)

// GetSoakGenesisBlock returns the deterministic 16-bit target Genesis block for soak testing [L3-5]
func GetSoakGenesisBlock() Block {
	soakGenesisOnce.Do(func() {
		coinbase := Transaction{
			Sender:    "SYSTEM",
			SenderPub: "",
			Recipient: SoakGenesisMiner,
			Amount:    InitialReward,
			Fee:       0,
			Nonce:     0,
			Signature: "REWARD_SIG",
		}

		b := Block{
			Index:        0,
			Timestamp:    1710000002,
			Transactions: []Transaction{coinbase},
			PrevHash:     GenesisPrevHash,
			MerkleRoot:   CalculateMerkleRootForNetwork([]Transaction{coinbase}, SoakProfile.NetworkID),
			Miner:        SoakGenesisMiner,
			Difficulty:   4,
			Target:       SoakProfile.PowLimitHex,
		}
		target := DifficultyToTarget(4)
		for {
			var err error
			b.Hash, err = b.CalculateHashWithError()
			if err == nil && HashMeetsTarget(b.Hash, target) {
				break
			}
			b.Nonce++
		}
		soakGenesisBlock = b
	})
	cp := soakGenesisBlock
	cp.Transactions = append([]Transaction(nil), soakGenesisBlock.Transactions...)
	return cp
}

// GetRegtestGenesisBlock returns the deterministic 8-bit target Genesis block for local regression testing [L4-8]
func GetRegtestGenesisBlock() Block {
	regtestGenesisOnce.Do(func() {
		coinbase := Transaction{
			Sender:    "SYSTEM",
			SenderPub: "",
			Recipient: RegtestGenesisMiner,
			Amount:    InitialReward,
			Fee:       0,
			Nonce:     0,
			Signature: "REWARD_SIG",
		}

		b := Block{
			Index:        0,
			Timestamp:    1710000003,
			Transactions: []Transaction{coinbase},
			PrevHash:     GenesisPrevHash,
			MerkleRoot:   CalculateMerkleRootForNetwork([]Transaction{coinbase}, RegtestProfile.NetworkID),
			Miner:        RegtestGenesisMiner,
			Difficulty:   2,
			Target:       RegtestProfile.PowLimitHex,
		}
		target := b.GetTarget()
		for {
			var err error
			b.Hash, err = b.CalculateHashWithError()
			if err == nil && HashMeetsTarget(b.Hash, target) {
				break
			}
			b.Nonce++
		}
		regtestGenesisBlk = b
	})
	cp := regtestGenesisBlk
	cp.Transactions = append([]Transaction(nil), regtestGenesisBlk.Transactions...)
	return cp
}

// GetGenesisBlockForNetwork returns the Genesis block for a specific network ID [C-33, P2-6]
func GetGenesisBlockForNetwork(netID string) Block {
	canon := strings.ToUpper(strings.TrimSpace(netID))
	switch canon {
	case "ARMX-MAINNET-1", "MAINNET":
		return GetMainnetGenesisBlock()
	case "ARMX-TESTNET-1", "TESTNET":
		return GetTestnetGenesisBlock()
	case "ARMX-REGTEST-1", "REGTEST":
		return GetRegtestGenesisBlock()
	case "ARMX-SOAK-1", "SOAK":
		return GetSoakGenesisBlock()
	default:
		return GetGenesisBlock()
	}
}

// GetGenesisBlock returns the Genesis block for the active network profile [C-33, P2-6, L3-5, L4-8]
func GetGenesisBlock() Block {
	if GetActiveNetwork().Name == "testnet" {
		return GetTestnetGenesisBlock()
	}
	if GetActiveNetwork().Name == "regtest" {
		return GetRegtestGenesisBlock()
	}
	if GetActiveNetwork().Name == "soak" {
		return GetSoakGenesisBlock()
	}
	return GetMainnetGenesisBlock()
}

// ValidateGenesisBlockForNetwork strictly verifies that block 0 matches the network's protocol Genesis specification [C-23, P2-6]
func ValidateGenesisBlockForNetwork(b *Block, netID string) error {
	if b == nil {
		return fmt.Errorf("genesis block is nil")
	}
	gen := GetGenesisBlockForNetwork(netID)

	if b.Index != 0 {
		return fmt.Errorf("genesis block index must be 0, got %d", b.Index)
	}
	if b.Hash != gen.Hash {
		return fmt.Errorf("genesis block hash mismatch: got %s, expected %s", b.Hash, gen.Hash)
	}
	if b.PrevHash != GenesisPrevHash {
		return fmt.Errorf("genesis block prevhash mismatch")
	}
	if b.Timestamp != gen.Timestamp {
		return fmt.Errorf("genesis block timestamp mismatch: got %d, expected %d", b.Timestamp, gen.Timestamp)
	}
	if b.MerkleRoot != gen.MerkleRoot {
		return fmt.Errorf("genesis block merkle root mismatch: got %s, expected %s", b.MerkleRoot, gen.MerkleRoot)
	}
	if b.Nonce != gen.Nonce {
		return fmt.Errorf("genesis block nonce mismatch")
	}
	if b.Miner != gen.Miner {
		return fmt.Errorf("genesis block miner mismatch: got %s, expected %s", b.Miner, gen.Miner)
	}
	calcHash, err := b.CalculateHashWithError()
	if err != nil {
		return fmt.Errorf("genesis block calculated hash error: %w", err)
	}
	if b.Hash == "" || calcHash != gen.Hash {
		return fmt.Errorf("genesis block calculated hash mismatch")
	}
	if b.CalculateMerkleRootForNetwork(netID) != gen.MerkleRoot {
		return fmt.Errorf("genesis block calculated merkle root mismatch")
	}
	if len(b.Transactions) != len(gen.Transactions) {
		return fmt.Errorf("genesis block transaction count mismatch: got %d, expected %d", len(b.Transactions), len(gen.Transactions))
	}
	strictGenesis := isStrictCoinbaseConsensusActive(netID)
	for i := range b.Transactions {
		t1 := b.Transactions[i]
		t2 := gen.Transactions[i]
		if t1.Sender != t2.Sender || t1.Recipient != t2.Recipient || t1.Amount != t2.Amount ||
			t1.Fee != t2.Fee || t1.Nonce != t2.Nonce || t1.Signature != t2.Signature ||
			t1.Type != t2.Type || t1.SenderPub != t2.SenderPub {
			return fmt.Errorf("genesis transaction %d payload mismatch", i)
		}
		if strictGenesis && (t1.SigAlg != t2.SigAlg || t1.HashLock != t2.HashLock || t1.TimeLock != t2.TimeLock || t1.Preimage != t2.Preimage) {
			return fmt.Errorf("genesis transaction %d payload mismatch", i)
		}
		if t1.WitnessHashHexForNetwork(netID) != t2.WitnessHashHexForNetwork(netID) {
			return fmt.Errorf("genesis transaction %d hash mismatch", i)
		}
	}
	return nil
}

// ValidateGenesisBlock strictly verifies that block 0 matches the active network profile's Genesis specification [C-23, P2-6].
func ValidateGenesisBlock(b *Block) error {
	return ValidateGenesisBlockForNetwork(b, GetActiveNetwork().NetworkID)
}
