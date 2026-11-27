// Command armreconcile is the offline Testnet-to-Mainnet migration toolkit.
//
// It signs and verifies pool ledger exports and migration claims, reconciles a final Testnet
// snapshot into a deterministic Merkle-committed manifest, and plans staged Mainnet treasury
// tranches. It only reads node databases (read-only, so stop the node or use a copy), never
// broadcasts, and cannot enable Mainnet: a Mainnet tranche plan needs a launched Mainnet profile.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/armaxcoincore/armax-coin/core"
	"github.com/armaxcoincore/armax-coin/wallet"
)

// passphraseEnvVar is read for keystore passphrases before prompting, like armsend.
const passphraseEnvVar = "ARMX_WALLET_PASSPHRASE"

const usage = `armreconcile - offline Testnet-to-Mainnet migration toolkit

Usage:
  armreconcile version
  armreconcile ledger-sign     -in ledger.json -keystore pool.json -out signed.json
  armreconcile ledger-verify   -ledger signed.json -policy policy.json -db blockchain.db
  armreconcile claim-sign      -keystore wallet.json -target-address ax1... -cutoff H -out claim.json
  armreconcile claim-verify    -claim claim.json -cutoff H
  armreconcile reconcile       -db blockchain.db -policy policy.json [-ledgers DIR] [-claims DIR] -out manifest.json
  armreconcile manifest-verify -manifest manifest.json
  armreconcile tranche         -network mainnet -db blockchain.db -manifest manifest.json
                               -treasury ADDRESS -target-total ARMOIDS [-budget ARMOIDS] -out plan.json

Every subcommand accepts -network (default testnet, the migration source network).
Keystore passphrases are read from ` + passphraseEnvVar + ` or prompted for.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches a subcommand and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version", "-version", "--version":
		fmt.Fprintf(stdout, "armreconcile v%s\n", core.Version)
		return 0
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	handlers := map[string]func([]string, io.Writer) error{
		"ledger-sign":     cmdLedgerSign,
		"ledger-verify":   cmdLedgerVerify,
		"claim-sign":      cmdClaimSign,
		"claim-verify":    cmdClaimVerify,
		"reconcile":       cmdReconcile,
		"manifest-verify": cmdManifestVerify,
		"tranche":         cmdTranche,
	}
	h, ok := handlers[cmd]
	if !ok {
		fmt.Fprintf(stderr, "unknown subcommand %q\n\n%s", cmd, usage)
		return 2
	}
	if err := h(rest, stdout); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(stderr, "armreconcile %s: %v\n", cmd, err)
		return 1
	}
	return 0
}

// newFlags returns a flag set with the shared -network flag.
func newFlags(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	network := fs.String("network", "testnet", "network profile of the input database and keys")
	return fs, network
}

// parseAndSetNetwork parses flags, rejects positional arguments, and selects the network.
func parseAndSetNetwork(fs *flag.FlagSet, network *string, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	return core.SetNetwork(*network)
}

func requireFlags(pairs ...string) error {
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] == "" {
			return fmt.Errorf("-%s is required", pairs[i])
		}
	}
	return nil
}

// loadKeystore unlocks an ML-DSA-65 keystore. The caller must Zeroize the wallet.
func loadKeystore(path string) (*wallet.Wallet, error) {
	pass, err := wallet.PromptPassphrase(passphraseEnvVar, "Enter keystore passphrase: ")
	if err != nil {
		return nil, err
	}
	return wallet.DecryptAndLoadKeystore(pass, path)
}

func cmdLedgerSign(args []string, stdout io.Writer) error {
	fs, network := newFlags("ledger-sign")
	in := fs.String("in", "", "unsigned pool ledger manifest (JSON)")
	keystore := fs.String("keystore", "", "pool operator keystore")
	out := fs.String("out", "", "signed ledger output path")
	if err := parseAndSetNetwork(fs, network, args); err != nil {
		return err
	}
	if err := requireFlags("in", *in, "keystore", *keystore, "out", *out); err != nil {
		return err
	}
	var m PoolLedgerManifest
	if err := readStrictJSON(*in, &m); err != nil {
		return err
	}
	w, err := loadKeystore(*keystore)
	if err != nil {
		return err
	}
	defer w.Zeroize()
	signed, err := signPoolLedger(m, w.PrivateKey)
	if err != nil {
		return err
	}
	if err := writeJSONFile(*out, signed); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "signed pool ledger %s (%d payouts), sha256 %s\n", m.PoolAddress, len(m.Payouts), signed.SHA256)
	return nil
}

func cmdLedgerVerify(args []string, stdout io.Writer) error {
	fs, network := newFlags("ledger-verify")
	ledgerPath := fs.String("ledger", "", "signed pool ledger")
	policyPath := fs.String("policy", "", "migration policy (JSON)")
	db := fs.String("db", "", "source network blockchain.db (read-only)")
	if err := parseAndSetNetwork(fs, network, args); err != nil {
		return err
	}
	if err := requireFlags("ledger", *ledgerPath, "policy", *policyPath, "db", *db); err != nil {
		return err
	}
	policy, err := loadPolicy(*policyPath)
	if err != nil {
		return err
	}
	var l SignedPoolLedger
	if err := readStrictJSON(*ledgerPath, &l); err != nil {
		return err
	}
	if err := l.verifySignature(policy); err != nil {
		return err
	}
	act, err := scanChain(*db, policy)
	if err != nil {
		return err
	}
	if err := l.crossCheck(act.transfers, policy.CutoffHeight); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "pool ledger %s OK: signature valid, payouts up to #%d match the canonical chain (tip #%d %s)\n",
		l.SHA256, policy.CutoffHeight, act.info.TipHeight, act.info.TipHash)
	return nil
}

func cmdClaimSign(args []string, stdout io.Writer) error {
	fs, network := newFlags("claim-sign")
	keystore := fs.String("keystore", "", "eligible source network wallet keystore")
	target := fs.String("target-address", "", "new target network address that receives the payout")
	targetNet := fs.String("target-network", "ARMX-MAINNET-1", "target network ID")
	cutoff := fs.Int("cutoff", 0, "announced snapshot cutoff height")
	out := fs.String("out", "", "claim output path")
	if err := parseAndSetNetwork(fs, network, args); err != nil {
		return err
	}
	if err := requireFlags("keystore", *keystore, "target-address", *target, "out", *out); err != nil {
		return err
	}
	if err := checkClaimTarget(*targetNet, *target, *cutoff); err != nil {
		return err
	}
	w, err := loadKeystore(*keystore)
	if err != nil {
		return err
	}
	defer w.Zeroize()
	c, err := signMigrationClaim(w.PrivateKey, *targetNet, *target, *cutoff)
	if err != nil {
		return err
	}
	if err := writeJSONFile(*out, c); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "claim signed: %s -> %s at cutoff #%d\n", c.SourceAddress, c.TargetAddress, c.CutoffHeight)
	return nil
}

func cmdClaimVerify(args []string, stdout io.Writer) error {
	fs, network := newFlags("claim-verify")
	claimPath := fs.String("claim", "", "claim file")
	targetNet := fs.String("target-network", "ARMX-MAINNET-1", "expected target network ID")
	cutoff := fs.Int("cutoff", 0, "announced snapshot cutoff height")
	if err := parseAndSetNetwork(fs, network, args); err != nil {
		return err
	}
	if err := requireFlags("claim", *claimPath); err != nil {
		return err
	}
	var c MigrationClaim
	if err := readStrictJSON(*claimPath, &c); err != nil {
		return err
	}
	if err := c.verify(*targetNet, *cutoff); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "claim OK: %s -> %s at cutoff #%d\n", c.SourceAddress, c.TargetAddress, c.CutoffHeight)
	return nil
}

// jsonFilesIn lists *.json files in dir in name order; an empty dir argument means none.
func jsonFilesIn(dir string) ([]string, error) {
	if dir == "" {
		return nil, nil
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	return matches, nil
}

func cmdReconcile(args []string, stdout io.Writer) error {
	fs, network := newFlags("reconcile")
	db := fs.String("db", "", "source network blockchain.db (read-only)")
	policyPath := fs.String("policy", "", "migration policy (JSON)")
	ledgerDir := fs.String("ledgers", "", "directory of signed pool ledgers (*.json)")
	claimDir := fs.String("claims", "", "directory of migration claims (*.json)")
	out := fs.String("out", "", "manifest output path")
	if err := parseAndSetNetwork(fs, network, args); err != nil {
		return err
	}
	if err := requireFlags("db", *db, "policy", *policyPath, "out", *out); err != nil {
		return err
	}
	policy, err := loadPolicy(*policyPath)
	if err != nil {
		return err
	}
	in := reconcileInputs{DBPath: *db, Policy: policy}
	ledgerFiles, err := jsonFilesIn(*ledgerDir)
	if err != nil {
		return err
	}
	for _, f := range ledgerFiles {
		l := new(SignedPoolLedger)
		if err := readStrictJSON(f, l); err != nil {
			return err
		}
		in.Ledgers = append(in.Ledgers, l)
	}
	claimFiles, err := jsonFilesIn(*claimDir)
	if err != nil {
		return err
	}
	for _, f := range claimFiles {
		c := new(MigrationClaim)
		if err := readStrictJSON(f, c); err != nil {
			// A malformed user submission must not block the run; it is reported instead.
			in.Malformed = append(in.Malformed, RejectedClaim{Reason: "malformed claim file: " + err.Error()})
			continue
		}
		in.Claims = append(in.Claims, c)
	}
	m, err := reconcile(in)
	if err != nil {
		return err
	}
	if err := writeJSONFile(*out, m); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "manifest written: %d entries, reserved %s, claimed %s, root %s\n",
		len(m.Entries), core.FormatArmxDecimal(m.Totals.ReservedArmoids), core.FormatArmxDecimal(m.Totals.ClaimedArmoids), m.EntriesMerkleRoot)
	if m.Totals.ProRataApplied {
		fmt.Fprintf(stdout, "pro-rata applied: capped total %s scaled to program cap %s\n",
			core.FormatArmxDecimal(m.Totals.CappedTotalArmoids), core.FormatArmxDecimal(policy.ProgramCapArmoids))
	}
	return nil
}

func cmdManifestVerify(args []string, stdout io.Writer) error {
	fs, network := newFlags("manifest-verify")
	path := fs.String("manifest", "", "manifest file")
	if err := parseAndSetNetwork(fs, network, args); err != nil {
		return err
	}
	if err := requireFlags("manifest", *path); err != nil {
		return err
	}
	var m Manifest
	if err := readStrictJSON(*path, &m); err != nil {
		return err
	}
	if err := verifyManifest(&m); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "manifest OK: %d entries, root %s, cutoff #%d (%s), tip #%d %s\n",
		len(m.Entries), m.EntriesMerkleRoot, m.Policy.CutoffHeight, m.CutoffBlockHash, m.TipHeight, m.TipHash)
	return nil
}

func cmdTranche(args []string, stdout io.Writer) error {
	fs, network := newFlags("tranche")
	db := fs.String("db", "", "target network blockchain.db (read-only)")
	manifestPath := fs.String("manifest", "", "verified migration manifest")
	treasury := fs.String("treasury", "", "treasury address on the target network")
	targetTotal := fs.Int64("target-total", 0, "approved program size in target network Armoids")
	budget := fs.Int64("budget", 0, "tranche budget in Armoids (0 = all confirmed unspent funds)")
	out := fs.String("out", "", "tranche plan output path")
	if err := parseAndSetNetwork(fs, network, args); err != nil {
		return err
	}
	if err := requireFlags("db", *db, "manifest", *manifestPath, "treasury", *treasury, "out", *out); err != nil {
		return err
	}
	var m Manifest
	if err := readStrictJSON(*manifestPath, &m); err != nil {
		return err
	}
	plan, err := buildTranche(trancheInputs{DBPath: *db, Manifest: &m, TreasuryAddress: *treasury, TargetTotal: *targetTotal, Budget: *budget})
	if err != nil {
		return err
	}
	if err := writeJSONFile(*out, plan); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "tranche plan: %d targets, planned %s of available %s (confirmed through #%d)\n",
		len(plan.Payouts), core.FormatArmxDecimal(plan.PlannedArmoids), core.FormatArmxDecimal(plan.AvailableArmoids), plan.ConfirmedThroughHeight)
	return nil
}
