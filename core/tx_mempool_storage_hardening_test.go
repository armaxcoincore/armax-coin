package core

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math/big"
	"path/filepath"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func setupEasyTargetForMempoolTest(t *testing.T) {
	t.Helper()
	easyTarget := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	oldHook := targetOverrideHook
	targetOverrideHook = func() *big.Int {
		return easyTarget
	}
	t.Cleanup(func() {
		targetOverrideHook = oldHook
	})
}

func mineSignedTestBlock(t *testing.T, prev Block, height int, miner string, reward int64, txs []Transaction, target *big.Int) Block {
	t.Helper()
	var totalFees int64
	for _, tx := range txs {
		totalFees += tx.Fee
	}
	cb := Transaction{
		Sender:    "SYSTEM",
		Recipient: miner,
		Amount:    reward + totalFees,
		Fee:       0,
		Nonce:     int64(height),
		Signature: "REWARD_SIG",
	}
	allTxs := make([]Transaction, 0, len(txs)+1)
	allTxs = append(allTxs, cb)
	allTxs = append(allTxs, txs...)

	b := *NewBlock(allTxs, prev.Hash, height, miner, target)
	b.Timestamp = prev.Timestamp + TargetBlockTimeSeconds
	for {
		b.Hash = b.CalculateHash()
		if HashMeetsTarget(b.Hash, target) {
			break
		}
		b.Nonce++
	}
	return b
}

// TestTxMempoolStorage_ReorgPreservesHigherNonceMempoolChain verifies:
// When a reorg disconnects a block containing sender nonce N while nonce N+1 is already
// sitting in bc.Mempool, orphaned nonce N must be re-queued BEFORE nonce N+1 so
// CleanMempoolLocked() preserves both transactions instead of dropping nonce N+1.
func TestTxMempoolStorage_ReorgPreservesHigherNonceMempoolChain(t *testing.T) {
	unlock := AcquireNetworkTestLock()
	defer unlock()
	setupEasyTargetForMempoolTest(t)

	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "reorg_mempool_order.db")
	bc, err := NewBlockchainWithStoragePath(1, dbPath)
	if err != nil {
		t.Fatalf("NewBlockchainWithStoragePath failed: %v", err)
	}
	bc.CoinbaseMaturity = 1
	bc.State.CoinbaseMaturity = 1
	defer bc.Close()

	accSender := newTestAccount(t, "sender")
	accRecip := newTestAccount(t, "recip")

	// Block #1 funds sender
	if _, err := bc.MineBlockContext(context.Background(), accSender.Address); err != nil {
		t.Fatalf("MineBlock 1 failed: %v", err)
	}

	tx1 := Transaction{
		Sender:    accSender.Address,
		Recipient: accRecip.Address,
		Amount:    5 * CoinUnit,
		Fee:       CalculateMinMempoolFee(12_000),
		Nonce:     1,
		Type:      TxTypeStandard,
	}
	if err := tx1.Sign(accSender.PrivKey); err != nil {
		t.Fatalf("sign tx1 failed: %v", err)
	}
	tx2 := Transaction{
		Sender:    accSender.Address,
		Recipient: accRecip.Address,
		Amount:    7 * CoinUnit,
		Fee:       CalculateMinMempoolFee(12_000),
		Nonce:     2,
		Type:      TxTypeStandard,
	}
	if err := tx2.Sign(accSender.PrivKey); err != nil {
		t.Fatalf("sign tx2 failed: %v", err)
	}

	if ok, msg := bc.AddTransaction(tx1); !ok {
		t.Fatalf("AddTransaction(tx1) failed: %s", msg)
	}
	// Mine Block #2 including tx1
	if _, err := bc.MineBlockContext(context.Background(), GenesisMiner); err != nil {
		t.Fatalf("MineBlock 2 failed: %v", err)
	}

	// Admit tx2 (Nonce 2) while tx1 is confirmed on local Block #2
	if ok, msg := bc.AddTransaction(tx2); !ok {
		t.Fatalf("AddTransaction(tx2) failed: %s", msg)
	}

	// Competing heavier fork from ancestor #1 without tx1/tx2
	target := bc.CalculateLWMA1()
	b2Fork := mineSignedTestBlock(t, bc.Chain[1], 2, GenesisMiner, InitialReward, nil, target)
	b3Fork := mineSignedTestBlock(t, b2Fork, 3, GenesisMiner, InitialReward, nil, target)

	if err := bc.ResolveFork(1, []Block{b2Fork, b3Fork}); err != nil {
		t.Fatalf("ResolveFork failed: %v", err)
	}

	bc.Lock.RLock()
	memLen := len(bc.Mempool)
	var gotHashes []string
	var gotNonces []int64
	for _, mTx := range bc.Mempool {
		gotHashes = append(gotHashes, mTx.HashHex())
		gotNonces = append(gotNonces, mTx.Nonce)
	}
	selected, _ := bc.SelectCandidateTransactionsLocked(10, MaxBlockSize)
	bc.Lock.RUnlock()

	if memLen != 2 {
		t.Fatalf("Expected both orphaned tx1 (nonce 1) and pending tx2 (nonce 2) in mempool, got len=%d nonces=%v", memLen, gotNonces)
	}
	if gotHashes[0] != tx1.HashHex() || gotHashes[1] != tx2.HashHex() {
		t.Fatalf("Mempool order mismatch: got nonces %v, want [1, 2]", gotNonces)
	}
	if len(selected) != 2 {
		t.Fatalf("Expected SelectCandidateTransactionsLocked to select both tx1 and tx2, got %d", len(selected))
	}
}

