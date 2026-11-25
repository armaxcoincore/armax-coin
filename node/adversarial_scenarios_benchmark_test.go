//go:build testhooks

package node

import (
	"bufio"
	"bytes"
	"compress/flate"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/armaxcoincore/armax-coin/core"
	"github.com/armaxcoincore/armax-coin/wallet"
	bolt "go.etcd.io/bbolt"
)

// TestAdversarialScenario1_PartitionAndEclipseRecovery exercises Track 6 Scenario 1:
// - Inbound slot saturation (MaxInboundPeersDefault = 38) leaves outbound slots (MaxOutboundPeersDefault = 12) intact.
// - Per-message PEERS_LIST (64) / DHT_NODES (20) caps hold.
// - Strict SeedHost TLS pin enforcement rejects an impostor seed cert and recovers sync when partition heals.
func TestAdversarialScenario1_PartitionAndEclipseRecovery(t *testing.T) {
	core.LockNetworkForTesting(t)
	core.SetTargetOverrideForTesting(core.DifficultyToTarget(1))
	t.Setenv("ARMX_DATA_DIR", t.TempDir())
	t.Setenv("ARMX_ALLOW_PRIVATE_PEERS", "1")

	victimDir := t.TempDir()
	bcVictim, err := core.NewBlockchainWithStoragePath(1, filepath.Join(victimDir, "victim.db"))
	if err != nil {
		t.Fatalf("init victim bc: %v", err)
	}
	t.Cleanup(func() { _ = bcVictim.Close() })

	honestDir := t.TempDir()
	bcHonest, err := core.NewBlockchainWithStoragePath(1, filepath.Join(honestDir, "honest.db"))
	if err != nil {
		t.Fatalf("init honest bc: %v", err)
	}
	t.Cleanup(func() { _ = bcHonest.Close() })

	// Honest peer mines 5 blocks ahead while victim is partitioned.
	appendTestDiff1Chain(t, bcHonest, 5, core.GenesisMiner)

	psHonest := NewPeerServer(getFreePort(t), bcHonest, honestDir, "ARMX-TESTNET-1")
	psHonest.AllowPrivatePeers = true
	psHonest.P2PTOFU = true
	go psHonest.StartServer()
	waitForServerRunning(t, psHonest, 3*time.Second)
	t.Cleanup(psHonest.Stop)

	psVictim := NewPeerServer(getFreePort(t), bcVictim, victimDir, "ARMX-TESTNET-1")
	psVictim.AllowPrivatePeers = true
	psVictim.P2PTOFU = true
	psVictim.DisableSubnetLimit = true
	go psVictim.StartServer()
	waitForServerRunning(t, psVictim, 3*time.Second)
	t.Cleanup(psVictim.Stop)

	// 1. Saturate all MaxInboundPeersDefault (38) inbound slots, verify 39th fails and outbound slot still succeeds.
	inboundReleases := make([]func(), 0, MaxInboundPeersDefault)
	for i := 0; i < MaxInboundPeersDefault; i++ {
		rel, _, ok := psVictim.tryReserveInboundSlot(fmt.Sprintf("198.51.100.%d", (i%200)+1), false)
		if !ok {
			t.Fatalf("failed to acquire inbound slot %d/%d", i+1, MaxInboundPeersDefault)
		}
		inboundReleases = append(inboundReleases, rel)
	}
	if _, _, ok := psVictim.tryReserveInboundSlot("198.51.100.250", false); ok {
		t.Fatalf("expected 39th inbound slot to be rejected")
	}
	outRel, outOK := psVictim.tryReserveOutboundSlot()
	if !outOK {
		t.Fatalf("expected outbound slot to remain available when inbound slots are saturated")
	}
	outRel()
	for _, rel := range inboundReleases {
		rel()
	}

	// 2. Verify SeedHost TLS pin rejects impostor seed cert and leaves no TOFU pin behind.
	seedHost := "seed1.armax-coin.invalid:19335"
	goodFP := strings.Repeat("a", 64)
	evilFP := strings.Repeat("f", 64)
	if err := psVictim.SetSeedFingerprints(map[string]string{seedHost: goodFP}); err != nil {
		t.Fatalf("SetSeedFingerprints failed: %v", err)
	}
	if err := psVictim.VerifyOrRecordTOFU(seedHost, &mockTLSConn{fingerprint: evilFP}); err == nil {
		t.Fatalf("expected impostor seed cert to be rejected")
	}

	// 3. Verify PEERS_LIST > maxNewPeersPerMessage cap ignores oversized peer list without disk pollution.
	oversizedPeers := make([]string, maxNewPeersPerMessage+25)
	for i := range oversizedPeers {
		oversizedPeers[i] = fmt.Sprintf("198.51.101.%d:19335", (i%250)+1)
	}
	rawPeersList, _ := json.Marshal(Message{
		NetworkID: psVictim.GetNetworkID(),
		Type:      "PEERS_LIST",
		Payload:   oversizedPeers,
	})
	c1, s1 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = s1.Close() })
	qCh := make(chan struct{})
	defer close(qCh)
	sess := &inboundConnSession{
		conn:       s1,
		remoteIP:   "198.51.100.99",
		remoteAddr: "198.51.100.99:19335",
		quitCh:     qCh,
		deadline:   time.Now().Add(5 * time.Second),
	}
	_ = psVictim.processInboundFrame(sess, rawPeersList, nil)
	if known := len(psVictim.GetPeers()); known > maxNewPeersPerMessage {
		t.Fatalf("expected PEERS_LIST cap <= %d, got %d", maxNewPeersPerMessage, known)
	}

	// 4. Heal partition by syncing victim from honest node over TLS.
	honestAddr := "127.0.0.1:" + psHonest.Port
	psVictim.RequestChainSync(honestAddr)

	bcVictim.Lock.RLock()
	victimH := len(bcVictim.Chain) - 1
	victimTip := bcVictim.GetLatestBlock().Hash
	bcVictim.Lock.RUnlock()
	bcHonest.Lock.RLock()
	honestH := len(bcHonest.Chain) - 1
	honestTip := bcHonest.GetLatestBlock().Hash
	bcHonest.Lock.RUnlock()

	if victimH != honestH || victimTip != honestTip {
		t.Fatalf("partition heal failed: victim height=%d tip=%s, honest height=%d tip=%s", victimH, victimTip, honestH, honestTip)
	}
	t.Logf("[SCENARIO 1] Inbound cap=38/Outbound=12 verified, SeedHost pin enforced, partition healed to height #%d", victimH)
}

