//go:build testhooks

package core

import (
	"encoding/hex"
	"math"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCryptoConsensus_CoinbaseRejectsExtendedAndSigAlg(t *testing.T) {
	LockNetworkForTesting(t)
	SetNetworkForTesting(true)
	SetAllowMainnetForTesting(true)
	defer func() {
		_ = SetNetwork("testnet")
	}()

	// The recipient is set per network below (Testnet "armx1", Mainnet "ax1") before each section runs.
	baseTx := Transaction{
		Sender:    "SYSTEM",
		Recipient: TestnetGenesisMiner,
		Amount:    InitialReward,
		Nonce:     1,
		Signature: "REWARD_SIG",
	}

	cases := []struct {
		name   string
		mutate func(*Transaction)
	}{
		{
			name:   "invalid SigAlg",
			mutate: func(tx *Transaction) { tx.SigAlg = 0x02 },
		},
		{
			name:   "non-empty HashLock",
			mutate: func(tx *Transaction) { tx.HashLock = "abcd" },
		},
		{
			name:   "non-zero TimeLock",
			mutate: func(tx *Transaction) { tx.TimeLock = 12345 },
		},
		{
			name:   "non-empty Preimage",
			mutate: func(tx *Transaction) { tx.Preimage = "secret" },
		},
	}

	// Option 1A: Testnet preserves v1.0.12 coinbase block compatibility.
	if err := SetNetwork("testnet"); err != nil {
		t.Fatalf("SetNetwork(testnet) failed: %v", err)
	}
	for _, tc := range cases {
		t.Run("testnet_compat_"+tc.name, func(t *testing.T) {
			sm := NewStateManager()
			cb := baseTx
			tc.mutate(&cb)
			b := &Block{Index: 1, Transactions: []Transaction{cb}}
			if err := sm.VerifyAndApplyBlock(b, InitialReward, CoinbaseMaturity); err != nil {
				t.Fatalf("expected Testnet v1.0.12 coinbase compatibility for %s, got %v", tc.name, err)
			}
		})
	}

	// Option 1A: Mainnet strictly rejects extended/invalid-SigAlg coinbase tx[0].
	if err := SetNetwork("mainnet"); err != nil {
		t.Fatalf("SetNetwork(mainnet) failed: %v", err)
	}
	baseTx.Recipient = MainnetGenesisMiner
	for _, tc := range cases {
		t.Run("mainnet_strict_"+tc.name, func(t *testing.T) {
			sm := NewStateManager()
			cb := baseTx
			tc.mutate(&cb)
			b := &Block{Index: 1, Transactions: []Transaction{cb}}
			if err := sm.VerifyAndApplyBlock(b, InitialReward, CoinbaseMaturity); err == nil {
				t.Fatalf("expected Mainnet coinbase rejection for %s, got nil", tc.name)
			}
		})
	}

	// Verify valid canonical coinbase still succeeds cleanly on Mainnet.
	sm := NewStateManager()
	if err := sm.VerifyAndApplyBlock(&Block{Index: 1, Transactions: []Transaction{baseTx}}, InitialReward, CoinbaseMaturity); err != nil {
		t.Fatalf("valid coinbase rejected unexpectedly on Mainnet: %v", err)
	}
}

func TestCryptoConsensus_ASERTExtremeDriftSafety(t *testing.T) {
	anchorHex := "0000000000000000000000000000000000000000000000000000ffff00000000"
	anchorTarget, ok := new(big.Int).SetString(anchorHex, 16)
	if !ok {
		t.Fatal("failed to parse anchor hex")
	}

	highMax := ComputeASERTFromDrift(anchorTarget, math.MaxInt64)
	highClamped := ComputeASERTFromDrift(anchorTarget, 1_000_000_000)
	if highMax == nil || highMax.Sign() <= 0 || highMax.Cmp(highClamped) != 0 {
		t.Fatalf("expected clamped positive upper ASERT target, got max=%v clamped=%v", highMax, highClamped)
	}

	lowMin := ComputeASERTFromDrift(anchorTarget, math.MinInt64)
	lowClamped := ComputeASERTFromDrift(anchorTarget, -1_000_000_000)
	if lowMin == nil || lowMin.Cmp(big.NewInt(1)) != 0 || lowClamped.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("expected lower extreme ASERT drift to clamp to 1, got min=%v clamped=%v", lowMin, lowClamped)
	}
}

