package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/armaxcoincore/armax-coin/core"
	"github.com/armaxcoincore/armax-coin/wallet"
)

const multisigUsage = `armwallet multisig - air-gapped ML-DSA-65 M-of-N custody

Usage:
  armwallet multisig pubkey    -wallet <FILE>                              print this cosigner's public key
  armwallet multisig create    -m <M> -pubkey <HEX|@FILE> ... -out <FILE>  build a descriptor (2 <= M <= 3, M <= N <= 4)
  armwallet multisig propose   -descriptor <FILE> -to <ADDR> -amount <ARMX> -nonce <N> [-fee <ARMX>] -out <FILE>
  armwallet multisig sign      -in <FILE> -wallet <FILE> -out <FILE> [-yes]
  armwallet multisig combine   -in <FILE> -in <FILE> ... -out <FILE>       aggregate shares into the final transaction
  armwallet multisig broadcast -in <FILE> [-daemon <URL>]                  submit a combined transaction

Every subcommand accepts -network (testnet, mainnet, regtest) and -local-mainnet. Files record
their network; a file from another network is refused before signing.

Key rotation or quorum recovery: create a new descriptor with the replacement keys, then spend
the whole balance of the old account to the new account address with the old quorum.
`

// stringList collects a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// multisigEnv carries the I/O and hooks a subcommand needs, so tests can drive it.
type multisigEnv struct {
	stdout     io.Writer
	stderr     io.Writer
	stdin      io.Reader
	passphrase func() (string, error)
	httpClient *http.Client
}

// runMultisig dispatches "armwallet multisig <subcommand>" and returns the process exit code.
func runMultisig(args []string, env multisigEnv) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "-help" || args[0] == "help" {
		fmt.Fprint(env.stderr, multisigUsage)
		return 2
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("multisig "+sub, flag.ContinueOnError)
	fs.SetOutput(env.stderr)
	network := fs.String("network", "testnet", "Network profile (testnet, mainnet, regtest)")
	localMainnet := fs.Bool("local-mainnet", false, "Use the strictly local, isolated Mainnet mode")
	walletPath := fs.String("wallet", "", "Cosigner keystore file")
	out := fs.String("out", "", "Output file")
	m := fs.Int("m", 2, "Required signatures (threshold)")
	var pubkeys, inputs stringList
	fs.Var(&pubkeys, "pubkey", "Cosigner public key hex, or @FILE (repeatable)")
	fs.Var(&inputs, "in", "Input spend file (repeatable for combine)")
	descriptorPath := fs.String("descriptor", "", "Descriptor file")
	to := fs.String("to", "", "Recipient address")
	amount := fs.String("amount", "", "Amount in ARMX")
	fee := fs.String("fee", "", "Fee in ARMX (default: minimum relay fee)")
	nonce := fs.Int64("nonce", 0, "Next nonce of the multisig account")
	yes := fs.Bool("yes", false, "Sign without the interactive confirmation")
	daemon := fs.String("daemon", "", "RPC daemon URL (default: profile port)")
	rpcUser := fs.String("rpcuser", os.Getenv("ARMX_RPC_USER"), "RPC username (or ARMX_RPC_USER)")
	rpcPass := fs.String("rpcpassword", os.Getenv("ARMX_RPC_PASSWORD"), "RPC password (prefer ARMX_RPC_PASSWORD)")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "network" {
			explicit = true
		}
	})
	cliNet, err := selectMultisigNetwork(*network, explicit, *localMainnet)
	if err != nil {
		fmt.Fprintf(env.stderr, "[-] %v\n", err)
		return 1
	}

	switch sub {
	case "pubkey":
		err = multisigPubkey(env, *walletPath)
	case "create":
		err = multisigCreate(env, *m, pubkeys, *out)
	case "propose":
		err = multisigPropose(env, *descriptorPath, *to, *amount, *fee, *nonce, *out)
	case "sign":
		err = multisigSign(env, inputs, *walletPath, *out, *yes)
	case "combine":
		err = multisigCombine(env, inputs, *out)
	case "broadcast":
		daemonURL := *daemon
		if daemonURL == "" {
			daemonURL = cliNet.DaemonURL
		}
		err = multisigBroadcast(env, inputs, daemonURL, *rpcUser, *rpcPass)
	default:
		fmt.Fprintf(env.stderr, "[-] unknown multisig subcommand %q\n\n%s", sub, multisigUsage)
		return 2
	}
	if err != nil {
		fmt.Fprintf(env.stderr, "[-] %v\n", err)
		return 1
	}
	return 0
}

