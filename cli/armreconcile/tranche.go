package main

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/armaxcoincore/armax-coin/core"
)

const (
	// TrancheDomain identifies tranche plans.
	TrancheDomain = "ARMX-MIGRATION-TRANCHE-V1"

	// TrancheVersion is the only tranche plan version.
	TrancheVersion = 1

	// treasuryRewardSignature is the signature sentinel of the treasury coinbase (block tx[1])
	// defined by the Mainnet treasury subsidy split. Only those outputs fund tranches.
	treasuryRewardSignature = "TREASURY_REWARD_SIG"
)

// TranchePayout is one planned treasury payment to a bound target address.
type TranchePayout struct {
	TargetAddress    string `json:"target_address"`
	TargetArmoids    int64  `json:"target_armoids"`
	PaidArmoids      int64  `json:"paid_armoids"`
	RemainingArmoids int64  `json:"remaining_armoids"`
	PayArmoids       int64  `json:"pay_armoids"`
}

// TranchePlan is a deterministic staged-tranche plan. It only proposes payments; the treasury
// multisig signers still create and co-sign each spend.
type TranchePlan struct {
	Version                int             `json:"version"`
	Domain                 string          `json:"domain"`
	NetworkID              string          `json:"network_id"`
	ManifestMerkleRoot     string          `json:"manifest_merkle_root"`
	TreasuryAddress        string          `json:"treasury_address"`
	TargetTotalArmoids     int64           `json:"target_total_armoids"`
	TipHeight              int             `json:"tip_height"`
	TipHash                string          `json:"tip_hash"`
	ConfirmedThroughHeight int             `json:"confirmed_through_height"`
	ConfirmedInflowArmoids int64           `json:"confirmed_inflow_armoids"`
	OutflowArmoids         int64           `json:"outflow_armoids"`
	AvailableArmoids       int64           `json:"available_armoids"`
	BudgetArmoids          int64           `json:"budget_armoids"`
	PlannedArmoids         int64           `json:"planned_armoids"`
	Payouts                []TranchePayout `json:"payouts"`
}

// trancheInputs are the inputs of a tranche plan.
type trancheInputs struct {
	DBPath          string
	Manifest        *Manifest
	TreasuryAddress string
	// TargetTotal is the governance-approved size of the whole migration program in target
	// network Armoids. The program cap maps onto it, so a fully subscribed program pays exactly
	// TargetTotal; the tool never assumes a conversion ratio.
	TargetTotal int64
	// Budget caps this tranche; zero means everything confirmed and unspent.
	Budget int64
}

