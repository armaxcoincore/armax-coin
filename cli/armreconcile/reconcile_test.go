//go:build testhooks

package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/armaxcoincore/armax-coin/core"
	"github.com/armaxcoincore/armax-coin/core/pqsig"
)

const testCutoff = 10

// migrationFixture is a Testnet chain that exercises every category of the 7-category matrix.
type migrationFixture struct {
	db      string
	chain   *testChain
	keys    map[string]*pqsig.PrivateKey
	addr    map[string]string
	poolKey *pqsig.PrivateKey
	ledger  PoolLedgerManifest
	// poolTx are the hashes of the pool's on-chain payouts by label.
	poolTx map[string]core.Transaction
}

func newMigrationFixture(t *testing.T) *migrationFixture {
	t.Helper()
	f := &migrationFixture{
		chain:   newTestChain(),
		keys:    map[string]*pqsig.PrivateKey{},
		addr:    map[string]string{},
		poolKey: testKey(t, "pool-operator"),
		poolTx:  map[string]core.Transaction{},
	}
	for _, l := range []string{"soloA", "poolB", "mixedC", "devD", "pendingE", "faucetF", "transferG", "pool", "bigH"} {
		f.keys[l] = testKey(t, l)
		f.addr[l] = addrOn(t, f.keys[l], "")
	}
	a := f.addr
	c := f.chain
	c.add(coinbase(a["soloA"], armx(30))) // 1
	c.add(coinbase(a["soloA"], armx(30))) // 2
	c.add(coinbase(a["pool"], armx(100))) // 3
	c.add(coinbase(a["pool"], armx(100))) // 4
	f.poolTx["B1"] = c.transfer(a["pool"], a["poolB"], armx(40))
	f.poolTx["C1"] = c.transfer(a["pool"], a["mixedC"], armx(20))
	c.add(coinbase(a["devD"], armx(1)), f.poolTx["B1"], f.poolTx["C1"]) // 5
	// A pool send that no ledger lists is an ordinary transfer.
	f.poolTx["G1"] = c.transfer(a["pool"], a["transferG"], armx(10))
	c.add(coinbase(a["mixedC"], armx(15)), f.poolTx["G1"])                                     // 6
	c.add(coinbase(a["devD"], armx(50)))                                                       // 7
	c.add(coinbase(a["pendingE"], armx(25)))                                                   // 8
	c.add(coinbase(a["faucetF"], armx(80)), c.transfer(a["faucetF"], a["transferG"], armx(5))) // 9
	c.add(coinbase(a["bigH"], armx(500)))                                                      // 10 = cutoff
	c.add(coinbase(a["soloA"], armx(30)))                                                      // 11: after the cutoff
	f.poolTx["B2"] = c.transfer(a["pool"], a["poolB"], armx(5))
	c.add(coinbase(a["pool"], armx(1)), f.poolTx["B2"]) // 12: after the cutoff
	c.padTo(testCutoff + FinalityDepth)
	f.db = c.write(t)

	f.ledger = PoolLedgerManifest{
		PoolAddress: a["pool"],
		StartHeight: 1,
		EndHeight:   12,
		Payouts:     []PoolPayout{},
	}
	for _, l := range []string{"B1", "C1", "B2"} {
		tx := f.poolTx[l]
		h := 5
		if l == "B2" {
			h = 12
		}
		f.ledger.Payouts = append(f.ledger.Payouts, PoolPayout{Height: h, TxHash: tx.HashHex(), Recipient: tx.Recipient, Amount: tx.Amount})
	}
	sortPayouts(f.ledger.Payouts)
	return f
}

func sortPayouts(p []PoolPayout) {
	for i := 1; i < len(p); i++ {
		for j := i; j > 0 && (p[j].Height < p[j-1].Height || p[j].Height == p[j-1].Height && p[j].TxHash < p[j-1].TxHash); j-- {
			p[j], p[j-1] = p[j-1], p[j]
		}
	}
}