// selectMultisigNetwork applies the same network rules as the main armwallet flags: regtest only
// when named explicitly, and -local-mainnet only with mainnet.
func selectMultisigNetwork(choice string, explicit, localMainnet bool) (core.CLINetworkConfig, error) {
	if !explicit {
		if envNet := os.Getenv("ARMX_NETWORK"); envNet != "" && !strings.EqualFold(envNet, "regtest") {
			choice = envNet
		}
	}
	if strings.EqualFold(choice, "regtest") && !explicit {
		return core.CLINetworkConfig{}, errors.New("regtest profile requires an explicit '-network regtest'")
	}
	if localMainnet {
		if explicit && !strings.EqualFold(choice, "mainnet") {
			return core.CLINetworkConfig{}, fmt.Errorf("-local-mainnet cannot be combined with network '%s'", choice)
		}
		core.EnableLocalMainnet()
		choice = "mainnet"
	}
	cliNet, err := core.ResolveCLINetwork(choice)
	if err != nil {
		return core.CLINetworkConfig{}, fmt.Errorf("invalid network profile '%s': %v", choice, err)
	}
	if err := core.SetNetwork(choice); err != nil {
		return core.CLINetworkConfig{}, fmt.Errorf("failed to set network profile '%s': %v", choice, err)
	}
	return cliNet, nil
}

func requireFlag(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("-%s is required", name)
	}
	return nil
}

func loadCosignerWallet(env multisigEnv, path string) (*wallet.Wallet, error) {
	if err := requireFlag("wallet", path); err != nil {
		return nil, err
	}
	pass, err := env.passphrase()
	if err != nil {
		return nil, err
	}
	return wallet.DecryptAndLoadKeystore(pass, path)
}

func multisigPubkey(env multisigEnv, walletPath string) error {
	w, err := loadCosignerWallet(env, walletPath)
	if err != nil {
		return err
	}
	defer w.Zeroize()
	fmt.Fprintln(env.stdout, w.PublicKey)
	return nil
}

