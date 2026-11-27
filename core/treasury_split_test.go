//go:build testhooks

package core

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"github.com/armaxcoincore/armax-coin/core/pqsig"
)

// regtestTreasuryFixtureSeed returns the public seed of Regtest treasury fixture key i (1..3).
func regtestTreasuryFixtureSeed(i int) [32]byte {
	return sha256.Sum256([]byte(fmt.Sprintf("armax-regtest-treasury-fixture:%d", i)))
}

func regtestTreasuryFixtureKeys(t *testing.T) []*pqsig.PrivateKey {
	t.Helper()
	keys := make([]*pqsig.PrivateKey, 3)
	for i := range keys {
		k, err := pqsig.GenerateFromSeed(regtestTreasuryFixtureSeed(i + 1))
		if err != nil {
			t.Fatalf("GenerateFromSeed: %v", err)
		}
		keys[i] = k
	}
	return keys
}

func regtestTreasuryDescriptor(t *testing.T) *MultisigDescriptor {
	t.Helper()
	keys := regtestTreasuryFixtureKeys(t)
	pubs := make([][]byte, len(keys))
	for i, k := range keys {
		pubs[i] = k.PublicKeyBytes()
	}
	d, err := NewMultisigDescriptor(2, pubs)
	if err != nil {
		t.Fatalf("NewMultisigDescriptor: %v", err)
	}
	return d
}

// TestRegtestTreasuryAddress_IsFixtureMultisig pins RegtestTreasuryAddress to the 2-of-3 fixture.
func TestRegtestTreasuryAddress_IsFixtureMultisig(t *testing.T) {
	addr, err := regtestTreasuryDescriptor(t).AddressForNetwork(RegtestProfile.NetworkID)
	if err != nil {
		t.Fatalf("AddressForNetwork: %v", err)
	}
	if addr != RegtestTreasuryAddress {
		t.Fatalf("RegtestTreasuryAddress = %s, fixture multisig is %s", RegtestTreasuryAddress, addr)
	}
}

// TestTreasurySplit_ProfilesAndLaunchGate pins the per-network parameters and blocks a Mainnet
// launch without a published ax1 treasury address.
func TestTreasurySplit_ProfilesAndLaunchGate(t *testing.T) {
	if MainnetProfile.TreasurySplitEndHeight != 108000 {
		t.Fatalf("Mainnet split end = %d, want 108000", MainnetProfile.TreasurySplitEndHeight)
	}
	if TestnetProfile.TreasurySplitEndHeight > 0 || TestnetProfile.TreasuryAddress != "" ||
		SoakProfile.TreasurySplitEndHeight > 0 || SoakProfile.TreasuryAddress != "" {
		t.Fatalf("treasury split must stay disabled on Testnet and Soak")
	}
	if RegtestProfile.TreasurySplitEndHeight > 0 || RegtestProfile.TreasuryAddress != RegtestTreasuryAddress {
		t.Fatalf("Regtest split must default off with the fixture treasury address")
	}
	if MainnetLaunched && MainnetProfile.TreasuryAddress == "" {
		t.Fatalf("Mainnet cannot launch without a published treasury multisig address")
	}
	if a := MainnetProfile.TreasuryAddress; a != "" && !ValidateAddressForNetwork(a, MainnetProfile.NetworkID) {
		t.Fatalf("Mainnet treasury address %s is not an ax1 address", a)
	}
	if got := TreasuryShare(InitialReward); got != 12*CoinUnit {
		t.Fatalf("TreasuryShare(60 ARMX) = %d, want 12 ARMX", got)
	}
	if total := TreasuryShare(InitialReward) * MainnetProfile.TreasurySplitEndHeight; total != 1_296_000*CoinUnit {
		t.Fatalf("Mainnet treasury total = %d, want 1,296,000 ARMX", total)
	}
}