// TestTxMempoolStorage_MempoolEvictionNeverBreaksSameSenderNonceChain verifies:
// Submitting sender A nonce 2 with a higher fee when mempool is full must never evict
// sender A's own prerequisite nonce 1; it may only evict another sender's lower-fee tail tx.
func TestTxMempoolStorage_MempoolEvictionNeverBreaksSameSenderNonceChain(t *testing.T) {
	unlock := AcquireNetworkTestLock()
	defer unlock()

	tempDir := t.TempDir()
	bc, err := NewBlockchainWithStoragePath(1, filepath.Join(tempDir, "mempool_evict_self.db"))
	if err != nil {
		t.Fatalf("NewBlockchainWithStoragePath failed: %v", err)
	}
	defer bc.Close()

	accA := newTestAccount(t, "senderA")
	accB := newTestAccount(t, "senderB")
	accC := newTestAccount(t, "senderC")
	recip := newTestAccount(t, "recip")

	bc.State.Lock.Lock()
	bc.State.Accounts[accA.AddressHex] = AccountState{Balance: 10_000_000 * CoinUnit, Nonce: 0}
	bc.State.Accounts[accB.AddressHex] = AccountState{Balance: 10_000_000 * CoinUnit, Nonce: 0}
	bc.State.Accounts[accC.AddressHex] = AccountState{Balance: 10_000_000 * CoinUnit, Nonce: 0}
	bc.State.Lock.Unlock()

	txA1 := Transaction{
		Sender:    accA.Address,
		Recipient: recip.Address,
		Amount:    CoinUnit,
		Fee:       MinRelayFee * 12,
		Nonce:     1,
		Type:      TxTypeStandard,
	}
	if err := txA1.Sign(accA.PrivKey); err != nil {
		t.Fatalf("sign txA1 failed: %v", err)
	}

	txA2 := Transaction{
		Sender:    accA.Address,
		Recipient: recip.Address,
		Amount:    CoinUnit,
		Fee:       MinRelayFee * 15,
		Nonce:     2,
		Type:      TxTypeStandard,
	}
	if err := txA2.Sign(accA.PrivKey); err != nil {
		t.Fatalf("sign txA2 failed: %v", err)
	}

	// Fill mempool to MaxMempoolSize: txA1 (fee 12k) + 4999 accB txs (fee 20k)
	bc.Lock.Lock()
	bc.Mempool = make([]Transaction, 0, MaxMempoolSize)
	bc.Mempool = append(bc.Mempool, txA1)
	for i := 1; i < MaxMempoolSize; i++ {
		bc.Mempool = append(bc.Mempool, Transaction{
			Sender:    accB.Address,
			Recipient: recip.Address,
			Amount:    1,
			Fee:       MinRelayFee * 20,
			Nonce:     int64(i),
			Type:      TxTypeStandard,
		})
	}
	bc.CleanMempoolLocked()
	bc.Lock.Unlock()

	// Case 1: All other senders pay >= txA2.Fee (20k > 15k), so txA2 MUST be rejected
	// and must NEVER evict txA1 (fee 12k).
	if ok, _ := bc.AddTransaction(txA2); ok {
		t.Fatalf("Expected txA2 to be rejected when only own nonce 1 had lower fee")
	}

	bc.Lock.RLock()
	hasA1 := false
	for _, mTx := range bc.Mempool {
		if mTx.HashHex() == txA1.HashHex() {
			hasA1 = true
		}
		if mTx.HashHex() == txA2.HashHex() {
			bc.Lock.RUnlock()
			t.Fatalf("txA2 should not be present in mempool")
		}
	}
	bc.Lock.RUnlock()
	if !hasA1 {
		t.Fatalf("Sender A nonce 1 was evicted by its own nonce 2 submission")
	}

	// Case 2: Replace one accB slot with accC (fee 13k < txA2.Fee 15k):
	// txA2 must evict accC and keep accA nonces [1, 2] intact.
	bc.Lock.Lock()
	bc.Mempool[len(bc.Mempool)-1] = Transaction{
		Sender:    accC.Address,
		Recipient: recip.Address,
		Amount:    1,
		Fee:       MinRelayFee * 13,
		Nonce:     1,
		Type:      TxTypeStandard,
	}
	bc.CleanMempoolLocked()
	bc.Lock.Unlock()

	ok, msg := bc.AddTransaction(txA2)
	if !ok {
		t.Fatalf("Expected txA2 to evict accC tail tx cleanly, got: %s", msg)
	}

	bc.Lock.RLock()
	var noncesA []int64
	foundC := false
	for _, mTx := range bc.Mempool {
		if mTx.Sender == accA.Address {
			noncesA = append(noncesA, mTx.Nonce)
		}
		if mTx.Sender == accC.Address {
			foundC = true
		}
	}
	bc.Lock.RUnlock()

	if foundC {
		t.Fatalf("Expected accC low-fee tail tx to be evicted")
	}
	if len(noncesA) != 2 || noncesA[0] != 1 || noncesA[1] != 2 {
		t.Fatalf("Expected intact sender A nonces [1, 2], got %v", noncesA)
	}
}

