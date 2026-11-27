package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
)

const (
	// MainnetAddressHRP is the Bech32 human-readable prefix for ARMX-MAINNET-1 addresses ("ax1...").
	MainnetAddressHRP = "ax"
	// TestnetAddressHRP is the Bech32 human-readable prefix for ARMX-TESTNET-1 addresses ("armx1...").
	// It is frozen: Testnet address strings are bound into Testnet transaction hashes and Genesis.
	TestnetAddressHRP = "armx"
	// RegtestAddressHRP is the Bech32 human-readable prefix for ARMX-REGTEST-1 addresses ("axrt1...").
	RegtestAddressHRP = "axrt"
	// SoakAddressHRP is the Bech32 human-readable prefix for ARMX-SOAK-1 addresses ("axsk1...").
	SoakAddressHRP = "axsk"

	// Bech32HRP is the Testnet address prefix, kept for callers that predate per-network prefixes.
	// Consensus and wallet paths must use ActiveAddressHRP or AddressHRPForNetwork instead.
	Bech32HRP = TestnetAddressHRP

	// AddressPayloadLen is the exact byte length of the address preimage hash (SHA-256 = 32 bytes) [P2-2]
	AddressPayloadLen = 32

	bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
)

var bech32Gen = []int{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}

func bech32Polymod(values []int) int {
	chk := 1
	for _, v := range values {
		b := chk >> 25
		chk = ((chk & 0x1ffffff) << 5) ^ v
		for i := 0; i < 5; i++ {
			if (b>>i)&1 == 1 {
				chk ^= bech32Gen[i]
			}
		}
	}
	return chk
}

func bech32HrpExpand(hrp string) []int {
	var ret []int
	for _, c := range hrp {
		ret = append(ret, int(c>>5))
	}
	ret = append(ret, 0)
	for _, c := range hrp {
		ret = append(ret, int(c&31))
	}
	return ret
}

func bech32VerifyChecksum(hrp string, data []int) bool {
	return bech32Polymod(append(bech32HrpExpand(hrp), data...)) == 1
}

func bech32CreateChecksum(hrp string, data []int) []int {
	values := append(bech32HrpExpand(hrp), data...)
	values = append(values, []int{0, 0, 0, 0, 0, 0}...)
	mod := bech32Polymod(values) ^ 1
	var ret []int
	for i := 0; i < 6; i++ {
		ret = append(ret, (mod>>uint(5*(5-i)))&31)
	}
	return ret
}

func convertBits(data []byte, frombits, tobits uint, pad bool) ([]int, error) {
	acc := 0
	bits := uint(0)
	var ret []int
	maxv := (1 << tobits) - 1
	for _, value := range data {
		acc = (acc << frombits) | int(value)
		bits += frombits
		for bits >= tobits {
			bits -= tobits
			ret = append(ret, (acc>>bits)&maxv)
		}
	}
	if pad {
		if bits > 0 {
			ret = append(ret, (acc<<(tobits-bits))&maxv)
		}
	} else if bits >= frombits || ((acc<<(tobits-bits))&maxv) != 0 {
		return nil, fmt.Errorf("invalid bits padding")
	}
	return ret, nil
}

func convertBitsFrom5(data []int, frombits, tobits uint, pad bool) ([]byte, error) {
	acc := 0
	bits := uint(0)
	var ret []byte
	maxv := (1 << tobits) - 1
	for _, value := range data {
		if value < 0 || (value>>frombits) != 0 {
			return nil, fmt.Errorf("invalid data value")
		}
		acc = (acc << frombits) | value
		bits += frombits
		for bits >= tobits {
			bits -= tobits
			ret = append(ret, byte((acc>>bits)&maxv))
		}
	}
	if pad {
		if bits > 0 {
			ret = append(ret, byte((acc<<(tobits-bits))&maxv))
		}
	} else if bits >= frombits || ((acc<<(tobits-bits))&maxv) != 0 {
		return nil, fmt.Errorf("invalid bits padding")
	}
	return ret, nil
}

