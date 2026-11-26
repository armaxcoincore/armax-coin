package wallet

import (
	"crypto/hkdf"
	"crypto/mldsa"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"testing"

	"github.com/armaxcoincore/armax-coin/core"
	"github.com/armaxcoincore/armax-coin/core/pqsig"
)

// Golden end-to-end protocol vectors for the Armax L1 signing pipeline:
//
//	BIP-39 mnemonic -> BIP-39 seed -> HKDF-SHA256 account seed -> ML-DSA-65 key
//	-> armx1 address -> per-network transaction sighash -> deterministic signature
//	-> consensus verification.
//
// None of these steps is covered by upstream crypto vectors: they are Armax-specific
// consensus and wallet-recovery rules. Every value is pinned, and every derivation is
// also recomputed independently of the production code path, so a silent change in
// HKDF parameters, the context string, address encoding, sighash layout, or the Go
// toolchain fails here instead of forking the chain or breaking wallet restore.
const (
	goldenMnemonic   = "legal winner thank year wave sausage worth useful legal winner thank year wave sausage worth useful legal winner thank year wave sausage worth title"
	goldenPassphrase = "TREZOR"
	// Official BIP-39 seed for the mnemonic above (also pinned in bip39_official_vectors_test.go).
	goldenBIP39Seed = "bc09fca1804f7e69da93c2f2028eb238c227f2e9dda30cd63699232578480a4021b146ad717fbb7e451ce9eb835f43620bf5c514db0f8add49f5d121449d3e87"
)

type goldenAccount struct {
	index         uint32
	accountSeed   string // HKDF-SHA256 output (32 bytes)
	pubKeySHA256  string // SHA-256 of the 1,952-byte ML-DSA-65 public key
	address       string
	addressSHA256 string // Bech32 payload: SHA-256(SigAlg || pubKey)
}

var goldenAccounts = []goldenAccount{
	{
		index:         0,
		accountSeed:   "da72fa84b64d2c3810c5d8a2f97ad73ad055ad3b58295f901578870fb34e7564",
		pubKeySHA256:  "ac2c4bd3aacc9a873ad8458b55b806be909c881be5eb69b13a69cbbcf7d285e7",
		address:       "armx13kqn0sanhg0mlz66z9z4dp3slx0wxc0vjnuarhpvg2dlq4kfx8xq7f9vu0",
		addressSHA256: "8d8137c3b3ba1fbf8b5a1145568630f99ee361ec94f9d1dc2c429bf056c931cc",
	},
	{
		index:         1,
		accountSeed:   "516ae577cb31b982d573138456059b5bbe329303f409a8453e41db3a29b5ce62",
		pubKeySHA256:  "690fa8d1cc38a7140e2de0face30064bcc1394f3b5e6653a2b41a5ee0bc9c978",
		address:       "armx1dym0zr3c50j26jzw8fvtg5mpnrv86jvw7g3hqa9zd2v8v2hxf3lq0sf226",
		addressSHA256: "6936f10e38a3e4ad484e3a58b4536198d87d498ef2237074a26a98762ae64c7e",
	},
}

type goldenNetworkVector struct {
	networkID string
	sighash   string // SHA-256d of the canonical transaction record
	sigSHA256 string // SHA-256 of the deterministic 3,309-byte signature
}

var goldenNetworkVectors = []goldenNetworkVector{
	{
		networkID: "ARMX-MAINNET-1",
		sighash:   "b55509c8284cac2acfaa1d8d1ce54153727711fb15b6d246244a6a25f8165628",
		sigSHA256: "ee0cd5ca5475301e791411bda45ebe51b71aa00a29810ab4fedf0b72c2409e9e",
	},
	{
		networkID: "ARMX-TESTNET-1",
		sighash:   "c118d16ed52901ec49580b5ccae9cc9d7ae78a213a7b579cda44b3496bca4216",
		sigSHA256: "bf4e85c8458741a3c1942741cb25733cfd0eefed753c32dbaf876b1a0fad71ec",
	},
}