// TestTxMempoolStorage_ReorgTxIndexSurvivesCrossHeightMove verifies:
// When a transaction is confirmed at height #3 on branch A and moves to height #2
// on heavier branch B during ResolveFork, persistent BoltDB bktTxIndex must keep it indexed at height #2.
func TestTxMempoolStorage_ReorgTxIndexSurvivesCrossHeightMove(t *testing.T) {
	unlock := AcquireNetworkTestLock()
	defer unlock()
	setupEasyTargetForMempoolTest(t)

	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "reorg_txindex.db")
	bc, err := NewBlockchainWithStoragePath(1, dbPath)
	if err != nil {
		t.Fatalf("NewBlockchainWithStoragePath failed: %v", err)
	}
	bc.CoinbaseMaturity = 1
	bc.State.CoinbaseMaturity = 1
	defer bc.Close()

	accSender := newTestAccount(t, "sender")
	accRecip := newTestAccount(t, "recip")

	// Block #1 funds sender
	if _, err := bc.MineBlockContext(context.Background(), accSender.Address); err != nil {
		t.Fatalf("MineBlock 1 failed: %v", err)
	}
	// Block #2 is empty
	if _, err := bc.MineBlockContext(context.Background(), GenesisMiner); err != nil {
		t.Fatalf("MineBlock 2 failed: %v", err)
	}

	txA := Transaction{
		Sender:    accSender.Address,
		Recipient: accRecip.Address,
		Amount:    5 * CoinUnit,
		Fee:       CalculateMinMempoolFee(12_000),
		Nonce:     1,
		Type:      TxTypeStandard,
	}
	if err := txA.Sign(accSender.PrivKey); err != nil {
		t.Fatalf("sign txA failed: %v", err)
	}
	if ok, msg := bc.AddTransaction(txA); !ok {
		t.Fatalf("AddTransaction(txA) failed: %s", msg)
	}
	// Block #3 confirms txA at height 3 on branch A
	if _, err := bc.MineBlockContext(context.Background(), GenesisMiner); err != nil {
		t.Fatalf("MineBlock 3 failed: %v", err)
	}

	// Build competing heavier fork from ancestor #1 where txA moves earlier to height #2
	target := bc.CalculateLWMA1()
	b2Fork := mineSignedTestBlock(t, bc.Chain[1], 2, GenesisMiner, InitialReward, []Transaction{txA}, target)
	b3Fork := mineSignedTestBlock(t, b2Fork, 3, GenesisMiner, InitialReward, nil, target)
	b4Fork := mineSignedTestBlock(t, b3Fork, 4, GenesisMiner, InitialReward, nil, target)

	if err := bc.ResolveFork(1, []Block{b2Fork, b3Fork, b4Fork}); err != nil {
		t.Fatalf("ResolveFork failed: %v", err)
	}

	// Verify persistent bktTxIndex on disk directly so in-memory fallback cannot mask index deletion
	txHash := txA.HashHex()
	db, err := bc.getStoreDB()
	if err != nil {
		t.Fatalf("getStoreDB failed: %v", err)
	}
	var indexedHeight uint64
	var indexedFound bool
	if err := db.View(func(tx *bolt.Tx) error {
		bktT := tx.Bucket(bktTxIndex)
		if bktT == nil {
			return nil
		}
		loc := bktT.Get([]byte(txHash))
		if len(loc) == 0 {
			return nil
		}
		h, _, decErr := decodeTxLocation(loc)
		if decErr != nil {
			return decErr
		}
		indexedHeight = h
		indexedFound = true
		return nil
	}); err != nil {
		t.Fatalf("bktTxIndex check failed: %v", err)
	}
	if !indexedFound || indexedHeight != 2 {
		t.Fatalf("bktTxIndex entry missing or wrong height after reorg: found=%v height=%d want=2", indexedFound, indexedHeight)
	}

	gotTx, gotIdx, _, gotBlkHash, err := bc.GetTransactionByHash(txHash)
	if err != nil || gotTx == nil || gotIdx != 2 || gotBlkHash != b2Fork.Hash {
		t.Fatalf("GetTransactionByHash failed after reorg: idx=%d hash=%s err=%v", gotIdx, gotBlkHash, err)
	}
}

