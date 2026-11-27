package wallet

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/armaxcoincore/armax-coin/core"
	"github.com/armaxcoincore/armax-coin/core/pqsig"
)

// Air-gapped M-of-N multisig custody files.
//
// The flow never moves a private key between machines:
//  1. each cosigner exports its public key (derived from the decrypted keystore, never from
//     unencrypted keystore metadata, which could be swapped);
//  2. a coordinator builds a descriptor file from the public keys;
//  3. a coordinator proposes an unsigned spend file;
//  4. each cosigner signs the spend file offline, adding one partial signature;
//  5. any party combines the signed files into the final transaction and broadcasts it.
//
// Every file names its network, and every step refuses a file from another network before
// signing, following the wallet send guard.

const (
	// MultisigFileVersion is the format version of descriptor and spend files.
	MultisigFileVersion = 1
	// MaxMultisigFileSize bounds descriptor and spend file reads.
	MaxMultisigFileSize = 256 * 1024
)

// MultisigDescriptorFile records an M-of-N account so every cosigner can check the same keys.
type MultisigDescriptorFile struct {
	Version    int      `json:"version"`
	Network    string   `json:"network"`
	M          int      `json:"m"`
	N          int      `json:"n"`
	PubKeys    []string `json:"pubkeys"`
	Descriptor string   `json:"descriptor"`
	Address    string   `json:"address"`
}

// MultisigSpendFile is an unsigned or partially signed multisig spend passed between cosigners.
type MultisigSpendFile struct {
	Version           int               `json:"version"`
	Network           string            `json:"network"`
	Address           string            `json:"address"`
	TxHash            string            `json:"tx_hash"`
	Tx                core.Transaction  `json:"tx"`
	PartialSignatures map[string]string `json:"partial_signatures"`
}

// NewMultisigDescriptorFile builds a descriptor file for the active network from m and the
// cosigner public keys (hex, any order).
func NewMultisigDescriptorFile(m int, pubKeyHexes []string) (*MultisigDescriptorFile, error) {
	pubs := make([][]byte, len(pubKeyHexes))
	for i, h := range pubKeyHexes {
		b, err := hex.DecodeString(strings.TrimSpace(h))
		if err != nil || len(b) != pqsig.PublicKeySize {
			return nil, fmt.Errorf("cosigner public key %d is not a %d-byte ML-DSA-65 key", i+1, pqsig.PublicKeySize)
		}
		pubs[i] = b
	}
	d, err := core.NewMultisigDescriptor(m, pubs)
	if err != nil {
		return nil, err
	}
	addr, err := d.Address()
	if err != nil {
		return nil, err
	}
	f := &MultisigDescriptorFile{
		Version:    MultisigFileVersion,
		Network:    core.NetworkID(),
		M:          d.M,
		N:          d.N(),
		Descriptor: d.Hex(),
		Address:    addr,
	}
	for _, k := range d.PubKeys {
		f.PubKeys = append(f.PubKeys, hex.EncodeToString(k))
	}
	return f, nil
}

// checkMultisigNetwork refuses a file created for another network.
func checkMultisigNetwork(kind, network string) error {
	if network != core.NetworkID() {
		return fmt.Errorf("rejected before signing:%s belongs to network %s; active network is %s", kind, network, core.NetworkID())
	}
	return nil
}