// buildTranche plans one tranche. Funding counts only treasury coinbase outputs buried at least
// FinalityDepth blocks below the tip, and every treasury spend already on chain (confirmed or not)
// is subtracted, so a plan never spends inflows a reorganization could still remove. Amounts
// already received by a target from the treasury count as paid.
func buildTranche(in trancheInputs) (*TranchePlan, error) {
	m := in.Manifest
	if err := verifyManifest(m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if m.Policy.TargetNetworkID != core.NetworkID() {
		return nil, fmt.Errorf("manifest targets %q, active network is %q", m.Policy.TargetNetworkID, core.NetworkID())
	}
	if !core.ValidateAddress(in.TreasuryAddress) {
		return nil, errors.New("treasury address is not a valid address of the active network")
	}
	if in.TargetTotal <= 0 || in.Budget < 0 {
		return nil, errors.New("target total must be positive and budget must not be negative")
	}

	targets, err := trancheTargets(m, in.TargetTotal)
	if err != nil {
		return nil, err
	}

	type inflow struct {
		height int
		amount int64
	}
	var inflows []inflow
	var outflow int64
	paid := make(map[string]int64)
	info, err := core.ScanCanonicalBlocks(in.DBPath, 5*time.Second, func(b *core.Block) error {
		for i := range b.Transactions {
			tx := &b.Transactions[i]
			var err error
			switch {
			case i == 1 && tx.Sender == "SYSTEM" && tx.Signature == treasuryRewardSignature && tx.Recipient == in.TreasuryAddress:
				if tx.Amount <= 0 {
					return fmt.Errorf("block %d treasury coinbase amount is not positive", b.Index)
				}
				inflows = append(inflows, inflow{b.Index, tx.Amount})
			case tx.Sender == in.TreasuryAddress:
				spent, e := addArmoids(tx.Amount, tx.Fee)
				if e == nil {
					outflow, e = addArmoids(outflow, spent)
				}
				if e == nil {
					if _, ok := targets[tx.Recipient]; ok {
						paid[tx.Recipient], e = addArmoids(paid[tx.Recipient], tx.Amount)
					}
				}
				err = e
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

	plan := &TranchePlan{
		Version:                TrancheVersion,
		Domain:                 TrancheDomain,
		NetworkID:              core.NetworkID(),
		ManifestMerkleRoot:     m.EntriesMerkleRoot,
		TreasuryAddress:        in.TreasuryAddress,
		TargetTotalArmoids:     in.TargetTotal,
		TipHeight:              info.TipHeight,
		TipHash:                info.TipHash,
		ConfirmedThroughHeight: info.TipHeight - FinalityDepth,
		OutflowArmoids:         outflow,
		Payouts:                []TranchePayout{},
	}
	for _, f := range inflows {
		if f.height <= plan.ConfirmedThroughHeight {
			if plan.ConfirmedInflowArmoids, err = addArmoids(plan.ConfirmedInflowArmoids, f.amount); err != nil {
				return nil, err
			}
		}
	}
	plan.AvailableArmoids = max(plan.ConfirmedInflowArmoids-outflow, 0)
	plan.BudgetArmoids = in.Budget
	if plan.BudgetArmoids == 0 {
		plan.BudgetArmoids = plan.AvailableArmoids
	}
	if plan.BudgetArmoids > plan.AvailableArmoids {
		return nil, fmt.Errorf("budget %d exceeds the confirmed unspent treasury balance %d", plan.BudgetArmoids, plan.AvailableArmoids)
	}

	addrs := make([]string, 0, len(targets))
	for a := range targets {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs)
	remainingTotal := new(big.Int)
	for _, a := range addrs {
		p := TranchePayout{TargetAddress: a, TargetArmoids: targets[a], PaidArmoids: paid[a]}
		p.RemainingArmoids = max(p.TargetArmoids-p.PaidArmoids, 0)
		remainingTotal.Add(remainingTotal, big.NewInt(p.RemainingArmoids))
		plan.Payouts = append(plan.Payouts, p)
	}
	// Pay everyone the same fraction of what they are still owed, rounding down, so no target
	// is ever paid ahead of another and the plan never exceeds the budget.
	fullyFunded := remainingTotal.Cmp(big.NewInt(plan.BudgetArmoids)) <= 0
	for i := range plan.Payouts {
		p := &plan.Payouts[i]
		if fullyFunded {
			p.PayArmoids = p.RemainingArmoids
		} else {
			v := new(big.Int).Mul(big.NewInt(p.RemainingArmoids), big.NewInt(plan.BudgetArmoids))
			p.PayArmoids = v.Quo(v, remainingTotal).Int64()
		}
		if plan.PlannedArmoids, err = addArmoids(plan.PlannedArmoids, p.PayArmoids); err != nil {
			return nil, err
		}
	}
	return plan, nil
}

// trancheTargets maps each claimed target address to its total in target network Armoids:
// floor(sum of its entitlements * targetTotal / program_cap). Several source addresses may bind
// the same target; their entitlements are summed before rounding.
func trancheTargets(m *Manifest, targetTotal int64) (map[string]int64, error) {
	sums := make(map[string]*big.Int)
	for _, e := range m.Entries {
		if e.Status != StatusClaimed {
			continue
		}
		if sums[e.TargetAddress] == nil {
			sums[e.TargetAddress] = new(big.Int)
		}
		sums[e.TargetAddress].Add(sums[e.TargetAddress], big.NewInt(e.EntitlementArmoids))
	}
	out := make(map[string]int64, len(sums))
	for a, s := range sums {
		v := new(big.Int).Mul(s, big.NewInt(targetTotal))
		v.Quo(v, big.NewInt(m.Policy.ProgramCapArmoids))
		if !v.IsInt64() {
			return nil, errors.New("tranche target overflows")
		}
		out[a] = v.Int64()
	}
	return out, nil
}