func (f *migrationFixture) policy(t *testing.T, walletCap, programCap int64) *Policy {
	return testPolicy(t, Policy{
		CutoffHeight:      testCutoff,
		WalletCapArmoids:  walletCap,
		ProgramCapArmoids: programCap,
		ExcludedAddresses: []string{f.addr["devD"]},
		PendingAddresses:  []string{f.addr["pendingE"]},
		FaucetAddresses:   []string{f.addr["faucetF"]},
		Pools:             []PoolPolicy{{Address: f.addr["pool"], OperatorPublicKeys: []string{hexKey(f.poolKey)}}},
	})
}

func hexKey(k *pqsig.PrivateKey) string {
	var b strings.Builder
	const digits = "0123456789abcdef"
	for _, c := range k.PublicKeyBytes() {
		b.WriteByte(digits[c>>4])
		b.WriteByte(digits[c&0x0f])
	}
	return b.String()
}

func mainnetAddr(t *testing.T, label string) string {
	return addrOn(t, testKey(t, "mainnet:"+label), "ARMX-MAINNET-1")
}

func entryFor(t *testing.T, m *Manifest, addr string) Entry {
	t.Helper()
	for _, e := range m.Entries {
		if e.SourceAddress == addr {
			return e
		}
	}
	t.Fatalf("no manifest entry for %s", addr)
	return Entry{}
}

func (f *migrationFixture) claims(t *testing.T) []*MigrationClaim {
	k := f.keys
	return []*MigrationClaim{
		mustClaim(t, k["soloA"], "ARMX-MAINNET-1", mainnetAddr(t, "A"), testCutoff),
		// The same binding submitted twice counts once.
		mustClaim(t, k["poolB"], "ARMX-MAINNET-1", mainnetAddr(t, "B"), testCutoff),
		mustClaim(t, k["poolB"], "ARMX-MAINNET-1", mainnetAddr(t, "B"), testCutoff),
		// Two different bindings for one source address put it in escrow.
		mustClaim(t, k["mixedC"], "ARMX-MAINNET-1", mainnetAddr(t, "C1"), testCutoff),
		mustClaim(t, k["mixedC"], "ARMX-MAINNET-1", mainnetAddr(t, "C2"), testCutoff),
		mustClaim(t, k["pendingE"], "ARMX-MAINNET-1", mainnetAddr(t, "E"), testCutoff),
		mustClaim(t, k["transferG"], "ARMX-MAINNET-1", mainnetAddr(t, "G"), testCutoff),
		mustClaim(t, k["bigH"], "ARMX-MAINNET-1", mainnetAddr(t, "H"), testCutoff+1),
	}
}

