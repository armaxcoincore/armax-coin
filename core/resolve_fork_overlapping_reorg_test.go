package core

import (
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// buildCoinbaseOnlyFork mines heights [from, to] on top of parent, paying every coinbase to miner.
func buildCoinbaseOnlyFork(t *testing.T, bc *Blockchain, parent Block, from, to int, miner string) []Block {
	t.Helper()
	var blocks []Block
	prev := parent
	for i := from; i <= to; i++ {
		target := bc.CalculateLWMA1()
		b := *NewBlock([]Transaction{{
			Sender:    "SYSTEM",
			Recipient: miner,
			Amount:    InitialReward,
			Nonce:     int64(i),
			Signature: "REWARD_SIG",
		}}, prev.Hash, i, miner, target)
		b.Timestamp = prev.Timestamp + 1
		for {
			b.Hash = b.CalculateHash()
			if HashMeetsTarget(b.Hash, target) {
				break
			}
			b.Nonce++
		}
		blocks = append(blocks, b)
		prev = b
	}
	return blocks
}

// replayStateFromGenesis rebuilds the account state of chain from scratch for comparison.
func replayStateFromGenesis(t *testing.T, bc *Blockchain, chain []Block) *StateManager {
	t.Helper()
	sm := NewStateManager()
	sm.CoinbaseMaturity = bc.GetCoinbaseMaturity()
	var supply int64
	for i := range chain {
		reward := CalculateBlockRewardGivenSupply(i, supply)
		if err := sm.VerifyAndApplyBlock(&chain[i], reward, bc.GetCoinbaseMaturity()); err != nil {
			t.Fatalf("replay block #%d: %v", i, err)
		}
		supply += BlockMintedSubsidy(&chain[i], reward)
	}
	return sm
}

// TestResolveFork_OverlappingReorgsKeepStateConsistent commits a shallower reorganization while a
// deeper one is still rolling back its state snapshot. The final state must equal a replay of the
// winning chain from genesis; accounts that only mined orphaned blocks must not keep any balance.
func TestResolveFork_OverlappingReorgsKeepStateConsistent(t *testing.T) {
	setupC24EasyTarget(t)
	tempDir := t.TempDir()
	t.Setenv("ARMX_DATA_DIR", tempDir)

	bc := NewBlockchain(1)
	bc.StoragePath = filepath.Join(tempDir, "blockchain.db")
	defer bc.Close()

	minerP := newTestAccount(t, "minerP").Address // local blocks #1..#3 (shared prefix of fork B)
	minerA := newTestAccount(t, "minerA").Address // local blocks #4..#6 (orphaned by both forks)
	minerB := newTestAccount(t, "minerB").Address // fork B #4..#8 from ancestor #3
	minerC := newTestAccount(t, "minerC").Address // fork C #2..#10 from ancestor #1 (heaviest)

	for i := 1; i <= 3; i++ {
		if _, err := bc.MineBlockWithErr(minerP); err != nil {
			t.Fatalf("mine local #%d: %v", i, err)
		}
	}
	for i := 4; i <= 6; i++ {
		if _, err := bc.MineBlockWithErr(minerA); err != nil {
			t.Fatalf("mine local #%d: %v", i, err)
		}
	}

	forkB := buildCoinbaseOnlyFork(t, bc, bc.Chain[3], 4, 8, minerB)
	forkC := buildCoinbaseOnlyFork(t, bc, bc.Chain[1], 2, 10, minerC)

	forkBDone := make(chan error, 1)
	var triggered atomic.Bool
	resolveForkRollbackStepHook = func(height int) {
		// Only the first rollback step of fork C starts fork B; fork B's own steps pass through.
		if height != 6 || !triggered.CompareAndSwap(false, true) {
			return
		}
		go func() { forkBDone <- bc.ResolveFork(3, forkB) }()
		// Give the shallower reorganization a chance to commit in the middle of this rollback.
		// When reorganizations are serialized it cannot start yet, and this wait simply times out.
		select {
		case err := <-forkBDone:
			forkBDone <- err
		case <-time.After(2 * time.Second):
		}
	}
	t.Cleanup(func() { resolveForkRollbackStepHook = nil })

	if err := bc.ResolveFork(1, forkC); err != nil {
		t.Fatalf("ResolveFork(fork C) failed: %v", err)
	}
	select {
	case <-forkBDone:
		// Fork B either committed first or was rejected after fork C won; both are acceptable.
	case <-time.After(30 * time.Second):
		t.Fatal("ResolveFork(fork B) did not finish")
	}

	if tip := bc.GetLatestBlock(); tip.Hash != forkC[len(forkC)-1].Hash {
		t.Fatalf("tip = #%d %s, want fork C tip #%d", tip.Index, tip.Hash, forkC[len(forkC)-1].Index)
	}

	bc.Lock.RLock()
	chain := append([]Block(nil), bc.Chain...)
	live := bc.State.Clone()
	bc.Lock.RUnlock()
	want := replayStateFromGenesis(t, bc, chain)

	for name, addr := range map[string]string{"minerP": minerP, "minerA": minerA, "minerB": minerB, "minerC": minerC} {
		if got, exp := live.GetBalance(addr), want.GetBalance(addr); got != exp {
			t.Errorf("%s balance after overlapping reorgs = %d, replay from genesis = %d", name, got, exp)
		}
	}
	if len(live.Accounts) != len(want.Accounts) {
		t.Errorf("account count after overlapping reorgs = %d, replay from genesis = %d", len(live.Accounts), len(want.Accounts))
	}
}