// TestTxMempoolStorage_CorruptSnapshotHealsFromGenesisOnLoad verifies:
// A tampered or corrupted state snapshot in bktSnapshots is detected via supply/invariant
// validation during loadBlockchainFromPath and automatically healed via Genesis replay.
func TestTxMempoolStorage_CorruptSnapshotHealsFromGenesisOnLoad(t *testing.T) {
	unlock := AcquireNetworkTestLock()
	defer unlock()
	setupEasyTargetForMempoolTest(t)

	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "corrupt_snapshot.db")
	bc, err := NewBlockchainWithStoragePath(1, dbPath)
	if err != nil {
		t.Fatalf("NewBlockchainWithStoragePath failed: %v", err)
	}

	minerAcc := newTestAccount(t, "miner")
	for i := 0; i < 3; i++ {
		if _, err := bc.MineBlockContext(context.Background(), minerAcc.Address); err != nil {
			t.Fatalf("MineBlock %d failed: %v", i+1, err)
		}
	}
	expectedMinerBal := bc.GetBalance(minerAcc.Address)
	expectedSupply := bc.GetCirculatingSupply()
	if err := bc.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Corrupt snapshot at height 0 with inflated balance (mismatches chain supply)
	db, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	tamperedState := NewStateManager()
	tamperedState.Accounts[minerAcc.AddressHex] = AccountState{
		Balance: 99_999_999 * CoinUnit,
		Nonce:   0,
	}
	tamperedBytes, err := json.Marshal(tamperedState)
	if err != nil {
		db.Close()
		t.Fatalf("marshal tampered state failed: %v", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bktSnapshots).Put(uint64ToBytes(0), encodeRecord(tamperedBytes))
	}); err != nil {
		db.Close()
		t.Fatalf("inject tampered snapshot failed: %v", err)
	}
	_ = db.Close()

	loadedBc, found, err := loadBlockchainFromPath(dbPath)
	if err != nil || !found || loadedBc == nil {
		t.Fatalf("Expected corrupt snapshot to heal via Genesis replay, got found=%v err=%v", found, err)
	}
	defer loadedBc.Close()

	if len(loadedBc.Chain) != 4 {
		t.Fatalf("expected 4 blocks after reload, got %d", len(loadedBc.Chain))
	}
	if got := loadedBc.GetBalance(minerAcc.Address); got != expectedMinerBal {
		t.Fatalf("corrupt snapshot leaked balance: got %d, want %d", got, expectedMinerBal)
	}
	if got := loadedBc.GetCirculatingSupply(); got != expectedSupply {
		t.Fatalf("expected circulating supply %d, got %d", expectedSupply, got)
	}
	_ = loadedBc.Close()

	// Case 2 (REV-T2-01): Same-total cross-account balance shift at height #0 snapshot must be detected and healed.
	genMinerHex, _ := AddressToHex(GenesisMiner)
	otherAcc := newTestAccount(t, "other-snap")
	dbShift, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore shift failed: %v", err)
	}
	shiftedState := NewStateManager()
	shiftedState.Accounts[genMinerHex] = AccountState{
		Balance: InitialReward - 10*CoinUnit,
		Nonce:   0,
	}
	shiftedState.Accounts[otherAcc.AddressHex] = AccountState{
		Balance: 10 * CoinUnit,
		Nonce:   0,
	}
	shiftedBytes, _ := json.Marshal(shiftedState)
	if err := dbShift.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bktSnapshots).Put(uint64ToBytes(0), encodeRecord(shiftedBytes))
	}); err != nil {
		_ = dbShift.Close()
		t.Fatalf("inject shifted snapshot failed: %v", err)
	}
	_ = dbShift.Close()

	healedShiftBc, found, err := loadBlockchainFromPath(dbPath)
	if err != nil || !found || healedShiftBc == nil {
		t.Fatalf("expected same-total shifted snapshot to heal via Genesis replay, got found=%v err=%v", found, err)
	}
	if got := healedShiftBc.GetBalance(minerAcc.Address); got != expectedMinerBal {
		_ = healedShiftBc.Close()
		t.Fatalf("same-total shifted snapshot leaked miner balance: got %d, want %d", got, expectedMinerBal)
	}
	if gotOther := healedShiftBc.GetBalance(otherAcc.Address); gotOther != 0 {
		_ = healedShiftBc.Close()
		t.Fatalf("same-total shifted snapshot leaked other account balance: got %d, want 0", gotOther)
	}
	_ = healedShiftBc.Close()

	// Case 3 (REV-T2-01): Tampered account Nonce in snapshot with unchanged balance must be detected and healed.
	dbNonce, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore nonce failed: %v", err)
	}
	nonceState := NewStateManager()
	nonceState.Accounts[genMinerHex] = AccountState{
		Balance: InitialReward,
		Nonce:   99,
	}
	nonceBytes, _ := json.Marshal(nonceState)
	if err := dbNonce.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bktSnapshots).Put(uint64ToBytes(0), encodeRecord(nonceBytes))
	}); err != nil {
		_ = dbNonce.Close()
		t.Fatalf("inject nonce snapshot failed: %v", err)
	}
	_ = dbNonce.Close()

	healedNonceBc, found, err := loadBlockchainFromPath(dbPath)
	if err != nil || !found || healedNonceBc == nil {
		t.Fatalf("expected nonce-tampered snapshot to heal via Genesis replay, got found=%v err=%v", found, err)
	}
	defer healedNonceBc.Close()
	if gotNonce := healedNonceBc.GetAccountNonce(GenesisMiner); gotNonce != 0 {
		t.Fatalf("nonce-tampered snapshot leaked nonce %d, want 0", gotNonce)
	}
	_ = healedNonceBc.Close()

	// Case 4: Consistent pre-snapshot block (#1) + snapshot (#100) tampering must fail
	// pre-snapshot Merkle/PoW/header validation in loadBlockchainFromPath.
	snapPath := filepath.Join(t.TempDir(), "consistent_block_snap_tamper.db")
	snapBc, err := NewBlockchainWithStoragePath(1, snapPath)
	if err != nil {
		t.Fatalf("NewBlockchainWithStoragePath snapBc failed: %v", err)
	}
	for i := 0; i < SnapshotInterval; i++ {
		if _, err := snapBc.MineBlockContext(context.Background(), minerAcc.Address); err != nil {
			t.Fatalf("MineBlock #%d failed: %v", i+1, err)
		}
	}
	if err := snapBc.Close(); err != nil {
		t.Fatalf("snapBc.Close failed: %v", err)
	}

	dbBoth, err := OpenStore(snapPath)
	if err != nil {
		t.Fatalf("OpenStore snapPath failed: %v", err)
	}
	if err := dbBoth.Update(func(tx *bolt.Tx) error {
		bktB := tx.Bucket(bktBlocks)
		bktS := tx.Bucket(bktSnapshots)
		rawB1 := bktB.Get(uint64ToBytes(1))
		var b1 Block
		if uerr := unmarshalRecord(rawB1, &b1); uerr != nil {
			return uerr
		}
		// Tamper coinbase amount + block hash/merkle inconsistently or consistently with snapshot #100
		const extraMint = 5 * CoinUnit
		b1.Transactions[0].Amount += extraMint
		b1.MerkleRoot = CalculateMerkleRootForNetwork(b1.Transactions, GetActiveNetwork().NetworkID)
		if err := bktB.Put(uint64ToBytes(1), encodeRecord(func() []byte {
			d, _ := json.Marshal(b1)
			return d
		}())); err != nil {
			return err
		}

		rawS100 := bktS.Get(uint64ToBytes(SnapshotInterval))
		var s100 StateManager
		if uerr := unmarshalRecord(rawS100, &s100); uerr != nil {
			return uerr
		}
		acc := s100.Accounts[minerAcc.AddressHex]
		acc.Balance += extraMint
		s100.Accounts[minerAcc.AddressHex] = acc
		sData, _ := json.Marshal(&s100)
		return bktS.Put(uint64ToBytes(SnapshotInterval), encodeRecord(sData))
	}); err != nil {
		_ = dbBoth.Close()
		t.Fatalf("tamper block+snapshot failed: %v", err)
	}
	_ = dbBoth.Close()

	if badBc, found, err := loadBlockchainFromPath(snapPath); err == nil || found || badBc != nil {
		if badBc != nil {
			_ = badBc.Close()
		}
		t.Fatalf("expected consistent block+snapshot tampering before snapshot #%d to be rejected, got found=%v err=%v", SnapshotInterval, found, err)
	}
}