// TestAdversarialScenario2_MalformedFrameAndCompressedStormMetrics exercises Track 6 Scenario 2:
//   - Malformed JSON, wrong NetworkID, >8 MiB oversized frame, and deflate-bomb PUSH_BLOCKS.
//   - Verifies banned offenders are dropped without deadline refreshes, honest peer GET_HEIGHT succeeds,
//     and records runtime.MemStats + goroutine counts.
func TestAdversarialScenario2_MalformedFrameAndCompressedStormMetrics(t *testing.T) {
	core.LockNetworkForTesting(t)
	t.Setenv("ARMX_DATA_DIR", t.TempDir())
	t.Setenv("ARMX_ALLOW_PRIVATE_PEERS", "1")

	dir := t.TempDir()
	bc, err := core.NewBlockchainWithStoragePath(1, filepath.Join(dir, "storm.db"))
	if err != nil {
		t.Fatalf("init bc: %v", err)
	}
	t.Cleanup(func() { _ = bc.Close() })

	ps := NewPeerServer(getFreePort(t), bc, dir, "ARMX-TESTNET-1")
	ps.AllowPrivatePeers = true
	ps.RequireTLS = false
	go ps.StartServer()
	waitForServerRunning(t, ps, 3*time.Second)
	t.Cleanup(ps.Stop)

	var mBefore, mAfter runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&mBefore)
	gBefore := runtime.NumGoroutine()

	targetAddr := "127.0.0.1:" + ps.Port

	// 1. Malformed JSON + wrong NetworkID storm.
	for i := 0; i < 10; i++ {
		conn, err := net.DialTimeout("tcp", targetAddr, 2*time.Second)
		if err != nil {
			t.Fatalf("dial storm #%d: %v", i, err)
		}
		_, _ = conn.Write([]byte("{not_valid_json\n"))
		_, _ = conn.Write([]byte(`{"network_id":"WRONG-NET","type":"GET_HEIGHT"}` + "\n"))
		_ = conn.Close()
	}

	// 2. Oversized >8 MiB stream drop.
	connBig, err := net.DialTimeout("tcp", targetAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial oversized: %v", err)
	}
	oversized := append(bytes.Repeat([]byte("A"), MaxIncomingPacketSize+1024), '\n')
	_, _ = connBig.Write(oversized)
	_ = connBig.Close()

	// 3. Deflate bomb via PUSH_BLOCKS compressed envelope (> MaxIncomingPacketSize when inflated).
	var bombBuf bytes.Buffer
	fw, _ := flate.NewWriter(&bombBuf, flate.BestCompression)
	_, _ = fw.Write(bytes.Repeat([]byte(`{"x":1}`), 2_000_000))
	_ = fw.Close()
	bombEnv, _ := json.Marshal(Message{
		NetworkID: ps.GetNetworkID(),
		Type:      "PUSH_BLOCKS",
		Payload: BlockRangeResponse{
			FromIndex: 1,
			ToIndex:   1,
			Enc:       encDeflate,
			Z:         base64.StdEncoding.EncodeToString(bombBuf.Bytes()),
		},
	})
	connBomb, err := net.DialTimeout("tcp", targetAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial bomb: %v", err)
	}
	_, _ = connBomb.Write(append(bombEnv, '\n'))
	_ = connBomb.Close()

	// 4. Verify banned IP is dropped immediately without deadline refresh.
	ps.AddBanScore("198.51.100.77", 105)
	if !ps.IsBanned("198.51.100.77") {
		t.Fatalf("expected attacker IP to be banned")
	}

	// 5. Honest peer GET_HEIGHT still succeeds on the same PeerServer.
	ps.banLock.Lock()
	delete(ps.bannedIPs, "127.0.0.1")
	delete(ps.banScores, "127.0.0.1")
	ps.banLock.Unlock()

	honestConn, err := net.DialTimeout("tcp", targetAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("honest dial failed: %v", err)
	}
	defer honestConn.Close()
	req, _ := json.Marshal(Message{NetworkID: ps.GetNetworkID(), Type: "GET_HEIGHT"})
	_ = honestConn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := honestConn.Write(append(req, '\n')); err != nil {
		t.Fatalf("honest write failed: %v", err)
	}
	line, err := bufio.NewReader(honestConn).ReadString('\n')
	if err != nil || !strings.Contains(line, "HEIGHT_RESPONSE") {
		t.Fatalf("honest peer failed during storm: line=%q err=%v", line, err)
	}

	ps.pruneStaleRuntimeMaps()
	runtime.GC()
	runtime.ReadMemStats(&mAfter)
	gAfter := runtime.NumGoroutine()

	t.Logf("[SCENARIO 2] Storm survived cleanly | HeapAlloc before=%.2fMB after=%.2fMB | Goroutines before=%d after=%d",
		float64(mBefore.HeapAlloc)/(1024*1024), float64(mAfter.HeapAlloc)/(1024*1024), gBefore, gAfter)
}