// ParsedDescriptor validates the file against the active network and returns its descriptor.
// Every redundant field (keys, M, N, address) must agree with the canonical descriptor.
func (f *MultisigDescriptorFile) ParsedDescriptor() (*core.MultisigDescriptor, error) {
	if f.Version != MultisigFileVersion {
		return nil, fmt.Errorf("unsupported multisig descriptor file version %d", f.Version)
	}
	if err := checkMultisigNetwork("descriptor file", f.Network); err != nil {
		return nil, err
	}
	d, err := core.ParseMultisigDescriptorHex(f.Descriptor)
	if err != nil {
		return nil, err
	}
	if d.M != f.M || d.N() != f.N || len(f.PubKeys) != d.N() {
		return nil, errors.New("descriptor file M/N/pubkeys do not match the descriptor")
	}
	for i, k := range d.PubKeys {
		if f.PubKeys[i] != hex.EncodeToString(k) {
			return nil, fmt.Errorf("descriptor file pubkey %d does not match the descriptor", i+1)
		}
	}
	addr, err := d.Address()
	if err != nil || addr != f.Address {
		return nil, errors.New("descriptor file address does not match the descriptor")
	}
	return d, nil
}

// ProjectedMultisigFee returns the minimum relay fee for a spend from d, using the exact witness
// size of a fully signed spend.
func ProjectedMultisigFee(d *core.MultisigDescriptor, recipient string, amount, nonce int64) (int64, error) {
	tmp := core.Transaction{Recipient: recipient, Amount: amount, Nonce: nonce, Type: core.TxTypeStandard}
	if err := tmp.PrepareMultisigSpend(d); err != nil {
		return 0, err
	}
	tmp.Signature = strings.Repeat("00", 1+d.M*pqsig.SignatureSize)
	size := core.FeeEffectiveTxSize(&tmp)
	if size <= 0 {
		return 0, errors.New("cannot compute projected multisig spend size")
	}
	return core.CalculateMinMempoolFee(size), nil
}

// NewMultisigSpend proposes an unsigned spend from the descriptor's account. A negative fee
// selects the minimum relay fee for the projected size. The nonce must be the account's next
// nonce; it is explicit because the signing machines are offline.
func NewMultisigSpend(df *MultisigDescriptorFile, recipient string, amount, fee, nonce int64) (*MultisigSpendFile, error) {
	d, err := df.ParsedDescriptor()
	if err != nil {
		return nil, err
	}
	if !core.ValidateAddress(recipient) {
		if netID, ok := core.NetworkIDForAddress(recipient); ok {
			return nil, fmt.Errorf("rejected before signing: recipient is a %s address; this spend is on %s (expected %s1...)", netID, core.NetworkID(), core.ActiveAddressHRP())
		}
		return nil, fmt.Errorf("invalid recipient address %q (expected %s1...)", recipient, core.ActiveAddressHRP())
	}
	if nonce < 1 {
		return nil, errors.New("nonce must be the account's next nonce (>= 1)")
	}
	if fee < 0 {
		if fee, err = ProjectedMultisigFee(d, recipient, amount, nonce); err != nil {
			return nil, err
		}
	}
	tx := core.Transaction{Recipient: recipient, Amount: amount, Fee: fee, Nonce: nonce, Type: core.TxTypeStandard}
	if err := tx.PrepareMultisigSpend(d); err != nil {
		return nil, err
	}
	return &MultisigSpendFile{
		Version:           MultisigFileVersion,
		Network:           core.NetworkID(),
		Address:           df.Address,
		TxHash:            tx.HashHex(),
		Tx:                tx,
		PartialSignatures: map[string]string{},
	}, nil
}

// descriptor validates the spend file and returns the descriptor bound into its transaction.
func (s *MultisigSpendFile) descriptor() (*core.MultisigDescriptor, error) {
	if s.Version != MultisigFileVersion {
		return nil, fmt.Errorf("unsupported multisig spend file version %d", s.Version)
	}
	if err := checkMultisigNetwork("spend file", s.Network); err != nil {
		return nil, err
	}
	if s.Tx.SigAlg != core.SigAlgMultisigMLDSA65 {
		return nil, errors.New("spend file transaction is not a multisig spend")
	}
	d, err := core.ParseMultisigDescriptorHex(s.Tx.SenderPub)
	if err != nil {
		return nil, err
	}
	addr, err := d.Address()
	if err != nil || addr != s.Tx.Sender || addr != s.Address {
		return nil, errors.New("spend file sender does not match its descriptor")
	}
	if !core.ValidateAddress(s.Tx.Recipient) {
		return nil, fmt.Errorf("rejected before signing: recipient %s is not a %s address", s.Tx.Recipient, core.NetworkID())
	}
	if s.Tx.HashHex() != s.TxHash {
		return nil, errors.New("spend file tx_hash does not match its transaction")
	}
	return d, nil
}

