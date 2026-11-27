package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/armaxcoincore/armax-coin/core"
	"github.com/armaxcoincore/armax-coin/core/pqsig"
)

const (
	// PoolLedgerDomain is the ML-DSA-65 context and manifest domain of pool ledger exports. It is
	// distinct from the transaction context, so a ledger signature can never authorize a spend.
	PoolLedgerDomain = "ARMX-POOL-LEDGER-V1"

	// PoolLedgerVersion is the only accepted pool ledger manifest version.
	PoolLedgerVersion = 1
)

// PoolLedgerManifest is a pool operator's statement of the on-chain payouts it made to external
// miners in a height range. Payouts must be sorted by (height, tx_hash) with no duplicates, so the
// canonical encoding, and therefore the digest, is unique.
type PoolLedgerManifest struct {
	Version     int          `json:"version"`
	Domain      string       `json:"domain"`
	NetworkID   string       `json:"network_id"`
	PoolAddress string       `json:"pool_address"`
	StartHeight int          `json:"start_height"`
	EndHeight   int          `json:"end_height"`
	Payouts     []PoolPayout `json:"payouts"`
}

// PoolPayout is one on-chain payout from the pool hot wallet.
type PoolPayout struct {
	Height    int    `json:"height"`
	TxHash    string `json:"tx_hash"`
	Recipient string `json:"recipient"`
	Amount    int64  `json:"amount"`
}

// SignedPoolLedger is a pool ledger manifest with its SHA-256 digest and the operator's
// ML-DSA-65 signature over that digest.
type SignedPoolLedger struct {
	Manifest  PoolLedgerManifest `json:"manifest"`
	SHA256    string             `json:"sha256"`
	PublicKey string             `json:"public_key"`
	Signature string             `json:"signature"`
}

// validate checks the manifest shape for the active network without touching the chain.
func (m *PoolLedgerManifest) validate() error {
	if m.Version != PoolLedgerVersion || m.Domain != PoolLedgerDomain {
		return errors.New("unsupported pool ledger version or domain")
	}
	if m.NetworkID != core.NetworkID() {
		return fmt.Errorf("pool ledger is for %q, active network is %q", m.NetworkID, core.NetworkID())
	}
	if !core.ValidateAddress(m.PoolAddress) {
		return fmt.Errorf("pool_address %q is not a valid address", m.PoolAddress)
	}
	if m.StartHeight < 1 || m.EndHeight < m.StartHeight {
		return errors.New("pool ledger height range is invalid")
	}
	for i, p := range m.Payouts {
		if p.Height < m.StartHeight || p.Height > m.EndHeight {
			return fmt.Errorf("payout %d height %d is outside the ledger range", i, p.Height)
		}
		if len(p.TxHash) != 64 || !isLowerHex(p.TxHash) {
			return fmt.Errorf("payout %d tx_hash must be 64 lowercase hex characters", i)
		}
		if !core.ValidateAddress(p.Recipient) || p.Recipient == m.PoolAddress {
			return fmt.Errorf("payout %d recipient is invalid", i)
		}
		if p.Amount <= 0 {
			return fmt.Errorf("payout %d amount must be positive", i)
		}
		if i > 0 {
			prev := m.Payouts[i-1]
			if p.Height < prev.Height || (p.Height == prev.Height && p.TxHash <= prev.TxHash) {
				return errors.New("payouts must be sorted by (height, tx_hash) without duplicates")
			}
		}
	}
	return nil
}

// digest returns the SHA-256 of the manifest's canonical JSON encoding.
func (m *PoolLedgerManifest) digest() ([32]byte, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(data), nil
}

// signPoolLedger validates m and signs its digest with key.
func signPoolLedger(m PoolLedgerManifest, key *pqsig.PrivateKey) (*SignedPoolLedger, error) {
	if err := m.validate(); err != nil {
		return nil, err
	}
	d, err := m.digest()
	if err != nil {
		return nil, err
	}
	sig, err := key.SignWithContext(d[:], PoolLedgerDomain)
	if err != nil {
		return nil, err
	}
	return &SignedPoolLedger{
		Manifest:  m,
		SHA256:    hex.EncodeToString(d[:]),
		PublicKey: hex.EncodeToString(key.PublicKeyBytes()),
		Signature: hex.EncodeToString(sig),
	}, nil
}

// verifySignature checks the digest, the signature, and that the signing key is one of the
// policy's operator keys for the ledger's pool.
func (s *SignedPoolLedger) verifySignature(policy *Policy) error {
	if err := s.Manifest.validate(); err != nil {
		return err
	}
	d, err := s.Manifest.digest()
	if err != nil {
		return err
	}
	if s.SHA256 != hex.EncodeToString(d[:]) {
		return errors.New("pool ledger sha256 does not match its manifest")
	}
	pool, ok := policy.pool(s.Manifest.PoolAddress)
	if !ok {
		return fmt.Errorf("pool %s is not listed in the policy", s.Manifest.PoolAddress)
	}
	trusted := false
	for _, k := range pool.OperatorPublicKeys {
		if k == s.PublicKey {
			trusted = true
			break
		}
	}
	if !trusted {
		return fmt.Errorf("pool ledger for %s is signed by a key the policy does not list", s.Manifest.PoolAddress)
	}
	pub, err := decodePublicKeyHex(s.PublicKey)
	if err != nil {
		return err
	}
	sig, err := decodeSignatureHex(s.Signature)
	if err != nil {
		return err
	}
	if !pqsig.VerifyWithContext(pub, d[:], sig, PoolLedgerDomain) {
		return errors.New("pool ledger signature is invalid")
	}
	return nil
}

// onChainTransfer is a canonical non-coinbase transfer seen while scanning the chain.
type onChainTransfer struct {
	Height    int
	Sender    string
	Recipient string
	Amount    int64
}

// crossCheck requires every payout at or below upTo to exist on the canonical chain exactly as the
// ledger states it. Payouts above upTo are ignored by the caller and not checked here.
func (s *SignedPoolLedger) crossCheck(transfers map[string]onChainTransfer, upTo int) error {
	for _, p := range s.Manifest.Payouts {
		if p.Height > upTo {
			continue
		}
		tx, ok := transfers[p.TxHash]
		if !ok {
			return fmt.Errorf("pool %s payout %s is not on the canonical chain", s.Manifest.PoolAddress, p.TxHash)
		}
		if tx.Height != p.Height || tx.Sender != s.Manifest.PoolAddress || tx.Recipient != p.Recipient || tx.Amount != p.Amount {
			return fmt.Errorf("pool %s payout %s does not match the on-chain transaction", s.Manifest.PoolAddress, p.TxHash)
		}
	}
	return nil
}
