//go:build testhooks

package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/armaxcoincore/armax-coin/core"
	"github.com/armaxcoincore/armax-coin/wallet"
)

const multisigCLIPass = "DisposableMultisigCLI#2026"

func runMultisigForTest(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runMultisig(args, multisigEnv{
		stdout:     &out,
		stderr:     &errOut,
		stdin:      strings.NewReader(stdin),
		passphrase: func() (string, error) { return multisigCLIPass, nil },
	})
	return code, out.String(), errOut.String()
}

// TestMultisigCLI_AirGappedFlow drives pubkey -> create -> propose -> sign x2 -> combine ->
// broadcast on Regtest, and checks the broadcast refuses a daemon that does not report the
// active network.
func TestMultisigCLI_AirGappedFlow(t *testing.T) {
	core.LockNetworkForTesting(t)
	core.SetNetworkForTesting(true)
	t.Cleanup(func() { core.SetNetworkForTesting(false) })
	t.Cleanup(func() { _ = core.SetNetwork("testnet") })
	if err := core.SetNetwork("regtest"); err != nil {
		t.Fatalf("SetNetwork(regtest): %v", err)
	}
	dir := t.TempDir()
	net := []string{"-network", "regtest"}
	with := func(sub string, extra ...string) []string {
		return append(append([]string{sub}, net...), extra...)
	}

	var pubArgs []string
	keystores := make([]string, 3)
	for i := range keystores {
		w := wallet.MustNewWallet()
		keystores[i] = filepath.Join(dir, "cosigner"+string(rune('1'+i))+".json")
		if err := wallet.EncryptAndSaveKeystore(w, multisigCLIPass, keystores[i]); err != nil {
			t.Fatalf("EncryptAndSaveKeystore: %v", err)
		}
		w.Zeroize()
		code, out, errOut := runMultisigForTest(t, "", with("pubkey", "-wallet", keystores[i])...)
		if code != 0 {
			t.Fatalf("pubkey exit %d: %s", code, errOut)
		}
		pubArgs = append(pubArgs, "-pubkey", strings.TrimSpace(out))
	}

	descPath := filepath.Join(dir, "treasury.descriptor.json")
	if code, _, errOut := runMultisigForTest(t, "", with("create", append([]string{"-m", "2", "-out", descPath}, pubArgs...)...)...); code != 0 {
		t.Fatalf("create exit %d: %s", code, errOut)
	}
	if code, _, _ := runMultisigForTest(t, "", with("create", append([]string{"-m", "1", "-out", filepath.Join(dir, "x.json")}, pubArgs...)...)...); code == 0 {
		t.Fatalf("1-of-3 descriptor accepted")
	}

	recipient := wallet.MustNewWallet()
	defer recipient.Zeroize()
	spendPath := filepath.Join(dir, "spend.json")
	if code, _, errOut := runMultisigForTest(t, "", with("propose", "-descriptor", descPath, "-to", recipient.Address, "-amount", "2.5", "-nonce", "1", "-out", spendPath)...); code != 0 {
		t.Fatalf("propose exit %d: %s", code, errOut)
	}

	signed1 := filepath.Join(dir, "spend.s1.json")
	signed3 := filepath.Join(dir, "spend.s3.json")
	if code, _, _ := runMultisigForTest(t, "no\n", with("sign", "-in", spendPath, "-wallet", keystores[0], "-out", signed1)...); code == 0 {
		t.Fatalf("sign proceeded without confirmation")
	}
	if code, out, errOut := runMultisigForTest(t, "sign\n", with("sign", "-in", spendPath, "-wallet", keystores[0], "-out", signed1)...); code != 0 || !strings.Contains(out, "REGTEST") {
		t.Fatalf("sign 1 exit %d: %s / %s", code, out, errOut)
	}
	if code, _, errOut := runMultisigForTest(t, "", with("sign", "-in", spendPath, "-wallet", keystores[2], "-out", signed3, "-yes")...); code != 0 {
		t.Fatalf("sign 3 exit %d: %s", code, errOut)
	}

	finalPath := filepath.Join(dir, "final.json")
	if code, _, _ := runMultisigForTest(t, "", with("combine", "-in", signed1, "-out", finalPath)...); code == 0 {
		t.Fatalf("combine succeeded with one share")
	}
	if code, _, errOut := runMultisigForTest(t, "", with("combine", "-in", signed1, "-in", signed3, "-out", finalPath)...); code != 0 {
		t.Fatalf("combine exit %d: %s", code, errOut)
	}

	var posted atomic.Int32
	daemon := func(reportNetwork bool) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if reportNetwork {
				w.Header().Set("X-Network-ID", core.NetworkID())
			}
			if r.Method == http.MethodPost && r.URL.Path == "/tx/new" {
				body, _ := io.ReadAll(r.Body)
				if strings.Contains(string(body), `"sig_alg":2`) {
					posted.Add(1)
				}
			}
			_, _ = w.Write([]byte(`{}`))
		}))
	}

	silent := daemon(false)
	defer silent.Close()
	if code, _, errOut := runMultisigForTest(t, "", with("broadcast", "-in", finalPath, "-daemon", silent.URL)...); code == 0 || !strings.Contains(errOut, "X-Network-ID") {
		t.Fatalf("broadcast to a daemon without X-Network-ID: exit %d %s", code, errOut)
	}
	if posted.Load() != 0 {
		t.Fatalf("transaction posted to a daemon that did not report its network")
	}

	good := daemon(true)
	defer good.Close()
	if code, _, errOut := runMultisigForTest(t, "", with("broadcast", "-in", finalPath, "-daemon", good.URL)...); code != 0 {
		t.Fatalf("broadcast exit %d: %s", code, errOut)
	}
	if posted.Load() != 1 {
		t.Fatalf("expected one multisig transaction posted, got %d", posted.Load())
	}

	// The same files are refused on another network before signing.
	if code, _, errOut := runMultisigForTest(t, "", "sign", "-network", "testnet", "-in", spendPath, "-wallet", keystores[1], "-out", filepath.Join(dir, "x.json"), "-yes"); code == 0 || !strings.Contains(errOut, "belongs to network ARMX-REGTEST-1") {
		t.Fatalf("regtest spend file signed on testnet: exit %d %s", code, errOut)
	}
}
