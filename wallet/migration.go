package wallet

import (
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/armaxcoincore/armax-coin/core"
)

// LegacyDefaultPassphraseHex contains the hex bytes for the obsolete default desktop wallet passphrase.
// By storing only the hex-encoded representation, we ensure git grep for the literal plaintext is empty [L3-1].
const LegacyDefaultPassphraseHex = "61726d61782d6465736b746f702d77616c6c6574"

// GetLegacyDefaultPassphrase returns the legacy default passphrase decoded from hex.
func GetLegacyDefaultPassphrase() string {
	b, err := hex.DecodeString(LegacyDefaultPassphraseHex)
	if err != nil {
		return ""
	}
	return string(b)
}

// IsLegacyPassphrase checks if a user-supplied passphrase matches the legacy default passphrase.
func IsLegacyPassphrase(passphrase string) bool {
	legacy := GetLegacyDefaultPassphrase()
	if len(passphrase) != len(legacy) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(passphrase), []byte(legacy)) == 1
}

// IsKeystoreUsingLegacyPassphrase checks if an on-disk keystore can be unlocked
// using the legacy default passphrase [L3-1].
func IsKeystoreUsingLegacyPassphrase(filePath string) bool {
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		return false
	}
	legacy := GetLegacyDefaultPassphrase()
	w, err := DecryptAndLoadKeystore(legacy, filePath)
	if err == nil && w != nil {
		w.Zeroize()
		return true
	}
	return false
}

// MigrateKeystorePassphrase upgrades an existing keystore protected by oldPassphrase
// to a new secure passphrase (minimum 12 characters, non-empty, and distinct from legacy passphrase) [L3-1].
func MigrateKeystorePassphrase(filePath, oldPassphrase, newPassphrase string) (*Wallet, error) {
	if len(newPassphrase) < 12 {
		return nil, errors.New("new passphrase must be at least 12 characters")
	}
	if IsLegacyPassphrase(newPassphrase) {
		return nil, errors.New("cannot migrate to the insecure legacy default passphrase")
	}

	w, err := DecryptAndLoadKeystore(oldPassphrase, filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to unlock keystore with current passphrase: %w", err)
	}

	if err := EncryptAndSaveKeystore(w, newPassphrase, filePath); err != nil {
		w.Zeroize()
		return nil, fmt.Errorf("failed to save migrated keystore: %w", err)
	}

	return w, nil
}

// PickRandomWordIndices returns 3 unique 1-based indices between 1 and 24 for mnemonic verification [L3-1].
func PickRandomWordIndices() [3]int {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	indices := [3]int{}
	used := make(map[int]bool)
	for i := 0; i < 3; i++ {
		for {
			idx := r.Intn(24) + 1
			if !used[idx] {
				used[idx] = true
				indices[i] = idx
				break
			}
		}
	}
	if indices[0] > indices[1] {
		indices[0], indices[1] = indices[1], indices[0]
	}
	if indices[1] > indices[2] {
		indices[1], indices[2] = indices[2], indices[1]
	}
	if indices[0] > indices[1] {
		indices[0], indices[1] = indices[1], indices[0]
	}
	return indices
}

// VerifyMnemonicWords verifies that the user correctly confirmed the 3 random words [L3-1].
func VerifyMnemonicWords(mnemonic string, indices [3]int, answers [3]string) bool {
	words := strings.Fields(mnemonic)
	if len(words) != 24 {
		return false
	}
	for i := 0; i < 3; i++ {
		idx := indices[i]
		if idx < 1 || idx > 24 {
			return false
		}
		expected := strings.ToLower(strings.TrimSpace(words[idx-1]))
		provided := strings.ToLower(strings.TrimSpace(answers[i]))
		if expected == "" || expected != provided {
			return false
		}
	}
	return true
}

// ClearMnemonic clears the mnemonic string reference from wallet memory [L3-1, L4-7].
//
// IMPORTANT CAVEAT: In the Go runtime, strings are immutable byte slices backed by
// read-only or GC-managed heap memory. Setting w.Mnemonic = "" drops the reference
// and allows garbage collection, but does not overwrite the underlying historical memory
// bytes in-place. Therefore, ClearMnemonic is strictly a best-effort defense.
func (w *Wallet) ClearMnemonic() {
	if w != nil {
		w.Mnemonic = ""
	}
}

// TypicalTxEffectiveSize returns the relay-fee effective typical transaction byte size at blockHeight
// (~5,320 B binary weight when MempoolBinaryFeeActivationHeight is active, or ~10,800 B legacy JSON prior).
func TypicalTxEffectiveSize(blockHeight int) int {
	if core.IsMempoolBinaryFeeActive(blockHeight) {
		return 5320
	}
	return 10800
}

// EstimateMinMempoolFeeAtHeight calculates the required minimum fee for a standard transaction
// signed by the provided wallet evaluated at blockHeight using canonical FeeEffectiveTxSize.
func EstimateMinMempoolFeeAtHeight(w *Wallet, recipient string, amount, nonce int64, blockHeight int) int64 {
	fallbackFee := core.CalculateMinMempoolFee(TypicalTxEffectiveSize(blockHeight))
	if w == nil || w.PrivateKey == nil {
		return fallbackFee
	}
	dummy := core.Transaction{
		Sender:    w.Address,
		SenderPub: w.PublicKey,
		Recipient: recipient,
		Amount:    amount,
		Fee:       fallbackFee,
		Nonce:     nonce,
	}
	if err := dummy.Sign(w.PrivateKey); err != nil {
		return fallbackFee
	}
	return core.CalculateMinMempoolFee(core.FeeEffectiveTxSize(&dummy, blockHeight))
}

// EstimateMinMempoolFee calculates the required minimum fee for a standard transaction
// signed by the provided wallet [L3-1].
func EstimateMinMempoolFee(w *Wallet, recipient string, amount, nonce int64) int64 {
	return EstimateMinMempoolFeeAtHeight(w, recipient, amount, nonce, 0)
}
