package core

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math/bits"
	"sort"
	"sync"

	"github.com/armaxcoincore/armax-coin/core/pqsig"
)

// ML-DSA-65 M-of-N multisig custody.
//
// A multisig account is identified by its descriptor: the threshold M followed by N distinct
// ML-DSA-65 public keys in strictly ascending byte order. The account address is
// SHA-256(SigAlgMultisigMLDSA65 || descriptor), so it can never collide with a single-key
// address (SHA-256(0x01 || pubkey)).
//
// A spend reuses the existing transaction fields, so the JSON schema does not change:
//   - SigAlg    = SigAlgMultisigMLDSA65
//   - SenderPub = hex(descriptor)
//   - Signature = hex(witness), witness = signer bitmap (1 byte) || M signatures in key order
//
// Every signer signs the ordinary transaction hash. That hash already commits to SigAlg and to
// the full descriptor, so a cosigner signature can never be replayed as a single-key spend or
// as a spend from a different multisig account.
//
// Size bound (Founder decision 2026-11-27): N <= 4 and M <= 3, so every spend fits
// MaxTxSize (20,000 bytes) in TxBinaryV1. 4-of-4 would need about 21.1 KB and is rejected.
const (
	// SigAlgMultisigMLDSA65 identifies an ML-DSA-65 M-of-N multisig spend.
	SigAlgMultisigMLDSA65 byte = 0x02
	// MultisigMinSigners is the smallest accepted threshold; 1-of-N custody is not allowed.
	MultisigMinSigners = 2
	// MultisigMaxKeys is the largest accepted number of cosigner keys.
	MultisigMaxKeys = 4
	// MultisigMaxSigners is the largest accepted threshold, bounded by MaxTxSize.
	MultisigMaxSigners = 3
)

var (
	multisigActivationHeightMu       sync.RWMutex
	multisigActivationHeightOverride *int64 // nil means use the network profile
)

// GetMultisigActivationHeight returns the first block height that accepts multisig spends on the
// active network. Zero or negative means multisig spends are disabled.
func GetMultisigActivationHeight() int64 {
	multisigActivationHeightMu.RLock()
	override := multisigActivationHeightOverride
	multisigActivationHeightMu.RUnlock()
	if override != nil {
		return *override
	}
	return GetActiveNetwork().MultisigActivationHeight
}

// SetMultisigActivationHeight overrides the multisig activation height (for tests).
func SetMultisigActivationHeight(height int64) {
	multisigActivationHeightMu.Lock()
	defer multisigActivationHeightMu.Unlock()
	multisigActivationHeightOverride = &height
}

// ResetMultisigActivationHeightOverride restores the network profile default.
func ResetMultisigActivationHeightOverride() {
	multisigActivationHeightMu.Lock()
	defer multisigActivationHeightMu.Unlock()
	multisigActivationHeightOverride = nil
}

// IsMultisigEnabled reports whether the active network accepts multisig spends at any height.
func IsMultisigEnabled() bool {
	return GetMultisigActivationHeight() > 0
}

// IsMultisigActive reports whether a block at blockIndex may contain multisig spends.
func IsMultisigActive(blockIndex int) bool {
	act := GetMultisigActivationHeight()
	return act > 0 && int64(blockIndex) >= act
}

// checkMultisigActivation rejects a multisig spend in a block below the activation height.
func checkMultisigActivation(tx *Transaction, blockIndex int) error {
	if tx != nil && tx.SigAlg == SigAlgMultisigMLDSA65 && !IsMultisigActive(blockIndex) {
		return fmt.Errorf("multisig spends are not active at height %d on %s", blockIndex, NetworkID())
	}
	return nil
}

// MultisigDescriptor is a canonical M-of-N set of ML-DSA-65 cosigner public keys.
type MultisigDescriptor struct {
	M       int
	PubKeys [][]byte // strictly ascending, each pqsig.PublicKeySize bytes
}

