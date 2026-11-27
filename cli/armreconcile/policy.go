package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/armaxcoincore/armax-coin/core"
	"github.com/armaxcoincore/armax-coin/core/pqsig"
)

// PolicyVersion is the only accepted policy file version.
const PolicyVersion = 1

// Policy carries every governance input of a reconciliation run. Nothing here is hardcoded in the
// tool: the excluded, pending, faucet and pool address lists, the cutoff height and the caps are
// published decisions, and the manifest embeds the normalized policy and its digest so anyone can
// rerun the same reconciliation. No conversion ratio is applied; entitlements stay in source
// network Armoids.
type Policy struct {
	Version           int          `json:"version"`
	SourceNetworkID   string       `json:"source_network_id"`
	TargetNetworkID   string       `json:"target_network_id"`
	CutoffHeight      int          `json:"cutoff_height"`
	WalletCapArmoids  int64        `json:"wallet_cap_armoids"`
	ProgramCapArmoids int64        `json:"program_cap_armoids"`
	ExcludedAddresses []string     `json:"excluded_addresses"`
	PendingAddresses  []string     `json:"pending_addresses"`
	FaucetAddresses   []string     `json:"faucet_addresses"`
	BurnAddresses     []string     `json:"burn_addresses"`
	Pools             []PoolPolicy `json:"pools"`
}

// PoolPolicy names a pool hot wallet and the ML-DSA-65 keys allowed to sign its ledger exports.
type PoolPolicy struct {
	Address            string   `json:"address"`
	OperatorPublicKeys []string `json:"operator_public_keys"`
}

// addressRole is the policy role of a source network address.
type addressRole int

const (
	roleNone addressRole = iota
	roleExcluded
	rolePending
	roleFaucet
	roleBurn
	rolePool
)

// loadPolicy reads, validates and normalizes a policy file for the active (source) network.
func loadPolicy(path string) (*Policy, error) {
	var p Policy
	if err := readStrictJSON(path, &p); err != nil {
		return nil, err
	}
	if err := p.normalize(); err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}
	return &p, nil
}

// normalize validates the policy and sorts every list so the digest does not depend on how the
// file was written.
func (p *Policy) normalize() error {
	if p.Version != PolicyVersion {
		return fmt.Errorf("unsupported version %d", p.Version)
	}
	if p.SourceNetworkID != core.NetworkID() {
		return fmt.Errorf("source_network_id %q does not match the active network %q", p.SourceNetworkID, core.NetworkID())
	}
	if _, ok := core.AddressHRPForNetwork(p.TargetNetworkID); !ok || p.TargetNetworkID == "" {
		return fmt.Errorf("unknown target_network_id %q", p.TargetNetworkID)
	}
	if p.TargetNetworkID == p.SourceNetworkID {
		return errors.New("target_network_id must differ from source_network_id")
	}
	if p.CutoffHeight < 1 {
		return errors.New("cutoff_height must be >= 1")
	}
	if p.WalletCapArmoids <= 0 || p.ProgramCapArmoids <= 0 {
		return errors.New("wallet_cap_armoids and program_cap_armoids must be positive")
	}
	if p.WalletCapArmoids > p.ProgramCapArmoids {
		return errors.New("wallet_cap_armoids must not exceed program_cap_armoids")
	}

	// The zero-payload burn address of the source network is a protocol fact, not a governance
	// choice, so it is always present.
	p.BurnAddresses = append(p.BurnAddresses, core.GenesisMinerForNetwork(p.SourceNetworkID))

	seen := make(map[string]string)
	norm := func(name string, list []string) ([]string, error) {
		out := make([]string, 0, len(list))
		for _, a := range list {
			if !core.ValidateAddressForNetwork(a, p.SourceNetworkID) {
				return nil, fmt.Errorf("%s: %q is not a %s address", name, a, p.SourceNetworkID)
			}
			if prev, dup := seen[a]; dup {
				if prev == name && name == "burn_addresses" {
					continue
				}
				return nil, fmt.Errorf("%s: %q is already listed in %s", name, a, prev)
			}
			seen[a] = name
			out = append(out, a)
		}
		sort.Strings(out)
		return out, nil
	}
	var err error
	if p.ExcludedAddresses, err = norm("excluded_addresses", p.ExcludedAddresses); err != nil {
		return err
	}
	if p.PendingAddresses, err = norm("pending_addresses", p.PendingAddresses); err != nil {
		return err
	}
	if p.FaucetAddresses, err = norm("faucet_addresses", p.FaucetAddresses); err != nil {
		return err
	}
	if p.BurnAddresses, err = norm("burn_addresses", p.BurnAddresses); err != nil {
		return err
	}
	poolAddrs := make([]string, 0, len(p.Pools))
	for i := range p.Pools {
		poolAddrs = append(poolAddrs, p.Pools[i].Address)
	}
	if _, err = norm("pools", poolAddrs); err != nil {
		return err
	}
	for i := range p.Pools {
		pool := &p.Pools[i]
		if len(pool.OperatorPublicKeys) == 0 {
			return fmt.Errorf("pool %s has no operator_public_keys", pool.Address)
		}
		keys := make(map[string]bool)
		for _, k := range pool.OperatorPublicKeys {
			if _, err := decodePublicKeyHex(k); err != nil {
				return fmt.Errorf("pool %s: %w", pool.Address, err)
			}
			if keys[k] {
				return fmt.Errorf("pool %s lists an operator key twice", pool.Address)
			}
			keys[k] = true
		}
		sort.Strings(pool.OperatorPublicKeys)
	}
	sort.Slice(p.Pools, func(i, j int) bool { return p.Pools[i].Address < p.Pools[j].Address })
	return nil
}

// Digest is the SHA-256 of the normalized policy's JSON encoding.
func (p *Policy) Digest() string {
	data, _ := json.Marshal(p)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// roles maps every listed address to its policy role.
func (p *Policy) roles() map[string]addressRole {
	r := make(map[string]addressRole)
	for _, a := range p.ExcludedAddresses {
		r[a] = roleExcluded
	}
	for _, a := range p.PendingAddresses {
		r[a] = rolePending
	}
	for _, a := range p.FaucetAddresses {
		r[a] = roleFaucet
	}
	for _, a := range p.BurnAddresses {
		r[a] = roleBurn
	}
	for _, pool := range p.Pools {
		r[pool.Address] = rolePool
	}
	return r
}

// pool returns the pool entry for address, if any.
func (p *Policy) pool(address string) (*PoolPolicy, bool) {
	for i := range p.Pools {
		if p.Pools[i].Address == address {
			return &p.Pools[i], true
		}
	}
	return nil, false
}

// decodePublicKeyHex accepts only lowercase hex of exactly one ML-DSA-65 public key.
func decodePublicKeyHex(s string) ([]byte, error) {
	if len(s) != 2*pqsig.PublicKeySize || !isLowerHex(s) {
		return nil, errors.New("public key must be lowercase hex of an ML-DSA-65 public key")
	}
	return hex.DecodeString(s)
}

// decodeSignatureHex accepts only lowercase hex of exactly one ML-DSA-65 signature.
func decodeSignatureHex(s string) ([]byte, error) {
	if len(s) != 2*pqsig.SignatureSize || !isLowerHex(s) {
		return nil, errors.New("signature must be lowercase hex of an ML-DSA-65 signature")
	}
	return hex.DecodeString(s)
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