func TestCryptoConsensus_NegativeRewardHeightSafety(t *testing.T) {
	if r := CalculateBlockRewardGivenSupply(-1, 0); r != 0 {
		t.Fatalf("expected 0 reward for blockIndex=-1, got %d", r)
	}
	if r := CalculateBlockRewardGivenSupply(math.MinInt32, -500); r != 0 {
		t.Fatalf("expected 0 reward for negative blockIndex, got %d", r)
	}
	if r := CalculateBlockRewardGivenSupply(0, -500); r != InitialReward {
		t.Fatalf("expected InitialReward (%d) when supply clamped from negative, got %d", InitialReward, r)
	}
	if total := CalculateCirculatingSupply([]Block{{Index: -1}, {Index: 0}}); total != InitialReward {
		t.Fatalf("expected circulating supply %d while skipping negative index, got %d", InitialReward, total)
	}
}

func TestCryptoConsensus_CheckpointHookAndRegtestEnvBounds(t *testing.T) {
	LockNetworkForTesting(t)

	if err := SetNetwork("regtest"); err != nil {
		t.Fatalf("SetNetwork(regtest) failed: %v", err)
	}

	cleanup := SetCheckpointsForTesting(map[int]string{
		0: "deadbeef",
		1: "cafebabe",
	})
	defer cleanup()

	if ckpts := GetCheckpointsForNetwork("ARMX-MAINNET-1"); ckpts[0] != GenesisHash || ckpts[1] != "" {
		t.Fatalf("expected Mainnet checkpoints to ignore test override hook, got %v", ckpts)
	}
	if ckpts := GetCheckpointsForNetwork("MAINNET"); ckpts[0] != GenesisHash || ckpts[1] != "" {
		t.Fatalf("expected MAINNET checkpoints to ignore test override hook, got %v", ckpts)
	}
	if ckpts := GetCheckpointsForNetwork("ARMX-REGTEST-1"); ckpts[1] != "cafebabe" {
		t.Fatalf("expected Regtest checkpoints to honor test override hook, got %v", ckpts)
	}

	// Out-of-bounds regtest env heights must fall back to network defaults (-1 on regtest).
	t.Setenv("ARMX_ARMAXHASH_HEIGHT", "999999999999")
	if h := GetArmaxHashActivationHeight(); h != RegtestProfile.ArmaxHashActivationHeight {
		t.Fatalf("expected fallback ArmaxHash height %d on overflow env, got %d", RegtestProfile.ArmaxHashActivationHeight, h)
	}
	t.Setenv("ARMX_ANTI_STALL_HEIGHT", "-2")
	if h := GetAntiStallActivationHeight(); h != RegtestProfile.AntiStallActivationHeight {
		t.Fatalf("expected fallback AntiStall height %d on < -1 env, got %d", RegtestProfile.AntiStallActivationHeight, h)
	}
	t.Setenv("ARMX_FUTURE_LIMIT_HEIGHT", "999999999999")
	if h := GetFutureTimestampLimitActivationHeight(); h != RegtestProfile.FutureTimestampLimitActivationHeight {
		t.Fatalf("expected fallback FutureLimit height %d on overflow env, got %d", RegtestProfile.FutureTimestampLimitActivationHeight, h)
	}
	t.Setenv("ARMX_BINARY_SIZE_HEIGHT", "-5")
	if h := GetBlockBinarySizeActivationHeight(); h != RegtestProfile.BlockBinarySizeActivationHeight {
		t.Fatalf("expected fallback BinarySize height %d on < -1 env, got %d", RegtestProfile.BlockBinarySizeActivationHeight, h)
	}

	// Valid bounded values are still accepted in regtest.
	t.Setenv("ARMX_ARMAXHASH_HEIGHT", "10")
	t.Setenv("ARMX_ANTI_STALL_HEIGHT", "11")
	t.Setenv("ARMX_FUTURE_LIMIT_HEIGHT", "12")
	t.Setenv("ARMX_BINARY_SIZE_HEIGHT", "13")
	if GetArmaxHashActivationHeight() != 10 || GetAntiStallActivationHeight() != 11 ||
		GetFutureTimestampLimitActivationHeight() != 12 || GetBlockBinarySizeActivationHeight() != 13 {
		t.Fatal("expected valid bounded regtest env overrides to be honored")
	}
}

