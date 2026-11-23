package wallet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/armaxcoincore/armax-coin/core"
)

func TestWallet_AtomicKeystoreAndBackupWrite(t *testing.T) {
	dir := t.TempDir()
	ksPath := filepath.Join(dir, "wallet.json")

	w1, err := NewWallet()
	if err != nil {
		t.Fatalf("NewWallet w1 failed: %v", err)
	}
	defer w1.Zeroize()

	if err := EncryptAndSaveKeystore(w1, "Str0ng!Pass1", ksPath); err != nil {
		t.Fatalf("EncryptAndSaveKeystore failed: %v", err)
	}

	loaded1, err := DecryptAndLoadKeystore("Str0ng!Pass1", ksPath)
	if err != nil {
		t.Fatalf("DecryptAndLoadKeystore failed: %v", err)
	}
	defer loaded1.Zeroize()
	if loaded1.Address != w1.Address {
		t.Fatalf("expected address %s, got %s", w1.Address, loaded1.Address)
	}

	w2, err := NewWallet()
	if err != nil {
		t.Fatalf("NewWallet w2 failed: %v", err)
	}
	defer w2.Zeroize()

	bakPath, err := CreateNewWalletKeystore(w2, "Str0ng!Pass2", ksPath, false)
	if err != nil {
		t.Fatalf("CreateNewWalletKeystore failed: %v", err)
	}
	if bakPath == "" {
		t.Fatalf("expected non-empty backup path when overwriting existing keystore")
	}

	loadedBak, err := DecryptAndLoadKeystore("Str0ng!Pass1", bakPath)
	if err != nil {
		t.Fatalf("DecryptAndLoadKeystore backup failed: %v", err)
	}
	defer loadedBak.Zeroize()
	if loadedBak.Address != w1.Address {
		t.Fatalf("backup mismatch: expected %s, got %s", w1.Address, loadedBak.Address)
	}

	loaded2, err := DecryptAndLoadKeystore("Str0ng!Pass2", ksPath)
	if err != nil {
		t.Fatalf("DecryptAndLoadKeystore overwritten failed: %v", err)
	}
	defer loaded2.Zeroize()
	if loaded2.Address != w2.Address {
		t.Fatalf("overwritten wallet mismatch: expected %s, got %s", w2.Address, loaded2.Address)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("unexpected leftover temporary file after atomic write: %s", entry.Name())
		}
	}
}

func TestMultiWallet_ImportKeystoreJSONValidation(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "export.json")

	w, err := NewWallet()
	if err != nil {
		t.Fatalf("NewWallet failed: %v", err)
	}
	defer w.Zeroize()

	if err := EncryptAndSaveKeystore(w, "ExportPass!99", srcPath); err != nil {
		t.Fatalf("EncryptAndSaveKeystore failed: %v", err)
	}
	validRaw, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	var baseKS KeystoreFile
	if err := json.Unmarshal(validRaw, &baseKS); err != nil {
		t.Fatalf("Unmarshal valid keystore failed: %v", err)
	}

	other, err := NewWallet()
	if err != nil {
		t.Fatalf("NewWallet other failed: %v", err)
	}
	defer other.Zeroize()

	wmDir := t.TempDir()
	wm := NewWalletManagerForDir(wmDir)

	mustMarshal := func(ks KeystoreFile) []byte {
		b, mErr := json.Marshal(ks)
		if mErr != nil {
			t.Fatalf("Marshal failed: %v", mErr)
		}
		return b
	}

	ksWrongVer := baseKS
	ksWrongVer.Version = KeystoreVersion + 1

	ksWrongCipher := baseKS
	ksWrongCipher.Crypto.Cipher = "chacha20-poly1305"

	ksWrongKDF := baseKS
	ksWrongKDF.Crypto.KDF = "pbkdf2"

	ksShortNonce := baseKS
	ksShortNonce.Crypto.Nonce = "abcd"

	ksMismatchAddr := baseKS
	ksMismatchAddr.Address = other.Address

	oversized := make([]byte, MaxKeystoreFileSize+1)
	for i := range oversized {
		oversized[i] = ' '
	}

	invalidCases := []struct {
		name string
		raw  []byte
	}{
		{"invalid_json", []byte(`{"version":`)},
		{"unsupported_version", mustMarshal(ksWrongVer)},
		{"unsupported_cipher", mustMarshal(ksWrongCipher)},
		{"unsupported_kdf", mustMarshal(ksWrongKDF)},
		{"short_nonce", mustMarshal(ksShortNonce)},
		{"mismatched_address_pubkey", mustMarshal(ksMismatchAddr)},
		{"oversized_payload", oversized},
	}

	for _, tc := range invalidCases {
		if _, err := wm.ImportKeystoreJSON("bad-"+tc.name, tc.raw); err == nil {
			t.Fatalf("ImportKeystoreJSON(%s) should have failed, but succeeded", tc.name)
		}
	}

	entry, err := wm.ImportKeystoreJSON("valid-wallet", validRaw)
	if err != nil {
		t.Fatalf("ImportKeystoreJSON valid failed: %v", err)
	}
	if entry.Address != w.Address {
		t.Fatalf("imported address mismatch: expected %s, got %s", w.Address, entry.Address)
	}
}