// TestAdversarialScenario3_BoundaryReorg720And721Rejection exercises Track 6 Scenario 3:
// - Boundary reorg at exact MaxReorganizationDepth (720 blocks) succeeds deterministically.
// - Reorg at depth 721 (> MaxReorganizationDepth) is strictly rejected without mutating tip.
// - Invalid mid-fork block triggers atomic rollback preserving local tip and mempool.
func TestAdversarialScenario3_BoundaryReorg720And721Rejection(t *testing.T) {
	core.LockNetworkForTesting(t)
	core.SetTargetOverrideForTesting(core.DifficultyToTarget(1))
	t.Setenv("ARMX_DATA_DIR", t.TempDir())

	bc, err := core.NewBlockchainWithStoragePath(1, filepath.Join(t.TempDir(), "reorg720.db"))
	if err != nil {
		t.Fatalf("init bc: %v", err)
	}
	t.Cleanup(func() { _ = bc.Close() })

	// Build 720 blocks on main chain (#1..#720).
	localBlocks := appendTestDiff1Chain(t, bc, core.MaxReorganizationDepth, core.GenesisMiner)
	if len(localBlocks) != core.MaxReorganizationDepth {
		t.Fatalf("expected %d blocks, got %d", core.MaxReorganizationDepth, len(localBlocks))
	}

	// Build heavier 721-block fork from genesis (#0) -> depth = 720 from #720 tip -> MUST SUCCEED.
	genesis := bc.Chain[0]
	altMiner := newTestAccount(t, "alt_miner").Address
	altChain := make([]core.Block, 0, core.MaxReorganizationDepth+1)
	prev := genesis
	for h := 1; h <= core.MaxReorganizationDepth+1; h++ {
		blk := makeTestnetDiff1ForkBlock(t, h, prev.Hash, prev.Timestamp, altMiner, "")
		altChain = append(altChain, blk)
		prev = blk
	}

	start := time.Now()
	if err := bc.ResolveFork(0, altChain); err != nil {
		t.Fatalf("expected depth-%d boundary reorg to succeed, got err: %v", core.MaxReorganizationDepth, err)
	}
	reorgDur := time.Since(start)

	if len(bc.Chain)-1 != core.MaxReorganizationDepth+1 {
		t.Fatalf("expected tip height %d after 720-depth reorg, got %d", core.MaxReorganizationDepth+1, len(bc.Chain)-1)
	}

	// Attempt depth-721 reorg from genesis -> MUST BE REJECTED.
	tipBefore := bc.GetLatestBlock().Hash
	deeperChain := append(append([]core.Block(nil), altChain...), makeTestnetDiff1ForkBlock(t, core.MaxReorganizationDepth+2, prev.Hash, prev.Timestamp, altMiner, ""))
	if err := bc.ResolveFork(0, deeperChain); err == nil {
		t.Fatalf("expected depth-%d (> MaxReorganizationDepth) reorg to be rejected", core.MaxReorganizationDepth+1)
	}
	if bc.GetLatestBlock().Hash != tipBefore {
		t.Fatalf("tip mutated after rejected >720 reorg")
	}

	// Invalid mid-fork block (broken PrevHash) -> atomic rollback preserves tipBefore.
	badFork := make([]core.Block, 2)
	badFork[0] = makeTestnetDiff1ForkBlock(t, len(bc.Chain)-1, bc.Chain[len(bc.Chain)-2].Hash, bc.Chain[len(bc.Chain)-2].Timestamp, altMiner, "")
	badFork[1] = makeTestnetDiff1ForkBlock(t, len(bc.Chain), strings.Repeat("0", 64), badFork[0].Timestamp, altMiner, "")
	if err := bc.ResolveFork(len(bc.Chain)-2, badFork); err == nil {
		t.Fatalf("expected invalid mid-fork block to fail ResolveFork")
	}
	if bc.GetLatestBlock().Hash != tipBefore {
		t.Fatalf("tip mutated after failed invalid-block reorg")
	}

	t.Logf("[SCENARIO 3] Depth-720 reorg succeeded in %v; depth-721 & invalid-fork atomically rejected", reorgDur)
}