func uppercaseFirstHexLetter(t *testing.T, s string) string {
	t.Helper()
	for i := 0; i < len(s); i++ {
		if s[i] >= 'a' && s[i] <= 'f' {
			return s[:i] + strings.ToUpper(s[i:i+1]) + s[i+1:]
		}
	}
	t.Fatalf("no lowercase hex letter found in string of length %d", len(s))
	return s
}

func TestCryptoConsensus_RejectsUppercaseSignatureAndPubKeyMalleability(t *testing.T) {
	LockNetworkForTesting(t)
	SetNetworkForTesting(true)
	SetAllowMainnetForTesting(true)
	defer func() {
		_ = SetNetwork("testnet")
	}()
	if err := SetNetwork("regtest"); err != nil {
		t.Fatalf("SetNetwork(regtest) failed: %v", err)
	}

	t.Setenv("ARMX_DATA_DIR", t.TempDir())
	bc := NewBlockchain(1)
	defer bc.Close()

	acc := newTestAccount(t)
	bc.State.Lock.Lock()
	bc.State.Accounts[acc.AddressHex] = AccountState{Balance: 100 * CoinUnit, Nonce: 0}
	bc.State.Lock.Unlock()

	tx := Transaction{
		Sender:    acc.Address,
		SenderPub: acc.PubHex,
		Recipient: GenesisMinerForNetwork(""),
		Amount:    1_000_000,
		Fee:       12_000,
		Nonce:     1,
	}
	if err := tx.Sign(acc.PrivKey); err != nil {
		t.Fatalf("tx.Sign failed: %v", err)
	}
	if err := tx.VerifyWithError(); err != nil {
		t.Fatalf("valid signed tx rejected: %v", err)
	}
	if err := tx.ValidateCanonicalHex(); err != nil {
		t.Fatalf("valid signed tx rejected by ValidateCanonicalHex: %v", err)
	}
	rawBin, err := tx.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary failed on canonical tx: %v", err)
	}
	var decoded Transaction
	if err := decoded.UnmarshalBinary(rawBin); err != nil {
		t.Fatalf("UnmarshalBinary failed: %v", err)
	}
	if decoded.HashHex() != tx.HashHex() || decoded.WitnessHashHex() != tx.WitnessHashHex() {
		t.Fatalf("canonical binary round-trip altered tx/witness hash")
	}

	txSigUpper := tx
	txSigUpper.InvalidateCache()
	txSigUpper.Signature = uppercaseFirstHexLetter(t, tx.Signature)
	if err := txSigUpper.ValidateCanonicalHex(); err == nil {
		t.Fatal("expected ValidateCanonicalHex to reject uppercase hex signature")
	}
	// Non-mainnet preserves v1.0.12 mempool relay and block validation compatibility for uppercase hex.
	if ok, reason := bc.AddTransaction(txSigUpper); !ok {
		t.Fatalf("expected non-mainnet AddTransaction to relay v1.0.12 uppercase hex signature, got: %s", reason)
	}
	if err := txSigUpper.VerifyWithError(); err != nil {
		t.Fatalf("expected non-mainnet VerifyWithError to remain v1.0.12 compatible, got: %v", err)
	}
	if _, err := txSigUpper.MarshalBinary(); err != nil {
		t.Fatalf("expected non-mainnet MarshalBinary to remain v1.0.12 compatible, got: %v", err)
	}

	txPubUpper := tx
	txPubUpper.InvalidateCache()
	txPubUpper.SenderPub = uppercaseFirstHexLetter(t, tx.SenderPub)
	txPubUpper.Nonce = 2
	// Re-sign over the uppercase SenderPub payload (as a v1.0.12 client with uppercase pubkey would)
	// so signature verification isolates the hex-casing check.
	upperSig, err := acc.PrivKey.Sign(txPubUpper.Hash())
	if err != nil {
		t.Fatalf("Sign uppercase SenderPub tx failed: %v", err)
	}
	txPubUpper.Signature = hex.EncodeToString(upperSig)
	if err := txPubUpper.ValidateCanonicalHex(); err == nil {
		t.Fatal("expected ValidateCanonicalHex to reject uppercase hex sender_pub")
	}
	if ok, reason := bc.AddTransaction(txPubUpper); !ok {
		t.Fatalf("expected non-mainnet AddTransaction to relay v1.0.12 uppercase hex sender_pub, got: %s", reason)
	}
	if err := txPubUpper.VerifyWithError(); err != nil {
		t.Fatalf("expected non-mainnet VerifyWithError to remain v1.0.12 compatible for sender_pub, got: %v", err)
	}
	if _, err := txPubUpper.MarshalBinary(); err != nil {
		t.Fatalf("expected non-mainnet MarshalBinary to remain v1.0.12 compatible for sender_pub, got: %v", err)
	}

	// Mainnet strictly rejects uppercase hex in mempool, VerifyWithError, and MarshalBinary.
	if err := SetNetwork("mainnet"); err != nil {
		t.Fatalf("SetNetwork(mainnet) failed: %v", err)
	}
	bcMain, err := NewBlockchainWithStoragePath(1, filepath.Join(t.TempDir(), "mainnet_hex.db"))
	if err != nil {
		t.Fatalf("NewBlockchainWithStoragePath(mainnet) failed: %v", err)
	}
	defer bcMain.Close()
	bcMain.State.Lock.Lock()
	bcMain.State.Accounts[acc.AddressHex] = AccountState{Balance: 100 * CoinUnit, Nonce: 0}
	bcMain.State.Lock.Unlock()
	if ok, _ := bcMain.AddTransaction(txSigUpper); ok {
		t.Fatal("expected Mainnet AddTransaction to reject uppercase hex signature")
	}
	if ok, _ := bcMain.AddTransaction(txPubUpper); ok {
		t.Fatal("expected Mainnet AddTransaction to reject uppercase hex sender_pub")
	}
	if err := txSigUpper.VerifyWithError(); err == nil {
		t.Fatal("expected Mainnet VerifyWithError to reject uppercase hex signature")
	}
	if _, err := txSigUpper.MarshalBinary(); err == nil {
		t.Fatal("expected Mainnet MarshalBinary to reject uppercase hex signature")
	}
	if err := txPubUpper.VerifyWithError(); err == nil {
		t.Fatal("expected Mainnet VerifyWithError to reject uppercase hex sender_pub")
	}
	if _, err := txPubUpper.MarshalBinary(); err == nil {
		t.Fatal("expected Mainnet MarshalBinary to reject uppercase hex sender_pub")
	}
}