// TestTreasurySplit_Boundaries verifies the activation predicate at Genesis and the window edges,
// and that an empty treasury address keeps the split off.
func TestTreasurySplit_Boundaries(t *testing.T) {
	useRegtestForMultisig(t)
	t.Cleanup(ResetTreasuryOverrides)
	SetTreasurySplitEndHeight(5)
	for h, want := range map[int]bool{0: false, 1: true, 5: true, 6: false} {
		if got := IsTreasurySplitActive(h); got != want {
			t.Errorf("IsTreasurySplitActive(%d) = %v, want %v", h, got, want)
		}
	}
	SetTreasuryAddress("")
	if IsTreasurySplitActive(1) {
		t.Fatalf("split active without a treasury address")
	}
	ResetTreasuryOverrides()
	if IsTreasurySplitActive(1) {
		t.Fatalf("Regtest split active by default")
	}
	t.Setenv("ARMX_TREASURY_SPLIT_END_HEIGHT", "3")
	if !IsTreasurySplitActive(3) || IsTreasurySplitActive(4) {
		t.Fatalf("ARMX_TREASURY_SPLIT_END_HEIGHT not honoured on Regtest")
	}
}

// splitTestBlock builds an unmined block at height 1 with the given transactions.
func splitTestBlock(txs ...Transaction) *Block {
	return &Block{Index: 1, Transactions: txs}
}

// TestTreasurySplit_StateRules exercises tx[0]/tx[1] validation in VerifyAndApplyBlock.
func TestTreasurySplit_StateRules(t *testing.T) {
	useRegtestForMultisig(t)
	t.Cleanup(ResetTreasuryOverrides)
	SetTreasurySplitEndHeight(10)

	miner := newTestAccount(t, "miner").Address
	reward := InitialReward
	share := TreasuryShare(reward)
	cb := Transaction{Sender: "SYSTEM", Recipient: miner, Amount: reward - share, Nonce: 1, Signature: "REWARD_SIG"}
	tc := NewTreasuryCoinbase(1, reward)

	apply := func(b *Block) error {
		sm := NewStateManager()
		return sm.VerifyAndApplyBlock(b, reward, 1)
	}
	if err := apply(splitTestBlock(cb, tc)); err != nil {
		t.Fatalf("valid split block rejected: %v", err)
	}

	mutate := func(f func(*Transaction)) Transaction {
		c := tc
		f(&c)
		return c
	}
	bad := map[string]*Block{
		"missing tx[1]":       splitTestBlock(cb),
		"tx[0] over-claims":   splitTestBlock(Transaction{Sender: "SYSTEM", Recipient: miner, Amount: reward, Nonce: 1, Signature: "REWARD_SIG"}, tc),
		"under-paid treasury": splitTestBlock(cb, mutate(func(x *Transaction) { x.Amount-- })),
		"over-paid treasury":  splitTestBlock(cb, mutate(func(x *Transaction) { x.Amount++ })),
		"wrong recipient":     splitTestBlock(cb, mutate(func(x *Transaction) { x.Recipient = miner })),
		"wrong sentinel":      splitTestBlock(cb, mutate(func(x *Transaction) { x.Signature = "REWARD_SIG" })),
		"non-zero fee":        splitTestBlock(cb, mutate(func(x *Transaction) { x.Fee = 1 })),
		"sender_pub set":      splitTestBlock(cb, mutate(func(x *Transaction) { x.SenderPub = "00" })),
		"wrong nonce":         splitTestBlock(cb, mutate(func(x *Transaction) { x.Nonce = 2 })),
		"sig_alg 0x02":        splitTestBlock(cb, mutate(func(x *Transaction) { x.SigAlg = SigAlgMultisigMLDSA65 })),
		"HTLC type":           splitTestBlock(cb, mutate(func(x *Transaction) { x.Type = "HTLC_LOCK" })),
		"time_lock set":       splitTestBlock(cb, mutate(func(x *Transaction) { x.TimeLock = 5 })),
		"lowercase system":    splitTestBlock(cb, mutate(func(x *Transaction) { x.Sender = "system" })),
		"SYSTEM at index 2":   splitTestBlock(cb, tc, tc),
	}
	for name, b := range bad {
		if err := apply(b); err == nil {
			t.Errorf("%s: block accepted", name)
		}
	}

	// Outside the window a SYSTEM tx[1] is rejected exactly as before.
	SetTreasurySplitEndHeight(-1)
	full := Transaction{Sender: "SYSTEM", Recipient: miner, Amount: reward, Nonce: 1, Signature: "REWARD_SIG"}
	if err := apply(splitTestBlock(full, tc)); err == nil {
		t.Fatalf("treasury coinbase accepted while the split is inactive")
	}
	if err := apply(splitTestBlock(full)); err != nil {
		t.Fatalf("single-coinbase block rejected while the split is inactive: %v", err)
	}
}

