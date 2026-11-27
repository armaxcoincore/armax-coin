//go:build testhooks

package core

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/armaxcoincore/armax-coin/core/pqsig"
)

// multisigTestKeys returns n deterministic cosigner keys for the calling test.
func multisigTestKeys(t *testing.T, n int) ([]*pqsig.PrivateKey, [][]byte) {
	t.Helper()
	keys := make([]*pqsig.PrivateKey, n)
	pubs := make([][]byte, n)
	for i := range n {
		acc := newTestAccount(t, "cosigner-"+string(rune('a'+i)))
		keys[i] = acc.PrivKey
		pubs[i] = acc.PrivKey.PublicKeyBytes()
	}
	return keys, pubs
}

// signMultisigSpend prepares tx for d and attaches signatures from the given cosigner keys.
func signMultisigSpend(t *testing.T, tx *Transaction, d *MultisigDescriptor, signers []*pqsig.PrivateKey) {
	t.Helper()
	if err := tx.PrepareMultisigSpend(d); err != nil {
		t.Fatalf("PrepareMultisigSpend: %v", err)
	}
	sigs := make(map[int][]byte, len(signers))
	for _, k := range signers {
		idx, sig, err := tx.MultisigPartialSign(k)
		if err != nil {
			t.Fatalf("MultisigPartialSign: %v", err)
		}
		sigs[idx] = sig
	}
	if err := tx.AttachMultisigWitness(sigs); err != nil {
		t.Fatalf("AttachMultisigWitness: %v", err)
	}
}

func useRegtestForMultisig(t *testing.T) {
	t.Helper()
	LockNetworkForTesting(t)
	SetNetworkForTesting(true)
	t.Cleanup(func() { SetNetworkForTesting(false) })
	if err := SetNetwork("regtest"); err != nil {
		t.Fatalf("SetNetwork(regtest): %v", err)
	}
	t.Cleanup(func() { _ = SetNetwork("testnet") })
}

// TestMultisigDescriptor_CanonicalBounds verifies key ordering, duplicate rejection, and the
// Founder-approved bounds (2 <= M <= 3, M <= N <= 4).
func TestMultisigDescriptor_CanonicalBounds(t *testing.T) {
	_, pubs := multisigTestKeys(t, 5)

	d1, err := NewMultisigDescriptor(2, [][]byte{pubs[2], pubs[0], pubs[1]})
	if err != nil {
		t.Fatalf("2-of-3: %v", err)
	}
	d2, err := NewMultisigDescriptor(2, [][]byte{pubs[1], pubs[2], pubs[0]})
	if err != nil {
		t.Fatalf("2-of-3 reordered: %v", err)
	}
	if d1.Hex() != d2.Hex() {
		t.Fatalf("descriptor encoding depends on input key order")
	}
	parsed, err := ParseMultisigDescriptor(d1.Bytes())
	if err != nil || parsed.Hex() != d1.Hex() {
		t.Fatalf("descriptor round trip failed: %v", err)
	}

	cases := []struct {
		name string
		m    int
		keys [][]byte
	}{
		{"1-of-2 prohibited", 1, pubs[:2]},
		{"1-of-1 prohibited", 1, pubs[:1]},
		{"M greater than N", 3, pubs[:2]},
		{"4-of-4 exceeds MaxTxSize", 4, pubs[:4]},
		{"N above 4", 2, pubs[:5]},
		{"duplicate key", 2, [][]byte{pubs[0], pubs[0], pubs[1]}},
		{"short key", 2, [][]byte{pubs[0], pubs[1][:100]}},
	}
	for _, tc := range cases {
		if _, err := NewMultisigDescriptor(tc.m, tc.keys); err == nil {
			t.Errorf("%s: expected rejection", tc.name)
		}
	}

	// Non-canonical encodings of a valid key set are rejected by the parser.
	raw := d1.Bytes()
	swapped := append([]byte(nil), raw...)
	copy(swapped[2:2+pqsig.PublicKeySize], raw[2+pqsig.PublicKeySize:2+2*pqsig.PublicKeySize])
	copy(swapped[2+pqsig.PublicKeySize:2+2*pqsig.PublicKeySize], raw[2:2+pqsig.PublicKeySize])
	if _, err := ParseMultisigDescriptor(swapped); err == nil {
		t.Errorf("unsorted descriptor accepted")
	}
	if _, err := ParseMultisigDescriptor(append(append([]byte(nil), raw...), 0x00)); err == nil {
		t.Errorf("descriptor with trailing byte accepted")
	}
	if _, err := ParseMultisigDescriptorHex(strings.ToUpper(d1.Hex())); err == nil {
		t.Errorf("uppercase descriptor hex accepted")
	}
}

