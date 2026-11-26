package pqsig

import (
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

const (
	// SigAlgMLDSA65 identifies ML-DSA-65 (NIST FIPS 204) algorithm in Armaxcoin transactions
	SigAlgMLDSA65 = byte(0x01)

	// ContextString is the domain separation context string required by FIPS 204 for Armaxcoin layer-1 transactions
	ContextString = "ARMX-TX-V1"

	// SeedSize is the exact raw private key seed length (32 bytes) for ML-DSA-65
	SeedSize = mldsa.PrivateKeySize
)

var (
	params = mldsa.MLDSA65()

	// PublicKeySize is the encoded public key size (1,952 bytes) for ML-DSA-65
	PublicKeySize = params.PublicKeySize()

	// SignatureSize is the signature size (3,309 bytes) for ML-DSA-65
	SignatureSize = params.SignatureSize()

	signOpts = &mldsa.Options{
		Context: ContextString,
	}
)

// PrivateKey wraps the standard library ML-DSA-65 private key
type PrivateKey struct {
	inner *mldsa.PrivateKey
}

// GenerateFromSeed constructs a deterministic ML-DSA-65 private key from a 32-byte seed
func GenerateFromSeed(seed [32]byte) (*PrivateKey, error) {
	sk, err := mldsa.NewPrivateKey(params, seed[:])
	if err != nil {
		return nil, fmt.Errorf("failed to initialize ML-DSA-65 private key: %w", err)
	}
	return &PrivateKey{inner: sk}, nil
}

// GenerateKey generates a fresh random ML-DSA-65 key pair using crypto/rand
func GenerateKey() (*PrivateKey, [32]byte, error) {
	var seed [32]byte
	if _, err := io.ReadFull(rand.Reader, seed[:]); err != nil {
		return nil, seed, fmt.Errorf("failed to read entropy for seed: %w", err)
	}
	sk, err := GenerateFromSeed(seed)
	return sk, seed, err
}

// PublicKeyBytes returns the raw 1952-byte encoding of the ML-DSA-65 public key
func (sk *PrivateKey) PublicKeyBytes() []byte {
	return sk.inner.PublicKey().Bytes()
}

// Sign signs a message digest using ML-DSA-65 with the protocol context string
func (sk *PrivateKey) Sign(msg []byte) ([]byte, error) {
	sig, err := sk.inner.Sign(rand.Reader, msg, signOpts)
	if err != nil {
		return nil, fmt.Errorf("ml-dsa-65 signing failed: %w", err)
	}
	return sig, nil
}

// Verify verifies a message signature against a public key using ML-DSA-65 and the protocol context string
func Verify(pubBytes []byte, msg []byte, sig []byte) bool {
	return verifyWithOptions(pubBytes, msg, sig, signOpts)
}

// verifyWithOptions is the single ML-DSA-65 verification path behind Verify. It is
// split out so the NIST ACVP and Wycheproof vectors, which carry their own context
// strings, exercise exactly the same length gates and key decoding that consensus
// uses for ContextString.
func verifyWithOptions(pubBytes []byte, msg []byte, sig []byte, opts *mldsa.Options) bool {
	if len(pubBytes) != PublicKeySize || len(sig) != SignatureSize {
		return false
	}
	pk, err := mldsa.NewPublicKey(params, pubBytes)
	if err != nil {
		return false
	}
	return mldsa.Verify(pk, msg, sig, opts) == nil
}

// DeriveSeedFromBIP39 derives a deterministic 32-byte ML-DSA-65 account seed from a 64-byte BIP-39 root seed
// using HKDF-SHA256(salt="armax-pqc-v1", info="ml-dsa-65/account/<index>")
func DeriveSeedFromBIP39(bip39Seed []byte, accountIndex uint32) ([32]byte, error) {
	var seed32 [32]byte
	if len(bip39Seed) < SeedSize {
		return seed32, fmt.Errorf("bip39 seed too short: got %d bytes, require at least %d", len(bip39Seed), SeedSize)
	}
	salt := []byte("armax-pqc-v1")
	info := []byte(fmt.Sprintf("ml-dsa-65/account/%d", accountIndex))

	kdf := hkdf.New(sha256.New, bip39Seed, salt, info)
	if _, err := io.ReadFull(kdf, seed32[:]); err != nil {
		return seed32, fmt.Errorf("hkdf seed derivation failed: %w", err)
	}
	return seed32, nil
}
