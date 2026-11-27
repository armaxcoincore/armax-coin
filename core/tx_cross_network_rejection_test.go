//go:build testhooks

package core

import (
	"strings"
	"testing"
)

// TestL1_2_DynamicNetworkID_TxCrossNetworkRejection verifies:
// 1. Identical tx data produces different tx.Hash() across mainnet vs testnet
// 2. Tx signed in testnet is rejected by Verify() in mainnet and vice-versa
// 3. SetNetwork cannot be called twice in non-testing mode
func TestL1_2_DynamicNetworkID_TxCrossNetworkRejection(t *testing.T) {
	LockNetworkForTesting(t)
	// Enable test override mode
	SetNetworkForTesting(true)
	defer SetNetworkForTesting(true)

	// Step 1: Set to testnet
	if err := SetNetwork("testnet"); err != nil {
		t.Fatalf("Failed to set testnet: %v", err)
	}
	if NetworkID() != "ARMX-TESTNET-1" {
		t.Fatalf("Expected ARMX-TESTNET-1, got %s", NetworkID())
	}

	acc := newTestAccount(t)
	txTestnet := Transaction{
		SigAlg:    1,
		Sender:    acc.Address,
		SenderPub: acc.PubHex,
		Recipient: "armx1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqplwcdd",
		Amount:    1000000,
		Fee:       12000,
		Nonce:     1,
	}
	if err := txTestnet.Sign(acc.PrivKey); err != nil {
		t.Fatalf("Sign failed: %v", err)
	}
	hashTestnet := txTestnet.HashHex()

	// Verify valid in testnet
	if err := txTestnet.VerifyWithError(); err != nil {
		t.Fatalf("txTestnet should be valid in testnet: %v", err)
	}

	// Step 2: Switch to mainnet
	if err := SetNetwork("mainnet"); err != nil {
		t.Fatalf("Failed to set mainnet: %v", err)
	}
	if NetworkID() != "ARMX-MAINNET-1" {
		t.Fatalf("Expected ARMX-MAINNET-1, got %s", NetworkID())
	}

	hashMainnet := txTestnet.HashHex()
	if hashTestnet == hashMainnet {
		t.Fatalf("Identical tx data MUST produce different tx.Hash() across networks! got same: %s", hashTestnet)
	}

	// Step 3: Verify tx signed on testnet is REJECTED in mainnet. Its armx1 addresses are already
	// rejected by the Mainnet address prefix check.
	errVerifyMainnet := txTestnet.VerifyWithError()
	if errVerifyMainnet == nil {
		t.Fatalf("Transaction signed on testnet MUST be rejected in mainnet!")
	}

	// Step 3b: Re-encode the same payloads with the Mainnet prefix but keep the Testnet signature.
	// The signature must still fail because the transaction hash commits to the Network ID.
	mainSender, err := PubHexToAddress(acc.PubHex)
	if err != nil || !strings.HasPrefix(mainSender, MainnetAddressHRP+"1") {
		t.Fatalf("PubHexToAddress on mainnet = %s, %v", mainSender, err)
	}
	replayed := txTestnet
	replayed.Sender = mainSender
	replayed.Recipient = MainnetGenesisMiner
	errReplay := replayed.VerifyWithError()
	if errReplay == nil || !strings.Contains(errReplay.Error(), "signature verification failed") {
		t.Fatalf("Expected signature verification failure for re-encoded testnet tx, got: %v", errReplay)
	}

	// Step 4: Sign on mainnet, verify in mainnet, then switch to testnet and verify rejected
	txMainnet := Transaction{
		SigAlg:    1,
		Sender:    mainSender,
		SenderPub: acc.PubHex,
		Recipient: MainnetGenesisMiner,
		Amount:    1000000,
		Fee:       12000,
		Nonce:     1,
	}
	if err := txMainnet.Sign(acc.PrivKey); err != nil {
		t.Fatalf("Sign failed: %v", err)
	}
	if err := txMainnet.VerifyWithError(); err != nil {
		t.Fatalf("txMainnet should be valid in mainnet: %v", err)
	}

	if err := SetNetwork("testnet"); err != nil {
		t.Fatalf("Failed to set testnet: %v", err)
	}
	errVerifyTestnet := txMainnet.VerifyWithError()
	if errVerifyTestnet == nil {
		t.Fatalf("Transaction signed on mainnet MUST be rejected in testnet!")
	}

	// Step 4b: Same check in reverse: Testnet-encoded addresses with the Mainnet signature fail
	// signature verification on Testnet.
	reverse := txMainnet
	reverse.Sender = acc.Address
	reverse.Recipient = TestnetGenesisMiner
	errReverse := reverse.VerifyWithError()
	if errReverse == nil || !strings.Contains(errReverse.Error(), "signature verification failed") {
		t.Fatalf("Expected signature verification failure for re-encoded mainnet tx, got: %v", errReverse)
	}

	// Step 5: Test SetNetwork once-only enforcement when SetNetworkForTesting(false)
	SetNetworkForTesting(false)
	errSecond := SetNetwork("mainnet")
	if errSecond == nil {
		t.Fatalf("Expected second call to SetNetwork to fail when testing override is disabled!")
	}
	if !strings.Contains(errSecond.Error(), "already initialized") {
		t.Fatalf("Expected 'already initialized' error message, got: %v", errSecond)
	}
}
