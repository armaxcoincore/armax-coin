//go:build testhooks

package node

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/armaxcoincore/armax-coin/core"
)

func mineP2PTestBlockOnChain(t *testing.T, bc *core.Blockchain, chain []core.Block, prev core.Block, miner string, ts int64) core.Block {
	t.Helper()
	target := core.CalculateExpectedTargetForCandidate(chain, prev.Index+1, ts)
	cb := core.Transaction{
		Sender:    "SYSTEM",
		Recipient: miner,
		Amount:    bc.GetBlockReward(prev.Index + 1),
		Nonce:     int64(prev.Index + 1),
		Signature: "REWARD_SIG",
	}
	blk := *core.NewBlock([]core.Transaction{cb}, prev.Hash, prev.Index+1, miner, target)
	blk.Timestamp = ts
	for {
		h, err := blk.CalculateHashWithError()
		if err != nil {
			t.Fatalf("CalculateHashWithError failed: %v", err)
		}
		if core.HashMeetsTarget(h, target) {
			blk.Hash = h
			break
		}
		blk.Nonce++
	}
	return blk
}

// TestP2P_ClockSkew3TierParityAcrossNewBlockAndReorg verifies:
//  1. ProcessIncomingBlock (tip path), handlePushedBlocks direct tip extension, and handlePushedBlocks
//     reorg (ResolveFork) path all apply the unified 3-tier future-timestamp policy via handleBlockValidationError:
//     - <=30s skew: 0 ban score, not negative-cached
//     - 31..600s skew: clock-skew strike system (+5 only after >=3 strikes), not negative-cached
//     - >600s skew: immediate +10 ban score and negative-cached
//     - Non-clock-skew error: +20 ban score and negative-cached
func TestP2P_ClockSkew3TierParityAcrossNewBlockAndReorg(t *testing.T) {
	core.LockNetworkForTesting(t)
	core.SetTargetOverrideForTesting(core.DifficultyToTarget(1))
	core.SetFutureTimestampLimitActivationHeight(1)

	dbPath := filepath.Join(t.TempDir(), "p2p_skew_parity.db")
	bc, err := core.NewBlockchainWithStoragePath(1, dbPath)
	if err != nil {
		t.Fatalf("NewBlockchainWithStoragePath failed: %v", err)
	}
	t.Cleanup(func() { _ = bc.Close() })

	ps := NewPeerServer(getFreePort(t), bc, t.TempDir())
	minerAcc := newTestAccount(t)
	genesis := bc.GetLatestBlock()
	now := time.Now().Unix()

	// Tier 1 (<=30s skew beyond 600s C6 limit) via ProcessIncomingBlock: 0 ban, not negative-cached.
	tier1Blk := mineP2PTestBlockOnChain(t, bc, bc.Chain, genesis, minerAcc.Address, now+core.FutureTimestampLimitSeconds+15)
	if ok := ps.ProcessIncomingBlock(tier1Blk, "198.51.100.1", "198.51.100.1:19335"); ok {
		t.Fatalf("expected future-timestamp Tier 1 block to be rejected")
	}
	if s := ps.GetBanScore("198.51.100.1"); s != 0 {
		t.Fatalf("Tier 1 ProcessIncomingBlock expected ban score 0, got %d", s)
	}
	if ps.isBlockNegativeCached(tier1Blk.Hash) {
		t.Fatalf("Tier 1 future-timestamp block must NOT be negative-cached")
	}

	// Tier 2 (31..600s skew) via handlePushedBlocks direct tip extension path: 0 ban on 1st strike, not negative-cached.
	tier2Blk := mineP2PTestBlockOnChain(t, bc, bc.Chain, genesis, minerAcc.Address, now+core.FutureTimestampLimitSeconds+120)
	ps.handlePushedBlocks([]core.Block{tier2Blk}, "198.51.100.2", "198.51.100.2:19335")
	if s := ps.GetBanScore("198.51.100.2"); s != 0 {
		t.Fatalf("Tier 2 1st strike handlePushedBlocks expected ban score 0, got %d", s)
	}
	if ps.isBlockNegativeCached(tier2Blk.Hash) {
		t.Fatalf("Tier 2 future-timestamp block must NOT be negative-cached")
	}

	// Tier 3 (>600s skew) via ProcessIncomingBlock: immediate +10 ban and negative-cached.
	tier3Blk := mineP2PTestBlockOnChain(t, bc, bc.Chain, genesis, minerAcc.Address, now+core.FutureTimestampLimitSeconds+700)
	if ok := ps.ProcessIncomingBlock(tier3Blk, "198.51.100.3", "198.51.100.3:19335"); ok {
		t.Fatalf("expected future-timestamp Tier 3 block to be rejected")
	}
	if s := ps.GetBanScore("198.51.100.3"); s != 10 {
		t.Fatalf("Tier 3 ProcessIncomingBlock expected ban score 10, got %d", s)
	}
	if !ps.isBlockNegativeCached(tier3Blk.Hash) {
		t.Fatalf("Tier 3 (>600s) future-timestamp block MUST be negative-cached")
	}

	// Reorg (ResolveFork) path parity: add 1 valid block to local tip first, then push a higher-work 2-block fork
	// whose second block has a Tier 2 (+120s) future timestamp -> must NOT incur +20 hard ban!
	tip1 := mineP2PTestBlockOnChain(t, bc, bc.Chain, genesis, minerAcc.Address, now+1)
	if ok, verr := bc.ValidateAndAddBlockWithError(tip1); !ok || verr != nil {
		t.Fatalf("ValidateAndAddBlockWithError(tip1) failed: ok=%v err=%v", ok, verr)
	}

	altAcc := newTestAccount(t)
	altBlk1 := mineP2PTestBlockOnChain(t, bc, []core.Block{genesis}, genesis, altAcc.Address, now+2)
	altChain1 := []core.Block{genesis, altBlk1}
	altBlk2 := mineP2PTestBlockOnChain(t, bc, altChain1, altBlk1, altAcc.Address, now+core.FutureTimestampLimitSeconds+120)

	reorgIP := "198.51.100.4"
	ps.handlePushedBlocks([]core.Block{altBlk1, altBlk2}, reorgIP, reorgIP+":19335")
	if s := ps.GetBanScore(reorgIP); s != 0 {
		t.Fatalf("Reorg Tier 2 future-timestamp expected ban score 0 (not +20), got %d", s)
	}
	if ps.isBlockNegativeCached(altBlk2.Hash) {
		t.Fatalf("Reorg Tier 2 future-timestamp block must NOT be negative-cached")
	}

	// Direct non-clock-skew validation error through handleBlockValidationError still scores +20 & negative caches.
	ps.handleBlockValidationError("198.51.100.5", "bad-non-skew-hash", fmt.Errorf("merkle root mismatch"))
	if s := ps.GetBanScore("198.51.100.5"); s != 20 {
		t.Fatalf("Non-clock-skew error expected ban score +20, got %d", s)
	}
	if !ps.isBlockNegativeCached("bad-non-skew-hash") {
		t.Fatalf("Non-clock-skew error expected negative cache entry")
	}
}