func readPubkeyArg(arg string) (string, error) {
	if !strings.HasPrefix(arg, "@") {
		return arg, nil
	}
	data, err := os.ReadFile(strings.TrimPrefix(arg, "@"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func multisigCreate(env multisigEnv, m int, pubkeyArgs []string, out string) error {
	if err := requireFlag("out", out); err != nil {
		return err
	}
	hexes := make([]string, 0, len(pubkeyArgs))
	for _, a := range pubkeyArgs {
		h, err := readPubkeyArg(a)
		if err != nil {
			return err
		}
		hexes = append(hexes, h)
	}
	df, err := wallet.NewMultisigDescriptorFile(m, hexes)
	if err != nil {
		return err
	}
	if err := wallet.WriteMultisigJSON(out, df); err != nil {
		return err
	}
	fmt.Fprintf(env.stdout, "[+] %d-of-%d multisig account on %s: %s\n", df.M, df.N, df.Network, df.Address)
	return nil
}

func multisigPropose(env multisigEnv, descriptorPath, to, amountStr, feeStr string, nonce int64, out string) error {
	for name, v := range map[string]string{"descriptor": descriptorPath, "to": to, "amount": amountStr, "out": out} {
		if err := requireFlag(name, v); err != nil {
			return err
		}
	}
	var df wallet.MultisigDescriptorFile
	if err := wallet.ReadMultisigJSON(descriptorPath, &df); err != nil {
		return fmt.Errorf("read descriptor: %w", err)
	}
	amount, err := core.ParseArmxDecimal(amountStr)
	if err != nil {
		return fmt.Errorf("invalid -amount: %w", err)
	}
	fee := int64(-1)
	if feeStr != "" {
		if fee, err = core.ParseArmxDecimal(feeStr); err != nil {
			return fmt.Errorf("invalid -fee: %w", err)
		}
	}
	spend, err := wallet.NewMultisigSpend(&df, to, amount, fee, nonce)
	if err != nil {
		return err
	}
	if err := wallet.WriteMultisigJSON(out, spend); err != nil {
		return err
	}
	fmt.Fprintf(env.stdout, "[+] Proposed spend: %s\n", spend.Summary())
	return nil
}

func readSpend(path string) (*wallet.MultisigSpendFile, error) {
	var s wallet.MultisigSpendFile
	if err := wallet.ReadMultisigJSON(path, &s); err != nil {
		return nil, fmt.Errorf("read spend file %s: %w", path, err)
	}
	return &s, nil
}

func multisigSign(env multisigEnv, inputs []string, walletPath, out string, yes bool) error {
	if len(inputs) != 1 {
		return errors.New("sign takes exactly one -in spend file")
	}
	if err := requireFlag("out", out); err != nil {
		return err
	}
	spend, err := readSpend(inputs[0])
	if err != nil {
		return err
	}
	fmt.Fprintf(env.stdout, "[%s] Review before signing: %s\n", strings.ToUpper(core.GetActiveNetwork().Name), spend.Summary())
	if !yes {
		fmt.Fprint(env.stdout, "Type 'sign' to add your signature: ")
		line, _ := bufio.NewReader(env.stdin).ReadString('\n')
		if strings.TrimSpace(line) != "sign" {
			return errors.New("signing cancelled")
		}
	}
	w, err := loadCosignerWallet(env, walletPath)
	if err != nil {
		return err
	}
	defer w.Zeroize()
	if err := spend.Sign(w); err != nil {
		return err
	}
	if err := wallet.WriteMultisigJSON(out, spend); err != nil {
		return err
	}
	fmt.Fprintf(env.stdout, "[+] Signed (%d share(s) in file)\n", len(spend.PartialSignatures))
	return nil
}

func multisigCombine(env multisigEnv, inputs []string, out string) error {
	if len(inputs) == 0 {
		return errors.New("combine needs at least one -in spend file")
	}
	if err := requireFlag("out", out); err != nil {
		return err
	}
	spends := make([]*wallet.MultisigSpendFile, 0, len(inputs))
	for _, p := range inputs {
		s, err := readSpend(p)
		if err != nil {
			return err
		}
		spends = append(spends, s)
	}
	tx, err := wallet.CombineMultisigSpends(spends)
	if err != nil {
		return err
	}
	if err := wallet.WriteMultisigJSON(out, tx); err != nil {
		return err
	}
	fmt.Fprintf(env.stdout, "[+] Combined transaction %s (%d bytes TxBinaryV1)\n", tx.HashHex(), tx.BinarySize())
	return nil
}

func multisigBroadcast(env multisigEnv, inputs []string, daemonURL, user, pass string) error {
	if len(inputs) != 1 {
		return errors.New("broadcast takes exactly one -in combined transaction file")
	}
	var tx core.Transaction
	if err := wallet.ReadMultisigJSON(inputs[0], &tx); err != nil {
		return fmt.Errorf("read transaction: %w", err)
	}
	if tx.SigAlg != core.SigAlgMultisigMLDSA65 {
		return errors.New("file is not a combined multisig transaction")
	}
	if err := tx.VerifyWithError(); err != nil {
		return fmt.Errorf("transaction does not verify on %s: %w", core.NetworkID(), err)
	}
	client := env.httpClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	do := func(method, endpoint string, body io.Reader) (*http.Response, error) {
		req, err := http.NewRequest(method, endpoint, body)
		if err != nil {
			return nil, err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if user != "" || pass != "" {
			req.SetBasicAuth(user, pass)
		}
		return client.Do(req)
	}

	// Fail closed unless the daemon positively reports the active network.
	resp, err := do(http.MethodGet, daemonURL+"/balance?address="+url.QueryEscape(tx.Sender), nil)
	if err != nil {
		return fmt.Errorf("daemon unreachable: %w", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if netID := strings.TrimSpace(resp.Header.Get("X-Network-ID")); !strings.EqualFold(netID, core.NetworkID()) {
		if netID == "" {
			return fmt.Errorf("network check failed: daemon did not report X-Network-ID; refusing to broadcast for %s", core.NetworkID())
		}
		return fmt.Errorf("network check failed: daemon is on %s, transaction is for %s", netID, core.NetworkID())
	}

	body, _ := json.Marshal(tx)
	resp, err = do(http.MethodPost, daemonURL+"/tx/new", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("broadcast failed: %w", err)
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("transaction rejected by node (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	fmt.Fprintf(env.stdout, "[+] Broadcast %s\n", tx.HashHex())
	return nil
}