func TestReconcile_SevenCategoryMatrixCapsAndClaims(t *testing.T) {
	core.LockNetworkForTesting(t)
	if err := core.SetNetwork("testnet"); err != nil {
		t.Fatal(err)
	}
	f := newMigrationFixture(t)
	p := f.policy(t, armx(100), armx(1000))
	in := reconcileInputs{
		DBPath:  f.db,
		Policy:  p,
		Ledgers: []*SignedPoolLedger{mustLedger(t, f.poolKey, f.ledger)},
		Claims:  f.claims(t),
	}
	m, err := reconcile(in)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	a := f.addr
	want := []struct {
		label, category, status string
		solo, pool, entitlement int64
		target                  string
	}{
		{"soloA", CategoryEligibleSolo, StatusClaimed, armx(60), 0, armx(60), mainnetAddr(t, "A")},
		{"poolB", CategoryEligiblePool, StatusClaimed, 0, armx(40), armx(40), mainnetAddr(t, "B")},
		{"mixedC", CategoryEligibleSoloPool, StatusClaimConflict, armx(15), armx(20), armx(35), ""},
		{"devD", CategoryDevExcluded, StatusNone, armx(51), 0, 0, ""},
		{"pendingE", CategoryPending, StatusPending, armx(25), 0, armx(25), mainnetAddr(t, "E")},
		{"faucetF", CategoryTransferOnly, StatusNone, armx(80), 0, 0, ""},
		{"transferG", CategoryTransferOnly, StatusNone, 0, 0, 0, ""},
		{"pool", CategoryPoolHotWallet, StatusNone, armx(200), 0, 0, ""},
		{"bigH", CategoryEligibleSolo, StatusUnclaimed, armx(500), 0, armx(100), ""},
	}
	for _, w := range want {
		e := entryFor(t, m, a[w.label])
		if e.Category != w.category || e.Status != w.status || e.SoloArmoids != w.solo || e.PoolPayoutArmoids != w.pool || e.EntitlementArmoids != w.entitlement || e.TargetAddress != w.target {
			t.Errorf("%s: got %+v, want category=%s status=%s solo=%d pool=%d entitlement=%d target=%q",
				w.label, e, w.category, w.status, w.solo, w.pool, w.entitlement, w.target)
		}
	}
	if h := entryFor(t, m, a["bigH"]); h.CappedArmoids != armx(100) || h.ExcessArmoids != armx(400) {
		t.Errorf("wallet cap must clip only the surplus: %+v", h)
	}
	if g := entryFor(t, m, a["transferG"]); g.TransferInArmoids != armx(15) {
		t.Errorf("transferG must record 10 (unlisted pool send) + 5 (faucet) received: %+v", g)
	}
	burn := entryFor(t, m, core.GenesisMinerForNetwork(""))
	if burn.Category != CategoryTransferOnly || burn.EntitlementArmoids != 0 {
		t.Errorf("burn address must be excluded: %+v", burn)
	}

	wantTotals := Totals{
		ReservedArmoids:    armx(260),
		ClaimedArmoids:     armx(100),
		UnclaimedArmoids:   armx(100),
		EscrowArmoids:      armx(60),
		CappedTotalArmoids: armx(260),
	}
	if m.Totals != wantTotals {
		t.Errorf("totals: got %+v, want %+v", m.Totals, wantTotals)
	}

	reasons := map[string]int{}
	for _, r := range m.RejectedClaims {
		switch {
		case r.SourceAddress == a["transferG"] && strings.Contains(r.Reason, "no entitlement"):
			reasons["G"]++
		case r.SourceAddress == a["bigH"] && strings.Contains(r.Reason, "cutoff"):
			reasons["H"]++
		case r.SourceAddress == a["mixedC"] && strings.Contains(r.Reason, "conflicting"):
			reasons["C"]++
		default:
			t.Errorf("unexpected rejected claim %+v", r)
		}
	}
	if reasons["G"] != 1 || reasons["H"] != 1 || reasons["C"] != 2 {
		t.Errorf("rejected claims: %v (%+v)", reasons, m.RejectedClaims)
	}
	if m.CutoffBlockHash != f.chain.blocks[testCutoff].Hash || m.TipHeight != testCutoff+FinalityDepth {
		t.Errorf("manifest chain anchors are wrong: cutoff %s tip %d", m.CutoffBlockHash, m.TipHeight)
	}
	if err := verifyManifest(m); err != nil {
		t.Fatalf("verifyManifest: %v", err)
	}

	// Same inputs, in another order, give a byte-identical manifest.
	in.Claims = append([]*MigrationClaim(nil), in.Claims...)
	for i, j := 0, len(in.Claims)-1; i < j; i, j = i+1, j-1 {
		in.Claims[i], in.Claims[j] = in.Claims[j], in.Claims[i]
	}
	m2, err := reconcile(in)
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := json.Marshal(m)
	b2, _ := json.Marshal(m2)
	if !bytes.Equal(b1, b2) {
		t.Fatal("reconcile is not deterministic")
	}
}

func TestReconcile_ProRataScalesWithIntegerFloor(t *testing.T) {
	core.LockNetworkForTesting(t)
	if err := core.SetNetwork("testnet"); err != nil {
		t.Fatal(err)
	}
	f := newMigrationFixture(t)
	p := f.policy(t, armx(100), armx(200))
	m, err := reconcile(reconcileInputs{DBPath: f.db, Policy: p, Ledgers: []*SignedPoolLedger{mustLedger(t, f.poolKey, f.ledger)}})
	if err != nil {
		t.Fatal(err)
	}
	if !m.Totals.ProRataApplied {
		t.Fatal("capped total 260 over program cap 200 must scale")
	}
	// floor(capped * 200 / 260) for capped = 60, 40, 35, 25, 100.
	want := map[string]int64{"soloA": 4615384615, "poolB": 3076923076, "mixedC": 2692307692, "pendingE": 1923076923, "bigH": 7692307692}
	var sum int64
	for label, v := range want {
		if got := entryFor(t, m, f.addr[label]).EntitlementArmoids; got != v {
			t.Errorf("%s entitlement %d, want %d", label, got, v)
		}
		sum += v
	}
	if sum+m.Totals.RoundingRemainder != armx(200) || m.Totals.RoundingRemainder < 0 || m.Totals.ReservedArmoids != sum {
		t.Errorf("scaled entitlements %d + remainder %d must equal the program cap", sum, m.Totals.RoundingRemainder)
	}
	if err := verifyManifest(m); err != nil {
		t.Fatal(err)
	}
}