// TestTreasurySplit_MinerIsTreasury covers the edge case where the miner pays itself as treasury.
func TestTreasurySplit_MinerIsTreasury(t *testing.T) {
	useRegtestForMultisig(t)
	t.Cleanup(ResetTreasuryOverrides)
	SetTreasurySplitEndHeight(10)
	reward := InitialReward
	cb := Transaction{Sender: "SYSTEM", Recipient: RegtestTreasuryAddress, Amount: reward - TreasuryShare(reward), Nonce: 1, Signature: "REWARD_SIG"}
	b := splitTestBlock(cb, NewTreasuryCoinbase(1, reward))
	if cb.HashHex() == b.Transactions[1].HashHex() {
		t.Fatalf("tx[0] and tx[1] share a hash")
	}
	sm := NewStateManager()
	if err := sm.VerifyAndApplyBlock(b, reward, 1); err != nil {
		t.Fatalf("miner == treasury rejected: %v", err)
	}
	if got := sm.GetBalance(RegtestTreasuryAddress); got != reward {
		t.Fatalf("treasury+miner balance = %d, want %d", got, reward)
	}
}

// TestTreasurySplit_ReorgAcrossWindowEnd reorganizes from a 2-block chain to a 5-block fork that
// crosses the window end (#3 -> #4) and checks balances and supply parity afterwards.
func TestTreasurySplit_ReorgAcrossWindowEnd(t *testing.T) {
	t.Setenv("ARMX_DATA_DIR", t.TempDir())
	useRegtestForMultisig(t)
	t.Cleanup(ResetTreasuryOverrides)
	SetTreasurySplitEndHeight(3)

	bc := NewBlockchain(1)
	defer bc.Close()
	minerA := newTestAccount(t, "minerA").Address
	minerB := newTestAccount(t, "minerB").Address
	for range 2 {
		if _, err := bc.MineBlockContext(context.Background(), minerA); err != nil {
			t.Fatalf("mine chain A: %v", err)
		}
	}

	target := bc.CalculateLWMA1()
	prev := bc.Chain[0]
	var chainB []Block
	for i := 1; i <= 5; i++ {
		share := int64(0)
		if IsTreasurySplitActive(i) {
			share = TreasuryShare(InitialReward)
		}
		txs := []Transaction{{Sender: "SYSTEM", Recipient: minerB, Amount: InitialReward - share, Nonce: int64(i), Signature: "REWARD_SIG"}}
		if share > 0 {
			txs = append(txs, NewTreasuryCoinbase(i, InitialReward))
		}
		b := *NewBlock(txs, prev.Hash, i, minerB, target)
		b.Timestamp = prev.Timestamp + int64(TargetBlockTimeSeconds)
		for {
			b.Hash = b.CalculateHash()
			if HashMeetsTarget(b.Hash, target) {
				break
			}
			b.Nonce++
		}
		chainB = append(chainB, b)
		prev = b
	}
	if err := bc.ResolveFork(0, chainB); err != nil {
		t.Fatalf("ResolveFork: %v", err)
	}
	share := TreasuryShare(InitialReward)
	if got := bc.GetBalance(RegtestTreasuryAddress); got != 3*share {
		t.Fatalf("treasury balance after reorg = %d, want %d", got, 3*share)
	}
	if got := bc.GetBalance(minerA); got != 0 {
		t.Fatalf("orphaned miner keeps %d", got)
	}
	incremental := bc.GetCirculatingSupply()
	if want := 6 * InitialReward; incremental != want || CalculateCirculatingSupply(bc.Chain) != want {
		t.Fatalf("supply after reorg = %d (calc %d), want %d", incremental, CalculateCirculatingSupply(bc.Chain), want)
	}
	if err := bc.RebuildState(); err != nil || bc.GetCirculatingSupply() != incremental {
		t.Fatalf("rebuild parity after reorg: err=%v supply=%d want %d", err, bc.GetCirculatingSupply(), incremental)
	}
}