// TestMultisigAddress_DomainSeparated verifies a multisig address differs per descriptor, never
// equals a cosigner's single-key address, and carries the network prefix.
func TestMultisigAddress_DomainSeparated(t *testing.T) {
	useRegtestForMultisig(t)
	_, pubs := multisigTestKeys(t, 3)
	d23, _ := NewMultisigDescriptor(2, pubs)
	d33, _ := NewMultisigDescriptor(3, pubs)
	a23, err := d23.Address()
	if err != nil {
		t.Fatalf("Address: %v", err)
	}
	a33, _ := d33.Address()
	if a23 == a33 {
		t.Fatalf("2-of-3 and 3-of-3 over the same keys share an address")
	}
	if !strings.HasPrefix(a23, RegtestAddressHRP+"1") {
		t.Fatalf("multisig address %s lacks the regtest prefix", a23)
	}
	for _, p := range pubs {
		single, _ := PubToAddress(pqsig.SigAlgMLDSA65, p)
		if single == a23 {
			t.Fatalf("multisig address collides with a single-key address")
		}
	}
	main, _ := d23.AddressForNetwork(MainnetProfile.NetworkID)
	if !strings.HasPrefix(main, MainnetAddressHRP+"1") {
		t.Fatalf("mainnet multisig address %s lacks the ax prefix", main)
	}
}

// TestMultisigSpend_VerifyAndTamper covers a valid 2-of-3 spend, the size rules, the TxBinaryV1
// round trip, and rejection of tampered, short, or replayed witnesses.
func TestMultisigSpend_VerifyAndTamper(t *testing.T) {
	useRegtestForMultisig(t)
	keys, pubs := multisigTestKeys(t, 3)
	d, _ := NewMultisigDescriptor(2, pubs)
	recipient := newTestAccount(t, "recipient").Address

	tx := Transaction{Recipient: recipient, Amount: 5 * CoinUnit, Fee: CoinUnit, Nonce: 1, Type: TxTypeStandard}
	signMultisigSpend(t, &tx, d, []*pqsig.PrivateKey{keys[2], keys[0]})
	if err := tx.VerifyWithError(); err != nil {
		t.Fatalf("valid 2-of-3 spend rejected: %v", err)
	}
	if err := tx.ValidateCanonicalHex(); err != nil {
		t.Fatalf("ValidateCanonicalHex: %v", err)
	}

	bin := tx.BinarySize()
	if bin <= 0 || bin > MaxTxSize {
		t.Fatalf("2-of-3 binary size %d not within MaxTxSize", bin)
	}
	if tx.Size() <= MaxTxSize {
		t.Fatalf("expected JSON size above MaxTxSize to exercise binary sizing, got %d", tx.Size())
	}
	if eff := EffectiveTxSize(&tx, 1); eff != bin {
		t.Fatalf("EffectiveTxSize = %d, want binary size %d on a JSON-sized network", eff, bin)
	}
	if fee := FeeEffectiveTxSize(&tx, 1); fee != bin {
		t.Fatalf("FeeEffectiveTxSize = %d, want binary size %d", fee, bin)
	}

	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	var back Transaction
	if err := back.UnmarshalBinary(raw); err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}
	if back.HashHex() != tx.HashHex() || back.WitnessHashHex() != tx.WitnessHashHex() || back.Sender != tx.Sender {
		t.Fatalf("binary round trip changed the transaction")
	}

	// Flip one byte inside the first signature.
	bad := tx
	w, _ := hex.DecodeString(tx.Signature)
	w[10] ^= 0x01
	bad.Signature = hex.EncodeToString(w)
	bad.InvalidateCache()
	if bad.Verify() {
		t.Fatalf("tampered cosigner signature accepted")
	}

	// Bitmap naming three signers with only two signatures.
	w2, _ := hex.DecodeString(tx.Signature)
	w2[0] = 0x07
	bad.Signature = hex.EncodeToString(w2)
	if bad.Verify() {
		t.Fatalf("bitmap/signature count mismatch accepted")
	}

	// Moving the same signatures to a different descriptor (3-of-3 over the same keys) fails:
	// the descriptor is part of the signed hash.
	d33, _ := NewMultisigDescriptor(3, pubs)
	replay := tx
	if err := replay.PrepareMultisigSpend(d33); err != nil {
		t.Fatalf("PrepareMultisigSpend: %v", err)
	}
	if err := replay.AttachMultisigWitness(map[int][]byte{0: w[1 : 1+pqsig.SignatureSize]}); err == nil {
		t.Fatalf("signature from another descriptor accepted as a partial share")
	}

	// A single cosigner cannot spend alone.
	short := Transaction{Recipient: recipient, Amount: CoinUnit, Fee: CoinUnit, Nonce: 1}
	if err := short.PrepareMultisigSpend(d); err != nil {
		t.Fatalf("PrepareMultisigSpend: %v", err)
	}
	_, sig, err := short.MultisigPartialSign(keys[1])
	if err != nil {
		t.Fatalf("MultisigPartialSign: %v", err)
	}
	idx := d.KeyIndex(keys[1].PublicKeyBytes())
	if err := short.AttachMultisigWitness(map[int][]byte{idx: sig}); err == nil {
		t.Fatalf("1 of 2 required signatures aggregated")
	}

	// A non-cosigner key cannot produce a share.
	outsider := newTestAccount(t, "outsider").PrivKey
	if _, _, err := short.MultisigPartialSign(outsider); err == nil {
		t.Fatalf("non-cosigner key produced a partial signature")
	}
}