func TestReconcile_RequiresFinality(t *testing.T) {
	core.LockNetworkForTesting(t)
	if err := core.SetNetwork("testnet"); err != nil {
		t.Fatal(err)
	}
	c := newTestChain()
	k := testKey(t, "miner")
	c.add(coinbase(addrOn(t, k, ""), armx(10)))
	c.padTo(testCutoff + FinalityDepth - 1)
	p := testPolicy(t, Policy{CutoffHeight: testCutoff, WalletCapArmoids: armx(1), ProgramCapArmoids: armx(1)})
	if _, err := reconcile(reconcileInputs{DBPath: c.write(t), Policy: p}); err == nil || !strings.Contains(err.Error(), "not final") {
		t.Fatalf("a snapshot one block short of cutoff+%d must be refused, got %v", FinalityDepth, err)
	}
}

func TestReconcile_LedgersFailClosed(t *testing.T) {
	core.LockNetworkForTesting(t)
	if err := core.SetNetwork("testnet"); err != nil {
		t.Fatal(err)
	}
	f := newMigrationFixture(t)
	p := f.policy(t, armx(100), armx(1000))
	run := func(l *SignedPoolLedger) error {
		_, err := reconcile(reconcileInputs{DBPath: f.db, Policy: p, Ledgers: []*SignedPoolLedger{l}})
		return err
	}

	t.Run("untrusted key", func(t *testing.T) {
		if err := run(mustLedger(t, testKey(t, "impostor"), f.ledger)); err == nil || !strings.Contains(err.Error(), "does not list") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("tampered after signing", func(t *testing.T) {
		l := mustLedger(t, f.poolKey, f.ledger)
		l.Manifest.Payouts[0].Amount++
		if err := run(l); err == nil || !strings.Contains(err.Error(), "sha256") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("digest recomputed but signature stale", func(t *testing.T) {
		l := mustLedger(t, f.poolKey, f.ledger)
		l.Manifest.Payouts[0].Amount++
		d, _ := l.Manifest.digest()
		l.SHA256 = hexBytes(d[:])
		if err := run(l); err == nil || !strings.Contains(err.Error(), "signature is invalid") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("payout differs from chain", func(t *testing.T) {
		m := f.ledger
		m.Payouts = append([]PoolPayout(nil), f.ledger.Payouts...)
		m.Payouts[0].Amount++
		if err := run(mustLedger(t, f.poolKey, m)); err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("payout not on chain", func(t *testing.T) {
		m := f.ledger
		m.Payouts = append([]PoolPayout(nil), f.ledger.Payouts...)
		m.Payouts[0].TxHash = strings.Repeat("0", 64)
		sortPayouts(m.Payouts)
		if err := run(mustLedger(t, f.poolKey, m)); err == nil || !strings.Contains(err.Error(), "not on the canonical chain") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("unsorted payouts", func(t *testing.T) {
		m := f.ledger
		m.Payouts = append([]PoolPayout(nil), f.ledger.Payouts...)
		m.Payouts[0], m.Payouts[1] = m.Payouts[1], m.Payouts[0]
		m.Version, m.Domain, m.NetworkID = PoolLedgerVersion, PoolLedgerDomain, core.NetworkID()
		if _, err := signPoolLedger(m, f.poolKey); err == nil || !strings.Contains(err.Error(), "sorted") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("overlapping exports must agree", func(t *testing.T) {
		weekly := f.ledger
		weekly.EndHeight = 5
		weekly.Payouts = f.ledger.Payouts[:2]
		_, err := reconcile(reconcileInputs{DBPath: f.db, Policy: p, Ledgers: []*SignedPoolLedger{
			mustLedger(t, f.poolKey, weekly), mustLedger(t, f.poolKey, f.ledger),
		}})
		if err != nil {
			t.Fatalf("identical overlapping payouts must be accepted once: %v", err)
		}
	})
}

func TestReconcile_PoolPayoutsBoundedByPoolCoinbase(t *testing.T) {
	core.LockNetworkForTesting(t)
	if err := core.SetNetwork("testnet"); err != nil {
		t.Fatal(err)
	}
	pool, miner, op := testKey(t, "pool2"), testKey(t, "miner2"), testKey(t, "op2")
	poolAddr, minerAddr := addrOn(t, pool, ""), addrOn(t, miner, "")
	c := newTestChain()
	c.add(coinbase(poolAddr, armx(10)))
	// The pool pays out more than it mined (funded by a transfer, which is not mining value).
	tx := c.transfer(poolAddr, minerAddr, armx(20))
	c.add(coinbase(minerAddr, armx(1)), tx)
	c.padTo(testCutoff + FinalityDepth)
	p := testPolicy(t, Policy{
		CutoffHeight: testCutoff, WalletCapArmoids: armx(100), ProgramCapArmoids: armx(1000),
		Pools: []PoolPolicy{{Address: poolAddr, OperatorPublicKeys: []string{hexKey(op)}}},
	})
	l := mustLedger(t, op, PoolLedgerManifest{PoolAddress: poolAddr, StartHeight: 1, EndHeight: testCutoff,
		Payouts: []PoolPayout{{Height: 2, TxHash: tx.HashHex(), Recipient: minerAddr, Amount: armx(20)}}})
	_, err := reconcile(reconcileInputs{DBPath: c.write(t), Policy: p, Ledgers: []*SignedPoolLedger{l}})
	if err == nil || !strings.Contains(err.Error(), "exceed its coinbase") {
		t.Fatalf("payouts above the pool's coinbase rewards must be refused, got %v", err)
	}
}

func TestClaim_BindingAndSignatureChecks(t *testing.T) {
	core.LockNetworkForTesting(t)
	if err := core.SetNetwork("testnet"); err != nil {
		t.Fatal(err)
	}
	k := testKey(t, "claimer")
	target := mainnetAddr(t, "claimer")
	good := mustClaim(t, k, "ARMX-MAINNET-1", target, testCutoff)
	if err := good.verify("ARMX-MAINNET-1", testCutoff); err != nil {
		t.Fatalf("valid claim: %v", err)
	}

	mutations := map[string]func(c *MigrationClaim){
		"retargeted address": func(c *MigrationClaim) { c.TargetAddress = mainnetAddr(t, "thief") },
		"other source":       func(c *MigrationClaim) { c.SourceAddress = addrOn(t, testKey(t, "other"), "") },
		"other cutoff":       func(c *MigrationClaim) { c.CutoffHeight++ },
		"testnet target":     func(c *MigrationClaim) { c.TargetAddress = addrOn(t, testKey(t, "x"), "") },
		"wrong domain":       func(c *MigrationClaim) { c.Domain = "ARMX-TX-V1" },
	}
	for name, mutate := range mutations {
		c := *good
		mutate(&c)
		if err := c.verify("ARMX-MAINNET-1", c.CutoffHeight); err == nil {
			t.Errorf("%s: claim must be rejected", name)
		}
	}

	// A transaction-context signature over the claim message is not a claim.
	c := *good
	sig, err := k.Sign(claimMessage(c.TargetNetworkID, c.TargetAddress, c.CutoffHeight))
	if err != nil {
		t.Fatal(err)
	}
	c.Signature = hexBytes(sig)
	if err := c.verify("ARMX-MAINNET-1", testCutoff); err == nil {
		t.Fatal("a signature under the transaction context must not authorize a claim")
	}

	if _, err := signMigrationClaim(k, "ARMX-TESTNET-1", addrOn(t, k, ""), testCutoff); err == nil {
		t.Fatal("a claim must not target its own source network")
	}
}

func TestManifestVerify_DetectsTampering(t *testing.T) {
	core.LockNetworkForTesting(t)
	if err := core.SetNetwork("testnet"); err != nil {
		t.Fatal(err)
	}
	f := newMigrationFixture(t)
	m, err := reconcile(reconcileInputs{
		DBPath: f.db, Policy: f.policy(t, armx(100), armx(1000)),
		Ledgers: []*SignedPoolLedger{mustLedger(t, f.poolKey, f.ledger)}, Claims: f.claims(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	idx := func(label string) int {
		for i, e := range m.Entries {
			if e.SourceAddress == f.addr[label] {
				return i
			}
		}
		t.Fatalf("missing %s", label)
		return -1
	}
	cases := map[string]func(m *Manifest){
		"raise entitlement": func(m *Manifest) { m.Entries[idx("soloA")].EntitlementArmoids++ },
		"promote dev wallet": func(m *Manifest) {
			e := &m.Entries[idx("devD")]
			e.Category, e.EligibleArmoids = CategoryEligibleSolo, e.SoloArmoids
		},
		"release pending escrow":  func(m *Manifest) { m.Entries[idx("pendingE")].Status = StatusClaimed },
		"redirect claimed target": func(m *Manifest) { m.Entries[idx("soloA")].TargetAddress = mainnetAddr(t, "thief") },
		"edit policy":             func(m *Manifest) { m.Policy.WalletCapArmoids++ },
		"drop finality":           func(m *Manifest) { m.TipHeight = m.Policy.CutoffHeight },
		"claim an unclaimed wallet": func(m *Manifest) {
			e := &m.Entries[idx("bigH")]
			e.Status, e.TargetAddress = StatusClaimed, mainnetAddr(t, "thief")
		},
	}
	for name, mutate := range cases {
		var cp Manifest
		data, _ := json.Marshal(m)
		if err := json.Unmarshal(data, &cp); err != nil {
			t.Fatal(err)
		}
		mutate(&cp)
		if err := verifyManifest(&cp); err == nil {
			t.Errorf("%s: tampered manifest must not verify", name)
		}
	}
}

func TestCLI_EndToEndWithFiles(t *testing.T) {
	core.LockNetworkForTesting(t)
	t.Setenv(passphraseEnvVar, "correct horse battery staple")
	if err := core.SetNetwork("testnet"); err != nil {
		t.Fatal(err)
	}
	f := newMigrationFixture(t)
	dir := t.TempDir()
	p := f.policy(t, armx(100), armx(1000))
	policyPath := filepath.Join(dir, "policy.json")
	if err := writeJSONFile(policyPath, p); err != nil {
		t.Fatal(err)
	}
	ledgerDir, claimDir := filepath.Join(dir, "ledgers"), filepath.Join(dir, "claims")
	for _, d := range []string{ledgerDir, claimDir} {
		if err := mkdir(d); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeJSONFile(filepath.Join(ledgerDir, "final.json"), mustLedger(t, f.poolKey, f.ledger)); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONFile(filepath.Join(claimDir, "a.json"), mustClaim(t, f.keys["soloA"], "ARMX-MAINNET-1", mainnetAddr(t, "A"), testCutoff)); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(claimDir, "junk.json"), []byte(`{"version":1,"unknown":true}`)); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "manifest.json")
	var stdout, stderr bytes.Buffer
	args := []string{"reconcile", "-db", f.db, "-policy", policyPath, "-ledgers", ledgerDir, "-claims", claimDir, "-out", out}
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("reconcile exit %d: %s", code, stderr.String())
	}
	if code := run([]string{"manifest-verify", "-manifest", out}, &stdout, &stderr); code != 0 {
		t.Fatalf("manifest-verify exit %d: %s", code, stderr.String())
	}
	data, err := readFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(dir)) || bytes.Contains(data, []byte(filepath.ToSlash(dir))) {
		t.Fatal("the published manifest must not contain local paths")
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if e := entryFor(t, &m, f.addr["soloA"]); e.Status != StatusClaimed {
		t.Fatalf("soloA must be claimed via the claim file: %+v", e)
	}
	malformed := 0
	for _, r := range m.RejectedClaims {
		if strings.HasPrefix(r.Reason, "malformed claim file") {
			malformed++
		}
	}
	if malformed != 1 {
		t.Fatalf("the junk claim file must be reported once, got %+v", m.RejectedClaims)
	}
	if code := run([]string{"reconcile", "-db", f.db, "-policy", policyPath, "-out", filepath.Join(dir, "x.json"), "stray"}, &stdout, &stderr); code == 0 {
		t.Fatal("positional arguments must be rejected")
	}
}
