//go:build testhooks

package wallet

import (
	"strings"
	"testing"

	"github.com/armaxcoincore/armax-coin/core"
)

func useRegtestForMultisigFiles(t *testing.T) {
	t.Helper()
	core.LockNetworkForTesting(t)
	core.SetNetworkForTesting(true)
	t.Cleanup(func() { core.SetNetworkForTesting(false) })
	if err := core.SetNetwork("regtest"); err != nil {
		t.Fatalf("SetNetwork(regtest): %v", err)
	}
	t.Cleanup(func() { _ = core.SetNetwork("testnet") })
}

func multisigCosigners(t *testing.T, n int) ([]*Wallet, []string) {
	t.Helper()
	ws := make([]*Wallet, n)
	pubs := make([]string, n)
	for i := range n {
		w, err := NewWallet()
		if err != nil {
			t.Fatalf("NewWallet: %v", err)
		}
		t.Cleanup(w.Zeroize)
		ws[i] = w
		pubs[i] = w.PublicKey
	}
	return ws, pubs
}

// TestMultisigFiles_TwoOfThreeFlow drives propose -> two independent signatures -> combine and
// checks the final transaction verifies and is bound to the descriptor account.
func TestMultisigFiles_TwoOfThreeFlow(t *testing.T) {
	useRegtestForMultisigFiles(t)
	ws, pubs := multisigCosigners(t, 3)
	df, err := NewMultisigDescriptorFile(2, pubs)
	if err != nil {
		t.Fatalf("NewMultisigDescriptorFile: %v", err)
	}
	if !strings.HasPrefix(df.Address, core.RegtestAddressHRP+"1") || df.Network != "ARMX-REGTEST-1" {
		t.Fatalf("descriptor file network/address = %s %s", df.Network, df.Address)
	}

	recipient := MustNewWallet()
	defer recipient.Zeroize()
	spend, err := NewMultisigSpend(df, recipient.Address, 3*core.CoinUnit, -1, 1)
	if err != nil {
		t.Fatalf("NewMultisigSpend: %v", err)
	}
	if spend.Tx.Fee <= 0 {
		t.Fatalf("projected fee not applied: %d", spend.Tx.Fee)
	}

	// Each cosigner signs its own copy, as on separate offline machines.
	a, b := *spend, *spend
	a.PartialSignatures, b.PartialSignatures = map[string]string{}, map[string]string{}
	if err := a.Sign(ws[0]); err != nil {
		t.Fatalf("cosigner 1 sign: %v", err)
	}
	if err := b.Sign(ws[2]); err != nil {
		t.Fatalf("cosigner 3 sign: %v", err)
	}
	if _, err := CombineMultisigSpends([]*MultisigSpendFile{&a}); err == nil {
		t.Fatalf("one share combined into a 2-of-3 spend")
	}
	tx, err := CombineMultisigSpends([]*MultisigSpendFile{&a, &b})
	if err != nil {
		t.Fatalf("CombineMultisigSpends: %v", err)
	}
	if err := tx.VerifyWithError(); err != nil {
		t.Fatalf("combined spend does not verify: %v", err)
	}
	if tx.Sender != df.Address || tx.HashHex() != spend.TxHash {
		t.Fatalf("combined spend not bound to the proposed transaction")
	}

	outsider := MustNewWallet()
	defer outsider.Zeroize()
	c := *spend
	c.PartialSignatures = map[string]string{}
	if err := c.Sign(outsider); err == nil {
		t.Fatalf("non-cosigner signed the spend")
	}
}

// TestMultisigFiles_TamperAndNetworkGuards verifies edited amounts, inconsistent descriptor files,
// cross-network recipients, and files from another network are refused before signing.
func TestMultisigFiles_TamperAndNetworkGuards(t *testing.T) {
	useRegtestForMultisigFiles(t)
	ws, pubs := multisigCosigners(t, 3)
	df, err := NewMultisigDescriptorFile(2, pubs)
	if err != nil {
		t.Fatalf("NewMultisigDescriptorFile: %v", err)
	}
	recipient := MustNewWallet()
	defer recipient.Zeroize()

	// Recipient from another network.
	payload, _ := core.AddressToHex(recipient.Address)
	if err := core.SetNetwork("mainnet"); err != nil {
		t.Fatalf("SetNetwork(mainnet): %v", err)
	}
	mainnetRecipient, _ := core.HexToAddress(payload)
	if err := core.SetNetwork("regtest"); err != nil {
		t.Fatalf("SetNetwork(regtest): %v", err)
	}
	if _, err := NewMultisigSpend(df, mainnetRecipient, core.CoinUnit, -1, 1); err == nil || !strings.Contains(err.Error(), "rejected before signing") {
		t.Fatalf("expected cross-network recipient rejection, got %v", err)
	}

	// Descriptor file whose address was edited.
	bad := *df
	bad.Address = recipient.Address
	if _, err := bad.ParsedDescriptor(); err == nil {
		t.Fatalf("descriptor file with a swapped address accepted")
	}

	spend, err := NewMultisigSpend(df, recipient.Address, core.CoinUnit, core.CoinUnit, 1)
	if err != nil {
		t.Fatalf("NewMultisigSpend: %v", err)
	}

	// Amount edited after the proposal: tx_hash no longer matches.
	edited := *spend
	edited.Tx.Amount = 100 * core.CoinUnit
	if err := edited.Sign(ws[0]); err == nil {
		t.Fatalf("spend with an edited amount was signed")
	}

	// Spend file from another network.
	foreign := *spend
	foreign.Network = "ARMX-MAINNET-1"
	if err := foreign.Sign(ws[0]); err == nil || !strings.Contains(err.Error(), "belongs to network ARMX-MAINNET-1") {
		t.Fatalf("expected foreign-network spend refusal, got %v", err)
	}

	// Shares for different transactions cannot be combined.
	other, err := NewMultisigSpend(df, recipient.Address, 2*core.CoinUnit, core.CoinUnit, 1)
	if err != nil {
		t.Fatalf("NewMultisigSpend: %v", err)
	}
	s1, s2 := *spend, *other
	s1.PartialSignatures, s2.PartialSignatures = map[string]string{}, map[string]string{}
	_ = s1.Sign(ws[0])
	_ = s2.Sign(ws[1])
	if _, err := CombineMultisigSpends([]*MultisigSpendFile{&s1, &s2}); err == nil {
		t.Fatalf("shares for different transactions were combined")
	}
}