// Bech32Encode encodes a byte payload into a BIP-173 Bech32 address with the specified HRP
func Bech32Encode(hrp string, payload []byte) (string, error) {
	data, err := convertBits(payload, 8, 5, true)
	if err != nil {
		return "", err
	}
	combined := append(data, bech32CreateChecksum(hrp, data)...)
	var sb strings.Builder
	sb.WriteString(strings.ToLower(hrp))
	sb.WriteString("1")
	for _, d := range combined {
		sb.WriteByte(bech32Charset[d])
	}
	return sb.String(), nil
}

// Bech32Decode decodes and validates the checksum of a BIP-173 Bech32 address.
// Rejects uppercase and mixed-case strings [C-20].
func Bech32Decode(bech string) (string, []byte, error) {
	if bech != strings.ToLower(bech) {
		return "", nil, fmt.Errorf("bech32 address must be strictly lowercase")
	}
	pos := strings.LastIndex(bech, "1")
	if pos < 1 || pos+7 > len(bech) {
		return "", nil, fmt.Errorf("invalid bech32 separator or length")
	}

	hrp := bech[:pos]
	dataStr := bech[pos+1:]

	var data []int
	for _, c := range dataStr {
		d := strings.IndexRune(bech32Charset, c)
		if d == -1 {
			return "", nil, fmt.Errorf("invalid bech32 character: %c", c)
		}
		data = append(data, d)
	}

	if !bech32VerifyChecksum(hrp, data) {
		return "", nil, fmt.Errorf("invalid bech32 checksum (typo detected)")
	}

	payload5 := data[:len(data)-6]
	decoded, err := convertBitsFrom5(payload5, 5, 8, false)
	if err != nil {
		return "", nil, err
	}

	return hrp, decoded, nil
}

// defaultAddressHRP maps a canonical network ID to its built-in address prefix.
func defaultAddressHRP(netID string) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(netID)) {
	case "ARMX-MAINNET-1", "MAINNET":
		return MainnetAddressHRP, true
	case "ARMX-TESTNET-1", "TESTNET":
		return TestnetAddressHRP, true
	case "ARMX-REGTEST-1", "REGTEST":
		return RegtestAddressHRP, true
	case "ARMX-SOAK-1", "SOAK":
		return SoakAddressHRP, true
	}
	return "", false
}

// ActiveAddressHRP returns the Bech32 address prefix of the active network profile.
// A profile without an explicit prefix falls back to the built-in prefix for its network ID,
// and finally to the Testnet prefix, so Testnet behavior is unchanged.
func ActiveAddressHRP() string {
	prof := GetActiveNetwork()
	if prof.AddressHRP != "" {
		return prof.AddressHRP
	}
	if hrp, ok := defaultAddressHRP(prof.NetworkID); ok {
		return hrp
	}
	return TestnetAddressHRP
}

// AddressHRPForNetwork returns the Bech32 address prefix for a network ID. An empty ID means the
// active network. The boolean is false for unknown network IDs so callers can fail closed.
func AddressHRPForNetwork(netID string) (string, bool) {
	if strings.TrimSpace(netID) == "" {
		return ActiveAddressHRP(), true
	}
	if hrp, ok := defaultAddressHRP(netID); ok {
		return hrp, true
	}
	return "", false
}

// NetworkIDForAddress reports which known network an address prefix belongs to. It only inspects
// the prefix and checksum, so it can explain cross-network mistakes ("this is a Testnet address")
// without accepting the address for the active network.
func NetworkIDForAddress(address string) (string, bool) {
	hrp, payload, err := Bech32Decode(address)
	if err != nil || len(payload) != AddressPayloadLen {
		return "", false
	}
	for _, netID := range []string{"ARMX-MAINNET-1", "ARMX-TESTNET-1", "ARMX-REGTEST-1", "ARMX-SOAK-1"} {
		if want, _ := defaultAddressHRP(netID); hrp == want {
			return netID, true
		}
	}
	return "", false
}

