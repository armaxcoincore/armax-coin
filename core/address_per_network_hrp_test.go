//go:build testhooks

package core

import (
	"strings"
	"testing"

	"github.com/armaxcoincore/armax-coin/core/pqsig"
)

// perNetworkHRPCases lists every built-in network with its expected address prefix and burn address.
var perNetworkHRPCases = []struct {
	name   string
	netID  string
	hrp    string
	burner string
}{
	{"mainnet", "ARMX-MAINNET-1", "ax", "ax1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqyum580"},
	{"testnet", "ARMX-TESTNET-1", "armx", "armx1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqplwcdd"},
	{"regtest", "ARMX-REGTEST-1", "axrt", "axrt1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqa862k7"},
	{"soak", "ARMX-SOAK-1", "axsk", "axsk1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqpactgh"},
}

// TestAddressHRP_ProfilesAndBurnAddresses verifies each profile carries its prefix and that every
// Genesis burn address is the zero payload encoded with that prefix.
func TestAddressHRP_ProfilesAndBurnAddresses(t *testing.T) {
	profiles := map[string]NetworkProfile{
		"ARMX-MAINNET-1": MainnetProfile,
		"ARMX-TESTNET-1": TestnetProfile,
		"ARMX-REGTEST-1": RegtestProfile,
		"ARMX-SOAK-1":    SoakProfile,
	}
	for _, tc := range perNetworkHRPCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := profiles[tc.netID].AddressHRP; got != tc.hrp {
				t.Fatalf("profile AddressHRP = %q, want %q", got, tc.hrp)
			}
			if got, ok := AddressHRPForNetwork(tc.netID); !ok || got != tc.hrp {
				t.Fatalf("AddressHRPForNetwork(%s) = %q,%v, want %q", tc.netID, got, ok, tc.hrp)
			}
			if got := GenesisMinerForNetwork(tc.netID); got != tc.burner {
				t.Fatalf("GenesisMinerForNetwork = %s, want %s", got, tc.burner)
			}
			hrp, payload, err := Bech32Decode(tc.burner)
			if err != nil || hrp != tc.hrp || len(payload) != AddressPayloadLen {
				t.Fatalf("burn address decode: hrp=%q len=%d err=%v", hrp, len(payload), err)
			}
			for _, b := range payload {
				if b != 0 {
					t.Fatalf("burn address payload is not all zero")
				}
			}
			if got, ok := NetworkIDForAddress(tc.burner); !ok || got != tc.netID {
				t.Fatalf("NetworkIDForAddress = %q,%v, want %s", got, ok, tc.netID)
			}
		})
	}
	if _, ok := AddressHRPForNetwork("ARMX-UNKNOWN-9"); ok {
		t.Fatalf("unknown network id must not resolve to an address prefix")
	}
}

// TestAddressHRP_CrossNetworkRejection verifies an address is accepted only by its own network,
// both through the explicit-network API and through the active profile.
func TestAddressHRP_CrossNetworkRejection(t *testing.T) {
	LockNetworkForTesting(t)
	SetNetworkForTesting(true)
	t.Cleanup(func() { SetNetworkForTesting(false) })

	sk, _, err := pqsig.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey failed: %v", err)
	}
	pub := sk.PublicKeyBytes()

	addrs := map[string]string{}
	for _, tc := range perNetworkHRPCases {
		addr, err := PubToAddressForNetwork(pqsig.SigAlgMLDSA65, pub, tc.netID)
		if err != nil {
			t.Fatalf("PubToAddressForNetwork(%s): %v", tc.netID, err)
		}
		if !strings.HasPrefix(addr, tc.hrp+"1") {
			t.Fatalf("address %s lacks prefix %s1", addr, tc.hrp)
		}
		addrs[tc.netID] = addr
	}

	// Same key, same payload: only the prefix differs between networks.
	wantHex, err := AddressToHexForNetwork(addrs["ARMX-TESTNET-1"], "ARMX-TESTNET-1")
	if err != nil {
		t.Fatalf("AddressToHexForNetwork testnet: %v", err)
	}
	for netID, addr := range addrs {
		gotHex, err := AddressToHexForNetwork(addr, netID)
		if err != nil || gotHex != wantHex {
			t.Fatalf("%s payload mismatch: %s err=%v", netID, gotHex, err)
		}
	}

	for _, active := range perNetworkHRPCases {
		t.Run("active_"+active.name, func(t *testing.T) {
			if err := SetNetwork(active.name); err != nil {
				t.Fatalf("SetNetwork(%s): %v", active.name, err)
			}
			if got := ActiveAddressHRP(); got != active.hrp {
				t.Fatalf("ActiveAddressHRP = %q, want %q", got, active.hrp)
			}
			for netID, addr := range addrs {
				own := netID == active.netID
				if ValidateAddress(addr) != own {
					t.Fatalf("ValidateAddress(%s) on %s = %v, want %v", addr, active.netID, !own, own)
				}
				if ValidateAddressForNetwork(addr, active.netID) != own {
					t.Fatalf("ValidateAddressForNetwork(%s, %s) wrong", addr, active.netID)
				}
				_, hexErr := AddressToHexCached(addr)
				if (hexErr == nil) != own {
					t.Fatalf("AddressToHexCached(%s) on %s err=%v, want accepted=%v", addr, active.netID, hexErr, own)
				}
				if !own && !strings.Contains(hexErr.Error(), netID) {
					t.Fatalf("cross-network error should name %s, got: %v", netID, hexErr)
				}
			}
			derived, err := PubToAddress(pqsig.SigAlgMLDSA65, pub)
			if err != nil || derived != addrs[active.netID] {
				t.Fatalf("PubToAddress on %s = %s err=%v, want %s", active.netID, derived, err, addrs[active.netID])
			}
		})
	}
}