// Summary returns a one-line description cosigners should check before signing.
func (s *MultisigSpendFile) Summary() string {
	return fmt.Sprintf("network=%s from=%s to=%s amount=%s fee=%s nonce=%d tx=%s",
		s.Network, s.Tx.Sender, s.Tx.Recipient, core.FormatArmxDecimal(s.Tx.Amount), core.FormatArmxDecimal(s.Tx.Fee), s.Tx.Nonce, s.TxHash)
}

// Sign adds w's partial signature. w must be one of the descriptor's cosigners.
func (s *MultisigSpendFile) Sign(w *Wallet) error {
	if w == nil || w.PrivateKey == nil {
		return errors.New("wallet has no private key")
	}
	if _, err := s.descriptor(); err != nil {
		return err
	}
	tx := s.Tx
	idx, sig, err := tx.MultisigPartialSign(w.PrivateKey)
	if err != nil {
		return err
	}
	if s.PartialSignatures == nil {
		s.PartialSignatures = map[string]string{}
	}
	s.PartialSignatures[strconv.Itoa(idx)] = hex.EncodeToString(sig)
	return nil
}

// CombineMultisigSpends merges partial signatures from spend files for the same transaction and
// returns the fully signed, verified transaction.
func CombineMultisigSpends(spends []*MultisigSpendFile) (*core.Transaction, error) {
	if len(spends) == 0 {
		return nil, errors.New("no spend files to combine")
	}
	if _, err := spends[0].descriptor(); err != nil {
		return nil, err
	}
	sigs := map[int][]byte{}
	for i, s := range spends {
		if _, err := s.descriptor(); err != nil {
			return nil, fmt.Errorf("spend file %d: %w", i+1, err)
		}
		if s.TxHash != spends[0].TxHash {
			return nil, fmt.Errorf("spend file %d is for a different transaction", i+1)
		}
		keys := make([]string, 0, len(s.PartialSignatures))
		for k := range s.PartialSignatures {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			idx, err := strconv.Atoi(k)
			if err != nil || strconv.Itoa(idx) != k {
				return nil, fmt.Errorf("spend file %d has invalid signer index %q", i+1, k)
			}
			sig, err := hex.DecodeString(s.PartialSignatures[k])
			if err != nil {
				return nil, fmt.Errorf("spend file %d signer %d: invalid signature hex", i+1, idx)
			}
			if prev, ok := sigs[idx]; ok && hex.EncodeToString(prev) != s.PartialSignatures[k] {
				// ML-DSA signing is randomized; two valid shares from one key may differ. Keep the first.
				continue
			}
			sigs[idx] = sig
		}
	}
	tx := spends[0].Tx
	if err := tx.AttachMultisigWitness(sigs); err != nil {
		return nil, err
	}
	if err := tx.VerifyWithError(); err != nil {
		return nil, fmt.Errorf("combined transaction does not verify: %w", err)
	}
	return &tx, nil
}

// ReadMultisigJSON reads a bounded JSON file into v, rejecting unknown fields.
func ReadMultisigJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxMultisigFileSize+1))
	if err != nil {
		return err
	}
	if len(data) > MaxMultisigFileSize {
		return fmt.Errorf("%s exceeds %d bytes", path, MaxMultisigFileSize)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// WriteMultisigJSON writes v as indented JSON with owner-only permissions.
func WriteMultisigJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic0600(path, append(data, '\n'), "multisig-*.tmp")
}