// validateAddressWithHRP is the shared strict check behind ValidateAddress and ValidateAddressForNetwork.
func validateAddressWithHRP(address, wantHRP string) bool {
	if wantHRP == "" || address == "" || strings.TrimSpace(address) != address || address != strings.ToLower(address) {
		return false
	}
	if !strings.HasPrefix(address, wantHRP+"1") || len(address) > 90 {
		return false
	}
	hrp, payload, err := Bech32Decode(address)
	return err == nil && hrp == wantHRP && len(payload) == AddressPayloadLen
}

// ValidateAddress strictly verifies that an address is a canonical lowercase Bech32 address
// with the active network's HRP and an AddressPayloadLen (32 bytes) payload [P2-2].
// Addresses of other networks are rejected.
func ValidateAddress(address string) bool {
	return validateAddressWithHRP(address, ActiveAddressHRP())
}

// ValidateAddressForNetwork is ValidateAddress against an explicit network ID instead of the
// active profile. Unknown network IDs are rejected.
func ValidateAddressForNetwork(address, netID string) bool {
	hrp, ok := AddressHRPForNetwork(netID)
	return ok && validateAddressWithHRP(address, hrp)
}

// PubToAddress derives the canonical Bech32 address for the active network from SigAlg and raw
// public key bytes: payload = SHA-256(SigAlg || publicKey) [P2-2].
func PubToAddress(sigAlg byte, pubBytes []byte) (string, error) {
	return PubToAddressForNetwork(sigAlg, pubBytes, "")
}

// PubToAddressForNetwork derives the canonical Bech32 address for an explicit network ID.
// The payload is network-independent; only the prefix differs between networks.
func PubToAddressForNetwork(sigAlg byte, pubBytes []byte, netID string) (string, error) {
	if len(pubBytes) == 0 {
		return "", errors.New("public key cannot be empty")
	}
	hrp, ok := AddressHRPForNetwork(netID)
	if !ok {
		return "", fmt.Errorf("unknown network id %q", netID)
	}
	preimage := append([]byte{sigAlg}, pubBytes...)
	hash := sha256.Sum256(preimage)
	return Bech32Encode(hrp, hash[:])
}

// PubHexToAddress derives the canonical Bech32 address from a hex-encoded public key.
// Backward compatibility bridge for wallet migration [P2-2/P2-3].
func PubHexToAddress(pubHex string) (string, error) {
	pubBytes, err := hex.DecodeString(pubHex)
	if err != nil {
		return "", err
	}
	// By default uses ML-DSA-65 (0x01)
	return PubToAddress(0x01, pubBytes)
}

// HexToAddress converts a 64-character hex pubkey hash to a canonical Bech32 address of the active network.
func HexToAddress(hexAddr string) (string, error) {
	rawBytes, err := hex.DecodeString(hexAddr)
	if err != nil || len(rawBytes) != AddressPayloadLen {
		return "", fmt.Errorf("invalid hex address: must be exactly %d hex characters (%d bytes)", 2*AddressPayloadLen, AddressPayloadLen)
	}
	return Bech32Encode(ActiveAddressHRP(), rawBytes)
}

// AddressToHex converts a canonical Bech32 address of the active network back to its 64-character
// lowercase hex pubkey hash. Strictly requires exact lowercase, the active network's HRP, and an
// AddressPayloadLen (32 bytes) payload [P2-2].
func AddressToHex(addr string) (string, error) {
	return addressToHexWithHRP(addr, ActiveAddressHRP())
}

// AddressToHexForNetwork is AddressToHex against an explicit network ID.
func AddressToHexForNetwork(addr, netID string) (string, error) {
	hrp, ok := AddressHRPForNetwork(netID)
	if !ok {
		return "", fmt.Errorf("unknown network id %q", netID)
	}
	return addressToHexWithHRP(addr, hrp)
}