// TestAddressHRP_ChecksumCoversPrefix verifies that swapping a prefix by hand (without re-encoding)
// is caught by the Bech32 checksum, so a mistyped network prefix can never decode as a valid address.
func TestAddressHRP_ChecksumCoversPrefix(t *testing.T) {
	mainnet := MainnetGenesisMiner
	forged := "axrt1" + strings.TrimPrefix(mainnet, "ax1")
	if _, _, err := Bech32Decode(forged); err == nil {
		t.Fatalf("prefix swap %s must fail the Bech32 checksum", forged)
	}
	for _, tc := range perNetworkHRPCases {
		if ValidateAddressForNetwork(forged, tc.netID) {
			t.Fatalf("forged prefix accepted on %s", tc.netID)
		}
	}
}

// TestAddressHRP_GenesisBlocksValid verifies every network's Genesis block validates and that
// Testnet Genesis is byte-identical to the deployed constants.
func TestAddressHRP_GenesisBlocksValid(t *testing.T) {
	LockNetworkForTesting(t)
	SetNetworkForTesting(true)
	t.Cleanup(func() { SetNetworkForTesting(false) })

	for _, tc := range perNetworkHRPCases {
		t.Run(tc.name, func(t *testing.T) {
			if err := SetNetwork(tc.name); err != nil {
				t.Fatalf("SetNetwork(%s): %v", tc.name, err)
			}
			gen := GetGenesisBlockForNetwork(tc.netID)
			if gen.Miner != tc.burner || gen.Transactions[0].Recipient != tc.burner {
				t.Fatalf("genesis miner/recipient = %s/%s, want %s", gen.Miner, gen.Transactions[0].Recipient, tc.burner)
			}
			if err := ValidateGenesisBlockForNetwork(&gen, tc.netID); err != nil {
				t.Fatalf("ValidateGenesisBlockForNetwork: %v", err)
			}
			if gen.CalculateSha256dHash() != gen.Hash {
				t.Fatalf("genesis hash does not recompute")
			}
			if gen.Hash > gen.Target {
				t.Fatalf("genesis hash %s above target %s", gen.Hash, gen.Target)
			}
		})
	}

	testnet := GetTestnetGenesisBlock()
	if testnet.Hash != "0000000dc84202e63a340a2439625fbf9cea5cb7b84119f93d52cff39f63a68b" ||
		testnet.MerkleRoot != "808a4007681dd123133bfe106b101d251ff0d2d023f600384394673bfc03768d" ||
		testnet.Nonce != 40272237 {
		t.Fatalf("Testnet Genesis changed: hash=%s merkle=%s nonce=%d", testnet.Hash, testnet.MerkleRoot, testnet.Nonce)
	}
}

// TestAddressHRP_BinaryRoundTripPerNetwork verifies TxBinaryV1 compact recipient encoding keeps
// working for each network's own prefix.
func TestAddressHRP_BinaryRoundTripPerNetwork(t *testing.T) {
	LockNetworkForTesting(t)
	SetNetworkForTesting(true)
	t.Cleanup(func() { SetNetworkForTesting(false) })

	for _, tc := range perNetworkHRPCases {
		t.Run(tc.name, func(t *testing.T) {
			if err := SetNetwork(tc.name); err != nil {
				t.Fatalf("SetNetwork(%s): %v", tc.name, err)
			}
			tx := Transaction{
				Sender:    "SYSTEM",
				Recipient: tc.burner,
				Amount:    InitialReward,
				Nonce:     1,
				Signature: "REWARD_SIG",
			}
			raw, err := tx.MarshalBinary()
			if err != nil {
				t.Fatalf("MarshalBinary: %v", err)
			}
			var back Transaction
			if err := back.UnmarshalBinary(raw); err != nil {
				t.Fatalf("UnmarshalBinary: %v", err)
			}
			if back.Recipient != tc.burner {
				t.Fatalf("round-trip recipient = %s, want %s", back.Recipient, tc.burner)
			}
		})
	}
}