// NewMultisigDescriptor builds a canonical descriptor from m and the cosigner public keys in any
// order. The keys are copied and sorted; duplicates and out-of-range thresholds are rejected.
func NewMultisigDescriptor(m int, pubKeys [][]byte) (*MultisigDescriptor, error) {
	keys := make([][]byte, len(pubKeys))
	for i, k := range pubKeys {
		keys[i] = append([]byte(nil), k...)
	}
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i], keys[j]) < 0 })
	d := &MultisigDescriptor{M: m, PubKeys: keys}
	if err := d.validate(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *MultisigDescriptor) validate() error {
	n := len(d.PubKeys)
	if n < MultisigMinSigners || n > MultisigMaxKeys {
		return fmt.Errorf("multisig key count %d out of range (%d..%d)", n, MultisigMinSigners, MultisigMaxKeys)
	}
	if d.M < MultisigMinSigners || d.M > MultisigMaxSigners || d.M > n {
		return fmt.Errorf("multisig threshold %d-of-%d out of range (M %d..min(%d, N))", d.M, n, MultisigMinSigners, MultisigMaxSigners)
	}
	for i, k := range d.PubKeys {
		if len(k) != pqsig.PublicKeySize {
			return fmt.Errorf("multisig key %d has length %d, want %d", i, len(k), pqsig.PublicKeySize)
		}
		if i > 0 && bytes.Compare(d.PubKeys[i-1], k) >= 0 {
			return errors.New("multisig keys must be distinct and in strictly ascending order")
		}
	}
	return nil
}

// N returns the number of cosigner keys.
func (d *MultisigDescriptor) N() int { return len(d.PubKeys) }

// Bytes returns the canonical encoding M || N || pubkey_1 || ... || pubkey_N.
func (d *MultisigDescriptor) Bytes() []byte {
	out := make([]byte, 0, 2+len(d.PubKeys)*pqsig.PublicKeySize)
	out = append(out, byte(d.M), byte(len(d.PubKeys)))
	for _, k := range d.PubKeys {
		out = append(out, k...)
	}
	return out
}

// Hex returns the lowercase hex of the canonical encoding (the SenderPub of a spend).
func (d *MultisigDescriptor) Hex() string { return hex.EncodeToString(d.Bytes()) }

// Address returns the multisig account address on the active network.
func (d *MultisigDescriptor) Address() (string, error) {
	return PubToAddress(SigAlgMultisigMLDSA65, d.Bytes())
}

// AddressForNetwork returns the multisig account address on an explicit network.
func (d *MultisigDescriptor) AddressForNetwork(netID string) (string, error) {
	return PubToAddressForNetwork(SigAlgMultisigMLDSA65, d.Bytes(), netID)
}

// KeyIndex returns the position of pub in the descriptor, or -1.
func (d *MultisigDescriptor) KeyIndex(pub []byte) int {
	for i, k := range d.PubKeys {
		if bytes.Equal(k, pub) {
			return i
		}
	}
	return -1
}

// ParseMultisigDescriptor decodes a canonical descriptor encoding. Non-canonical input (unsorted
// or duplicate keys, wrong length, out-of-range M or N) is rejected so each account has exactly
// one encoding and one address.
func ParseMultisigDescriptor(b []byte) (*MultisigDescriptor, error) {
	if len(b) < 2 {
		return nil, errors.New("multisig descriptor too short")
	}
	m, n := int(b[0]), int(b[1])
	if n < MultisigMinSigners || n > MultisigMaxKeys {
		return nil, fmt.Errorf("multisig key count %d out of range (%d..%d)", n, MultisigMinSigners, MultisigMaxKeys)
	}
	if len(b) != 2+n*pqsig.PublicKeySize {
		return nil, fmt.Errorf("multisig descriptor length %d, want %d for N=%d", len(b), 2+n*pqsig.PublicKeySize, n)
	}
	d := &MultisigDescriptor{M: m, PubKeys: make([][]byte, n)}
	for i := 0; i < n; i++ {
		off := 2 + i*pqsig.PublicKeySize
		d.PubKeys[i] = append([]byte(nil), b[off:off+pqsig.PublicKeySize]...)
	}
	if err := d.validate(); err != nil {
		return nil, err
	}
	return d, nil
}

// ParseMultisigDescriptorHex decodes a descriptor from canonical lowercase hex.
func ParseMultisigDescriptorHex(s string) (*MultisigDescriptor, error) {
	if !isLowerHex(s) {
		return nil, errors.New("multisig descriptor must be canonical lowercase hex")
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("multisig descriptor hex: %w", err)
	}
	return ParseMultisigDescriptor(b)
}

// multisigWitnessSize returns the exact witness length for a threshold of m.
func multisigWitnessSize(m int) int { return 1 + m*pqsig.SignatureSize }