func addressToHexWithHRP(addr, wantHRP string) (string, error) {
	if addr == "" || strings.TrimSpace(addr) != addr || addr != strings.ToLower(addr) {
		return "", errors.New("address must be strictly lowercase bech32 without leading or trailing spaces")
	}
	if !strings.HasPrefix(addr, wantHRP+"1") || len(addr) > 90 {
		if netID, ok := NetworkIDForAddress(addr); ok {
			return "", fmt.Errorf("address belongs to network %s; expected hrp '%s1'", netID, wantHRP)
		}
		return "", fmt.Errorf("address must begin with canonical hrp '%s1' and not exceed 90 characters", wantHRP)
	}
	hrp, payload, err := Bech32Decode(addr)
	if err != nil {
		return "", fmt.Errorf("invalid bech32 address: %w", err)
	}
	if hrp != wantHRP {
		return "", fmt.Errorf("invalid hrp: expected %s, got %s", wantHRP, hrp)
	}
	if len(payload) != AddressPayloadLen {
		return "", fmt.Errorf("invalid payload length: expected %d bytes, got %d", AddressPayloadLen, len(payload))
	}
	return hex.EncodeToString(payload), nil
}

type addressHexCache struct {
	sync.RWMutex
	entries map[string]string
	maxSize int
}

var (
	// addrHexCache caches validated address string to lowercase hex conversions with bounded memory [C-15]
	addrHexCache = addressHexCache{
		entries: make(map[string]string),
		maxSize: 10000,
	}
)

// AddressToHexCached provides thread-safe O(1) cached lookup of canonical 64-character hex addresses [C-15]
func AddressToHexCached(addr string) (string, error) {
	if addr == "" || addr != strings.ToLower(addr) {
		return "", errors.New("address must be strictly lowercase bech32")
	}

	// The cache is keyed by address string, so re-check the prefix on every hit: an entry cached
	// under one network must not be accepted after the active network changes.
	hrpPrefix := ActiveAddressHRP() + "1"
	addrHexCache.RLock()
	if val, ok := addrHexCache.entries[addr]; ok {
		addrHexCache.RUnlock()
		if !strings.HasPrefix(addr, hrpPrefix) {
			return AddressToHex(addr)
		}
		return val, nil
	}
	addrHexCache.RUnlock()

	hexStr, err := AddressToHex(addr)
	if err != nil {
		return "", err
	}

	addrHexCache.Lock()
	if len(addrHexCache.entries) >= addrHexCache.maxSize {
		// Evict 20% of entries to prevent unbounded memory growth under spam
		count := 0
		target := addrHexCache.maxSize / 5
		for k := range addrHexCache.entries {
			delete(addrHexCache.entries, k)
			count++
			if count >= target {
				break
			}
		}
	}
	addrHexCache.entries[addr] = hexStr
	addrHexCache.Unlock()

	return hexStr, nil
}

// MatchAddress compares two address strings for equality strictly under canonical Bech32 rules [C-20]
func MatchAddress(a, b string) bool {
	if a == "" || b == "" || a != strings.ToLower(a) || b != strings.ToLower(b) {
		return false
	}
	// Fast path: both addresses carry the active network prefix, so the bounded hex cache applies.
	activePrefix := ActiveAddressHRP() + "1"
	if strings.HasPrefix(a, activePrefix) && strings.HasPrefix(b, activePrefix) {
		hexA, errA := AddressToHexCached(a)
		hexB, errB := AddressToHexCached(b)
		return errA == nil && errB == nil && hexA == hexB
	}
	// Otherwise compare without consulting the active network, so consensus code that validates a
	// chain of another network (resolved from its Genesis) still matches its addresses. Both must
	// share the same known prefix: equal payloads under different prefixes are different addresses.
	hrpA, payloadA, errA := Bech32Decode(a)
	hrpB, payloadB, errB := Bech32Decode(b)
	return errA == nil && errB == nil && hrpA == hrpB && isKnownAddressHRP(hrpA) &&
		len(payloadA) == AddressPayloadLen && bytes.Equal(payloadA, payloadB)
}

// isKnownAddressHRP reports whether hrp is the address prefix of a built-in network.
func isKnownAddressHRP(hrp string) bool {
	switch hrp {
	case MainnetAddressHRP, TestnetAddressHRP, RegtestAddressHRP, SoakAddressHRP:
		return true
	}
	return false
}