// TestTxMempoolStorage_TxBinaryV1CanonicalExtendedModes verifies:
// UnmarshalBinary strictly rejects non-canonical extended flag/length encodings so every
// validated Transaction has a unique canonical TxBinaryV1 byte representation.
func TestTxMempoolStorage_TxBinaryV1CanonicalExtendedModes(t *testing.T) {
	cb := Transaction{
		Sender:    "SYSTEM",
		Recipient: GenesisMiner,
		Amount:    InitialReward,
		Fee:       0,
		Nonce:     1,
		Signature: "REWARD_SIG",
	}
	baseBin, err := cb.MarshalBinary()
	if err != nil {
		t.Fatalf("cb.MarshalBinary failed: %v", err)
	}

	// 1. Mutually exclusive flags (txFlagHasExtended | txFlagExplicitStandardType)
	mutExcl := append([]byte(nil), baseBin...)
	mutExcl[1] |= txFlagHasExtended | txFlagExplicitStandardType
	var out Transaction
	if err := out.UnmarshalBinary(mutExcl); err == nil {
		t.Fatalf("Expected mutually exclusive extended+standard flags to fail")
	}

	// Build a valid HTLC tx with 32-byte raw hashlock and 37-byte preimage, then craft non-canonical forms
	htlcTx := Transaction{
		Sender:    "SYSTEM",
		Recipient: GenesisMiner,
		Amount:    InitialReward,
		Fee:       0,
		Nonce:     1,
		Signature: "REWARD_SIG",
		Type:      TxTypeHTLCClaim,
		HashLock:  strings.Repeat("ab", 32),
		TimeLock:  1765000000,
		Preimage:  strings.Repeat("cd", 32),
	}
	htlcBin, err := htlcTx.MarshalBinary()
	if err != nil {
		t.Fatalf("htlcTx.MarshalBinary failed: %v", err)
	}
	if err := out.UnmarshalBinary(htlcBin); err != nil {
		t.Fatalf("canonical htlc UnmarshalBinary failed: %v", err)
	}

	// Construct extended section manually on a mutable header copy
	headerOnly := append([]byte(nil), baseBin...)
	headerOnly[1] |= txFlagHasExtended

	// Append helper for extended section tests
	makeExt := func(txType string, hlMode byte, hlData []byte, preMode byte, preData []byte) []byte {
		buf := append([]byte(nil), headerOnly...)
		var u16 [2]byte
		binary.LittleEndian.PutUint16(u16[:], uint16(len(txType)))
		buf = append(buf, u16[:]...)
		buf = append(buf, []byte(txType)...)
		buf = append(buf, hlMode)
		if hlMode == 0x01 {
			buf = append(buf, hlData...)
		} else if hlMode == 0x02 {
			binary.LittleEndian.PutUint16(u16[:], uint16(len(hlData)))
			buf = append(buf, u16[:]...)
			buf = append(buf, hlData...)
		}
		var u64 [8]byte
		buf = append(buf, u64[:]...) // TimeLock = 0
		buf = append(buf, preMode)
		if preMode != 0x00 {
			binary.LittleEndian.PutUint16(u16[:], uint16(len(preData)))
			buf = append(buf, u16[:]...)
			buf = append(buf, preData...)
		}
		return buf
	}

	badPayloads := []struct {
		name string
		data []byte
	}{
		{"empty extended section", makeExt("", 0x00, nil, 0x00, nil)},
		{"non-canonical hlMode=0x02 empty", makeExt(TxTypeHTLCLock, 0x02, []byte{}, 0x00, nil)},
		{"non-canonical hlMode=0x02 64 lowercase hex", makeExt(TxTypeHTLCLock, 0x02, []byte(strings.Repeat("aa", 32)), 0x00, nil)},
		{"invalid preMode=0x01 non-32-byte len", makeExt(TxTypeHTLCClaim, 0x01, make([]byte, 32), 0x01, make([]byte, 16))},
		{"non-canonical preMode=0x02 empty", makeExt(TxTypeHTLCClaim, 0x01, make([]byte, 32), 0x02, []byte{})},
		{"non-canonical preMode=0x02 64 lowercase hex", makeExt(TxTypeHTLCClaim, 0x01, make([]byte, 32), 0x02, []byte(strings.Repeat("cc", 32)))},
	}

	for _, tc := range badPayloads {
		if err := out.UnmarshalBinary(tc.data); err == nil {
			t.Fatalf("Expected UnmarshalBinary to reject %s", tc.name)
		}
	}
}