const (
	goldenAmount = int64(100_000_000) // 1 ARMX in Armoids
	goldenFee    = int64(100_000)
	goldenNonce  = int64(1)
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// independentSighash re-states the standard-transaction sighash layout from the
// protocol rules instead of calling Transaction.HashForNetwork.
func independentSighash(netID, sender, senderPubHex, recipient string) []byte {
	record := fmt.Sprintf("%s:%d:%s:%s:%s:%d:%d:%d",
		netID, pqsig.SigAlgMLDSA65, sender, senderPubHex, recipient, goldenAmount, goldenFee, goldenNonce)
	h1 := sha256.Sum256([]byte(record))
	h2 := sha256.Sum256(h1[:])
	return h2[:]
}

// TestProtocolGoldenVectors_MnemonicToVerifiedTransaction pins the full mnemonic to
// verified-transaction pipeline for mainnet and testnet. It only reads the active
// network (never mutates it), so it runs without the testhooks build tag.
func TestProtocolGoldenVectors_MnemonicToVerifiedTransaction(t *testing.T) {
	bip39Seed := MnemonicToSeed(goldenMnemonic, goldenPassphrase)
	if got := hex.EncodeToString(bip39Seed); got != goldenBIP39Seed {
		t.Fatalf("BIP-39 seed: got %s, want %s", got, goldenBIP39Seed)
	}

	wallets := make([]*Wallet, len(goldenAccounts))
	for i, acct := range goldenAccounts {
		t.Run("account="+strconv.Itoa(int(acct.index)), func(t *testing.T) {
			w, err := WalletFromMnemonic(goldenMnemonic, goldenPassphrase, acct.index)
			if err != nil {
				t.Fatalf("WalletFromMnemonic: %v", err)
			}
			wallets[i] = w

			// HKDF-SHA256(salt="armax-pqc-v1", info="ml-dsa-65/account/<index>"), recomputed
			// with the standard library instead of the x/crypto path used in production.
			indep, err := hkdf.Key(sha256.New, bip39Seed, []byte("armax-pqc-v1"),
				"ml-dsa-65/account/"+strconv.FormatUint(uint64(acct.index), 10), pqsig.SeedSize)
			if err != nil {
				t.Fatalf("hkdf.Key: %v", err)
			}
			if got := hex.EncodeToString(w.Seed32[:]); got != hex.EncodeToString(indep) {
				t.Fatalf("account seed differs from independent HKDF: %s vs %x", got, indep)
			}
			if got := hex.EncodeToString(w.Seed32[:]); got != acct.accountSeed {
				t.Errorf("account seed: got %s, want %s", got, acct.accountSeed)
			}

			// Key generation through the standard library directly.
			sk, err := mldsa.NewPrivateKey(mldsa.MLDSA65(), indep)
			if err != nil {
				t.Fatalf("mldsa.NewPrivateKey: %v", err)
			}
			pub := sk.PublicKey().Bytes()
			if hex.EncodeToString(pub) != w.PublicKey {
				t.Fatalf("wallet public key differs from crypto/mldsa key generation")
			}
			if len(pub) != pqsig.PublicKeySize {
				t.Fatalf("public key length %d, want %d", len(pub), pqsig.PublicKeySize)
			}
			if got := sha256Hex(pub); got != acct.pubKeySHA256 {
				t.Errorf("SHA-256(pubKey): got %s, want %s", got, acct.pubKeySHA256)
			}

			// Address = Bech32("armx", SHA-256(SigAlg || pubKey)).
			payload := sha256.Sum256(append([]byte{pqsig.SigAlgMLDSA65}, pub...))
			if got := hex.EncodeToString(payload[:]); got != acct.addressSHA256 {
				t.Errorf("address payload: got %s, want %s", got, acct.addressSHA256)
			}
			hrp, decoded, err := core.Bech32Decode(w.Address)
			if err != nil || hrp != "armx" || hex.EncodeToString(decoded) != hex.EncodeToString(payload[:]) {
				t.Fatalf("address %s does not decode to armx/SHA-256(SigAlg||pubKey): hrp=%q err=%v", w.Address, hrp, err)
			}
			if w.Address != acct.address {
				t.Errorf("address: got %s, want %s", w.Address, acct.address)
			}
		})
	}
	if t.Failed() {
		return
	}

	sender, recipient := wallets[0], wallets[1]
	activeNet := core.NetworkID()
	signOpts := &mldsa.Options{Context: "ARMX-TX-V1"}
	if pqsig.ContextString != signOpts.Context {
		t.Fatalf("protocol context string changed: %q", pqsig.ContextString)
	}
	sk, err := mldsa.NewPrivateKey(mldsa.MLDSA65(), sender.Seed32[:])
	if err != nil {
		t.Fatalf("mldsa.NewPrivateKey: %v", err)
	}

	coveredActive := false
	for _, nv := range goldenNetworkVectors {
		t.Run("network="+nv.networkID, func(t *testing.T) {
			tx := core.Transaction{
				Sender:    sender.Address,
				Recipient: recipient.Address,
				Amount:    goldenAmount,
				Fee:       goldenFee,
				Nonce:     goldenNonce,
				SigAlg:    pqsig.SigAlgMLDSA65,
				SenderPub: sender.PublicKey,
			}

			sighash := tx.HashForNetwork(nv.networkID)
			if hex.EncodeToString(sighash) != hex.EncodeToString(independentSighash(nv.networkID, sender.Address, sender.PublicKey, recipient.Address)) {
				t.Fatalf("HashForNetwork differs from the independently stated record layout")
			}
			if got := hex.EncodeToString(sighash); got != nv.sighash {
				t.Errorf("sighash: got %s, want %s", got, nv.sighash)
			}

			sig, err := sk.SignDeterministic(sighash, signOpts)
			if err != nil {
				t.Fatalf("SignDeterministic: %v", err)
			}
			if len(sig) != 3309 || len(sig) != pqsig.SignatureSize {
				t.Fatalf("signature length %d, want 3309 (pqsig.SignatureSize=%d)", len(sig), pqsig.SignatureSize)
			}
			if got := sha256Hex(sig); got != nv.sigSHA256 {
				t.Errorf("SHA-256(signature): got %s, want %s", got, nv.sigSHA256)
			}
			pubBytes, err := hex.DecodeString(sender.PublicKey)
			if err != nil {
				t.Fatalf("decode sender public key: %v", err)
			}
			if !pqsig.Verify(pubBytes, sighash, sig) {
				t.Fatalf("pqsig.Verify rejected the deterministic signature")
			}

			// Consensus verification on the active network: the signature is valid only
			// for the network it was bound to (no cross-network replay).
			tx.Signature = hex.EncodeToString(sig)
			err = tx.VerifyWithError()
			if nv.networkID == activeNet {
				coveredActive = true
				if err != nil {
					t.Fatalf("VerifyWithError on active network %s: %v", activeNet, err)
				}
			} else if err == nil {
				t.Fatalf("signature bound to %s verified on active network %s", nv.networkID, activeNet)
			}
		})
	}
	if !coveredActive {
		t.Fatalf("active network %s has no golden vector", activeNet)
	}
}