func TestWallet_LegacyPassphraseCheckAndMnemonicValidation(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "legacy.json")
	modernPath := filepath.Join(dir, "modern.json")

	w, err := NewWallet()
	if err != nil {
		t.Fatalf("NewWallet failed: %v", err)
	}
	defer w.Zeroize()

	if err := EncryptAndSaveKeystore(w, GetLegacyDefaultPassphrase(), legacyPath); err != nil {
		t.Fatalf("EncryptAndSaveKeystore legacy failed: %v", err)
	}
	if err := EncryptAndSaveKeystore(w, "NonLegacy!Passphrase2026", modernPath); err != nil {
		t.Fatalf("EncryptAndSaveKeystore modern failed: %v", err)
	}

	if !IsKeystoreUsingLegacyPassphrase(legacyPath) {
		t.Fatalf("expected legacy keystore check to return true")
	}
	if IsKeystoreUsingLegacyPassphrase(modernPath) {
		t.Fatalf("expected modern keystore check to return false")
	}
	if IsKeystoreUsingLegacyPassphrase(filepath.Join(dir, "missing.json")) {
		t.Fatalf("expected missing keystore check to return false")
	}

	phrase, err := GenerateMnemonic()
	if err != nil {
		t.Fatalf("GenerateMnemonic failed: %v", err)
	}
	if wc, ok := ValidateMnemonic(phrase); !ok || wc == 0 {
		t.Fatalf("ValidateMnemonic rejected freshly generated mnemonic (words=%d, ok=%v)", wc, ok)
	}
	if _, ok := ValidateMnemonic(phrase + " abandon"); ok {
		t.Fatalf("ValidateMnemonic accepted malformed word count")
	}
}

func TestWallet_BinaryWeightFeeEstimator(t *testing.T) {
	core.SetMempoolBinaryFeeActivationHeight(-1)
	defer core.ResetMempoolBinaryFeeActivationHeightOverride()

	w, err := NewWallet()
	if err != nil {
		t.Fatalf("NewWallet failed: %v", err)
	}
	defer w.Zeroize()

	rW, err := NewWallet()
	if err != nil {
		t.Fatalf("NewWallet recipient failed: %v", err)
	}
	defer rW.Zeroize()

	actHeight := int(core.TestnetProfile.BlockBinarySizeActivationHeight)
	if TypicalTxEffectiveSize(0) <= 9000 || TypicalTxEffectiveSize(actHeight) <= 9000 {
		t.Fatalf("expected Testnet (-1 mempool binary fee) to keep JSON size > 9000 for v1.0.12 compat, got h0=%d hAct=%d",
			TypicalTxEffectiveSize(0), TypicalTxEffectiveSize(actHeight))
	}

	legacyFee := EstimateMinMempoolFee(w, rW.Address, core.CoinUnit, 1)
	height0Fee := EstimateMinMempoolFeeAtHeight(w, rW.Address, core.CoinUnit, 1, 0)
	if legacyFee != height0Fee || legacyFee < 11000 {
		t.Fatalf("expected backwards-compatible height-0 fee >= 11000 matching legacy %d, got %d", legacyFee, height0Fee)
	}

	// Activate binary mempool fee at actHeight (and genesis #0 for Mainnet parity).
	core.SetMempoolBinaryFeeActivationHeight(int64(actHeight))
	if binEst := TypicalTxEffectiveSize(actHeight); binEst <= 0 || binEst > 6000 {
		t.Fatalf("expected activation height %d binary effective size in (0, 6000], got %d", actHeight, binEst)
	}
	binaryFee := EstimateMinMempoolFeeAtHeight(w, rW.Address, core.CoinUnit, 1, actHeight)
	if binaryFee != 6000 {
		t.Fatalf("expected TxBinaryV1 fee at activation height %d to equal 6000 Armoids, got %d", actHeight, binaryFee)
	}

	// Mainnet genesis parity (binary fee active from block #0).
	core.SetMempoolBinaryFeeActivationHeight(0)
	if binAt0 := TypicalTxEffectiveSize(0); binAt0 <= 0 || binAt0 > 6000 {
		t.Fatalf("expected genesis binary effective size in (0, 6000], got %d", binAt0)
	}
	if mainFee := EstimateMinMempoolFeeAtHeight(w, rW.Address, core.CoinUnit, 1, 0); mainFee != 6000 {
		t.Fatalf("expected TxBinaryV1 fee at genesis #0 activation to equal 6000 Armoids, got %d", mainFee)
	}
}