// TestTreasurySplit_ChainEndToEnd mines across the window end on Regtest, checks balances and
// supply parity between incremental tracking and a full rebuild, then spends matured treasury
// funds with the 2-of-3 fixture multisig.
func TestTreasurySplit_ChainEndToEnd(t *testing.T) {
	t.Setenv("ARMX_DATA_DIR", t.TempDir())
	useRegtestForMultisig(t)
	t.Cleanup(ResetTreasuryOverrides)
	SetTreasurySplitEndHeight(3)

	bc := NewBlockchain(1)
	defer bc.Close()
	miner := newTestAccount(t, "miner").Address
	for h := 1; h <= 4; h++ {
		b, err := bc.MineBlockContext(context.Background(), miner)
		if err != nil {
			t.Fatalf("mine block %d: %v", h, err)
		}
		hasTreasury := len(b.Transactions) >= 2 && b.Transactions[1].Signature == TreasuryRewardSignature
		if hasTreasury != (h <= 3) {
			t.Fatalf("block %d treasury coinbase present = %v", h, hasTreasury)
		}
	}
	share := TreasuryShare(InitialReward)
	if got := bc.GetBalance(RegtestTreasuryAddress); got != 3*share {
		t.Fatalf("treasury balance = %d, want %d", got, 3*share)
	}
	if got := bc.GetBalance(miner); got != 3*(InitialReward-share)+InitialReward {
		t.Fatalf("miner balance = %d", got)
	}
	incremental := bc.GetCirculatingSupply()
	if want := InitialReward + 4*InitialReward; incremental != want {
		t.Fatalf("incremental supply = %d, want %d (genesis + 4 full subsidies)", incremental, want)
	}
	if err := bc.RebuildState(); err != nil {
		t.Fatalf("RebuildState: %v", err)
	}
	if rebuilt := bc.GetCirculatingSupply(); rebuilt != incremental {
		t.Fatalf("supply parity: rebuilt %d != incremental %d", rebuilt, incremental)
	}
	if got := CalculateCirculatingSupply(bc.Chain); got != incremental {
		t.Fatalf("CalculateCirculatingSupply = %d, want %d", got, incremental)
	}
	if !bc.IsChainValid() {
		t.Fatalf("chain with treasury coinbases fails full revalidation")
	}

	// Spend matured treasury funds with two of the three fixture keys.
	keys := regtestTreasuryFixtureKeys(t)
	recipient := newTestAccount(t, "grant").Address
	tx := Transaction{Recipient: recipient, Amount: share, Fee: CoinUnit / 10, Nonce: 1, Type: TxTypeStandard}
	signMultisigSpend(t, &tx, regtestTreasuryDescriptor(t), keys[1:])
	if ok, reason := bc.AddTransaction(tx); !ok {
		t.Fatalf("treasury multisig spend rejected: %s", reason)
	}
	if _, err := bc.MineBlockContext(context.Background(), miner); err != nil {
		t.Fatalf("mine spend block: %v", err)
	}
	if got := bc.GetBalance(recipient); got != share {
		t.Fatalf("grant recipient balance = %d, want %d", got, share)
	}
	if !strings.HasPrefix(RegtestTreasuryAddress, RegtestAddressHRP+"1") {
		t.Fatalf("fixture treasury address lacks the regtest prefix")
	}
}
