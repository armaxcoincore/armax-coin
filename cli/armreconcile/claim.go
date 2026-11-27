package main

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/armaxcoincore/armax-coin/core"
	"github.com/armaxcoincore/armax-coin/core/pqsig"
)

const (
	// MigrationClaimDomain is the ML-DSA-65 context and message prefix of migration claims. It is
	// distinct from the transaction context, so a claim signature can never authorize a spend.
	MigrationClaimDomain = "ARMX-MIGRATION-V1"

	// MigrationClaimVersion is the only accepted claim file version.
	MigrationClaimVersion = 1
)

// MigrationClaim binds an eligible source network address to a new target network address.
// It is signed with the source address's own ML-DSA-65 key, so payouts never go to the source
// address re-encoded for the target network: source keys, often handled loosely on Testnet,
// never control target funds.
type MigrationClaim struct {
	Version         int    `json:"version"`
	Domain          string `json:"domain"`
	TargetNetworkID string `json:"target_network_id"`
	SourceAddress   string `json:"source_address"`
	TargetAddress   string `json:"target_address"`
	CutoffHeight    int    `json:"cutoff_height"`
	PublicKey       string `json:"public_key"`
	Signature       string `json:"signature"`
}

// claimMessage is MigrationClaimDomain || 0x00 || target network ID || 0x00 || target address ||
// 0x00 || uint64 big-endian cutoff height. None of the text fields can contain a NUL byte, so the
// encoding is unambiguous.
func claimMessage(targetNetworkID, targetAddress string, cutoff int) []byte {
	msg := make([]byte, 0, len(MigrationClaimDomain)+len(targetNetworkID)+len(targetAddress)+11)
	msg = append(msg, MigrationClaimDomain...)
	msg = append(msg, 0)
	msg = append(msg, targetNetworkID...)
	msg = append(msg, 0)
	msg = append(msg, targetAddress...)
	msg = append(msg, 0)
	return binary.BigEndian.AppendUint64(msg, uint64(cutoff))
}

// signMigrationClaim signs a claim with the source wallet key. The source address is derived from
// the key for the active (source) network.
func signMigrationClaim(key *pqsig.PrivateKey, targetNetworkID, targetAddress string, cutoff int) (*MigrationClaim, error) {
	if err := checkClaimTarget(targetNetworkID, targetAddress, cutoff); err != nil {
		return nil, err
	}
	pub := key.PublicKeyBytes()
	source, err := core.PubToAddress(pqsig.SigAlgMLDSA65, pub)
	if err != nil {
		return nil, err
	}
	sig, err := key.SignWithContext(claimMessage(targetNetworkID, targetAddress, cutoff), MigrationClaimDomain)
	if err != nil {
		return nil, err
	}
	return &MigrationClaim{
		Version:         MigrationClaimVersion,
		Domain:          MigrationClaimDomain,
		TargetNetworkID: targetNetworkID,
		SourceAddress:   source,
		TargetAddress:   targetAddress,
		CutoffHeight:    cutoff,
		PublicKey:       hex.EncodeToString(pub),
		Signature:       hex.EncodeToString(sig),
	}, nil
}

func checkClaimTarget(targetNetworkID, targetAddress string, cutoff int) error {
	if targetNetworkID == core.NetworkID() {
		return errors.New("target network must differ from the source network")
	}
	if !core.ValidateAddressForNetwork(targetAddress, targetNetworkID) {
		return fmt.Errorf("target address %q is not a %s address", targetAddress, targetNetworkID)
	}
	if cutoff < 1 {
		return errors.New("cutoff height must be >= 1")
	}
	return nil
}

// verify checks a claim against the expected target network and cutoff: the key must derive the
// source address on the active network, and the signature must cover the target binding.
func (c *MigrationClaim) verify(targetNetworkID string, cutoff int) error {
	if c.Version != MigrationClaimVersion || c.Domain != MigrationClaimDomain {
		return errors.New("unsupported claim version or domain")
	}
	if c.TargetNetworkID != targetNetworkID {
		return fmt.Errorf("claim targets %q, expected %q", c.TargetNetworkID, targetNetworkID)
	}
	if c.CutoffHeight != cutoff {
		return fmt.Errorf("claim is for cutoff %d, expected %d", c.CutoffHeight, cutoff)
	}
	if err := checkClaimTarget(c.TargetNetworkID, c.TargetAddress, c.CutoffHeight); err != nil {
		return err
	}
	if !core.ValidateAddress(c.SourceAddress) {
		return errors.New("source address is not a valid address of the active network")
	}
	pub, err := decodePublicKeyHex(c.PublicKey)
	if err != nil {
		return err
	}
	derived, err := core.PubToAddress(pqsig.SigAlgMLDSA65, pub)
	if err != nil || derived != c.SourceAddress {
		return errors.New("public key does not belong to the source address")
	}
	sig, err := decodeSignatureHex(c.Signature)
	if err != nil {
		return err
	}
	if !pqsig.VerifyWithContext(pub, claimMessage(c.TargetNetworkID, c.TargetAddress, c.CutoffHeight), sig, MigrationClaimDomain) {
		return errors.New("claim signature is invalid")
	}
	return nil
}