func TestCryptoConsensus_GenesisAndPreCheckRejectUnhashedScriptFields(t *testing.T) {
	LockNetworkForTesting(t)
	if err := SetNetwork("regtest"); err != nil {
		t.Fatalf("SetNetwork(regtest) failed: %v", err)
	}

	// 1. ValidateGenesisBlock must preserve v1.0.12 unhashed TimeLock/Preimage compatibility on non-Mainnet
	// while still rejecting SigAlg/HashLock mutations, and strictly reject all 4 on Mainnet.
	for _, tc := range []struct {
		name           string
		nonMainnetPass bool
		mutate         func(*Block)
	}{
		{"sig_alg", false, func(b *Block) { b.Transactions[0].SigAlg = 0x02 }},
		{"hash_lock", false, func(b *Block) { b.Transactions[0].HashLock = "ab" }},
		{"time_lock", true, func(b *Block) { b.Transactions[0].TimeLock = 42 }},
		{"preimage", true, func(b *Block) { b.Transactions[0].Preimage = "abc" }},
	} {
		t.Run("genesis_"+tc.name, func(t *testing.T) {
			gen := GetRegtestGenesisBlock()
			tc.mutate(&gen)
			err := ValidateGenesisBlock(&gen)
			if tc.nonMainnetPass && err != nil {
				t.Fatalf("expected non-mainnet ValidateGenesisBlock to allow v1.0.12 %s metadata, got %v", tc.name, err)
			}
			if !tc.nonMainnetPass && err == nil {
				t.Fatalf("expected non-mainnet ValidateGenesisBlock to reject mutated genesis %s", tc.name)
			}

			mainGen := GetGenesisBlock()
			tc.mutate(&mainGen)
			if err := ValidateGenesisBlockForNetwork(&mainGen, MainnetProfile.NetworkID); err == nil {
				t.Fatalf("expected Mainnet ValidateGenesisBlockForNetwork to reject mutated genesis %s", tc.name)
			}
		})
	}

	// 2. PreCheckBlock and validateBlockContext must reject regular txs with unhashed/extended fields.
	dbPath := filepath.Join(t.TempDir(), "precheck.db")
	bc, err := NewBlockchainWithStoragePath(1, dbPath)
	if err != nil {
		t.Fatalf("NewBlockchainWithStoragePath failed: %v", err)
	}
	defer bc.Close()

	acc := newTestAccount(t)
	if _, err := bc.MineBlockWithErr(acc.Address); err != nil {
		t.Fatalf("MineBlockWithErr #1 failed: %v", err)
	}

	signedTx := Transaction{
		Sender:    acc.Address,
		SenderPub: acc.PubHex,
		Recipient: GenesisMinerForNetwork(""),
		Amount:    100_000_000,
		Fee:       12_000,
		Nonce:     1,
	}
	if err := signedTx.Sign(acc.PrivKey); err != nil {
		t.Fatalf("Sign failed: %v", err)
	}

	prev := bc.GetLatestBlock()
	target := bc.CalculateLWMA1()
	cb := Transaction{
		Sender:    "SYSTEM",
		Recipient: acc.Address,
		Amount:    bc.GetBlockReward(prev.Index+1) + signedTx.Fee,
		Nonce:     int64(prev.Index + 1),
		Signature: "REWARD_SIG",
	}
	validBlk := *NewBlock([]Transaction{cb, signedTx}, prev.Hash, prev.Index+1, acc.Address, target)
	validBlk.Timestamp = time.Now().Unix()
	if validBlk.Timestamp <= prev.Timestamp {
		validBlk.Timestamp = prev.Timestamp + 1
	}
	for {
		validBlk.Hash = validBlk.CalculateHash()
		if HashMeetsTarget(validBlk.Hash, target) {
			break
		}
		validBlk.Nonce++
	}

	if err := bc.PreCheckBlock(&validBlk); err != nil {
		t.Fatalf("valid block failed PreCheckBlock: %v", err)
	}
	if err := validateBlockContext(&validBlk, &prev, bc.Chain); err != nil {
		t.Fatalf("valid block failed validateBlockContext: %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*Transaction)
	}{
		{"time_lock", func(tx *Transaction) { tx.TimeLock = 77 }},
		{"preimage", func(tx *Transaction) { tx.Preimage = "secret" }},
		{"type", func(tx *Transaction) { tx.Type = "HTLC_LOCK" }},
		{"hash_lock", func(tx *Transaction) { tx.HashLock = strings.Repeat("aa", 32) }},
	} {
		t.Run("block_tx_"+tc.name, func(t *testing.T) {
			badBlk := validBlk
			badBlk.Transactions = append([]Transaction(nil), validBlk.Transactions...)
			tc.mutate(&badBlk.Transactions[1])
			if err := bc.PreCheckBlock(&badBlk); err == nil {
				t.Fatalf("expected PreCheckBlock to reject tx with %s", tc.name)
			}
			if err := validateBlockContext(&badBlk, &prev, bc.Chain); err == nil {
				t.Fatalf("expected validateBlockContext to reject tx with %s", tc.name)
			}
		})
	}
}

func TestCryptoConsensus_CirculatingSupplyUnderclaimAndIsChainValidCrossProfile(t *testing.T) {
	LockNetworkForTesting(t)
	if err := SetNetwork("regtest"); err != nil {
		t.Fatalf("SetNetwork(regtest) failed: %v", err)
	}

	// 1. CalculateCirculatingSupply must reflect actual under-claimed coinbase subsidies.
	underclaimChain := []Block{
		GetTestnetGenesisBlock(),
		{
			Index: 1,
			Transactions: []Transaction{
				{Sender: "SYSTEM", Amount: 25 * CoinUnit},
			},
		},
	}
	wantSupply := InitialReward + 25*CoinUnit
	if got := CalculateCirculatingSupply(underclaimChain); got != wantSupply {
		t.Fatalf("CalculateCirculatingSupply got %d, want actual minted %d", got, wantSupply)
	}
	if gotReward := GetBlockRewardForChain(underclaimChain, 2); gotReward != InitialReward {
		t.Fatalf("GetBlockRewardForChain got %d, want %d", gotReward, InitialReward)
	}
	if gotStub := CalculateCirculatingSupply([]Block{{Index: 0}, {Index: 1}}); gotStub != InitialReward*2 {
		t.Fatalf("header-only stub CalculateCirculatingSupply got %d, want %d", gotStub, InitialReward*2)
	}

	// 2. IsChainValid must use the chain's own network profile and enforce header invariants.
	dbPath := filepath.Join(t.TempDir(), "cross.db")
	bc, err := NewBlockchainWithStoragePath(1, dbPath)
	if err != nil {
		t.Fatalf("NewBlockchainWithStoragePath failed: %v", err)
	}
	defer bc.Close()

	if _, err := bc.MineBlockWithErr(GenesisMinerForNetwork("")); err != nil {
		t.Fatalf("MineBlockWithErr failed: %v", err)
	}
	if !bc.IsChainValid() {
		t.Fatal("expected newly mined regtest chain to pass IsChainValid")
	}

	// Switch global ActiveNetwork away from regtest; IsChainValid must still validate the regtest chain.
	if err := SetNetwork("testnet"); err != nil {
		t.Fatalf("SetNetwork(testnet) failed: %v", err)
	}
	if !bc.IsChainValid() {
		t.Fatal("expected regtest IsChainValid to pass even while global ActiveNetwork is testnet")
	}

	origMiner := bc.Chain[1].Miner
	bc.Chain[1].Miner = ""
	if bc.IsChainValid() {
		t.Fatal("expected IsChainValid to reject block with empty Miner")
	}
	bc.Chain[1].Miner = origMiner

	origTarget := bc.Chain[1].Target
	bc.Chain[1].Target = ""
	if bc.IsChainValid() {
		t.Fatal("expected IsChainValid to reject block with empty Target")
	}
	bc.Chain[1].Target = origTarget

	origDiff := bc.Chain[1].Difficulty
	bc.Chain[1].Difficulty = 999
	if bc.IsChainValid() {
		t.Fatal("expected IsChainValid to reject block with mismatched Difficulty")
	}
	bc.Chain[1].Difficulty = origDiff
}
