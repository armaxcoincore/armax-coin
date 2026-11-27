package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/armaxcoincore/armax-coin/core"
)

const (
	// ManifestDomain identifies migration manifests.
	ManifestDomain = "ARMX-MIGRATION-MANIFEST-V1"

	// ManifestVersion is the only accepted manifest version.
	ManifestVersion = 1

	// FinalityDepth is the number of blocks that must follow the cutoff before a snapshot is
	// final. It equals MaxReorganizationDepth: coinbase maturity is a spendability rule and is not
	// enough, since a deeper reorganization could still rewrite blocks at or below the cutoff.
	FinalityDepth = core.MaxReorganizationDepth
)

// Wallet categories of the 7-category treatment matrix.
const (
	CategoryEligibleSolo     = "eligible_solo"
	CategoryEligiblePool     = "eligible_pool"
	CategoryEligibleSoloPool = "eligible_solo_pool"
	CategoryDevExcluded      = "dev_excluded"
	CategoryPoolHotWallet    = "pool_hot_wallet"
	CategoryPending          = "pending_verification"
	CategoryTransferOnly     = "transfer_only_faucet"
)

// Entry payout statuses.
const (
	StatusNone          = "none"
	StatusClaimed       = "claimed"
	StatusUnclaimed     = "unclaimed"
	StatusClaimConflict = "escrow_claim_conflict"
	StatusPending       = "escrow_pending_verification"
)

// Entry is one source address in the manifest. All amounts are source network Armoids.
type Entry struct {
	SourceAddress      string `json:"source_address"`
	Category           string `json:"category"`
	SoloArmoids        int64  `json:"solo_armoids"`
	PoolPayoutArmoids  int64  `json:"pool_payout_armoids"`
	TransferInArmoids  int64  `json:"transfer_in_armoids"`
	EligibleArmoids    int64  `json:"eligible_armoids"`
	CappedArmoids      int64  `json:"capped_armoids"`
	ExcessArmoids      int64  `json:"excess_armoids"`
	EntitlementArmoids int64  `json:"entitlement_armoids"`
	Status             string `json:"status"`
	TargetAddress      string `json:"target_address,omitempty"`
}

// LedgerRef records a pool ledger used by the run.
type LedgerRef struct {
	PoolAddress string `json:"pool_address"`
	SHA256      string `json:"sha256"`
	StartHeight int    `json:"start_height"`
	EndHeight   int    `json:"end_height"`
	Payouts     int    `json:"payouts"`
}

// RejectedClaim records a claim that was not applied and why.
type RejectedClaim struct {
	SourceAddress string `json:"source_address"`
	TargetAddress string `json:"target_address"`
	Reason        string `json:"reason"`
}

// Totals summarizes the manifest.
type Totals struct {
	ReservedArmoids    int64 `json:"reserved_armoids"`
	ClaimedArmoids     int64 `json:"claimed_armoids"`
	UnclaimedArmoids   int64 `json:"unclaimed_armoids"`
	EscrowArmoids      int64 `json:"escrow_armoids"`
	CappedTotalArmoids int64 `json:"capped_total_armoids"`
	RoundingRemainder  int64 `json:"rounding_remainder_armoids"`
	ProRataApplied     bool  `json:"pro_rata_applied"`
}

// Manifest is the deterministic output of a reconciliation run. The same inputs always produce a
// byte-identical manifest; EntriesMerkleRoot commits to the sorted entries.
type Manifest struct {
	Version           int             `json:"version"`
	Domain            string          `json:"domain"`
	Policy            Policy          `json:"policy"`
	PolicySHA256      string          `json:"policy_sha256"`
	FinalityDepth     int             `json:"finality_depth"`
	CutoffBlockHash   string          `json:"cutoff_block_hash"`
	TipHeight         int             `json:"tip_height"`
	TipHash           string          `json:"tip_hash"`
	Ledgers           []LedgerRef     `json:"ledgers"`
	Totals            Totals          `json:"totals"`
	RejectedClaims    []RejectedClaim `json:"rejected_claims"`
	Entries           []Entry         `json:"entries"`
	EntriesMerkleRoot string          `json:"entries_merkle_root"`
}

// chainActivity is what a scan collects at or below the cutoff.
type chainActivity struct {
	solo       map[string]int64
	transferIn map[string]int64
	// transfers holds every non-coinbase transaction sent by a policy pool address, by hash.
	transfers   map[string]onChainTransfer
	cutoffHash  string
	info        core.CanonicalChainInfo
	cutoffFound bool
}

func isCoinbaseSender(sender string) bool {
	return sender == "SYSTEM" || sender == "COINBASE"
}

