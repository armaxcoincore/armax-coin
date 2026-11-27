//go:build testhooks

package wallet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/armaxcoincore/armax-coin/core"
)

const networkGuardPass = "DisposableNetworkGuard#2026"

// writeTestnetKeystore creates a keystore on Testnet and returns its path and wallet address.
func writeTestnetKeystore(t *testing.T) (string, string) {
	t.Helper()
	if err := core.SetNetwork("testnet"); err != nil {
		t.Fatalf("SetNetwork(testnet): %v", err)
	}
	w, err := NewWallet()
	if err != nil {
		t.Fatalf("NewWallet: %v", err)
	}
	defer w.Zeroize()
	path := filepath.Join(t.TempDir(), "guard.json")
	if err := EncryptAndSaveKeystore(w, networkGuardPass, path); err != nil {
		t.Fatalf("EncryptAndSaveKeystore: %v", err)
	}
	return path, w.Address
}

// rewriteKeystoreField sets (or deletes, when value is nil) a top-level keystore JSON field.
func rewriteKeystoreField(t *testing.T, path, field string, value any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read keystore: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse keystore: %v", err)
	}
	if value == nil {
		delete(doc, field)
	} else {
		doc[field] = value
	}
	out, _ := json.Marshal(doc)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write keystore: %v", err)
	}
}

// TestKeystoreNetworkGuard_TagAndRefusal verifies a keystore records its network and is refused on
// another network before the passphrase is even tried.
func TestKeystoreNetworkGuard_TagAndRefusal(t *testing.T) {
	core.LockNetworkForTesting(t)
	core.SetNetworkForTesting(true)
	t.Cleanup(func() { core.SetNetworkForTesting(false) })

	path, addr := writeTestnetKeystore(t)
	raw, _ := os.ReadFile(path)
	var ks KeystoreFile
	if err := json.Unmarshal(raw, &ks); err != nil || ks.Network != "ARMX-TESTNET-1" {
		t.Fatalf("keystore network tag = %q (err %v), want ARMX-TESTNET-1", ks.Network, err)
	}

	// Same network: unlocks and derives the same address.
	w, err := DecryptAndLoadKeystore(networkGuardPass, path)
	if err != nil || w.Address != addr {
		t.Fatalf("testnet unlock: addr=%v err=%v", w, err)
	}
	w.Zeroize()

	// Other network: refused before decryption, so even a wrong passphrase reports the network.
	if err := core.SetNetwork("mainnet"); err != nil {
		t.Fatalf("SetNetwork(mainnet): %v", err)
	}
	_, err = DecryptAndLoadKeystore("wrong-passphrase", path)
	if err == nil || !strings.Contains(err.Error(), "belongs to network ARMX-TESTNET-1") {
		t.Fatalf("expected network refusal before decryption, got %v", err)
	}
}

// TestKeystoreNetworkGuard_LegacyUntagged verifies keystores written before the network tag are
// still Testnet keystores: they unlock on Testnet and are refused elsewhere via the address prefix.
func TestKeystoreNetworkGuard_LegacyUntagged(t *testing.T) {
	core.LockNetworkForTesting(t)
	core.SetNetworkForTesting(true)
	t.Cleanup(func() { core.SetNetworkForTesting(false) })

	path, addr := writeTestnetKeystore(t)
	rewriteKeystoreField(t, path, "network", nil)

	w, err := DecryptAndLoadKeystore(networkGuardPass, path)
	if err != nil || w.Address != addr {
		t.Fatalf("legacy testnet unlock: err=%v", err)
	}
	w.Zeroize()

	if err := core.SetNetwork("regtest"); err != nil {
		t.Fatalf("SetNetwork(regtest): %v", err)
	}
	if _, err := DecryptAndLoadKeystore(networkGuardPass, path); err == nil || !strings.Contains(err.Error(), "belongs to network ARMX-TESTNET-1") {
		t.Fatalf("expected legacy keystore refusal on regtest, got %v", err)
	}
}

// TestKeystoreNetworkGuard_ExplicitOverride verifies the recovery override unlocks a foreign
// keystore with the active network's address for the same key, and still checks key binding.
func TestKeystoreNetworkGuard_ExplicitOverride(t *testing.T) {
	core.LockNetworkForTesting(t)
	core.SetNetworkForTesting(true)
	t.Cleanup(func() { core.SetNetworkForTesting(false) })

	path, addr := writeTestnetKeystore(t)
	wantHex, err := core.AddressToHexForNetwork(addr, "ARMX-TESTNET-1")
	if err != nil {
		t.Fatalf("AddressToHexForNetwork: %v", err)
	}

	if err := core.SetNetwork("mainnet"); err != nil {
		t.Fatalf("SetNetwork(mainnet): %v", err)
	}
	t.Setenv(AllowForeignNetworkKeystoreEnv, "1")
	w, err := DecryptAndLoadKeystore(networkGuardPass, path)
	if err != nil {
		t.Fatalf("override unlock failed: %v", err)
	}
	defer w.Zeroize()
	if !strings.HasPrefix(w.Address, core.MainnetAddressHRP+"1") {
		t.Fatalf("override wallet address %s should use the Mainnet prefix", w.Address)
	}
	if gotHex, err := core.AddressToHex(w.Address); err != nil || gotHex != wantHex {
		t.Fatalf("override wallet payload mismatch: %s err=%v", gotHex, err)
	}
}

// TestKeystoreNetworkGuard_TagMustMatchAddress verifies a network tag that contradicts the stored
// address prefix is treated as tampering.
func TestKeystoreNetworkGuard_TagMustMatchAddress(t *testing.T) {
	core.LockNetworkForTesting(t)
	core.SetNetworkForTesting(true)
	t.Cleanup(func() { core.SetNetworkForTesting(false) })

	path, _ := writeTestnetKeystore(t)
	rewriteKeystoreField(t, path, "network", "ARMX-MAINNET-1")
	if _, err := DecryptAndLoadKeystore(networkGuardPass, path); err == nil || !strings.Contains(err.Error(), "integrity failure") {
		t.Fatalf("expected integrity failure for contradictory network tag, got %v", err)
	}
	rewriteKeystoreField(t, path, "network", "ARMX-UNKNOWN-9")
	if _, err := DecryptAndLoadKeystore(networkGuardPass, path); err == nil || !strings.Contains(err.Error(), "unknown network") {
		t.Fatalf("expected unknown network rejection, got %v", err)
	}
}