// EncodeMultisigWitness builds the witness from partial signatures keyed by descriptor key index.
// When more than M signatures are supplied, the M lowest key indices are used, so the same set
// of partial signatures always aggregates to the same witness.
func EncodeMultisigWitness(d *MultisigDescriptor, sigs map[int][]byte) ([]byte, error) {
	if d == nil {
		return nil, errors.New("multisig descriptor is nil")
	}
	indices := make([]int, 0, len(sigs))
	for idx, sig := range sigs {
		if idx < 0 || idx >= d.N() {
			return nil, fmt.Errorf("signature key index %d out of range for N=%d", idx, d.N())
		}
		if len(sig) != pqsig.SignatureSize {
			return nil, fmt.Errorf("signature for key %d has length %d, want %d", idx, len(sig), pqsig.SignatureSize)
		}
		indices = append(indices, idx)
	}
	if len(indices) < d.M {
		return nil, fmt.Errorf("have %d of %d required signatures", len(indices), d.M)
	}
	sort.Ints(indices)
	indices = indices[:d.M]
	var bitmap byte
	out := make([]byte, 1, multisigWitnessSize(d.M))
	for _, idx := range indices {
		bitmap |= 1 << uint(idx)
		out = append(out, sigs[idx]...)
	}
	out[0] = bitmap
	return out, nil
}

// multisigSigner is one signature in a parsed witness, bound to its descriptor key.
type multisigSigner struct {
	pub []byte
	sig []byte
}

// parseMultisigWitness checks the witness structure against d: exactly M bitmap bits, none at or
// above N, and exactly M signatures of the ML-DSA-65 size.
func parseMultisigWitness(d *MultisigDescriptor, w []byte) ([]multisigSigner, error) {
	if len(w) != multisigWitnessSize(d.M) {
		return nil, fmt.Errorf("multisig witness length %d, want %d for M=%d", len(w), multisigWitnessSize(d.M), d.M)
	}
	bitmap := w[0]
	if bitmap>>uint(d.N()) != 0 {
		return nil, errors.New("multisig witness bitmap names a key beyond N")
	}
	if bits.OnesCount8(bitmap) != d.M {
		return nil, fmt.Errorf("multisig witness bitmap has %d signers, want exactly %d", bits.OnesCount8(bitmap), d.M)
	}
	// Size the slice from the consensus bound, not from d.M: the descriptor arrives from the
	// network, and a fixed capacity keeps this allocation independent of decoded input even if a
	// caller ever skips descriptor validation.
	signers := make([]multisigSigner, 0, MultisigMaxSigners)
	off := 1
	for i := 0; i < d.N(); i++ {
		if bitmap&(1<<uint(i)) == 0 {
			continue
		}
		signers = append(signers, multisigSigner{pub: d.PubKeys[i], sig: w[off : off+pqsig.SignatureSize]})
		off += pqsig.SignatureSize
	}
	return signers, nil
}

// PrepareMultisigSpend binds tx to descriptor d (SigAlg, SenderPub, Sender) before cosigners sign.
// Signatures computed before this call would not cover the descriptor and would not verify.
func (tx *Transaction) PrepareMultisigSpend(d *MultisigDescriptor) error {
	if d == nil {
		return errors.New("multisig descriptor is nil")
	}
	if err := d.validate(); err != nil {
		return err
	}
	addr, err := d.Address()
	if err != nil {
		return err
	}
	tx.SigAlg = SigAlgMultisigMLDSA65
	tx.SenderPub = d.Hex()
	tx.Sender = addr
	tx.Signature = ""
	tx.InvalidateCache()
	return nil
}

// MultisigPartialSign signs a prepared multisig spend with one cosigner key and returns that key's
// descriptor index and signature. The key must belong to the descriptor in tx.SenderPub.
func (tx *Transaction) MultisigPartialSign(key *pqsig.PrivateKey) (int, []byte, error) {
	if key == nil {
		return -1, nil, errors.New("private key is nil")
	}
	if tx.SigAlg != SigAlgMultisigMLDSA65 {
		return -1, nil, errors.New("transaction is not a prepared multisig spend")
	}
	d, err := ParseMultisigDescriptorHex(tx.SenderPub)
	if err != nil {
		return -1, nil, err
	}
	idx := d.KeyIndex(key.PublicKeyBytes())
	if idx < 0 {
		return -1, nil, errors.New("signing key is not a cosigner of this multisig account")
	}
	sig, err := key.Sign(tx.Hash())
	if err != nil {
		return -1, nil, err
	}
	return idx, sig, nil
}