// TestTxMempoolStorage_LargeReorgInvalidOrphansDoNotEvictValidMempool verifies that when
// disconnected blocks hold >= MaxMempoolSize state-invalid orphaned transactions (conflicting with
// the winning fork state), ResolveFork purges invalid orphans via CleanMempoolLocked() BEFORE
// enforcing MaxMempoolSize so valid pending mempool transactions are not crowded out.
func TestTxMempoolStorage_LargeReorgInvalidOrphansDoNotEvictValidMempool(t *testing.T) {
	unlock := AcquireNetworkTestLock()
	defer unlock()
	setupEasyTargetForMempoolTest(t)

	dbPath := filepath.Join(t.TempDir(), "large_reorg_orphan_evict.db")
	bc, err := NewBlockchainWithStoragePath(1, dbPath)
	if err != nil {
		t.Fatalf("NewBlockchainWithStoragePath failed: %v", err)
	}
	bc.CoinbaseMaturity = 1
	bc.State.CoinbaseMaturity = 1
	defer bc.Close()

	accOrphan := newTestAccount(t, "orphan-sender")
	accValid := newTestAccount(t, "valid-sender")
	accRecip := newTestAccount(t, "recip")

	// Block #1 funds accOrphan; Block #2 funds accValid.
	if _, err := bc.MineBlockContext(context.Background(), accOrphan.Address); err != nil {
		t.Fatalf("MineBlock #1 failed: %v", err)
	}
	if _, err := bc.MineBlockContext(context.Background(), accValid.Address); err != nil {
		t.Fatalf("MineBlock #2 failed: %v", err)
	}

	// Valid pending transaction from accValid sitting in bc.Mempool before the reorg.
	validPendingTx := Transaction{
		Sender:    accValid.Address,
		Recipient: accRecip.Address,
		Amount:    5 * CoinUnit,
		Fee:       CalculateMinMempoolFee(12_000),
		Nonce:     1,
		Type:      TxTypeStandard,
	}
	if err := validPendingTx.Sign(accValid.PrivKey); err != nil {
		t.Fatalf("sign validPendingTx failed: %v", err)
	}
	if ok, msg := bc.AddTransaction(validPendingTx); !ok {
		t.Fatalf("AddTransaction(validPendingTx) failed: %s", msg)
	}

	// Sign 1 template tx from accOrphan, then synthesize MaxMempoolSize+5 disconnected transactions
	// across disconnected branch A blocks. On winning branch B, accOrphan spends its entire
	// balance at Block #3, making all 5005 orphaned txs state-invalid after reorg.
	tmplOrphan := Transaction{
		Sender:    accOrphan.Address,
		SenderPub: accOrphan.PubHex,
		Recipient: accRecip.Address,
		Amount:    1,
		Fee:       12_000,
		Nonce:     1,
		Type:      TxTypeStandard,
	}
	if err := tmplOrphan.Sign(accOrphan.PrivKey); err != nil {
		t.Fatalf("sign tmplOrphan failed: %v", err)
	}

	const totalInvalidOrphans = MaxMempoolSize + 5
	const perBlock = 900
	var orphanBlocks []Block
	prevBlk := bc.Chain[len(bc.Chain)-1]
	target := bc.CalculateLWMA1()
	nonceSeq := int64(1)
	for len(orphanBlocks)*perBlock < totalInvalidOrphans {
		var blkTxs []Transaction
		for j := 0; j < perBlock && int(nonceSeq) <= totalInvalidOrphans; j++ {
			oTx := tmplOrphan
			oTx.InvalidateCache()
			oTx.Nonce = nonceSeq
			oTx.MarkSignatureVerified()
			blkTxs = append(blkTxs, oTx)
			nonceSeq++
		}
		blk := mineSignedTestBlock(t, prevBlk, prevBlk.Index+1, GenesisMiner, InitialReward, blkTxs, target)
		orphanBlocks = append(orphanBlocks, blk)
		prevBlk = blk
	}

	// Winning branch B from ancestor #2: Block #3 spends accOrphan's full balance via a different tx.
	spendAllTx := Transaction{
		Sender:    accOrphan.Address,
		Recipient: accRecip.Address,
		Amount:    InitialReward - 12_000,
		Fee:       12_000,
		Nonce:     1,
		Type:      TxTypeStandard,
	}
	if err := spendAllTx.Sign(accOrphan.PrivKey); err != nil {
		t.Fatalf("sign spendAllTx failed: %v", err)
	}
	forkLen := len(orphanBlocks) + 1
	forkBlocks := make([]Block, 0, forkLen)
	prevFork := bc.Chain[2]
	for i := 0; i < forkLen; i++ {
		var txs []Transaction
		if i == 0 {
			txs = []Transaction{spendAllTx}
		}
		fb := mineSignedTestBlock(t, prevFork, prevFork.Index+1, GenesisMiner, InitialReward, txs, target)
		forkBlocks = append(forkBlocks, fb)
		prevFork = fb
	}

	// Simulate branch A blocks being active right before ResolveFork switches to branch B.
	bc.Lock.Lock()
	bc.Chain = append(bc.Chain, orphanBlocks...)
	bc.Lock.Unlock()

	if err := bc.ResolveFork(2, forkBlocks); err != nil {
		t.Fatalf("ResolveFork failed: %v", err)
	}

	bc.Lock.RLock()
	defer bc.Lock.RUnlock()
	if len(bc.Mempool) != 1 || bc.Mempool[0].HashHex() != validPendingTx.HashHex() {
		t.Fatalf("expected valid pending mempool tx to survive large reorg with invalid orphans; mempool len=%d", len(bc.Mempool))
	}
}
