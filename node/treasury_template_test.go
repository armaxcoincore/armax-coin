//go:build testhooks

package node

import (
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/armaxcoincore/armax-coin/core"
)

// TestGetBlockTemplate_TreasurySplit verifies the additive treasury fields, that coinbasevalue
// excludes the treasury share, and that a block assembled the way armminer does it (tx[0], then
// tx[1], then template transactions) is accepted through /block/submit.
func TestGetBlockTemplate_TreasurySplit(t *testing.T) {
	t.Setenv("ARMX_DATA_DIR", t.TempDir())
	core.LockNetworkForTesting(t)
	core.SetNetworkForTesting(true)
	t.Cleanup(func() { core.SetNetworkForTesting(false) })
	t.Cleanup(func() { _ = core.SetNetwork("testnet") })
	if err := core.SetNetwork("regtest"); err != nil {
		t.Fatalf("SetNetwork(regtest): %v", err)
	}
	core.SetTreasurySplitEndHeight(5)
	t.Cleanup(core.ResetTreasuryOverrides)
	core.SetTargetOverrideForTesting(core.DifficultyToTarget(1))
	t.Cleanup(func() { core.SetTargetOverrideForTesting(nil) })

	bc := core.NewBlockchain(1)
	defer bc.Close()
	handler := NewRPCHandler(bc, nil, newIPRateLimiter(), &ServerConfig{RPCBind: "127.0.0.1", MineKey: "miner-secret"})
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Host = "127.0.0.1:19333"
		req.Header.Set("X-Mine-Key", "miner-secret")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}

	w := do(http.MethodGet, "/getblocktemplate", "")
	if w.Code != http.StatusOK {
		t.Fatalf("getblocktemplate: %d %s", w.Code, w.Body.String())
	}
	var tmpl struct {
		Height          int                `json:"height"`
		Prev            string             `json:"previousblockhash"`
		CurTime         int64              `json:"curtime"`
		Target          string             `json:"target"`
		CoinbaseValue   int64              `json:"coinbasevalue"`
		Transactions    []core.Transaction `json:"transactions"`
		TreasuryActive  bool               `json:"treasury_active"`
		TreasuryAddress string             `json:"treasury_address"`
		TreasuryValue   int64              `json:"treasury_value"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &tmpl); err != nil {
		t.Fatalf("template JSON: %v", err)
	}
	share := core.TreasuryShare(core.InitialReward)
	if !tmpl.TreasuryActive || tmpl.TreasuryAddress != core.RegtestTreasuryAddress || tmpl.TreasuryValue != share {
		t.Fatalf("treasury fields = %v %q %d", tmpl.TreasuryActive, tmpl.TreasuryAddress, tmpl.TreasuryValue)
	}
	if tmpl.CoinbaseValue != core.InitialReward-share {
		t.Fatalf("coinbasevalue = %d, want %d", tmpl.CoinbaseValue, core.InitialReward-share)
	}

	miner := core.GenesisMinerForNetwork("")
	txs := []core.Transaction{
		{Sender: "SYSTEM", Recipient: miner, Amount: tmpl.CoinbaseValue, Nonce: int64(tmpl.Height), Signature: "REWARD_SIG"},
		{Sender: "SYSTEM", Recipient: tmpl.TreasuryAddress, Amount: tmpl.TreasuryValue, Nonce: int64(tmpl.Height), Signature: core.TreasuryRewardSignature},
	}
	txs = append(txs, tmpl.Transactions...)
	target, _ := new(big.Int).SetString(tmpl.Target, 16)
	blk := *core.NewBlock(txs, tmpl.Prev, tmpl.Height, miner, target)
	blk.Timestamp = tmpl.CurTime
	for {
		blk.Hash = blk.CalculateHash()
		if core.HashMeetsTarget(blk.Hash, target) {
			break
		}
		blk.Nonce++
	}
	body, _ := json.Marshal(blk)
	if w := do(http.MethodPost, "/block/submit", string(body)); w.Code != http.StatusOK {
		t.Fatalf("submit split block: %d %s", w.Code, w.Body.String())
	}
	if got := bc.GetBalance(core.RegtestTreasuryAddress); got != share {
		t.Fatalf("treasury balance = %d, want %d", got, share)
	}
}