// TestMultisigSpend_DeterministicAggregation verifies that given more than M shares the lowest
// key indices are used, so cosigners combining the same shares build the same witness.
func TestMultisigSpend_DeterministicAggregation(t *testing.T) {
	useRegtestForMultisig(t)
	keys, pubs := multisigTestKeys(t, 3)
	d, _ := NewMultisigDescriptor(2, pubs)
	tx := Transaction{Recipient: newTestAccount(t, "recipient").Address, Amount: CoinUnit, Fee: CoinUnit, Nonce: 1}
	if err := tx.PrepareMultisigSpend(d); err != nil {
		t.Fatalf("PrepareMultisigSpend: %v", err)
	}
	sigs := map[int][]byte{}
	for _, k := range keys {
		idx, sig, err := tx.MultisigPartialSign(k)
		if err != nil {
			t.Fatalf("MultisigPartialSign: %v", err)
		}
		sigs[idx] = sig
	}
	w, err := EncodeMultisigWitness(d, sigs)
	if err != nil {
		t.Fatalf("EncodeMultisigWitness: %v", err)
	}
	if w[0] != 0x03 {
		t.Fatalf("bitmap = %#x, want 0x03 (keys 0 and 1)", w[0])
	}
	if err := tx.AttachMultisigWitness(sigs); err != nil {
		t.Fatalf("AttachMultisigWitness: %v", err)
	}
	if !tx.Verify() {
		t.Fatalf("aggregated spend does not verify")
	}
}

// TestMultisigSpend_ThreeOfFourFitsLimits verifies the largest allowed spend fits MaxTxSize in
// TxBinaryV1 and that its JSON form fits the /tx/new body limit (2*MaxTxSize + 16KB).
func TestMultisigSpend_ThreeOfFourFitsLimits(t *testing.T) {
	useRegtestForMultisig(t)
	keys, pubs := multisigTestKeys(t, 4)
	d, err := NewMultisigDescriptor(3, pubs)
	if err != nil {
		t.Fatalf("3-of-4: %v", err)
	}
	tx := Transaction{Recipient: newTestAccount(t, "recipient").Address, Amount: CoinUnit, Fee: CoinUnit, Nonce: 1}
	signMultisigSpend(t, &tx, d, keys[:3])
	if !tx.Verify() {
		t.Fatalf("3-of-4 spend does not verify")
	}
	if bin := tx.BinarySize(); bin <= 0 || bin > MaxTxSize {
		t.Fatalf("3-of-4 binary size %d exceeds MaxTxSize", bin)
	}
	js, _ := json.Marshal(tx)
	if len(js) > 2*MaxTxSize+16*1024 {
		t.Fatalf("3-of-4 JSON size %d exceeds the /tx/new body limit", len(js))
	}
}