// scanChain collects coinbase credits, transfers received, and pool sends up to the cutoff.
func scanChain(dbPath string, policy *Policy) (*chainActivity, error) {
	act := &chainActivity{
		solo:       make(map[string]int64),
		transferIn: make(map[string]int64),
		transfers:  make(map[string]onChainTransfer),
	}
	pools := make(map[string]bool)
	for _, p := range policy.Pools {
		pools[p.Address] = true
	}
	info, err := core.ScanCanonicalBlocks(dbPath, 5*time.Second, func(b *core.Block) error {
		if b.Index == policy.CutoffHeight {
			act.cutoffHash = b.Hash
			act.cutoffFound = true
		}
		if b.Index == 0 || b.Index > policy.CutoffHeight {
			return nil
		}
		for i := range b.Transactions {
			tx := &b.Transactions[i]
			var err error
			if isCoinbaseSender(tx.Sender) {
				act.solo[tx.Recipient], err = addArmoids(act.solo[tx.Recipient], tx.Amount)
			} else {
				act.transferIn[tx.Recipient], err = addArmoids(act.transferIn[tx.Recipient], tx.Amount)
				if err == nil && pools[tx.Sender] {
					h := tx.HashHex()
					if _, dup := act.transfers[h]; dup {
						return fmt.Errorf("transaction %s appears twice on the canonical chain", h)
					}
					act.transfers[h] = onChainTransfer{Height: b.Index, Sender: tx.Sender, Recipient: tx.Recipient, Amount: tx.Amount}
				}
			}
			if err != nil {
				return fmt.Errorf("block %d: %w", b.Index, err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	act.info = info
	return act, nil
}

// reconcileInputs are the already loaded inputs of a reconciliation run.
type reconcileInputs struct {
	DBPath  string
	Policy  *Policy
	Ledgers []*SignedPoolLedger
	Claims  []*MigrationClaim
	// Malformed records claim files that could not be parsed.
	Malformed []RejectedClaim
}

// reconcile runs the whole pipeline. It fails closed on anything an operator controls (chain
// finality, ledgers, policy) and records, rather than fails on, bad claims submitted by users.
func reconcile(in reconcileInputs) (*Manifest, error) {
	p := in.Policy
	act, err := scanChain(in.DBPath, p)
	if err != nil {
		return nil, err
	}
	if !act.cutoffFound || act.info.TipHeight < p.CutoffHeight+FinalityDepth {
		return nil, fmt.Errorf("snapshot is not final: tip %d must be >= cutoff %d + %d", act.info.TipHeight, p.CutoffHeight, FinalityDepth)
	}

	poolPaid, ledgerRefs, err := applyLedgers(in.Ledgers, p, act)
	if err != nil {
		return nil, err
	}

	entries, err := classify(p, act, poolPaid)
	if err != nil {
		return nil, err
	}
	totals, err := applyCaps(entries, p)
	if err != nil {
		return nil, err
	}
	rejected := applyClaims(entries, in.Claims, in.Malformed, p)
	if err := finishTotals(entries, &totals); err != nil {
		return nil, err
	}

	m := &Manifest{
		Version:         ManifestVersion,
		Domain:          ManifestDomain,
		Policy:          *p,
		PolicySHA256:    p.Digest(),
		FinalityDepth:   FinalityDepth,
		CutoffBlockHash: act.cutoffHash,
		TipHeight:       act.info.TipHeight,
		TipHash:         act.info.TipHash,
		Ledgers:         ledgerRefs,
		Totals:          totals,
		RejectedClaims:  rejected,
		Entries:         entries,
	}
	root, err := entriesRoot(entries)
	if err != nil {
		return nil, err
	}
	m.EntriesMerkleRoot = root
	return m, nil
}

// applyLedgers verifies and cross-checks every ledger, then totals verified payouts per
// recipient. Ledgers may overlap (weekly plus final exports); the same payout may repeat but must
// be identical.
func applyLedgers(ledgers []*SignedPoolLedger, p *Policy, act *chainActivity) (map[string]int64, []LedgerRef, error) {
	poolPaid := make(map[string]int64)
	refs := make([]LedgerRef, 0, len(ledgers))
	seen := make(map[string]PoolPayout)
	paidByPool := make(map[string]int64)
	for _, l := range ledgers {
		if err := l.verifySignature(p); err != nil {
			return nil, nil, err
		}
		if err := l.crossCheck(act.transfers, p.CutoffHeight); err != nil {
			return nil, nil, err
		}
		refs = append(refs, LedgerRef{
			PoolAddress: l.Manifest.PoolAddress,
			SHA256:      l.SHA256,
			StartHeight: l.Manifest.StartHeight,
			EndHeight:   l.Manifest.EndHeight,
			Payouts:     len(l.Manifest.Payouts),
		})
		for _, pay := range l.Manifest.Payouts {
			if pay.Height > p.CutoffHeight {
				continue
			}
			if prev, dup := seen[pay.TxHash]; dup {
				if prev != pay {
					return nil, nil, fmt.Errorf("payout %s differs between ledgers", pay.TxHash)
				}
				continue
			}
			seen[pay.TxHash] = pay
			var err error
			if poolPaid[pay.Recipient], err = addArmoids(poolPaid[pay.Recipient], pay.Amount); err != nil {
				return nil, nil, err
			}
			if paidByPool[l.Manifest.PoolAddress], err = addArmoids(paidByPool[l.Manifest.PoolAddress], pay.Amount); err != nil {
				return nil, nil, err
			}
		}
	}
	// Pool coinbase rewards are the backing ceiling: a pool cannot have paid out more mined
	// value than it mined.
	for pool, paid := range paidByPool {
		if paid > act.solo[pool] {
			return nil, nil, fmt.Errorf("pool %s ledger payouts exceed its coinbase rewards up to the cutoff", pool)
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].PoolAddress != refs[j].PoolAddress {
			return refs[i].PoolAddress < refs[j].PoolAddress
		}
		return refs[i].SHA256 < refs[j].SHA256
	})
	return poolPaid, refs, nil
}

// classify assigns every address with activity, and every policy-listed address, to a category.
func classify(p *Policy, act *chainActivity, poolPaid map[string]int64) ([]Entry, error) {
	roles := p.roles()
	addrs := make(map[string]bool)
	for a := range act.solo {
		addrs[a] = true
	}
	for a := range act.transferIn {
		addrs[a] = true
	}
	for a := range poolPaid {
		addrs[a] = true
	}
	for a := range roles {
		addrs[a] = true
	}
	sorted := make([]string, 0, len(addrs))
	for a := range addrs {
		sorted = append(sorted, a)
	}
	sort.Strings(sorted)

	entries := make([]Entry, 0, len(sorted))
	for _, a := range sorted {
		e := Entry{
			SourceAddress:     a,
			SoloArmoids:       act.solo[a],
			PoolPayoutArmoids: poolPaid[a],
			TransferInArmoids: act.transferIn[a],
			Status:            StatusNone,
		}
		mined, err := addArmoids(e.SoloArmoids, e.PoolPayoutArmoids)
		if err != nil {
			return nil, err
		}
		e.Category = categoryFor(roles[a], e.SoloArmoids, e.PoolPayoutArmoids)
		if isEligibleCategory(e.Category) {
			e.EligibleArmoids = mined
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// applyCaps clips each wallet to the wallet cap and, if the capped total exceeds the program cap,
// scales every entitlement by program_cap / total with integer floor division. Pending entries
// reserve capacity too, so resolving them later never overdraws the program.
func applyCaps(entries []Entry, p *Policy) (Totals, error) {
	var t Totals
	total := new(big.Int)
	for i := range entries {
		e := &entries[i]
		e.CappedArmoids = min(e.EligibleArmoids, p.WalletCapArmoids)
		e.ExcessArmoids = e.EligibleArmoids - e.CappedArmoids
		total.Add(total, big.NewInt(e.CappedArmoids))
	}
	if !total.IsInt64() {
		return t, errors.New("capped total overflows")
	}
	t.CappedTotalArmoids = total.Int64()
	t.ProRataApplied = t.CappedTotalArmoids > p.ProgramCapArmoids
	var assigned int64
	for i := range entries {
		e := &entries[i]
		if !t.ProRataApplied {
			e.EntitlementArmoids = e.CappedArmoids
			continue
		}
		v := new(big.Int).Mul(big.NewInt(e.CappedArmoids), big.NewInt(p.ProgramCapArmoids))
		v.Quo(v, total)
		e.EntitlementArmoids = v.Int64()
		assigned += e.EntitlementArmoids
	}
	if t.ProRataApplied {
		t.RoundingRemainder = p.ProgramCapArmoids - assigned
	}
	return t, nil
}

// applyClaims verifies claims and sets each entry's status. Identical repeated claims count once;
// claims binding one source address to different targets put that address in escrow.
func applyClaims(entries []Entry, claims []*MigrationClaim, malformed []RejectedClaim, p *Policy) []RejectedClaim {
	rejected := append([]RejectedClaim{}, malformed...)
	index := make(map[string]int, len(entries))
	for i := range entries {
		index[entries[i].SourceAddress] = i
	}
	targets := make(map[string]map[string]bool)
	for _, c := range claims {
		if err := c.verify(p.TargetNetworkID, p.CutoffHeight); err != nil {
			rejected = append(rejected, RejectedClaim{c.SourceAddress, c.TargetAddress, err.Error()})
			continue
		}
		i, ok := index[c.SourceAddress]
		if !ok || entries[i].EntitlementArmoids == 0 && entries[i].Category != CategoryPending {
			rejected = append(rejected, RejectedClaim{c.SourceAddress, c.TargetAddress, "source address has no entitlement"})
			continue
		}
		if targets[c.SourceAddress] == nil {
			targets[c.SourceAddress] = make(map[string]bool)
		}
		targets[c.SourceAddress][c.TargetAddress] = true
	}
	for i := range entries {
		e := &entries[i]
		switch {
		case e.Category == CategoryPending:
			// Escrowed until ownership is verified; a single valid claim is kept so the entry can
			// settle without a new claim once it is resolved.
			e.Status = StatusPending
			if len(targets[e.SourceAddress]) == 1 {
				for t := range targets[e.SourceAddress] {
					e.TargetAddress = t
				}
			} else {
				for t := range targets[e.SourceAddress] {
					rejected = append(rejected, RejectedClaim{e.SourceAddress, t, "conflicting claims bind this source address to different targets"})
				}
			}
		case e.EntitlementArmoids == 0:
			e.Status = StatusNone
		case len(targets[e.SourceAddress]) > 1:
			e.Status = StatusClaimConflict
			for t := range targets[e.SourceAddress] {
				rejected = append(rejected, RejectedClaim{e.SourceAddress, t, "conflicting claims bind this source address to different targets"})
			}
		case len(targets[e.SourceAddress]) == 1:
			e.Status = StatusClaimed
			for t := range targets[e.SourceAddress] {
				e.TargetAddress = t
			}
		default:
			e.Status = StatusUnclaimed
		}
	}
	sort.Slice(rejected, func(i, j int) bool {
		a, b := rejected[i], rejected[j]
		if a.SourceAddress != b.SourceAddress {
			return a.SourceAddress < b.SourceAddress
		}
		if a.TargetAddress != b.TargetAddress {
			return a.TargetAddress < b.TargetAddress
		}
		return a.Reason < b.Reason
	})
	return rejected
}

// finishTotals sums entitlements by status.
func finishTotals(entries []Entry, t *Totals) error {
	for _, e := range entries {
		var err error
		if t.ReservedArmoids, err = addArmoids(t.ReservedArmoids, e.EntitlementArmoids); err != nil {
			return err
		}
		switch e.Status {
		case StatusClaimed:
			t.ClaimedArmoids, err = addArmoids(t.ClaimedArmoids, e.EntitlementArmoids)
		case StatusUnclaimed:
			t.UnclaimedArmoids, err = addArmoids(t.UnclaimedArmoids, e.EntitlementArmoids)
		case StatusClaimConflict, StatusPending:
			t.EscrowArmoids, err = addArmoids(t.EscrowArmoids, e.EntitlementArmoids)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// entriesRoot is the RFC 6962 Merkle root over the canonical JSON of each entry, in order.
func entriesRoot(entries []Entry) (string, error) {
	leaves := make([][]byte, 0, len(entries))
	for i := range entries {
		data, err := json.Marshal(entries[i])
		if err != nil {
			return "", err
		}
		leaves = append(leaves, data)
	}
	root := merkleRoot(leaves)
	return hex.EncodeToString(root[:]), nil
}

// checkEntryInvariants enforces the category and status rules that classify and applyClaims
// produce, so a hand-edited manifest cannot move value into a payable status.
func checkEntryInvariants(e *Entry, targetNetworkID string) error {
	mined, err := addArmoids(e.SoloArmoids, e.PoolPayoutArmoids)
	if err != nil {
		return err
	}
	// The category itself was already checked against the policy role by checkEntryRole.
	eligible := isEligibleCategory(e.Category)
	if eligible && e.EligibleArmoids != mined || !eligible && e.EligibleArmoids != 0 {
		return errors.New("eligible amount does not match the category")
	}
	switch e.Status {
	case StatusPending:
		if e.Category != CategoryPending {
			return errors.New("only pending_verification entries can be in pending escrow")
		}
	case StatusNone:
		if e.Category == CategoryPending || e.EntitlementArmoids != 0 || e.TargetAddress != "" {
			return errors.New("status none must carry no entitlement or target")
		}
	case StatusClaimed:
		if e.Category == CategoryPending || e.EntitlementArmoids == 0 {
			return errors.New("claimed entries must be eligible with an entitlement")
		}
	case StatusUnclaimed, StatusClaimConflict:
		if e.Category == CategoryPending || e.EntitlementArmoids == 0 || e.TargetAddress != "" {
			return errors.New("unclaimed or conflicting entries carry no target")
		}
	default:
		return fmt.Errorf("unknown status %q", e.Status)
	}
	if e.Status == StatusClaimed && e.TargetAddress == "" {
		return errors.New("claimed entries need a target address")
	}
	if e.TargetAddress != "" && !core.ValidateAddressForNetwork(e.TargetAddress, targetNetworkID) {
		return errors.New("target address is not on the target network")
	}
	return nil
}

// categoryFor maps a policy role and mined amounts to a 7-category matrix category. Policy roles
// win over mining history: a developer wallet that mined is still excluded. Transfers and faucet
// payouts never create entitlement on their own.
func categoryFor(role addressRole, solo, pool int64) string {
	switch role {
	case roleExcluded:
		return CategoryDevExcluded
	case rolePool:
		return CategoryPoolHotWallet
	case roleBurn, roleFaucet:
		return CategoryTransferOnly
	case rolePending:
		return CategoryPending
	}
	switch {
	case solo > 0 && pool > 0:
		return CategoryEligibleSoloPool
	case solo > 0:
		return CategoryEligibleSolo
	case pool > 0:
		return CategoryEligiblePool
	}
	return CategoryTransferOnly
}

// isEligibleCategory reports whether a category reserves program capacity.
func isEligibleCategory(c string) bool {
	switch c {
	case CategoryEligibleSolo, CategoryEligiblePool, CategoryEligibleSoloPool, CategoryPending:
		return true
	}
	return false
}

// checkEntryRole requires the category that classify assigns to an address with this policy role.
func checkEntryRole(e *Entry, role addressRole) error {
	if want := categoryFor(role, e.SoloArmoids, e.PoolPayoutArmoids); e.Category != want {
		return fmt.Errorf("category %q does not match the policy role (want %q)", e.Category, want)
	}
	return nil
}

// verifyManifest recomputes everything in a manifest that does not need the chain: the policy
// digest, entry order, caps and pro-rata scaling, status totals, and the Merkle root. A full
// independent check reruns reconcile on one's own node database and compares the output.
func verifyManifest(m *Manifest) error {
	if m.Version != ManifestVersion || m.Domain != ManifestDomain {
		return errors.New("unsupported manifest version or domain")
	}
	if m.FinalityDepth < FinalityDepth || m.TipHeight < m.Policy.CutoffHeight+m.FinalityDepth {
		return errors.New("manifest snapshot is not final")
	}
	if m.Policy.Digest() != m.PolicySHA256 {
		return errors.New("policy_sha256 does not match the embedded policy")
	}
	for i := 1; i < len(m.Entries); i++ {
		if m.Entries[i-1].SourceAddress >= m.Entries[i].SourceAddress {
			return errors.New("entries are not sorted by source address without duplicates")
		}
	}
	roles := m.Policy.roles()
	entries := make([]Entry, len(m.Entries))
	copy(entries, m.Entries)
	for i := range entries {
		if err := checkEntryRole(&entries[i], roles[entries[i].SourceAddress]); err != nil {
			return fmt.Errorf("entry %s: %w", entries[i].SourceAddress, err)
		}
	}
	for i := range entries {
		e := &entries[i]
		if e.SoloArmoids < 0 || e.PoolPayoutArmoids < 0 || e.TransferInArmoids < 0 || e.EligibleArmoids < 0 {
			return fmt.Errorf("entry %s has a negative amount", e.SourceAddress)
		}
		if err := checkEntryInvariants(e, m.Policy.TargetNetworkID); err != nil {
			return fmt.Errorf("entry %s: %w", e.SourceAddress, err)
		}
	}
	totals, err := applyCaps(entries, &m.Policy)
	if err != nil {
		return err
	}
	for i := range entries {
		if entries[i] != m.Entries[i] {
			return fmt.Errorf("entry %s caps or entitlement do not recompute", entries[i].SourceAddress)
		}
	}
	if err := finishTotals(entries, &totals); err != nil {
		return err
	}
	if totals != m.Totals {
		return errors.New("totals do not recompute")
	}
	root, err := entriesRoot(m.Entries)
	if err != nil {
		return err
	}
	if root != m.EntriesMerkleRoot {
		return errors.New("entries_merkle_root does not recompute")
	}
	return nil
}
