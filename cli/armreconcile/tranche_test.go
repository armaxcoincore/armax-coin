//go:build testhooks

package main

import (
	"strings"
	"testing"

	"github.com/armaxcoincore/armax-coin/core"
)

// regtestManifest reconciles the Testnet fixture with Regtest as the target network, so tranche
// planning can run end to end without unlocking Mainnet.
func regtestManifest(t *testing.T) (*Manifest, map[string]string) {
	t.Helper()
	if err := core.SetNetwork("testnet"); err != nil {
		t.Fatal(err)
	}
	f := newMigrationFixture(t)
	p := f.policy(t, armx(100), armx(1000))
	p.TargetNetworkID = "ARMX-REGTEST-1"
	if err := p.normalize(); err != nil {
		t.Fatal(err)
	}
	targets := map[string]string{
		"A": addrOn(t, testKey(t, "regtest:A"), "ARMX-REGTEST-1"),
		"B": addrOn(t, testKey(t, "regtest:B"), "ARMX-REGTEST-1"),
	}
	m, err := reconcile(reconcileInputs{
		DBPath:  f.db,
		Policy:  p,
		Ledgers: []*SignedPoolLedger{mustLedger(t, f.poolKey, f.ledger)},
		Claims: []*MigrationClaim{
			mustClaim(t, f.keys["soloA"], "ARMX-REGTEST-1", targets["A"], testCutoff),
			mustClaim(t, f.keys["poolB"], "ARMX-REGTEST-1", targets["B"], testCutoff),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, targets
}

func TestTranche_UsesOnlyFinalTreasuryInflowsAndPaysProRata(t *testing.T) {
	core.LockNetworkForTesting(t)
	m, targets := regtestManifest(t)

	if err := core.SetNetwork("regtest"); err != nil {
		t.Fatal(err)
	}
	treasury := addrOn(t, testKey(t, "regtest-treasury"), "")
	miner := addrOn(t, testKey(t, "regtest-miner"), "")
	c := newTestChain()
	for h := 1; h <= 20; h++ {
		tc := coinbase(treasury, armx(12))
		tc.Signature = treasuryRewardSignature
		c.add(coinbase(miner, armx(48)), tc)
	}
	// A treasury coinbase outside tx[1] or without the sentinel does not fund tranches.
	c.add(coinbase(treasury, armx(999)))
	paid := c.transfer(treasury, targets["A"], armx(10))
	c.add(coinbase(miner, armx(60)), paid)
	c.padTo(10 + FinalityDepth) // inflows #1..#10 are final, #11..#20 are not
	db := c.write(t)

	plan, err := buildTranche(trancheInputs{DBPath: db, Manifest: m, TreasuryAddress: treasury, TargetTotal: armx(1000)})
	if err != nil {
		t.Fatalf("tranche: %v", err)
	}
	if plan.ConfirmedThroughHeight != 10 || plan.ConfirmedInflowArmoids != armx(120) {
		t.Fatalf("only treasury coinbases at least %d deep count: %+v", FinalityDepth, plan)
	}
	if plan.OutflowArmoids != armx(10)+1000 || plan.AvailableArmoids != armx(110)-1000 {
		t.Fatalf("treasury spends must be subtracted with fees: %+v", plan)
	}
	// Targets at 1:1 (target total == program cap): A 60 (10 already paid), B 40.
	want := map[string]TranchePayout{
		targets["A"]: {TargetAddress: targets["A"], TargetArmoids: armx(60), PaidArmoids: armx(10), RemainingArmoids: armx(50), PayArmoids: armx(50)},
		targets["B"]: {TargetAddress: targets["B"], TargetArmoids: armx(40), PaidArmoids: 0, RemainingArmoids: armx(40), PayArmoids: armx(40)},
	}
	if len(plan.Payouts) != len(want) {
		t.Fatalf("payouts: %+v", plan.Payouts)
	}
	for _, p := range plan.Payouts {
		if p != want[p.TargetAddress] {
			t.Errorf("payout %+v, want %+v", p, want[p.TargetAddress])
		}
	}
	if plan.PlannedArmoids != armx(90) {
		t.Fatalf("planned %d", plan.PlannedArmoids)
	}

	// A smaller budget pays everyone the same fraction of what is still owed.
	plan, err = buildTranche(trancheInputs{DBPath: db, Manifest: m, TreasuryAddress: treasury, TargetTotal: armx(1000), Budget: armx(45)})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range plan.Payouts {
		if p.PayArmoids != p.RemainingArmoids/2 {
			t.Errorf("half budget must pay half of the remainder: %+v", p)
		}
	}
	if plan.PlannedArmoids > armx(45) {
		t.Fatalf("plan exceeds budget: %d", plan.PlannedArmoids)
	}

	if _, err := buildTranche(trancheInputs{DBPath: db, Manifest: m, TreasuryAddress: treasury, TargetTotal: armx(1000), Budget: armx(111)}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("a budget above the final unspent balance must be refused, got %v", err)
	}
}

func TestTranche_RefusesWrongNetworkOrTamperedManifest(t *testing.T) {
	core.LockNetworkForTesting(t)
	m, _ := regtestManifest(t)

	if err := core.SetNetwork("regtest"); err != nil {
		t.Fatal(err)
	}
	c := newTestChain()
	c.padTo(FinalityDepth + 1)
	db := c.write(t)
	treasury := addrOn(t, testKey(t, "regtest-treasury"), "")

	tampered := *m
	tampered.Entries = append([]Entry(nil), m.Entries...)
	for i := range tampered.Entries {
		if tampered.Entries[i].Status == StatusClaimed {
			tampered.Entries[i].EntitlementArmoids *= 2
			break
		}
	}
	if _, err := buildTranche(trancheInputs{DBPath: db, Manifest: &tampered, TreasuryAddress: treasury, TargetTotal: armx(1000)}); err == nil {
		t.Fatal("a tampered manifest must be refused")
	}

	if err := core.SetNetwork("soak"); err != nil {
		t.Fatal(err)
	}
	if _, err := buildTranche(trancheInputs{DBPath: db, Manifest: m, TreasuryAddress: treasury, TargetTotal: armx(1000)}); err == nil {
		t.Fatal("a manifest for another target network must be refused")
	}
}

func TestTranche_MainnetStaysLocked(t *testing.T) {
	core.LockNetworkForTesting(t)
	core.SetAllowMainnetForTesting(false)
	code := run([]string{"tranche", "-network", "mainnet", "-db", "x", "-manifest", "x", "-treasury", "x", "-out", "x"}, &strings.Builder{}, &strings.Builder{})
	if code == 0 {
		t.Fatal("tranche planning must not open Mainnet before launch")
	}
}