// TestMultisig_DisabledOnTestnet verifies Testnet rejects multisig spends at every layer, so a
// v1.0.13 node never accepts a transaction v1.0.12 peers cannot parse.
func TestMultisig_DisabledOnTestnet(t *testing.T) {
	LockNetworkForTesting(t)
	if TestnetProfile.MultisigActivationHeight > 0 || SoakProfile.MultisigActivationHeight > 0 {
		t.Fatalf("multisig must stay disabled on Testnet and Soak")
	}
	if MainnetProfile.MultisigActivationHeight != 1 || RegtestProfile.MultisigActivationHeight != 1 {
		t.Fatalf("multisig must be active from block #1 on Mainnet and Regtest")
	}

	keys, pubs := multisigTestKeys(t, 3)
	d, _ := NewMultisigDescriptor(2, pubs)
	if err := SetNetwork("testnet"); err != nil {
		t.Fatalf("SetNetwork(testnet): %v", err)
	}
	tx := Transaction{Recipient: newTestAccount(t, "recipient").Address, Amount: CoinUnit, Fee: CoinUnit, Nonce: 1}
	signMultisigSpend(t, &tx, d, keys[:2])
	if err := tx.VerifyWithError(); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("expected multisig to be disabled on testnet, got %v", err)
	}
	if IsMultisigActive(1_000_000) {
		t.Fatalf("IsMultisigActive true on testnet")
	}
}

// TestMultisig_ActivationHeight verifies the block-height gate used by mempool, templates and
// block validation.
func TestMultisig_ActivationHeight(t *testing.T) {
	useRegtestForMultisig(t)
	SetMultisigActivationHeight(10)
	t.Cleanup(ResetMultisigActivationHeightOverride)
	tx := &Transaction{SigAlg: SigAlgMultisigMLDSA65}
	if err := checkMultisigActivation(tx, 9); err == nil {
		t.Fatalf("multisig spend accepted below activation height")
	}
	if err := checkMultisigActivation(tx, 10); err != nil {
		t.Fatalf("multisig spend rejected at activation height: %v", err)
	}
	if err := checkMultisigActivation(&Transaction{SigAlg: pqsig.SigAlgMLDSA65}, 0); err != nil {
		t.Fatalf("single-key spend affected by multisig gate: %v", err)
	}
}

// TestMultisigSpend_ChainEndToEnd funds a 2-of-3 account through a coinbase on Regtest, spends
// it through the mempool, mines it, and checks balances, nonce, and full chain revalidation.
func TestMultisigSpend_ChainEndToEnd(t *testing.T) {
	t.Setenv("ARMX_DATA_DIR", t.TempDir())
	useRegtestForMultisig(t)

	bc := NewBlockchain(1)
	defer bc.Close()

	keys, pubs := multisigTestKeys(t, 3)
	d, _ := NewMultisigDescriptor(2, pubs)
	msAddr, err := d.Address()
	if err != nil {
		t.Fatalf("Address: %v", err)
	}
	other := newTestAccount(t, "other").Address
	recipient := newTestAccount(t, "recipient").Address

	if _, err := bc.MineBlockContext(context.Background(), msAddr); err != nil {
		t.Fatalf("mine funding block: %v", err)
	}
	if _, err := bc.MineBlockContext(context.Background(), other); err != nil {
		t.Fatalf("mine maturity block: %v", err)
	}
	if got := bc.GetSpendableBalance(msAddr); got != InitialReward {
		t.Fatalf("multisig spendable balance = %d, want %d", got, InitialReward)
	}

	const amount = 7 * CoinUnit
	const fee = CoinUnit / 10
	tx := Transaction{Recipient: recipient, Amount: amount, Fee: fee, Nonce: 1, Type: TxTypeStandard}
	signMultisigSpend(t, &tx, d, []*pqsig.PrivateKey{keys[0], keys[2]})
	if ok, reason := bc.AddTransaction(tx); !ok {
		t.Fatalf("mempool rejected multisig spend: %s", reason)
	}

	blk, err := bc.MineBlockContext(context.Background(), other)
	if err != nil {
		t.Fatalf("mine spend block: %v", err)
	}
	if len(blk.Transactions) != 2 {
		t.Fatalf("spend block has %d transactions, want coinbase + multisig spend", len(blk.Transactions))
	}
	if got := bc.GetBalance(recipient); got != amount {
		t.Fatalf("recipient balance = %d, want %d", got, amount)
	}
	if got := bc.GetBalance(msAddr); got != InitialReward-amount-fee {
		t.Fatalf("multisig balance = %d, want %d", got, InitialReward-amount-fee)
	}
	if got := bc.GetAccountNonce(msAddr); got != 1 {
		t.Fatalf("multisig nonce = %d, want 1", got)
	}
	if !bc.IsChainValid() {
		t.Fatalf("chain with a multisig spend fails full revalidation")
	}

	// Replaying the confirmed spend is rejected by the nonce rule.
	if ok, _ := bc.AddTransaction(tx); ok {
		t.Fatalf("replayed multisig spend accepted")
	}
}