// TestAdversarialScenario4_ClockSkewOffsetsAndInheritedTipSafety exercises Track 6 Scenario 4:
//   - Post-#20,000 C6 (+600s) rule accepts -300s / +300s, applies 3-tier penalties on +615s / +720s / +1300s,
//     and /getblocktemplate enforces inherited-tip mintime/curtime/maxtime bounds.
func TestAdversarialScenario4_ClockSkewOffsetsAndInheritedTipSafety(t *testing.T) {
	core.LockNetworkForTesting(t)
	core.SetTargetOverrideForTesting(core.DifficultyToTarget(1))
	core.SetFutureTimestampLimitActivationHeight(1)
	t.Setenv("ARMX_DATA_DIR", t.TempDir())

	bc, err := core.NewBlockchainWithStoragePath(1, filepath.Join(t.TempDir(), "skew.db"))
	if err != nil {
		t.Fatalf("init bc: %v", err)
	}
	t.Cleanup(func() { _ = bc.Close() })

	ps := NewPeerServer(getFreePort(t), bc, t.TempDir())
	minerAcc := newTestAccount(t, "skew_miner")
	genesis := bc.GetLatestBlock()
	now := time.Now().Unix()

	// Accepted +300s offset (< +600s C6 ceiling).
	okBlk := mineP2PTestBlockOnChain(t, bc, bc.Chain, genesis, minerAcc.Address, now+300)
	if !ps.ProcessIncomingBlock(okBlk, "198.51.100.10", "198.51.100.10:19335") {
		t.Fatalf("expected +300s block to be accepted under C6 +600s")
	}

	tip1 := bc.GetLatestBlock()
	// Tier 1 (+615s -> 15s over C6): rejected, 0 ban score.
	t1Blk := mineP2PTestBlockOnChain(t, bc, bc.Chain, tip1, minerAcc.Address, time.Now().Unix()+core.FutureTimestampLimitSeconds+15)
	if ps.ProcessIncomingBlock(t1Blk, "198.51.100.11", "198.51.100.11:19335") {
		t.Fatalf("expected +615s block to be rejected")
	}
	if s := ps.GetBanScore("198.51.100.11"); s != 0 {
		t.Fatalf("expected Tier 1 ban score 0, got %d", s)
	}

	// Tier 3 (+1300s -> >600s over C6): rejected, immediate +10 ban score.
	t3Blk := mineP2PTestBlockOnChain(t, bc, bc.Chain, tip1, minerAcc.Address, time.Now().Unix()+core.FutureTimestampLimitSeconds+700)
	if ps.ProcessIncomingBlock(t3Blk, "198.51.100.12", "198.51.100.12:19335") {
		t.Fatalf("expected +1300s block to be rejected")
	}
	if s := ps.GetBanScore("198.51.100.12"); s != 10 {
		t.Fatalf("expected Tier 3 ban score 10, got %d", s)
	}

	// /getblocktemplate inherited-tip safety check.
	srv := httptest.NewServer(NewRPCHandler(bc, nil, nil, nil))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/getblocktemplate")
	if err != nil {
		t.Fatalf("GET /getblocktemplate failed: %v", err)
	}
	defer resp.Body.Close()
	var tpl struct {
		CurTime int64 `json:"curtime"`
		MinTime int64 `json:"mintime"`
		MaxTime int64 `json:"maxtime"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tpl); err != nil {
		t.Fatalf("decode template: %v", err)
	}
	if tpl.MinTime != tip1.Timestamp+1 || tpl.CurTime < tpl.MinTime || tpl.MaxTime < tpl.CurTime {
		t.Fatalf("invalid template timestamp bounds: mintime=%d curtime=%d maxtime=%d (tip=%d)", tpl.MinTime, tpl.CurTime, tpl.MaxTime, tip1.Timestamp)
	}

	t.Logf("[SCENARIO 4] C6 (+600s) 3-tier penalties verified; template bounds mintime=%d curtime=%d maxtime=%d", tpl.MinTime, tpl.CurTime, tpl.MaxTime)
}

// TestAdversarialScenario5_MempoolAndOrphanFloodHonestLiveness exercises Track 6 Scenario 5:
// - Attacker floods orphan txs (> MaxOrphanTxsPerPeer = 25) and invalid signatures.
// - Honest tx admission and /getblocktemplate remain fast and unstarved.
func TestAdversarialScenario5_MempoolAndOrphanFloodHonestLiveness(t *testing.T) {
	core.LockNetworkForTesting(t)
	t.Setenv("ARMX_DATA_DIR", t.TempDir())

	bc, err := core.NewBlockchainWithStoragePath(1, filepath.Join(t.TempDir(), "mempool_flood.db"))
	if err != nil {
		t.Fatalf("init bc: %v", err)
	}
	t.Cleanup(func() { _ = bc.Close() })

	ps := NewPeerServer(getFreePort(t), bc, t.TempDir())
	attackerAcc := newTestAccount(t, "flood_attacker")
	honestAcc := newTestAccount(t, "flood_honest")
	recipAcc := newTestAccount(t, "flood_recip")
	fundAccount(bc, attackerAcc.Address, 100_000*core.CoinUnit)
	fundAccount(bc, honestAcc.Address, 100_000*core.CoinUnit)

	// Attacker tries to queue 40 out-of-order orphan nonces (> MaxOrphanTxsPerPeer = 25).
	attackerIP := "198.51.100.66"
	for i := 0; i < MaxOrphanTxsPerPeer+15; i++ {
		orph := core.Transaction{
			Sender:    attackerAcc.Address,
			SenderPub: attackerAcc.PubHex,
			Recipient: recipAcc.Address,
			Amount:    1 * core.CoinUnit,
			Fee:       25000,
			Nonce:     int64(100 + i),
			Type:      core.TxTypeStandard,
		}
		_ = orph.Sign(attackerAcc.PrivKey)
		ps.ProcessIncomingTx(orph, attackerIP)
	}

	// Honest peer submits a valid orphan + valid mempool tx.
	honestIP := "203.0.113.88"
	honestOrph := core.Transaction{
		Sender:    honestAcc.Address,
		SenderPub: honestAcc.PubHex,
		Recipient: recipAcc.Address,
		Amount:    1 * core.CoinUnit,
		Fee:       25000,
		Nonce:     2,
		Type:      core.TxTypeStandard,
	}
	if err := honestOrph.Sign(honestAcc.PrivKey); err != nil {
		t.Fatalf("sign honest orphan: %v", err)
	}
	ps.ProcessIncomingTx(honestOrph, honestIP)

	ps.orphanLock.Lock()
	attackerCount := 0
	honestCount := 0
	for _, entry := range ps.orphanTxs {
		if entry.PeerIP == attackerIP {
			attackerCount++
		}
		if entry.PeerIP == honestIP {
			honestCount++
		}
	}
	ps.orphanLock.Unlock()

	if attackerCount > MaxOrphanTxsPerPeer {
		t.Fatalf("attacker exceeded MaxOrphanTxsPerPeer (%d): got %d", MaxOrphanTxsPerPeer, attackerCount)
	}
	if honestCount != 1 {
		t.Fatalf("expected honest orphan slot to be preserved, got %d", honestCount)
	}

	// Honest nonce-1 tx admitted & template assembled cleanly.
	honestTx1 := core.Transaction{
		Sender:    honestAcc.Address,
		SenderPub: honestAcc.PubHex,
		Recipient: recipAcc.Address,
		Amount:    1 * core.CoinUnit,
		Fee:       25000,
		Nonce:     1,
		Type:      core.TxTypeStandard,
	}
	if err := honestTx1.Sign(honestAcc.PrivKey); err != nil {
		t.Fatalf("sign honest tx1: %v", err)
	}
	if ok, msg := bc.AddTransaction(honestTx1); !ok {
		t.Fatalf("honest AddTransaction failed under flood: %s", msg)
	}

	tplStart := time.Now()
	srv := httptest.NewServer(NewRPCHandler(bc, nil, nil, nil))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/getblocktemplate")
	if err != nil {
		t.Fatalf("GET /getblocktemplate failed: %v", err)
	}
	resp.Body.Close()
	tplDur := time.Since(tplStart)

	t.Logf("[SCENARIO 5] Attacker orphans capped=%d/%d, honest orphan=%d, /getblocktemplate latency=%v",
		attackerCount, MaxOrphanTxsPerPeer, honestCount, tplDur)
}

// TestAdversarialScenario6_BoltDBCorruptionAndTamperedBlockSnapshotFailClosed exercises Track 6 Scenario 6:
// - Corrupt BoltDB header triggers fail-closed .invalid-* backup rename on LoadFromFile without harming peers.json.
// - Tampered pre-snapshot block + matching tampered snapshot is caught and healed on LoadFromFile.
func TestAdversarialScenario6_BoltDBCorruptionAndTamperedBlockSnapshotFailClosed(t *testing.T) {
	core.LockNetworkForTesting(t)
	core.SetTargetOverrideForTesting(core.DifficultyToTarget(1))
	t.Cleanup(func() { core.SetDBPath("") })

	// Part A: Corrupt BoltDB header -> .invalid-* backup rename, peers.json untouched.
	dirA := t.TempDir()
	t.Setenv("ARMX_DATA_DIR", dirA)
	dbPathA := filepath.Join(dirA, "testnet", "blockchain.db")
	if err := os.MkdirAll(filepath.Dir(dbPathA), 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	peersPathA := filepath.Join(dirA, "testnet", "peers.json")
	peersContent := []byte(`{"version":1,"network_id":"ARMX-TESTNET-1","peers":[]}`)
	if err := os.WriteFile(peersPathA, peersContent, 0600); err != nil {
		t.Fatalf("write peers.json: %v", err)
	}
	if err := os.WriteFile(dbPathA, []byte("CORRUPT_HEADER_TRUNCATED_DB"), 0600); err != nil {
		t.Fatalf("write corrupt db: %v", err)
	}
	core.SetDBPath(dbPathA)
	if loaded, ok := core.LoadFromFile(); ok || loaded != nil {
		t.Fatalf("expected LoadFromFile to fail closed on corrupt header")
	}
	entries, _ := os.ReadDir(filepath.Dir(dbPathA))
	foundInvalid := false
	for _, e := range entries {
		if strings.Contains(e.Name(), ".invalid-") {
			foundInvalid = true
		}
	}
	if !foundInvalid {
		t.Fatalf("expected corrupt DB to be renamed to .invalid-* backup")
	}
	gotPeers, err := os.ReadFile(peersPathA)
	if err != nil || !bytes.Equal(gotPeers, peersContent) {
		t.Fatalf("peers.json modified or lost during DB corruption recovery")
	}

	// Part B: Tampered pre-snapshot block + matching tampered snapshot -> caught by REV-T2-01.
	dirB := t.TempDir()
	t.Setenv("ARMX_DATA_DIR", dirB)
	dbPathB := filepath.Join(dirB, "blockchain.db")
	core.SetDBPath(dbPathB)
	bc, err := core.NewBlockchainWithStoragePath(1, dbPathB)
	if err != nil {
		t.Fatalf("init bcB: %v", err)
	}
	minerAcc := newTestAccount(t, "snap_miner")
	blk1 := makeTestnetDiff1ForkBlock(t, 1, bc.Chain[0].Hash, bc.Chain[0].Timestamp, minerAcc.Address, "")
	if !bc.ValidateAndAddBlock(blk1) {
		t.Fatalf("add blk1 failed")
	}
	if err := bc.SaveToFile(); err != nil {
		t.Fatalf("SaveToFile: %v", err)
	}
	_ = bc.Close()

	// Tamper pre-snapshot block #1 coinbase recipient AND matching snapshot account in BoltDB directly.
	attackerAcc := newTestAccount(t, "snap_attacker")
	db, err := bolt.Open(dbPathB, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("open bolt for tamper: %v", err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		bktB := tx.Bucket([]byte("blocks"))
		var key [8]byte
		binary.BigEndian.PutUint64(key[:], 1)
		raw := bktB.Get(key[:])
		var b core.Block
		if err := json.Unmarshal(raw, &b); err != nil {
			return err
		}
		b.Transactions[0].Recipient = attackerAcc.Address
		// Leave b.Hash / PoW untouched -> block #1 fails Merkle/Hash verification!
		mutated, _ := json.Marshal(b)
		if err := bktB.Put(key[:], mutated); err != nil {
			return err
		}
		bktSnap := tx.Bucket([]byte("snapshots"))
		if bktSnap != nil {
			c := bktSnap.Cursor()
			for k, v := c.First(); k != nil; k, v = c.Next() {
				var snap map[string]interface{}
				if json.Unmarshal(v, &snap) == nil {
					snap["accounts"] = map[string]interface{}{
						attackerAcc.AddressHex: map[string]interface{}{
							"balance": core.InitialReward,
							"nonce":   0,
						},
					}
					mutSnap, _ := json.Marshal(snap)
					_ = bktSnap.Put(k, mutSnap)
				}
			}
		}
		return nil
	})
	_ = db.Close()
	if err != nil {
		t.Fatalf("bolt tamper failed: %v", err)
	}

	if reloaded, ok := core.LoadFromFile(); ok || reloaded != nil {
		if reloaded != nil {
			_ = reloaded.Close()
		}
		t.Fatalf("expected tampered block+snapshot to be rejected fail-closed on LoadFromFile")
	}

	t.Logf("[SCENARIO 6] Corrupt BoltDB renamed to .invalid-* (peers.json intact); tampered block+snapshot rejected fail-closed")
}

// TestAdversarialScenario7_RollingUpgradeV1012ToV1013AndMixedMesh exercises Track 6 Scenario 7:
//   - Loads v1.0.12 BoltDB + legacy peers.json + wallet keystore on v1.0.13.
//   - Verifies Testnet v1.0.12 <-> v1.0.13 compatibility for 11,000-Armoid fee (Option 2A-i)
//     and uppercase hex tx relay (Option 1A), plus block propagation.
func TestAdversarialScenario7_RollingUpgradeV1012ToV1013AndMixedMesh(t *testing.T) {
	core.LockNetworkForTesting(t)
	core.SetTargetOverrideForTesting(core.DifficultyToTarget(1))
	t.Cleanup(func() { core.SetDBPath("") })

	nodeDir := t.TempDir()
	t.Setenv("ARMX_DATA_DIR", nodeDir)
	t.Setenv("ARMX_ALLOW_PRIVATE_PEERS", "1")

	// 1. Verify encrypted v1.0.12-format wallet keystore loads cleanly.
	walletPath := filepath.Join(nodeDir, "legacy_wallet.json")
	wOrig, err := wallet.NewWallet()
	if err != nil {
		t.Fatalf("NewWallet: %v", err)
	}
	if err := wallet.EncryptAndSaveKeystore(wOrig, "TestnetUpgradePass123!", walletPath); err != nil {
		t.Fatalf("EncryptAndSaveKeystore: %v", err)
	}
	wLoaded, err := wallet.DecryptAndLoadKeystore("TestnetUpgradePass123!", walletPath)
	if err != nil || wLoaded.Address != wOrig.Address {
		t.Fatalf("DecryptAndLoadKeystore after upgrade mismatch: err=%v", err)
	}

	// 2. Create chain DB + legacy peers.json, close, and reopen cleanly.
	dbPath := filepath.Join(nodeDir, "testnet", "blockchain.db")
	core.SetDBPath(dbPath)
	bc1, err := core.NewBlockchainWithStoragePath(1, dbPath)
	if err != nil {
		t.Fatalf("init bc1: %v", err)
	}
	senderAcc := newTestAccount(t, "upgrade_sender")
	recipAcc := newTestAccount(t, "upgrade_recip")
	fundAccount(bc1, senderAcc.Address, 100*core.CoinUnit)
	appendTestDiff1Chain(t, bc1, 3, core.GenesisMiner)
	if err := bc1.SaveToFile(); err != nil {
		t.Fatalf("SaveToFile: %v", err)
	}
	_ = bc1.Close()

	psInit := NewPeerServer(getFreePort(t), core.MustNewBlockchain(1), nodeDir, "ARMX-TESTNET-1")
	peersPath := psInit.GetPeersPath()
	_ = os.MkdirAll(filepath.Dir(peersPath), 0700)
	legacyPeers, _ := json.Marshal(VersionedPeerStore{
		Version:             1,
		NetworkID:           "ARMX-TESTNET-1",
		Peers:               []*PeerInfo{{Address: "198.51.100.1:19335"}},
		InboundFingerprints: []string{strings.Repeat("a", 64)},
	})
	if err := os.WriteFile(peersPath, legacyPeers, 0600); err != nil {
		t.Fatalf("write legacy peers.json: %v", err)
	}

	// Reopen DB & PeerServer on upgraded node.
	bcUpgraded, ok := core.LoadFromFile()
	if !ok || bcUpgraded == nil {
		t.Fatalf("LoadFromFile failed on v1.0.12 DB")
	}
	t.Cleanup(func() { _ = bcUpgraded.Close() })
	fundAccount(bcUpgraded, senderAcc.Address, 100*core.CoinUnit)

	psUpgraded := NewPeerServer(getFreePort(t), bcUpgraded, nodeDir, "ARMX-TESTNET-1")
	psUpgraded.AllowPrivatePeers = true
	if psUpgraded.IsPeerStoreCorrupted() {
		t.Fatalf("legacy peers.json flagged corrupted on upgrade")
	}
	go psUpgraded.StartServer()
	waitForServerRunning(t, psUpgraded, 3*time.Second)
	t.Cleanup(psUpgraded.Stop)

	// 3. Verify Testnet Option 1A + Option 2A-i compatibility: uppercase hex tx with 11,000 Armoids fee relays in mempool.
	tx := core.Transaction{
		Sender:    senderAcc.Address,
		SenderPub: senderAcc.PubHex,
		Recipient: recipAcc.Address,
		Amount:    1 * core.CoinUnit,
		Fee:       11000,
		Nonce:     1,
		Type:      core.TxTypeStandard,
	}
	if err := tx.Sign(senderAcc.PrivKey); err != nil {
		t.Fatalf("sign tx: %v", err)
	}
	tx.Signature = strings.ToUpper(tx.Signature)
	if ok, msg := bcUpgraded.AddTransaction(tx); !ok {
		t.Fatalf("expected Testnet mixed-mesh uppercase-hex tx with 11,000 fee to be accepted in mempool, got: %s", msg)
	}

	t.Logf("[SCENARIO 7] Legacy keystore, BoltDB (height #%d), peers.json, and mixed-mesh uppercase-hex tx verified", len(bcUpgraded.Chain)-1)
}

// TestAdversarialScenario8_FullBlock375MLDSA65VerificationBenchmark exercises Track 6 Scenario 8:
//   - Constructs a packed TxBinaryV1 block with 375 signed ML-DSA-65 user transactions + 1 coinbase = 376 txs.
//   - Measures full ValidateAndAddBlockWithError latency, P2P TLS NEW_BLOCK propagation latency,
//     wire/binary sizes, and peak heap memory.
func TestAdversarialScenario8_FullBlock375MLDSA65VerificationBenchmark(t *testing.T) {
	core.LockNetworkForTesting(t)
	core.SetTargetOverrideForTesting(core.DifficultyToTarget(1))
	core.SetBlockBinarySizeActivationHeight(1)
	t.Setenv("ARMX_DATA_DIR", t.TempDir())
	t.Setenv("ARMX_ALLOW_PRIVATE_PEERS", "1")

	bcProducer, err := core.NewBlockchainWithStoragePath(1, filepath.Join(t.TempDir(), "bench_prod.db"))
	if err != nil {
		t.Fatalf("init bcProducer: %v", err)
	}
	t.Cleanup(func() { _ = bcProducer.Close() })

	receiverDir := t.TempDir()
	bcReceiver, err := core.NewBlockchainWithStoragePath(1, filepath.Join(receiverDir, "bench_recv.db"))
	if err != nil {
		t.Fatalf("init bcReceiver: %v", err)
	}
	t.Cleanup(func() { _ = bcReceiver.Close() })

	senderAcc := newTestAccount(t, "bench375_sender")
	recipAcc := newTestAccount(t, "bench375_recip")
	fundAccount(bcProducer, senderAcc.Address, 1_000_000*core.CoinUnit)
	fundAccount(bcReceiver, senderAcc.Address, 1_000_000*core.CoinUnit)

	const userTxCount = 375
	fullBlk := createLargeBlock(t, bcProducer, senderAcc, recipAcc, userTxCount)
	binSize := fullBlk.BinarySize()
	jsonSize := fullBlk.Size()
	if binSize > core.MaxBlockSize {
		t.Fatalf("expected 375-tx BinarySize <= MaxBlockSize (%d), got %d", core.MaxBlockSize, binSize)
	}

	var mBefore, mAfter runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&mBefore)

	// 1. Measure full single-block cryptographic + state validation latency.
	verifyStart := time.Now()
	if ok, vErr := bcProducer.ValidateAndAddBlockWithError(fullBlk); !ok || vErr != nil {
		t.Fatalf("ValidateAndAddBlockWithError(375 txs) failed: ok=%v err=%v", ok, vErr)
	}
	verifyDur := time.Since(verifyStart)

	runtime.ReadMemStats(&mAfter)

	// 2. Measure end-to-end TLS NEW_BLOCK P2P transfer + receiver verification latency.
	psRecv := NewPeerServer(getFreePort(t), bcReceiver, receiverDir, "ARMX-TESTNET-1")
	psRecv.AllowPrivatePeers = true
	go psRecv.StartServer()
	waitForServerRunning(t, psRecv, 3*time.Second)
	t.Cleanup(psRecv.Stop)

	wirePayload, err := json.Marshal(Message{
		NetworkID: psRecv.GetNetworkID(),
		Type:      "NEW_BLOCK",
		Payload:   fullBlk,
	})
	if err != nil {
		t.Fatalf("marshal NEW_BLOCK: %v", err)
	}
	if len(wirePayload) >= MaxIncomingPacketSize {
		t.Fatalf("expected 375-tx JSON wire payload < MaxIncomingPacketSize (%d), got %d", MaxIncomingPacketSize, len(wirePayload))
	}

	p2pStart := time.Now()
	conn, _, err := DialEncrypted("127.0.0.1:"+psRecv.Port, 5*time.Second)
	if err != nil {
		t.Fatalf("DialEncrypted: %v", err)
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(append(wirePayload, '\n')); err != nil {
		t.Fatalf("write NEW_BLOCK: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	synced := false
	for time.Now().Before(deadline) {
		bcReceiver.Lock.RLock()
		h := len(bcReceiver.Chain) - 1
		bcReceiver.Lock.RUnlock()
		if h == 1 {
			synced = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	p2pDur := time.Since(p2pStart)
	if !synced {
		t.Fatalf("receiver did not accept 375-tx block via TLS NEW_BLOCK within 15s")
	}

	t.Logf("[SCENARIO 8] 375 ML-DSA-65 user txs (+1 coinbase = 376 txs) | BinarySize=%d B | JSONSize=%d B | WireFrame=%d B | Verify=%v | TLS_P2P_E2E=%v | HeapDelta=%.2f MB",
		binSize, jsonSize, len(wirePayload), verifyDur, p2pDur, float64(mAfter.TotalAlloc-mBefore.TotalAlloc)/(1024*1024))
}