// AttachMultisigWitness aggregates partial signatures (keyed by descriptor index) into tx.
// Every supplied signature is verified first, so a bad cosigner share is reported by index
// instead of producing a transaction the network will reject.
func (tx *Transaction) AttachMultisigWitness(sigs map[int][]byte) error {
	if tx.SigAlg != SigAlgMultisigMLDSA65 {
		return errors.New("transaction is not a prepared multisig spend")
	}
	d, err := ParseMultisigDescriptorHex(tx.SenderPub)
	if err != nil {
		return err
	}
	msg := tx.Hash()
	for idx, sig := range sigs {
		if idx < 0 || idx >= d.N() {
			return fmt.Errorf("signature key index %d out of range for N=%d", idx, d.N())
		}
		if !pqsig.Verify(d.PubKeys[idx], msg, sig) {
			return fmt.Errorf("partial signature for key %d does not verify for this transaction", idx)
		}
	}
	w, err := EncodeMultisigWitness(d, sigs)
	if err != nil {
		return err
	}
	tx.Signature = hex.EncodeToString(w)
	tx.InvalidateCache()
	return nil
}

// multisigWireFields decodes the descriptor and witness for TxBinaryV1. Lowercase hex is required
// on every network: the decoder re-encodes these fields as lowercase, and SenderPub is part of the
// transaction hash, so any other casing would not round-trip to the same txid.
func (tx *Transaction) multisigWireFields() ([]byte, []byte, error) {
	if tx.SenderPub == "" || !isLowerHex(tx.SenderPub) {
		return nil, nil, errors.New("multisig descriptor must be non-empty canonical lowercase hex")
	}
	if tx.Signature == "" || !isLowerHex(tx.Signature) {
		return nil, nil, errors.New("multisig witness must be non-empty canonical lowercase hex")
	}
	pub, err := hex.DecodeString(tx.SenderPub)
	if err != nil {
		return nil, nil, fmt.Errorf("multisig descriptor hex: %w", err)
	}
	sig, err := hex.DecodeString(tx.Signature)
	if err != nil {
		return nil, nil, fmt.Errorf("multisig witness hex: %w", err)
	}
	return pub, sig, nil
}

// validateMultisigBasic checks a multisig spend's structure: activation on this network, canonical
// descriptor, sender bound to the descriptor, and a well-formed witness.
func (tx *Transaction) validateMultisigBasic() error {
	if !IsMultisigEnabled() {
		return fmt.Errorf("multisig spends (sig_alg 0x%02x) are not enabled on %s", SigAlgMultisigMLDSA65, NetworkID())
	}
	d, err := ParseMultisigDescriptorHex(tx.SenderPub)
	if err != nil {
		return fmt.Errorf("invalid multisig sender_pub: %w", err)
	}
	derived, err := d.Address()
	if err != nil || !MatchAddress(tx.Sender, derived) {
		return fmt.Errorf("sender address %s does not match derived multisig address %s", tx.Sender, derived)
	}
	if !isLowerHex(tx.Signature) {
		return errors.New("multisig signature must be canonical lowercase hex")
	}
	w, err := hex.DecodeString(tx.Signature)
	if err != nil {
		return errors.New("failed to decode multisig witness hex bytes")
	}
	if _, err := parseMultisigWitness(d, w); err != nil {
		return err
	}
	return nil
}

// verifyMultisigSignatures checks every cosigner signature in the witness against tx.Hash().
func (tx *Transaction) verifyMultisigSignatures() error {
	d, err := ParseMultisigDescriptorHex(tx.SenderPub)
	if err != nil {
		return fmt.Errorf("invalid multisig sender_pub: %w", err)
	}
	w, err := hex.DecodeString(tx.Signature)
	if err != nil {
		return errors.New("failed to decode multisig witness hex bytes")
	}
	signers, err := parseMultisigWitness(d, w)
	if err != nil {
		return err
	}
	msg := tx.Hash()
	for i, s := range signers {
		if !pqsig.Verify(s.pub, msg, s.sig) {
			return fmt.Errorf("multisig signature %d verification failed (invalid signature for network/message)", i)
		}
	}
	return nil
}
