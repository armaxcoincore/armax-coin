//go:build testhooks

package core

import (
	"strings"
	"testing"
)

func TestMempool_AddTransactionEffectiveSizeActivation(t *testing.T) {
	LockNetworkForTesting(t)
	SetNetworkForTesting(true)
	SetAllowMainnetForTesting(true)
	defer func() {
		ResetBlockBinarySizeActivationHeightOverride()
		ResetMempoolBinaryFeeActivationHeightOverride()
		_ = SetNetwork("testnet")
	}()

	// Verify Mainnet profile activates both binary block size and binary mempool fee at genesis (#0).
	if err := SetNetwork("mainnet"); err != nil {
		t.Fatalf("SetNetwork(mainnet) failed: %v", err)
	}
	if got := GetBlockBinarySizeActivationHeight(); got != 0 {
		t.Fatalf("expected ARMX-MAINNET-1 BlockBinarySizeActivationHeight = 0, got %d", got)
	}
	if got := GetMempoolBinaryFeeActivationHeight(); got != 0 {
		t.Fatalf("expected ARMX-MAINNET-1 MempoolBinaryFeeActivationHeight = 0, got %d", got)
	}
	if !IsBlockBinarySizeActive(0) || !IsMempoolBinaryFeeActive(0) {
		t.Fatalf("expected TxBinaryV1 size and fee weighting to be active from genesis #0 on ARMX-MAINNET-1")
	}

	if err := SetNetwork("testnet"); err != nil {
		t.Fatalf("SetNetwork(testnet) restore failed: %v", err)
	}
	if got := GetMempoolBinaryFeeActivationHeight(); got != -1 {
		t.Fatalf("expected ARMX-TESTNET-1 MempoolBinaryFeeActivationHeight = -1 for v1.0.12 relay compat, got %d", got)
	}
	if IsMempoolBinaryFeeActive(int(TestnetProfile.BlockBinarySizeActivationHeight)) {
		t.Fatalf("expected Testnet mempool relay fee to stay JSON-weighted at BlockBinarySizeActivationHeight for v1.0.12 compat")
	}
	ResetBlockBinarySizeActivationHeightOverride()
	ResetMempoolBinaryFeeActivationHeightOverride()

	t.Setenv("ARMX_DATA_DIR", t.TempDir())
	bc := NewBlockchain(1)
	defer bc.Close()

	sender := newTestAccount(t, "fee-weight-sender")
	recip := newTestAccount(t, "fee-weight-recip")

	senderHex, _ := AddressToHex(sender.Address)
	bc.State.Lock.Lock()
	bc.State.Accounts[senderHex] = AccountState{
		Balance: 100 * CoinUnit,
		Nonce:   0,
	}
	bc.State.Lock.Unlock()

	buildSignedTx := func(nonce, fee int64) Transaction {
		tx := Transaction{
			Sender:    sender.Address,
			SenderPub: sender.PubHex,
			Recipient: recip.Address,
			Amount:    5 * CoinUnit,
			Fee:       fee,
			Nonce:     nonce,
			Type:      TxTypeStandard,
		}
		if err := tx.Sign(sender.PrivKey); err != nil {
			t.Fatalf("tx.Sign failed: %v", err)
		}
		return tx
	}

	// Pre-activation (Testnet): legacy JSON weight (~10.8 KB -> 11,000 Armoids).
	// A 6,000-Armoid fee (binary weight min) must be rejected before MempoolBinaryFeeActivationHeight.
	txPreLow := buildSignedTx(1, 6000)
	if ok, reason := bc.AddTransaction(txPreLow); ok || !strings.Contains(reason, "below required relay fee") {
		t.Fatalf("expected pre-activation AddTransaction with 6000 fee to reject below required relay fee, got ok=%v reason=%q", ok, reason)
	}

	// Post-activation (MempoolBinaryFeeActivationHeight = 0 / Mainnet): FeeEffectiveTxSize drops to BinarySize() (~5,320 B -> 6,000 Armoids).
	SetMempoolBinaryFeeActivationHeight(0)
	defer ResetMempoolBinaryFeeActivationHeightOverride()

	txPostTooLow := buildSignedTx(1, 5000)
	if ok, reason := bc.AddTransaction(txPostTooLow); ok || !strings.Contains(reason, "below required relay fee") {
		t.Fatalf("expected post-activation AddTransaction with 5000 fee to reject below required relay fee, got ok=%v reason=%q", ok, reason)
	}

	binFee := CalculateMinMempoolFee(FeeEffectiveTxSize(&txPreLow, 1))
	if binFee != 6000 {
		t.Fatalf("expected TxBinaryV1 FeeEffectiveTxSize min fee = 6000 Armoids, got %d", binFee)
	}
	txPostValid := buildSignedTx(1, binFee)
	if ok, reason := bc.AddTransaction(txPostValid); !ok {
		t.Fatalf("expected post-activation AddTransaction with binary-weight fee %d to succeed, got reason=%q", binFee, reason)
	}
}
