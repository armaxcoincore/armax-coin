package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/armaxcoincore/armax-coin/core"
	"github.com/armaxcoincore/armax-coin/wallet"
)

func TestGUI_StartupLockBlocksCreateAndRestore(t *testing.T) {
	origAuthReq := startupAuthRequired.Load()
	origAuthed := startupAuthenticated.Load()
	defer func() {
		startupAuthRequired.Store(origAuthReq)
		startupAuthenticated.Store(origAuthed)
	}()

	startupAuthRequired.Store(true)
	startupAuthenticated.Store(false)

	const guiToken = "gui-hardening-test-token"
	handler := secureGUIHandler(buildGUIServeMux(), guiToken)

	for _, path := range []string{"/api/wallet/create", "/api/wallet/restore"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"passphrase":"NewPass!123"}`))
		req.Host = "127.0.0.1:18088"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Armx-GUI-Token", guiToken)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusLocked {
			t.Fatalf("%s when startup locked: expected HTTP %d, got %d (%s)", path, http.StatusLocked, rec.Code, rec.Body.String())
		}
		var body struct {
			StartupLocked bool `json:"startup_locked"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || !body.StartupLocked {
			t.Fatalf("%s expected startup_locked=true JSON, got %q (err=%v)", path, rec.Body.String(), err)
		}
		if startupAuthenticated.Load() {
			t.Fatalf("%s must not clear startupAuthenticated lock", path)
		}
	}
}

func TestGUI_FeeEstimateAtHeightBinaryWeight(t *testing.T) {
	w, err := wallet.NewWallet()
	if err != nil {
		t.Fatalf("NewWallet sender failed: %v", err)
	}
	defer w.Zeroize()

	recip, err := wallet.NewWallet()
	if err != nil {
		t.Fatalf("NewWallet recip failed: %v", err)
	}
	defer recip.Zeroize()

	testnetPre := ComputeGUIFeeEstimateAtHeight(w, recip.Address, core.CoinUnit, 0, core.TestnetProfile.NetworkID, 1)
	if testnetPre.MinFeeArmoids < 11000 || testnetPre.EstimatedTxBytes <= 9000 {
		t.Fatalf("expected Testnet height 1 JSON-weighted fee >= 11000 and bytes > 9000, got %+v", testnetPre)
	}

	mainnetEst := ComputeGUIFeeEstimateAtHeight(w, recip.Address, core.CoinUnit, 0, core.MainnetProfile.NetworkID, 1)
	if mainnetEst.MinFeeArmoids != 6000 || mainnetEst.EstimatedTxBytes <= 0 || mainnetEst.EstimatedTxBytes > 6000 {
		t.Fatalf("expected Mainnet height 1 binary-weighted min fee = 6000 and bytes in (0, 6000], got %+v", mainnetEst)
	}

	testnetPost := ComputeGUIFeeEstimateAtHeight(
		w,
		recip.Address,
		core.CoinUnit,
		0,
		core.TestnetProfile.NetworkID,
		int(core.TestnetProfile.BlockBinarySizeActivationHeight),
	)
	if testnetPost.MinFeeArmoids < 11000 {
		t.Fatalf("expected Testnet (MempoolBinaryFeeActivationHeight=-1) to keep JSON fee >= 11000 for v1.0.12 compat, got %+v", testnetPost)
	}

	core.SetMempoolBinaryFeeActivationHeight(core.TestnetProfile.BlockBinarySizeActivationHeight)
	defer core.ResetMempoolBinaryFeeActivationHeightOverride()
	activePost := ComputeGUIFeeEstimateAtHeight(
		w,
		recip.Address,
		core.CoinUnit,
		0,
		core.TestnetProfile.NetworkID,
		int(core.TestnetProfile.BlockBinarySizeActivationHeight),
	)
	if activePost.MinFeeArmoids != 6000 {
		t.Fatalf("expected binary fee = 6000 when MempoolBinaryFeeActivationHeight is active, got %+v", activePost)
	}
}

func TestGUI_HandleSendRejectsImmatureOnlyBalance(t *testing.T) {
	origRPCURL := rpcURL
	origAuthReq := startupAuthRequired.Load()
	origAuthed := startupAuthenticated.Load()

	unlockedMu.Lock()
	origActiveW := activeWallet
	origActiveAddr := activeAddress
	origActiveID := activeWalletID
	origMap := unlockedWallets
	unlockedWallets = make(map[string]*wallet.Wallet)
	unlockedMu.Unlock()

	defer func() {
		rpcURL = origRPCURL
		startupAuthRequired.Store(origAuthReq)
		startupAuthenticated.Store(origAuthed)
		unlockedMu.Lock()
		activeWallet = origActiveW
		activeAddress = origActiveAddr
		activeWalletID = origActiveID
		unlockedWallets = origMap
		unlockedMu.Unlock()
	}()

	sender, err := wallet.NewWallet()
	if err != nil {
		t.Fatalf("NewWallet sender failed: %v", err)
	}
	defer sender.Zeroize()

	recip, err := wallet.NewWallet()
	if err != nil {
		t.Fatalf("NewWallet recip failed: %v", err)
	}
	defer recip.Zeroize()

	var txSubmitCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Network-ID", core.ActiveNetwork.NetworkID)
		switch r.URL.Path {
		case "/stats":
			_, _ = w.Write([]byte(`{"height":10,"blocks":11,"network_id":"` + core.ActiveNetwork.NetworkID + `"}`))
		case "/balance":
			_, _ = w.Write([]byte(`{"balance":6000000000,"confirmed_balance":0,"pending_balance":0,"immature_balance":6000000000,"spendable_balance":0,"next_nonce":1}`))
		case "/tx/new":
			txSubmitCount.Add(1)
			http.Error(w, "unexpected tx submission", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	rpcURL = srv.URL
	startupAuthRequired.Store(false)
	startupAuthenticated.Store(true)

	unlockedMu.Lock()
	activeWalletID = "immature-balance-wallet"
	activeAddress = sender.Address
	activeWallet = sender
	unlockedWallets[activeWalletID] = sender
	unlockedMu.Unlock()

	payload, _ := json.Marshal(map[string]string{
		"to":     recip.Address,
		"amount": "1.0",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/send", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	handleSend(rec, req)

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Insufficient balance") {
		t.Fatalf("expected HTTP 400 Insufficient balance when only immature coins exist, got %d (%q)", rec.Code, rec.Body.String())
	}
	if txSubmitCount.Load() != 0 {
		t.Fatalf("expected 0 /tx/new broadcast calls for immature-only balance, got %d", txSubmitCount.Load())
	}
}
