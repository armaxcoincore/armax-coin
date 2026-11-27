package core

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Dual-coinbase treasury subsidy split.
//
// While IsTreasurySplitActive(h), a block carries two protocol coinbases:
//
//	tx[0] miner coinbase    Signature "REWARD_SIG",          Amount <= reward - treasuryShare + fees
//	tx[1] treasury coinbase Signature "TREASURY_REWARD_SIG", Amount == treasuryShare, Recipient == TreasuryAddress
//
// and user transactions start at index 2. Outside the window only tx[0] may be a SYSTEM
// transaction, exactly as before. Testnet and Soak keep the split disabled, so their blocks are
// unchanged. See docs/architecture/consensus/mainnet-treasury-subsidy-split.md.
const (
	// TreasuryRewardSignature is the signature sentinel of the tx[1] treasury coinbase.
	TreasuryRewardSignature = "TREASURY_REWARD_SIG"
	// TreasurySharePercent is the treasury's share of the block subsidy (fees excluded).
	TreasurySharePercent = 20

	// RegtestTreasuryAddress is a test-only 2-of-3 multisig built from public, deterministic
	// fixture keys: seed_i = SHA-256("armax-regtest-treasury-fixture:<i>"), i = 1..3. Anyone can
	// derive these keys, so the address must never hold value outside Regtest.
	RegtestTreasuryAddress = "axrt1jv7vzdww9f7xp2da8tpq4zm3dhy6ztttnc9ltr7sd0vxsvgrmp4sspy090"
)

var (
	treasuryOverrideMu             sync.RWMutex
	treasurySplitEndHeightOverride *int64
	treasuryAddressOverride        *string
)

// GetTreasurySplitEndHeight returns the last height of the treasury split on the active network.
// On Regtest, ARMX_TREASURY_SPLIT_END_HEIGHT enables a short local window.
func GetTreasurySplitEndHeight() int64 {
	treasuryOverrideMu.RLock()
	override := treasurySplitEndHeightOverride
	treasuryOverrideMu.RUnlock()
	if override != nil {
		return *override
	}
	prof := GetActiveNetwork()
	if prof.Name == "regtest" {
		if val := os.Getenv("ARMX_TREASURY_SPLIT_END_HEIGHT"); val != "" {
			if h, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil && h >= -1 && h <= math.MaxInt32 {
				return h
			}
		}
	}
	return prof.TreasurySplitEndHeight
}

// GetTreasuryAddress returns the treasury coinbase recipient on the active network.
func GetTreasuryAddress() string {
	treasuryOverrideMu.RLock()
	override := treasuryAddressOverride
	treasuryOverrideMu.RUnlock()
	if override != nil {
		return *override
	}
	return GetActiveNetwork().TreasuryAddress
}

// SetTreasurySplitEndHeight overrides the treasury split end height (for tests).
func SetTreasurySplitEndHeight(height int64) {
	treasuryOverrideMu.Lock()
	defer treasuryOverrideMu.Unlock()
	treasurySplitEndHeightOverride = &height
}

// SetTreasuryAddress overrides the treasury address (for tests).
func SetTreasuryAddress(addr string) {
	treasuryOverrideMu.Lock()
	defer treasuryOverrideMu.Unlock()
	treasuryAddressOverride = &addr
}

// ResetTreasuryOverrides restores the network profile treasury settings.
func ResetTreasuryOverrides() {
	treasuryOverrideMu.Lock()
	defer treasuryOverrideMu.Unlock()
	treasurySplitEndHeightOverride = nil
	treasuryAddressOverride = nil
}

// IsTreasurySplitActive reports whether the block at height must carry the tx[1] treasury
// coinbase. It is never active at Genesis, and never while the treasury address is unset.
func IsTreasurySplitActive(height int) bool {
	end := GetTreasurySplitEndHeight()
	return end >= 1 && height >= 1 && int64(height) <= end && GetTreasuryAddress() != ""
}

// TreasuryShare returns the treasury part of a block subsidy (integer division, fees excluded).
func TreasuryShare(allowedBlockReward int64) int64 {
	if allowedBlockReward <= 0 || allowedBlockReward > MaxSupply {
		return 0
	}
	return allowedBlockReward * TreasurySharePercent / 100
}

// FirstUserTxIndex returns the index of the first user transaction in a block at height.
func FirstUserTxIndex(height int) int {
	if IsTreasurySplitActive(height) {
		return 2
	}
	return 1
}

// NewTreasuryCoinbase builds the tx[1] treasury coinbase for a block at height.
func NewTreasuryCoinbase(height int, allowedBlockReward int64) Transaction {
	return Transaction{
		Sender:    "SYSTEM",
		Recipient: GetTreasuryAddress(),
		Amount:    TreasuryShare(allowedBlockReward),
		Nonce:     int64(height),
		Signature: TreasuryRewardSignature,
	}
}

// isTreasuryCoinbaseShape reports whether tx looks like a treasury coinbase. Supply accounting
// uses it on blocks that have already passed validation.
func isTreasuryCoinbaseShape(tx *Transaction) bool {
	return tx != nil && tx.Sender == "SYSTEM" && tx.Signature == TreasuryRewardSignature
}

// validateTreasuryCoinbase checks tx[1] of a block at height against the split rules.
func validateTreasuryCoinbase(tx *Transaction, height int, allowedBlockReward int64, validAddress func(string) bool) error {
	switch {
	case tx.Sender != "SYSTEM":
		return fmt.Errorf("block #%d tx #1 treasury coinbase sender must be exact 'SYSTEM', got '%s'", height, tx.Sender)
	case tx.Signature != TreasuryRewardSignature:
		return fmt.Errorf("block #%d tx #1 treasury coinbase signature sentinel mismatch", height)
	case tx.Fee != 0:
		return fmt.Errorf("block #%d tx #1 treasury coinbase fee must be 0, got %d", height, tx.Fee)
	case tx.SenderPub != "":
		return fmt.Errorf("block #%d tx #1 treasury coinbase sender_pub must be empty", height)
	case tx.Nonce != int64(height):
		return fmt.Errorf("block #%d tx #1 treasury coinbase nonce must match block height, got %d", height, tx.Nonce)
	case tx.SigAlg != 0 && tx.SigAlg != 0x01:
		return fmt.Errorf("block #%d tx #1 treasury coinbase sig_alg must be 0 or 0x01, got 0x%02x", height, tx.SigAlg)
	case tx.Type != "" && tx.Type != TxTypeStandard:
		return fmt.Errorf("block #%d tx #1 treasury coinbase type must be empty or STANDARD, got '%s'", height, tx.Type)
	case tx.HashLock != "" || tx.TimeLock != 0 || tx.Preimage != "":
		return fmt.Errorf("block #%d tx #1 treasury coinbase must not carry HTLC/extended fields", height)
	case tx.Amount != TreasuryShare(allowedBlockReward):
		return fmt.Errorf("block #%d tx #1 treasury coinbase amount must be exactly %d, got %d", height, TreasuryShare(allowedBlockReward), tx.Amount)
	case !validAddress(tx.Recipient) || !MatchAddress(tx.Recipient, GetTreasuryAddress()):
		return fmt.Errorf("block #%d tx #1 treasury coinbase recipient must be the treasury address", height)
	}
	return nil
}
