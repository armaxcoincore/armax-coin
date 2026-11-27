package node

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/armaxcoincore/armax-coin/core"
)

func waitForServerRunning(t *testing.T, ps *PeerServer, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ps.GetState() == ServerStateRunning {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("PeerServer did not reach ServerStateRunning within %v (state=%v)", timeout, ps.GetState())
}

func appendTestDiff1Chain(t *testing.T, bc *core.Blockchain, count int, miner string) []core.Block {
	t.Helper()
	bc.Lock.Lock()
	defer bc.Lock.Unlock()
	prev := bc.Chain[len(bc.Chain)-1]
	added := make([]core.Block, 0, count)
	for i := 0; i < count; i++ {
		blk := makeTestnetDiff1ForkBlock(t, prev.Index+1, prev.Hash, prev.Timestamp, miner, "")
		bc.Chain = append(bc.Chain, blk)
		added = append(added, blk)
		prev = blk
	}
	bc.InvalidateChainworkCache()
	return added
}

func makeTestnetDiff1ForkBlock(t *testing.T, index int, prevHash string, prevTimestamp int64, miner string, paddingSig string) core.Block {
	t.Helper()
	txs := []core.Transaction{{
		Sender:    "SYSTEM",
		Recipient: miner,
		Amount:    core.InitialReward,
		Nonce:     int64(index),
		Signature: "REWARD_SIG",
	}}
	if paddingSig != "" {
		txs = append(txs, core.Transaction{
			Sender:    miner,
			Recipient: miner,
			Amount:    1,
			Fee:       0,
			Nonce:     int64(index),
			Signature: paddingSig,
		})
	}
	target := core.DifficultyToTarget(1)
	b := *core.NewBlock(txs, prevHash, index, miner, target)
	b.Timestamp = prevTimestamp + int64(index)*10
	b.Difficulty = 1
	for {
		h, err := b.CalculateHashWithError()
		if err != nil {
			t.Fatalf("CalculateHashWithError failed at #%d: %v", index, err)
		}
		b.Hash = h
		if core.HashMeetsTarget(b.Hash, target) {
			break
		}
		b.Nonce++
	}
	return b
}

// TestP2P_HandleConnectionP2PTOFUSnapshotRaceFree verifies that HandleConnection snapshots P2PTOFU under RLock without data races:
// HandleConnection snapshots P2PTOFU under p.Lock.RLock() so concurrent toggles of
// PeerServer.P2PTOFU under p.Lock do not trigger data races.
func TestP2P_HandleConnectionP2PTOFUSnapshotRaceFree(t *testing.T) {
	t.Setenv("ARMX_DATA_DIR", t.TempDir())
	t.Setenv("ARMX_ALLOW_PRIVATE_PEERS", "1")

	bc, err := core.NewBlockchainWithStoragePath(1, filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatalf("NewBlockchainWithStoragePath failed: %v", err)
	}
	defer bc.Close()

	ps := NewPeerServer(getFreePort(t), bc, t.TempDir(), "ARMX-TESTNET-1")
	ps.AllowPrivatePeers = true
	ps.RequireTLS = false

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	connDone := make(chan struct{})
	go func() {
		defer close(connDone)
		ps.HandleConnection(serverConn, "127.0.0.1")
	}()

	stopToggle := make(chan struct{})
	var toggleWg sync.WaitGroup
	toggleWg.Add(1)
	go func() {
		defer toggleWg.Done()
		for {
			select {
			case <-stopToggle:
				return
			default:
				ps.Lock.Lock()
				ps.P2PTOFU = !ps.P2PTOFU
				ps.Lock.Unlock()
				time.Sleep(1 * time.Millisecond)
			}
		}
	}()

	reader := bufio.NewReader(clientConn)
	netID := ps.GetNetworkID()

	// Exercise all 5 P2PTOFU inspection sites across GET_HEIGHT, PEERS_LIST, and DHT_NODES.
	for i := 0; i < 6; i++ {
		req, err := json.Marshal(Message{NetworkID: netID, Type: "GET_HEIGHT", Payload: nil})
		if err != nil {
			t.Fatalf("marshal GET_HEIGHT: %v", err)
		}
		_ = clientConn.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := clientConn.Write(append(req, '\n')); err != nil {
			t.Fatalf("write GET_HEIGHT #%d: %v", i, err)
		}
		line, err := reader.ReadString('\n')
		if err != nil || !strings.Contains(line, "HEIGHT_RESPONSE") {
			t.Fatalf("expected HEIGHT_RESPONSE on #%d, got %q (err=%v)", i, line, err)
		}

		peersMsg, _ := json.Marshal(Message{NetworkID: netID, Type: "PEERS_LIST", Payload: []string{}})
		_, _ = clientConn.Write(append(peersMsg, '\n'))

		dhtMsg, _ := json.Marshal(Message{NetworkID: netID, Type: "DHT_NODES", Payload: []DHTNodeInfo{}})
		_, _ = clientConn.Write(append(dhtMsg, '\n'))
	}

	close(stopToggle)
	toggleWg.Wait()
	_ = clientConn.Close()

	select {
	case <-connDone:
	case <-time.After(3 * time.Second):
		t.Fatal("HandleConnection did not exit promptly after client close")
	}
}

// TestP2P_RequestChainSyncStreamingClosesOnStop verifies that lagging-peer streaming connections in RequestChainSync/pushChainToPeer register into cycle.activeConns and abort promptly on Stop():
// When RequestChainSync takes the else streaming branch to push blocks to a lagging peer,
// the streaming connection is tracked in cycle.activeConns and Stop() forcibly closes it
// and aborts the pacing loop without hanging until timeout.
func TestP2P_RequestChainSyncStreamingClosesOnStop(t *testing.T) {
	t.Setenv("ARMX_DATA_DIR", t.TempDir())
	t.Setenv("ARMX_ALLOW_PRIVATE_PEERS", "1")

	bc, err := core.NewBlockchainWithStoragePath(1, filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatalf("NewBlockchainWithStoragePath failed: %v", err)
	}
	defer bc.Close()

	// Append 25 fast diff-1 blocks so local tip is well ahead of mock peer height 0.
	appendTestDiff1Chain(t, bc, 25, core.GenesisMiner)

	mockDir := t.TempDir()
	mockTLS, err := EnsureNodeTLSConfig(mockDir)
	if err != nil {
		t.Fatalf("EnsureNodeTLSConfig mock failed: %v", err)
	}
	mockLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen mock failed: %v", err)
	}

	psDir := t.TempDir()
	ps := NewPeerServer(getFreePort(t), bc, psDir, "ARMX-TESTNET-1")
	ps.AllowPrivatePeers = true
	ps.RequireTLS = false
	go ps.StartServer()
	waitForServerRunning(t, ps, 3*time.Second)
	defer ps.Stop()

	mockAddr := mockLn.Addr().String()
	streamStarted := make(chan struct{})
	var streamOnce sync.Once
	streamConnCh := make(chan net.Conn, 1)
	mockStop := make(chan struct{})
	var stopOnce sync.Once
	closeMock := func() {
		stopOnce.Do(func() {
			_ = mockLn.Close()
			close(mockStop)
		})
	}

	var acceptWg sync.WaitGroup
	var connMu sync.Mutex
	var mockConns []net.Conn
	defer func() {
		closeMock()
		connMu.Lock()
		for _, c := range mockConns {
			_ = c.Close()
		}
		connMu.Unlock()
		acceptWg.Wait()
	}()

	acceptWg.Add(1)
	go func() {
		defer acceptWg.Done()
		for {
			rawConn, err := mockLn.Accept()
			if err != nil {
				return
			}
			acceptWg.Add(1)
			go func(rc net.Conn) {
				defer acceptWg.Done()
				secConn, _ := UpgradeToEncryptedTransportWithConfig(rc, mockTLS)
				if secConn == nil {
					_ = rc.Close()
					return
				}
				connMu.Lock()
				mockConns = append(mockConns, secConn)
				connMu.Unlock()

				br := bufio.NewReader(secConn)
				for {
					_ = secConn.SetReadDeadline(time.Now().Add(5 * time.Second))
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					var msg Message
					if json.Unmarshal([]byte(line), &msg) != nil {
						continue
					}
					if msg.Type == "GET_HEIGHT" {
						resp, _ := json.Marshal(Message{
							NetworkID: ps.GetNetworkID(),
							Type:      "HEIGHT_RESPONSE",
							Payload:   HeightResponse{Height: 0, LatestHash: core.GenesisHash, Chainwork: "1"},
						})
						_ = secConn.SetWriteDeadline(time.Now().Add(2 * time.Second))
						_, _ = secConn.Write(append(resp, '\n'))
						continue
					}
					if msg.Type == "NEW_BLOCK" {
						streamOnce.Do(func() {
							streamConnCh <- secConn
							close(streamStarted)
						})
						<-mockStop
						return
					}
				}
			}(rawConn)
		}
	}()

	syncDone := make(chan struct{})
	go func() {
		defer close(syncDone)
		ps.RequestChainSync(mockAddr)
	}()

	select {
	case <-streamStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("RequestChainSync did not start streaming NEW_BLOCK to lagging mock peer")
	}

	stopStart := time.Now()
	stopped := ps.StopWithTimeout(5 * time.Second)
	if !stopped {
		t.Fatalf("PeerServer.Stop() did not cleanly drain active streaming cycle")
	}

	select {
	case <-syncDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("RequestChainSync streaming goroutine did not exit promptly after Stop() (waited >2s)")
	}
	if elapsed := time.Since(stopStart); elapsed > 2*time.Second {
		t.Fatalf("Stop()+stream unwind took too long: %v", elapsed)
	}

	// Verify the server-side streaming peer observes connection closure from ps.Stop().
	sc := <-streamConnCh
	buf := make([]byte, 4096)
	closedObserved := false
	_ = sc.SetReadDeadline(time.Now().Add(1 * time.Second))
	for i := 0; i < 64; i++ {
		_, rErr := sc.Read(buf)
		if rErr != nil {
			var ne net.Error
			if errors.As(rErr, &ne) && ne.Timeout() {
				break
			}
			closedObserved = true
			break
		}
	}
	if !closedObserved {
		t.Fatalf("expected streaming connection to be closed by PeerServer.Stop()")
	}
}

// TestP2P_ForkPushPerServerRaceFreeAndByteBounded verifies that PUSH_BLOCKS staging sessions are scoped per PeerServer with mutex protection and a 32 MiB byte ceiling:
// 1) pushSessions state is scoped per PeerServer (no global sharing across instances),
// 2) concurrent handlePushedBlocks calls are race-free,
// 3) maxPushSessions (64) evicts the oldest staged session on overflow, and
// 4) MaxPushSessionBytes (32 MiB) rejects and penalizes both single and cumulative oversized pushes.
func TestP2P_ForkPushPerServerRaceFreeAndByteBounded(t *testing.T) {
	t.Setenv("ARMX_DATA_DIR", t.TempDir())
	t.Setenv("ARMX_ALLOW_PRIVATE_PEERS", "1")

	bc1, err := core.NewBlockchainWithStoragePath(1, filepath.Join(t.TempDir(), "chain1.db"))
	if err != nil {
		t.Fatalf("init bc1: %v", err)
	}
	defer bc1.Close()

	bc2, err := core.NewBlockchainWithStoragePath(1, filepath.Join(t.TempDir(), "chain2.db"))
	if err != nil {
		t.Fatalf("init bc2: %v", err)
	}
	defer bc2.Close()

	// Populate 25 fast diff-1 blocks so fork chunks (up to 10-20 blocks) stay lighter and get staged cleanly.
	added := appendTestDiff1Chain(t, bc1, 25, core.GenesisMiner)
	bc2.Lock.Lock()
	bc2.Chain = append(bc2.Chain, added...)
	bc2.InvalidateChainworkCache()
	bc2.Lock.Unlock()

	ps1 := NewPeerServer(getFreePort(t), bc1, t.TempDir(), "ARMX-TESTNET-1")
	ps2 := NewPeerServer(getFreePort(t), bc2, t.TempDir(), "ARMX-TESTNET-1")
	ps1.AllowPrivatePeers = true
	ps2.AllowPrivatePeers = true

	bc1.Lock.RLock()
	ancestor := bc1.Chain[1]
	bc1.Lock.RUnlock()

	forkMiner := "armx1forkminer00000000000000000000000000000"

	// Build a small 2-block diff-1 fork from ancestor (#1) with far less chainwork than local tip (#4).
	smallB2 := makeTestnetDiff1ForkBlock(t, 2, ancestor.Hash, ancestor.Timestamp, forkMiner, "")
	smallB3 := makeTestnetDiff1ForkBlock(t, 3, smallB2.Hash, smallB2.Timestamp, forkMiner, "")

	// 1. Per-server isolation & concurrent handlePushedBlocks under -race.
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			ip := fmt.Sprintf("198.51.100.%d", id+1)
			addr := fmt.Sprintf("%s:%d", ip, 9000+id)
			ps1.handlePushedBlocks([]core.Block{smallB2}, ip, addr)
			ps1.handlePushedBlocks([]core.Block{smallB3}, ip, addr)
		}(g)
	}
	wg.Wait()

	ps2.pushSessionsMu.Lock()
	ps2Sessions := len(ps2.pushSessions)
	ps2.pushSessionsMu.Unlock()
	if ps2Sessions != 0 {
		t.Fatalf("expected isolated ps2.pushSessions to stay empty (0), got %d", ps2Sessions)
	}

	// Clear ps1 sessions before capacity eviction check.
	ps1.pushSessionsMu.Lock()
	ps1.pushSessions = make(map[string]*pushBufferSession)
	ps1.pushSessionsMu.Unlock()

	// 2. Eviction at maxPushSessions = 64.
	oldestAddr := "198.51.101.0:1000"
	ps1.handlePushedBlocks([]core.Block{smallB2}, "198.51.101.0", oldestAddr)
	time.Sleep(25 * time.Millisecond) // Guarantee strictly earlier lastSeen on Windows clock resolution.

	for i := 1; i <= maxPushSessions; i++ {
		ip := fmt.Sprintf("198.51.101.%d", i)
		addr := fmt.Sprintf("%s:%d", ip, 1000+i)
		ps1.handlePushedBlocks([]core.Block{smallB2}, ip, addr)
	}

	ps1.pushSessionsMu.Lock()
	sessCount := len(ps1.pushSessions)
	_, oldestStillPresent := ps1.pushSessions[oldestAddr]
	ps1.pushSessionsMu.Unlock()

	if sessCount != maxPushSessions {
		t.Fatalf("expected pushSessions capped at %d, got %d", maxPushSessions, sessCount)
	}
	if oldestStillPresent {
		t.Fatalf("expected oldest push session %s to be evicted once exceeding %d", oldestAddr, maxPushSessions)
	}

	// 3. Cumulative byte limit (MaxPushSessionBytes = 32 MiB) across two staged chunks.
	bigSig := strings.Repeat("a", 1_800_000) // ~1.8 MB per block
	var chain20 []core.Block
	prev := ancestor
	for idx := 2; idx <= 21; idx++ {
		blk := makeTestnetDiff1ForkBlock(t, idx, prev.Hash, prev.Timestamp, forkMiner, bigSig)
		chain20 = append(chain20, blk)
		prev = blk
	}

	chunkA := chain20[:10] // ~18 MB (< 32 MiB): staged
	chunkB := chain20[10:] // +~18 MB -> ~36 MB (> 32 MiB): rejected + penalized

	cumIP := "198.51.102.10"
	cumAddr := cumIP + ":9100"
	ps1.handlePushedBlocks(chunkA, cumIP, cumAddr)

	ps1.pushSessionsMu.Lock()
	_, staged := ps1.pushSessions[cumAddr]
	ps1.pushSessionsMu.Unlock()
	if !staged {
		t.Fatalf("expected chunkA (~18 MB < 32 MiB) to be staged")
	}

	ps1.handlePushedBlocks(chunkB, cumIP, cumAddr)

	ps1.pushSessionsMu.Lock()
	_, stillStaged := ps1.pushSessions[cumAddr]
	ps1.pushSessionsMu.Unlock()
	if stillStaged {
		t.Fatalf("expected oversized cumulative session for %s to be purged", cumAddr)
	}
	ps1.banLock.RLock()
	cumScore := ps1.banScores[cumIP]
	ps1.banLock.RUnlock()
	if cumScore < 20 {
		t.Fatalf("expected ban score >= 20 for exceeding MaxPushSessionBytes cumulatively, got %d", cumScore)
	}

	// 4. Single-push > MaxPushSessionBytes (19 * ~1.8 MB ~= 34.2 MB).
	singleIP := "198.51.102.11"
	singleAddr := singleIP + ":9101"
	ps1.handlePushedBlocks(chain20[:19], singleIP, singleAddr)

	ps1.pushSessionsMu.Lock()
	_, singleStaged := ps1.pushSessions[singleAddr]
	ps1.pushSessionsMu.Unlock()
	if singleStaged {
		t.Fatalf("expected oversized single push (>32 MiB) not to be staged")
	}
	ps1.banLock.RLock()
	singleScore := ps1.banScores[singleIP]
	ps1.banLock.RUnlock()
	if singleScore < 20 {
		t.Fatalf("expected ban score >= 20 for oversized single push, got %d", singleScore)
	}
}

// TestP2P_QueryPeerMempoolExpectedResponseType verifies that QueryPeer maps GET_MEMPOOL to MEMPOOL_RESPONSE and drains stale responses:
// GET_MEMPOOL maps to MEMPOOL_RESPONSE in expectedResponseType / isStandardRPCResponseType
// and QueryPeer succeeds end-to-end against a local PeerServer.
func TestP2P_QueryPeerMempoolExpectedResponseType(t *testing.T) {
	if got := expectedResponseType("GET_MEMPOOL"); got != "MEMPOOL_RESPONSE" {
		t.Fatalf("expectedResponseType(GET_MEMPOOL) = %q, want %q", got, "MEMPOOL_RESPONSE")
	}
	if !isStandardRPCResponseType("MEMPOOL_RESPONSE") {
		t.Fatal("isStandardRPCResponseType(MEMPOOL_RESPONSE) = false, want true")
	}

	t.Setenv("ARMX_DATA_DIR", t.TempDir())
	t.Setenv("ARMX_ALLOW_PRIVATE_PEERS", "1")

	bcServer, err := core.NewBlockchainWithStoragePath(1, filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("init bcServer: %v", err)
	}
	defer bcServer.Close()

	bcClient, err := core.NewBlockchainWithStoragePath(1, filepath.Join(t.TempDir(), "client.db"))
	if err != nil {
		t.Fatalf("init bcClient: %v", err)
	}
	defer bcClient.Close()

	portServer := getFreePort(t)
	psServer := NewPeerServer(portServer, bcServer, t.TempDir(), "ARMX-TESTNET-1")
	psServer.AllowPrivatePeers = true
	psServer.RequireTLS = false
	go psServer.StartServer()
	waitForServerRunning(t, psServer, 3*time.Second)
	defer psServer.Stop()

	psClient := NewPeerServer(getFreePort(t), bcClient, t.TempDir(), "ARMX-TESTNET-1")
	psClient.AllowPrivatePeers = true
	psClient.RequireTLS = false
	defer psClient.Stop()

	resp, err := psClient.QueryPeer("127.0.0.1:"+portServer, Message{
		NetworkID: psServer.GetNetworkID(),
		Type:      "GET_MEMPOOL",
		Payload:   nil,
	}, 3*time.Second)
	if err != nil {
		t.Fatalf("QueryPeer(GET_MEMPOOL) failed: %v", err)
	}
	if resp == nil || resp.Type != "MEMPOOL_RESPONSE" {
		t.Fatalf("expected MEMPOOL_RESPONSE, got %+v", resp)
	}
}

// TestP2P_InboundFingerprintCacheBoundedAndNoDiskWrite verifies RAM-only inbound TLS fingerprint caching:
// verifyOrRecordInboundIdentity maintains a RAM-only FIFO cache capped at MaxInboundFingerprints (2048),
// never writes peers.json on inbound TLS connections, and safely reads legacy peers.json files.
func TestP2P_InboundFingerprintCacheBoundedAndNoDiskWrite(t *testing.T) {
	t.Setenv("ARMX_DATA_DIR", t.TempDir())
	t.Setenv("ARMX_ALLOW_PRIVATE_PEERS", "1")

	dir := t.TempDir()
	bc, err := core.NewBlockchainWithStoragePath(1, filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("init bc: %v", err)
	}
	defer bc.Close()

	ps := NewPeerServer(getFreePort(t), bc, dir, "ARMX-TESTNET-1")
	ps.P2PTOFU = true
	peersPath := ps.GetPeersPath()

	if _, statErr := os.Stat(peersPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected fresh peers.json not to exist yet, stat err=%v", statErr)
	}

	const totalCerts = MaxInboundFingerprints + 452 // 2500 total
	for i := 0; i < totalCerts; i++ {
		fp := fmt.Sprintf("%064x", i+1)
		if err := ps.verifyOrRecordInboundIdentity(fp); err != nil {
			t.Fatalf("verifyOrRecordInboundIdentity(%d) failed: %v", i, err)
		}
	}

	// Zero disk writes performed for all 2500 inbound certificates.
	if _, statErr := os.Stat(peersPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected zero disk writes during verifyOrRecordInboundIdentity, yet peers.json exists/err=%v", statErr)
	}

	ps.inboundIdentityMu.Lock()
	cacheLen := len(ps.inboundFingerprints)
	orderLen := len(ps.inboundFingerprintOrder)
	oldestFP := fmt.Sprintf("%064x", 1)
	newestFP := fmt.Sprintf("%064x", totalCerts)
	_, hasOldest := ps.inboundFingerprints[oldestFP]
	_, hasNewest := ps.inboundFingerprints[newestFP]
	ps.inboundIdentityMu.Unlock()

	if cacheLen != MaxInboundFingerprints || orderLen != MaxInboundFingerprints {
		t.Fatalf("expected FIFO cache capped at %d, got map=%d order=%d", MaxInboundFingerprints, cacheLen, orderLen)
	}
	if hasOldest {
		t.Fatalf("expected oldest fingerprint %s to have been evicted", oldestFP)
	}
	if !hasNewest {
		t.Fatalf("expected newest fingerprint %s to remain cached", newestFP)
	}

	// Even when peers.json already exists, inbound cert tracking must not rewrite it,
	// and savePeersLocked must not serialize inbound_fingerprints onto disk.
	if err := ps.AddPeer("198.51.100.5:19335"); err != nil {
		t.Fatalf("AddPeer failed: %v", err)
	}
	beforeBytes, err := os.ReadFile(peersPath)
	if err != nil {
		t.Fatalf("ReadFile(%s) failed: %v", peersPath, err)
	}
	if strings.Contains(string(beforeBytes), "inbound_fingerprints") {
		t.Fatalf("savePeersLocked must omit inbound_fingerprints from peers.json")
	}

	for i := 0; i < 50; i++ {
		if err := ps.verifyOrRecordInboundIdentity(fmt.Sprintf("f%063x", i+1)); err != nil {
			t.Fatalf("verifyOrRecordInboundIdentity post-AddPeer(%d) failed: %v", i, err)
		}
	}
	afterBytes, err := os.ReadFile(peersPath)
	if err != nil {
		t.Fatalf("ReadFile after inbound failed: %v", err)
	}
	if string(beforeBytes) != string(afterBytes) {
		t.Fatalf("peers.json unexpectedly modified by inbound identity tracking")
	}

	// Verify legacy peers.json containing > MaxInboundFingerprints InboundFingerprints loads cleanly & bounded.
	legacyDir := t.TempDir()
	bcLegacy, err := core.NewBlockchainWithStoragePath(1, filepath.Join(legacyDir, "chain.db"))
	if err != nil {
		t.Fatalf("init bcLegacy: %v", err)
	}
	defer bcLegacy.Close()

	// Pre-derive exact path using a dummy PeerServer first, then write legacy JSON and reload.
	psTmp := NewPeerServer(getFreePort(t), bcLegacy, legacyDir, "ARMX-TESTNET-1")
	legacyPeersPath := psTmp.GetPeersPath()
	legacyFPs := make([]string, 0, MaxInboundFingerprints+100)
	for i := 0; i < MaxInboundFingerprints+100; i++ {
		legacyFPs = append(legacyFPs, fmt.Sprintf("%064x", 100000+i))
	}
	legacyStoreBytes, err := json.Marshal(VersionedPeerStore{
		Version:             1,
		NetworkID:           psTmp.GetNetworkID(),
		Peers:               []*PeerInfo{},
		InboundFingerprints: legacyFPs,
	})
	if err != nil {
		t.Fatalf("marshal legacy store: %v", err)
	}
	if err := os.WriteFile(legacyPeersPath, legacyStoreBytes, 0600); err != nil {
		t.Fatalf("write legacy peers.json: %v", err)
	}

	psReloaded := NewPeerServer(getFreePort(t), bcLegacy, legacyDir, "ARMX-TESTNET-1")
	if psReloaded.IsPeerStoreCorrupted() {
		t.Fatalf("legacy peers.json with inbound_fingerprints must load without corruption flag")
	}
	psReloaded.inboundIdentityMu.Lock()
	reloadedLen := len(psReloaded.inboundFingerprints)
	psReloaded.inboundIdentityMu.Unlock()
	if reloadedLen != MaxInboundFingerprints {
		t.Fatalf("expected legacy inbound fingerprints bounded to %d, got %d", MaxInboundFingerprints, reloadedLen)
	}
}

// TestP2P_GetDialablePeersCanonicalAndStrictSeedHostPin verifies canonical dial endpoint separation and strict per-SeedHost TLS pin enforcement:
// - GetPeers returns display hostnames while GetDialablePeers returns canonical ip:port endpoints,
// - ReseedPeers DNS IP rotations inherit originating SeedHost pins instead of silently bypassing checkSeedPin, and
// - Two seed hostnames sharing an IP enforce strict per-SeedHost pins and fail closed on ambiguous bare IPs.
func TestP2P_GetDialablePeersCanonicalAndStrictSeedHostPin(t *testing.T) {
	t.Setenv("ARMX_DATA_DIR", t.TempDir())
	t.Setenv("ARMX_ALLOW_PRIVATE_PEERS", "1")

	dir := t.TempDir()
	bc, err := core.NewBlockchainWithStoragePath(1, filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("init bc: %v", err)
	}
	defer bc.Close()

	ps := NewPeerServer(getFreePort(t), bc, dir, "ARMX-TESTNET-1")
	ps.AllowPrivatePeers = true
	ps.P2PTOFU = true

	seedA := "seed-a.example.invalid:19335"
	seedB := "seed-b.example.invalid:19335"
	ipA1 := "203.0.113.10:19335"
	ipA2 := "203.0.113.11:19335"
	sharedIP := "203.0.113.50:19335"

	fpA := strings.Repeat("a", 64)
	fpB := strings.Repeat("b", 64)
	fpEvil := strings.Repeat("e", 64)

	var phase atomic.Int32
	ps.seedMu.Lock()
	ps.resolveSeed = func(addr string, _ bool) []string {
		switch phase.Load() {
		case 0:
			if strings.EqualFold(addr, seedA) {
				return []string{ipA1}
			}
			if strings.EqualFold(addr, seedB) {
				return []string{sharedIP}
			}
		case 1:
			if strings.EqualFold(addr, seedA) {
				return []string{ipA2}
			}
			if strings.EqualFold(addr, seedB) {
				return []string{sharedIP}
			}
		case 2:
			if strings.EqualFold(addr, seedA) || strings.EqualFold(addr, seedB) {
				return []string{sharedIP}
			}
		}
		return []string{addr}
	}
	ps.seedMu.Unlock()

	if err := ps.SetSeedFingerprints(map[string]string{seedA: fpA}); err != nil {
		t.Fatalf("SetSeedFingerprints phase 0: %v", err)
	}
	ps.SetSeedPeers([]string{seedA})

	displayPeers := ps.GetPeers()
	dialablePeers := ps.GetDialablePeers()
	if len(displayPeers) != 1 || displayPeers[0] != seedA {
		t.Fatalf("GetPeers() = %v, want [%s]", displayPeers, seedA)
	}
	if len(dialablePeers) != 1 || dialablePeers[0] != ipA1 {
		t.Fatalf("GetDialablePeers() = %v, want [%s]", dialablePeers, ipA1)
	}
	if err := ps.checkSeedPin(ipA1, fpA); err != nil {
		t.Fatalf("checkSeedPin(ipA1, fpA) unexpected error: %v", err)
	}
	if err := ps.checkSeedPin(ipA1, fpEvil); err == nil {
		t.Fatalf("checkSeedPin(ipA1, fpEvil) must reject wrong fingerprint")
	}

	// Rotate DNS of seedA to ipA2 -> ReseedPeers must propagate fpA pin and SeedHost=seedA.
	phase.Store(1)
	ps.ReseedPeers()

	if gotHost := ps.lookupPeerSeedHost(ipA2); gotHost != seedA {
		t.Fatalf("lookupPeerSeedHost(%s) = %q, want %q", ipA2, gotHost, seedA)
	}
	if err := ps.checkSeedPin(ipA2, fpA); err != nil {
		t.Fatalf("rotated IP %s did not inherit seed pin fpA: %v", ipA2, err)
	}
	if err := ps.checkSeedPin(ipA2, fpEvil); err == nil {
		t.Fatalf("rotated IP %s must reject evil fingerprint", ipA2)
	}

	// Shared IP across seedA (fpA) and seedB (fpB): strict per-SeedHost & fail-closed on bare sharedIP.
	phase.Store(2)
	if err := ps.SetSeedFingerprints(map[string]string{
		seedA: fpA,
		seedB: fpB,
	}); err != nil {
		t.Fatalf("SetSeedFingerprints phase 2: %v", err)
	}
	ps.SetSeedPeers([]string{seedA, seedB})

	if err := ps.checkSeedPinWithHost(sharedIP, seedA, fpA); err != nil {
		t.Fatalf("strict SeedHost check(seedA, fpA) failed: %v", err)
	}
	if err := ps.checkSeedPinWithHost(sharedIP, seedA, fpB); err == nil {
		t.Fatalf("strict SeedHost check(seedA, fpB) MUST reject another hostname's pin on shared IP")
	}
	if err := ps.checkSeedPinWithHost(sharedIP, seedB, fpB); err != nil {
		t.Fatalf("strict SeedHost check(seedB, fpB) failed: %v", err)
	}
	if err := ps.checkSeedPinWithHost(sharedIP, seedB, fpA); err == nil {
		t.Fatalf("strict SeedHost check(seedB, fpA) MUST reject fpA")
	}

	// Bare shared IP lookup has two conflicting pins and cleared SeedHost -> must fail closed!
	if err := ps.checkSeedPin(sharedIP, fpA); err == nil {
		t.Fatalf("ambiguous bare shared IP must fail closed for fpA")
	}
	if err := ps.checkSeedPin(sharedIP, fpB); err == nil {
		t.Fatalf("ambiguous bare shared IP must fail closed for fpB")
	}
	if err := ps.VerifyOrRecordTOFU(sharedIP, &mockTLSConn{fingerprint: fpA}); err == nil {
		t.Fatalf("VerifyOrRecordTOFU on ambiguous bare shared IP must fail closed")
	}
}

// TestP2P_InboundOutboundSlotReservationAndInvariant verifies atomic inbound/outbound slot separation preserving ActiveInbound + ActiveOutbound <= MaxPeers:
// - Default MaxPeers=50 splits strictly to 38 inbound / 12 outbound, custom MaxPeers=10 splits to 8/2,
// - Inbound exhaustion never steals reserved outbound slots, and
// - Concurrent reservations maintain activeInbound + activeOutbound == ActiveConns <= MaxPeers without leaks.
func TestP2P_InboundOutboundSlotReservationAndInvariant(t *testing.T) {
	t.Setenv("ARMX_DATA_DIR", t.TempDir())
	t.Setenv("ARMX_ALLOW_PRIVATE_PEERS", "1")

	bc, err := core.NewBlockchainWithStoragePath(1, filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatalf("init bc: %v", err)
	}
	defer bc.Close()

	ps := NewPeerServer(getFreePort(t), bc, t.TempDir(), "ARMX-TESTNET-1")
	ps.AllowPrivatePeers = true
	ps.RequireTLS = false

	// 1. Default 50 -> (38 inbound, 12 outbound)
	total, maxIn, maxOut := ps.effectiveSlotLimits()
	if total != 50 || maxIn != 38 || maxOut != 12 {
		t.Fatalf("default limits = (%d, %d, %d), want (50, 38, 12)", total, maxIn, maxOut)
	}

	var releases []func()
	for i := 0; i < 38; i++ {
		rel, rejectMsg, ok := ps.tryReserveInboundSlot(fmt.Sprintf("198.51.%d.0", i+1), false)
		if !ok || rejectMsg != "" || rel == nil {
			t.Fatalf("inbound reservation #%d unexpectedly failed: ok=%v msg=%q", i+1, ok, rejectMsg)
		}
		releases = append(releases, rel)
	}
	if _, rejectMsg, ok := ps.tryReserveInboundSlot("198.51.200.0", false); ok || rejectMsg != "ERROR: Connection limit exceeded\n" {
		t.Fatalf("39th inbound slot should be rejected with Connection limit exceeded, got ok=%v msg=%q", ok, rejectMsg)
	}

	// Outbound reserved slots (12) must remain completely available even with inbound saturated.
	for i := 0; i < 12; i++ {
		rel, ok := ps.tryReserveOutboundSlot()
		if !ok || rel == nil {
			t.Fatalf("reserved outbound slot #%d failed despite inbound-only saturation", i+1)
		}
		releases = append(releases, rel)
	}
	if _, ok := ps.tryReserveOutboundSlot(); ok {
		t.Fatal("13th outbound slot should be rejected at MaxPeers=50")
	}
	if got := atomic.LoadInt32(&ps.ActiveConns); got != 50 {
		t.Fatalf("ActiveConns = %d, want 50", got)
	}

	// DialPeer also fails fast when outbound slots are exhausted.
	if _, _, err := ps.DialPeer("127.0.0.1:"+getFreePort(t), 200*time.Millisecond); err == nil {
		t.Fatal("DialPeer must fail when outbound slots are exhausted")
	}

	for _, rel := range releases {
		rel()
		rel() // verify idempotency
	}
	if atomic.LoadInt32(&ps.ActiveConns) != 0 || atomic.LoadInt32(&ps.activeInbound) != 0 || atomic.LoadInt32(&ps.activeOutbound) != 0 {
		t.Fatalf("slot counters did not return to zero: ActiveConns=%d in=%d out=%d",
			atomic.LoadInt32(&ps.ActiveConns), atomic.LoadInt32(&ps.activeInbound), atomic.LoadInt32(&ps.activeOutbound))
	}

	// Calling DialPeer against an unreachable closed port releases its reserved outbound slot cleanly.
	_, _, dialErr := ps.DialPeer("127.0.0.1:"+getFreePort(t), 150*time.Millisecond)
	if dialErr == nil {
		t.Fatal("expected dial error against closed local port")
	}
	if atomic.LoadInt32(&ps.ActiveConns) != 0 || atomic.LoadInt32(&ps.activeOutbound) != 0 {
		t.Fatalf("failed DialPeer leaked outbound slot: ActiveConns=%d out=%d",
			atomic.LoadInt32(&ps.ActiveConns), atomic.LoadInt32(&ps.activeOutbound))
	}

	// 2. Custom MaxPeers = 10 -> (8 inbound, 2 outbound)
	ps.Lock.Lock()
	ps.MaxPeers = 10
	ps.Lock.Unlock()

	total10, in10, out10 := ps.effectiveSlotLimits()
	if total10 != 10 || in10 != 8 || out10 != 2 {
		t.Fatalf("MaxPeers=10 limits = (%d, %d, %d), want (10, 8, 2)", total10, in10, out10)
	}

	// 3. Concurrent reserve/release invariant storm under -race
	ps.Lock.Lock()
	ps.MaxPeers = 20
	ps.Lock.Unlock()
	totalMax, _, _ := ps.effectiveSlotLimits()

	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for iter := 0; iter < 30; iter++ {
				var rel func()
				var ok bool
				if (id+iter)%2 == 0 {
					rel, _, ok = ps.tryReserveInboundSlot(fmt.Sprintf("198.51.%d.0", (id+iter)%250+1), false)
				} else {
					rel, ok = ps.tryReserveOutboundSlot()
				}
				if ok {
					ps.connLock.Lock()
					in := int(atomic.LoadInt32(&ps.activeInbound))
					out := int(atomic.LoadInt32(&ps.activeOutbound))
					active := int(atomic.LoadInt32(&ps.ActiveConns))
					ps.connLock.Unlock()
					if in+out != active || active > totalMax {
						t.Errorf("slot invariant violated: in=%d out=%d active=%d totalMax=%d", in, out, active, totalMax)
					}
					time.Sleep(100 * time.Microsecond)
					rel()
				}
			}
		}(g)
	}
	wg.Wait()

	if atomic.LoadInt32(&ps.ActiveConns) != 0 || atomic.LoadInt32(&ps.activeInbound) != 0 || atomic.LoadInt32(&ps.activeOutbound) != 0 {
		t.Fatalf("post-storm slot leak: ActiveConns=%d in=%d out=%d",
			atomic.LoadInt32(&ps.ActiveConns), atomic.LoadInt32(&ps.activeInbound), atomic.LoadInt32(&ps.activeOutbound))
	}
}

// TestP2P_LargePacketAbove4MBAndAggregateBudget verifies 8 MiB frame acceptance and aggregate large-payload memory budgeting:
//   - Frames > 4 MiB (up to 8 MiB MaxIncomingPacketSize) are accepted cleanly without disconnects,
//   - Saturated MaxAggregateLargePayloadBytes (32 MiB) applies ~2s bounded backpressure (LargePayloadAcquireTimeout) that recovers
//     when a lease is freed in time and fails cleanly on deadline timeout without memory bloat.
func TestP2P_LargePacketAbove4MBAndAggregateBudget(t *testing.T) {
	t.Setenv("ARMX_DATA_DIR", t.TempDir())
	t.Setenv("ARMX_ALLOW_PRIVATE_PEERS", "1")

	dir := t.TempDir()
	bc, err := core.NewBlockchainWithStoragePath(1, filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("init bc: %v", err)
	}
	defer bc.Close()

	port := getFreePort(t)
	ps := NewPeerServer(port, bc, dir, "ARMX-TESTNET-1")
	ps.AllowPrivatePeers = true
	ps.RequireTLS = false
	go ps.StartServer()
	waitForServerRunning(t, ps, 3*time.Second)
	defer ps.Stop()

	// 1. Direct readBoundedLineBytes > 4.2 MiB with lease acquire/release accounting
	bigLine := append(bytes.Repeat([]byte("x"), 4_500_000), '\n')
	gotBytes, rel, err := readBoundedLineBytes(bufio.NewReaderSize(bytes.NewReader(bigLine), 64*1024), MaxIncomingPacketSize, func() (func(), bool) {
		return ps.acquireLargePayloadLease(MaxIncomingPacketSize, time.Now().Add(time.Second), nil)
	})
	if err != nil || len(gotBytes) != 4_500_000 {
		t.Fatalf("readBoundedLineBytes 4.5MB failed: len=%d err=%v", len(gotBytes), err)
	}
	ps.largePayloadMu.Lock()
	usedWhileHeld := ps.largePayloadUsed
	ps.largePayloadMu.Unlock()
	if usedWhileHeld != MaxIncomingPacketSize {
		t.Fatalf("expected largePayloadUsed == %d while lease held, got %d", MaxIncomingPacketSize, usedWhileHeld)
	}
	rel()
	ps.largePayloadMu.Lock()
	usedAfterRel := ps.largePayloadUsed
	ps.largePayloadMu.Unlock()
	if usedAfterRel != 0 {
		t.Fatalf("expected largePayloadUsed == 0 after release, got %d", usedAfterRel)
	}

	// 2. End-to-end > 4.2 MiB NEW_TX frame accepted over HandleConnection, keeping connection healthy
	conn, _, err := DialEncrypted("127.0.0.1:"+port, 3*time.Second)
	if err != nil {
		t.Fatalf("DialEncrypted: %v", err)
	}
	defer conn.Close()

	largeTxMsg, err := json.Marshal(Message{
		NetworkID: ps.GetNetworkID(),
		Type:      "NEW_TX",
		Payload: core.Transaction{
			Sender:   core.GenesisMiner,
			Preimage: strings.Repeat("b", 4_300_000),
		},
	})
	if err != nil {
		t.Fatalf("marshal large NEW_TX: %v", err)
	}
	if len(largeTxMsg) <= 4*1024*1024 {
		t.Fatalf("expected wire frame > 4 MiB, got %d", len(largeTxMsg))
	}
	_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write(append(largeTxMsg, '\n')); err != nil {
		t.Fatalf("write large NEW_TX: %v", err)
	}

	getHeightMsg, _ := json.Marshal(Message{NetworkID: ps.GetNetworkID(), Type: "GET_HEIGHT", Payload: nil})
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write(append(getHeightMsg, '\n')); err != nil {
		t.Fatalf("write GET_HEIGHT after >4MiB frame: %v", err)
	}
	respLine, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || !strings.Contains(respLine, "HEIGHT_RESPONSE") {
		t.Fatalf("expected HEIGHT_RESPONSE on same conn after >4MiB frame, got %q err=%v", respLine, err)
	}

	// 3. Aggregate 32 MiB saturation: transient waiter recovers < 2s when slot frees; sustained overflow fails
	held := make([]func(), 0, 4)
	for i := 0; i < 4; i++ {
		l, ok := ps.acquireLargePayloadLease(MaxIncomingPacketSize, time.Now().Add(3*time.Second), nil)
		if !ok {
			t.Fatalf("lease #%d unexpected fail", i+1)
		}
		held = append(held, l)
	}

	// Timeout case: short 120ms deadline with all 32 MiB held must fail cleanly
	if _, ok := ps.acquireLargePayloadLease(MaxIncomingPacketSize, time.Now().Add(120*time.Millisecond), nil); ok {
		t.Fatal("expected saturated aggregate budget acquire past deadline to fail")
	}

	// Backpressure recovery case: waiter unblocks as soon as held[0] frees at ~120ms (< LargePayloadAcquireTimeout)
	waitDone := make(chan bool, 1)
	go func() {
		l5, ok := ps.acquireLargePayloadLease(MaxIncomingPacketSize, time.Now().Add(5*time.Second), nil)
		if ok {
			l5()
		}
		waitDone <- ok
	}()
	time.Sleep(120 * time.Millisecond)
	held[0]()

	select {
	case ok := <-waitDone:
		if !ok {
			t.Fatal("expected blocked large-payload waiter to acquire once lease freed within LargePayloadAcquireTimeout wait window")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("large-payload waiter did not wake promptly after lease release")
	}

	for _, l := range held[1:] {
		l()
	}
	ps.largePayloadMu.Lock()
	finalUsed := ps.largePayloadUsed
	ps.largePayloadMu.Unlock()
	if finalUsed != 0 {
		t.Fatalf("largePayloadUsed leaked: %d", finalUsed)
	}
}

// TestP2P_HandleConnection_DropsBannedPeerAndNoDeadlineRefresh verifies immediate return for banned peers and strict deadline refresh behavior:
// Reaching BanScore >= 100 exits HandleConnection immediately without waiting for EOF,
// and rejected/throttled packets never advance the socket deadline.
func TestP2P_HandleConnection_DropsBannedPeerAndNoDeadlineRefresh(t *testing.T) {
	t.Setenv("ARMX_DATA_DIR", t.TempDir())
	t.Setenv("ARMX_ALLOW_PRIVATE_PEERS", "1")

	bc, err := core.NewBlockchainWithStoragePath(1, filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatalf("init bc: %v", err)
	}
	defer bc.Close()

	ps := NewPeerServer(getFreePort(t), bc, t.TempDir(), "ARMX-TESTNET-1")
	ps.AllowPrivatePeers = true
	ps.RequireTLS = false

	// 1. Mid-session ban immediately drops HandleConnection even while clientConn stays open
	srv, cli := net.Pipe()
	defer srv.Close()
	defer cli.Close()

	atomic.StoreInt32(&ps.ActiveConns, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ps.HandleConnection(srv, "127.0.0.1")
	}()

	for i := 0; i < 10; i++ {
		_ = cli.SetWriteDeadline(time.Now().Add(1 * time.Second))
		if _, err := cli.Write([]byte("not-valid-json\n")); err != nil {
			break
		}
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HandleConnection did not return promptly after peer hit BanScore >= 100")
	}
	if !ps.IsBanned("pipe") {
		t.Fatal("expected peer IP 'pipe' to be marked banned")
	}
	if got := atomic.LoadInt32(&ps.ActiveConns); got != 0 {
		t.Fatalf("expected ActiveConns decremented to 0 after ban exit, got %d", got)
	}

	// 2. Deadline stays strictly unchanged on invalid/throttled packets, and only refreshes on valid allowed frames.
	srv2, cli2 := net.Pipe()
	defer srv2.Close()
	defer cli2.Close()
	go func() {
		buf := make([]byte, 512)
		for {
			if _, err := cli2.Read(buf); err != nil {
				return
			}
		}
	}()

	t0 := time.Now().Add(3 * time.Second)
	sess := inboundConnSession{
		conn:                  srv2,
		remoteIP:              "198.51.100.7",
		remoteAddr:            "198.51.100.7:1234",
		p2pTOFU:               false,
		readTimeout:           InboundHandshakeTimeout,
		deadline:              t0,
		connMempoolTokens:     0, // empty per-conn mempool bucket
		connMempoolLastRefill: time.Now(),
	}

	if keep := ps.processInboundFrame(&sess, []byte("   "), nil); !keep || !sess.deadline.Equal(t0) {
		t.Fatalf("whitespace frame must not extend deadline: keep=%v deadline=%v t0=%v", keep, sess.deadline, t0)
	}
	if keep := ps.processInboundFrame(&sess, []byte(`{"network_id":"WRONG-NET","type":"GET_HEIGHT"}`), nil); !keep || !sess.deadline.Equal(t0) {
		t.Fatalf("alien network frame must not extend deadline: keep=%v deadline=%v", keep, sess.deadline)
	}
	mempoolFrame, _ := json.Marshal(Message{NetworkID: ps.GetNetworkID(), Type: "GET_MEMPOOL"})
	if keep := ps.processInboundFrame(&sess, mempoolFrame, nil); !keep || !sess.deadline.Equal(t0) {
		t.Fatalf("throttled GET_MEMPOOL must not extend deadline: keep=%v deadline=%v", keep, sess.deadline)
	}

	heightFrame, _ := json.Marshal(Message{NetworkID: ps.GetNetworkID(), Type: "GET_HEIGHT"})
	if keep := ps.processInboundFrame(&sess, heightFrame, nil); !keep || !sess.deadline.After(t0) {
		t.Fatalf("valid GET_HEIGHT should extend deadline past %v, got %v (keep=%v)", t0, sess.deadline, keep)
	}
}

// TestP2P_GetMempoolSharedNATAndLegacyPollingCompat verifies dual-tier GET_MEMPOOL rate limiting (NAT-safe per-connection + per-IP buckets, rolling v1.0.10-v1.0.12 compat):
// - Per-connection GET_MEMPOOL burst 3 passes and 4th throttles with BanScore == 0,
// - 3 honest NAT peers sharing one IP pass startup + simulated 30s polling without throttle, and
// - Aggregate per-IP GET_MEMPOOL spam past burst 6 throttles cleanly with zero ban score.
func TestP2P_GetMempoolSharedNATAndLegacyPollingCompat(t *testing.T) {
	t.Setenv("ARMX_DATA_DIR", t.TempDir())
	t.Setenv("ARMX_ALLOW_PRIVATE_PEERS", "1")

	bc, err := core.NewBlockchainWithStoragePath(1, filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatalf("init bc: %v", err)
	}
	defer bc.Close()

	ps := NewPeerServer(getFreePort(t), bc, t.TempDir(), "ARMX-TESTNET-1")
	ps.AllowPrivatePeers = true
	ps.RequireTLS = false

	// 1. Per-connection burst 3 passes, 4th silent throttle, connection alive & BanScore == 0
	srv, cli := net.Pipe()
	defer srv.Close()
	defer cli.Close()
	go ps.HandleConnection(srv, "198.51.100.0")

	reader := bufio.NewReader(cli)
	getMP, _ := json.Marshal(Message{NetworkID: ps.GetNetworkID(), Type: "GET_MEMPOOL"})
	for i := 0; i < 3; i++ {
		_ = cli.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := cli.Write(append(getMP, '\n')); err != nil {
			t.Fatalf("write GET_MEMPOOL #%d: %v", i+1, err)
		}
		line, err := reader.ReadString('\n')
		if err != nil || !strings.Contains(line, "MEMPOOL_RESPONSE") {
			t.Fatalf("GET_MEMPOOL #%d failed: line=%q err=%v", i+1, line, err)
		}
	}

	// 4th immediate GET_MEMPOOL on same connection throttled silently
	_ = cli.SetWriteDeadline(time.Now().Add(1 * time.Second))
	if _, err := cli.Write(append(getMP, '\n')); err != nil {
		t.Fatalf("write 4th GET_MEMPOOL: %v", err)
	}
	_ = cli.SetReadDeadline(time.Now().Add(120 * time.Millisecond))
	if extra, err := reader.ReadString('\n'); err == nil {
		t.Fatalf("expected 4th immediate GET_MEMPOOL on single conn to be throttled, got %q", extra)
	}

	// Verify connection stays healthy (GET_HEIGHT succeeds) and BanScore == 0
	getH, _ := json.Marshal(Message{NetworkID: ps.GetNetworkID(), Type: "GET_HEIGHT"})
	_ = cli.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := cli.Write(append(getH, '\n')); err != nil {
		t.Fatalf("write GET_HEIGHT after mempool throttle: %v", err)
	}
	hLine, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(hLine, "HEIGHT_RESPONSE") {
		t.Fatalf("expected HEIGHT_RESPONSE on healthy conn, got %q err=%v", hLine, err)
	}
	ps.banLock.RLock()
	pipeBanScore := ps.banScores["pipe"]
	ps.banLock.RUnlock()
	if pipeBanScore != 0 {
		t.Fatalf("GET_MEMPOOL throttle must not add ban score, got %d", pipeBanScore)
	}

	// 2. Shared NAT IP with 3 peers polling every 30s
	natIP := "198.51.100.20"
	for i := 0; i < 6; i++ {
		if !ps.AllowMessage(natIP, "GET_MEMPOOL") {
			t.Fatalf("expected NAT burst request #%d within IP burst 6 to be allowed", i+1)
		}
	}
	// Simulate 30s elapsed between legacy SyncMempool cycles (30s / 8s = 3.75 tokens refilled >= 3 NAT peers)
	ps.banLock.Lock()
	ps.rateTrackers[natIP].lastMempoolRefill = time.Now().Add(-30 * time.Second)
	ps.banLock.Unlock()

	for pIdx := 0; pIdx < 3; pIdx++ {
		if !ps.AllowMessage(natIP, "GET_MEMPOOL") {
			t.Fatalf("expected NAT peer #%d at 30s poll interval to pass without throttling", pIdx+1)
		}
	}

	// 3. Single-IP burst spam > 6 throttles gracefully with BanScore == 0
	spamIP := "198.51.100.21"
	allowed := 0
	for i := 0; i < 10; i++ {
		if ps.AllowMessage(spamIP, "GET_MEMPOOL") {
			allowed++
		}
	}
	if allowed != 6 {
		t.Fatalf("expected exact burst 6 allowed per IP, got %d", allowed)
	}
	ps.banLock.RLock()
	spamBanScore := ps.banScores[spamIP]
	ps.banLock.RUnlock()
	if spamBanScore != 0 {
		t.Fatalf("expected BanScore == 0 on throttled IP spam, got %d", spamBanScore)
	}
}

// TestP2P_RebroadcastMempool_DedupAndBoundedWorkers verifies per-(tx, peer) relay deduplication and bounded RebroadcastMempool workers:
// RebroadcastMempool caps txs at maxRebroadcastTxsPerCycle (100), skips peers already claimed in
// relayedTxs via claimTxRelay, and runs deduplicated cycles without redundant sends.
func TestP2P_RebroadcastMempool_DedupAndBoundedWorkers(t *testing.T) {
	t.Setenv("ARMX_DATA_DIR", t.TempDir())
	t.Setenv("ARMX_ALLOW_PRIVATE_PEERS", "1")

	dir := t.TempDir()
	bc, err := core.NewBlockchainWithStoragePath(1, filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("init bc: %v", err)
	}
	defer bc.Close()

	ps := NewPeerServer(getFreePort(t), bc, dir, "ARMX-TESTNET-1")
	ps.AllowPrivatePeers = true
	ps.RequireTLS = false
	defer ps.Stop()

	// Create 110 distinct mempool txs (exceeding maxRebroadcastTxsPerCycle = 100)
	sender := newTestAccount(t, "reb_sender")
	recip := newTestAccount(t, "reb_recip")
	allTxs := make([]core.Transaction, 0, 110)
	for i := 1; i <= 110; i++ {
		allTxs = append(allTxs, core.Transaction{
			Sender:    sender.Address,
			Recipient: recip.Address,
			Amount:    core.CoinUnit,
			Fee:       core.MinRelayFee,
			Nonce:     int64(i),
			Type:      core.TxTypeStandard,
		})
	}
	bc.Lock.Lock()
	bc.Mempool = append(bc.Mempool, allTxs...)
	bc.Lock.Unlock()

	mockDir := t.TempDir()
	mockTLS, err := EnsureNodeTLSConfig(mockDir)
	if err != nil {
		t.Fatalf("EnsureNodeTLSConfig: %v", err)
	}

	var receivedC atomic.Int32
	lnC, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen C: %v", err)
	}
	var dialB atomic.Int32
	lnB, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen B: %v", err)
	}

	var wg sync.WaitGroup
	stopMock := make(chan struct{})
	defer func() {
		close(stopMock)
		_ = lnC.Close()
		_ = lnB.Close()
		wg.Wait()
	}()

	acceptLoop := func(ln net.Listener, onNewTx func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				rc, err := ln.Accept()
				if err != nil {
					return
				}
				if onNewTx == nil {
					dialB.Add(1)
					_ = rc.Close()
					continue
				}
				wg.Add(1)
				go func(raw net.Conn) {
					defer wg.Done()
					sc, _ := UpgradeToEncryptedTransportWithConfig(raw, mockTLS)
					if sc == nil {
						_ = raw.Close()
						return
					}
					defer sc.Close()
					br := bufio.NewReader(sc)
					for {
						select {
						case <-stopMock:
							return
						default:
						}
						_ = sc.SetReadDeadline(time.Now().Add(3 * time.Second))
						line, err := br.ReadString('\n')
						if err != nil {
							return
						}
						var msg rawInboundMessage
						if json.Unmarshal([]byte(line), &msg) == nil && msg.Type == "NEW_TX" {
							onNewTx()
						}
					}
				}(rc)
			}
		}()
	}
	acceptLoop(lnC, func() { receivedC.Add(1) })
	acceptLoop(lnB, nil)

	addrB := lnB.Addr().String()
	addrC := lnC.Addr().String()

	// Pre-claim all 110 txs for peerB so RebroadcastMempool skips peerB cleanly.
	for _, tx := range allTxs {
		if _, claimed := ps.claimTxRelay(tx.HashHex(), addrB); !claimed {
			t.Fatalf("pre-claim failed for %s", addrB)
		}
	}

	ps.Lock.Lock()
	ps.Peers = []string{addrB, addrC}
	ps.dialablePeers = []string{addrB, addrC}
	ps.Lock.Unlock()

	ps.RebroadcastMempool()

	// Wait briefly for in-flight writes on peerC's TCP socket to flush to listener counter.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && receivedC.Load() < maxRebroadcastTxsPerCycle {
		time.Sleep(15 * time.Millisecond)
	}
	if got := receivedC.Load(); got != maxRebroadcastTxsPerCycle {
		t.Fatalf("peerC received %d txs, want exact cap %d", got, maxRebroadcastTxsPerCycle)
	}
	if bDials := dialB.Load(); bDials != 0 {
		t.Fatalf("expected 0 dials to pre-relayed peerB, got %d", bDials)
	}

	// Second immediate RebroadcastMempool call must deliver 0 duplicate txs to peerC.
	ps.RebroadcastMempool()
	time.Sleep(60 * time.Millisecond)
	if got := receivedC.Load(); got != maxRebroadcastTxsPerCycle {
		t.Fatalf("expected 0 duplicate rebroadcasts on second cycle, total=%d want=%d", got, maxRebroadcastTxsPerCycle)
	}
}

// TestP2P_QueryPeerTimeoutNoRedialVsResetTOFUProbe verifies that QueryPeer skips secondary TOFU probes on timeouts while still probing unexpected EOF/resets:
// - Silent read timeout skips TOFU re-dial probe (1 dial, ~timeout duration),
// - Unexpected EOF on a fresh non-reused session still triggers a 2nd TOFU verification probe dial.
func TestP2P_QueryPeerTimeoutNoRedialVsResetTOFUProbe(t *testing.T) {
	t.Setenv("ARMX_DATA_DIR", t.TempDir())
	t.Setenv("ARMX_ALLOW_PRIVATE_PEERS", "1")

	bc, err := core.NewBlockchainWithStoragePath(1, filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatalf("init bc: %v", err)
	}
	defer bc.Close()

	ps := NewPeerServer(getFreePort(t), bc, t.TempDir(), "ARMX-TESTNET-1")
	ps.AllowPrivatePeers = true
	ps.RequireTLS = true
	ps.P2PTOFU = true
	defer ps.Stop()

	mockTLS, err := EnsureNodeTLSConfig(t.TempDir())
	if err != nil {
		t.Fatalf("EnsureNodeTLSConfig: %v", err)
	}

	var wg sync.WaitGroup
	stopAll := make(chan struct{})

	// Listener A: accepts TLS handshake, reads GET_HEIGHT, stalls silently until stopAll.
	lnTimeout, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen A: %v", err)
	}
	var dialsTimeout atomic.Int32
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			rc, err := lnTimeout.Accept()
			if err != nil {
				return
			}
			dialsTimeout.Add(1)
			wg.Add(1)
			go func(raw net.Conn) {
				defer wg.Done()
				sc, _ := UpgradeToEncryptedTransportWithConfig(raw, mockTLS)
				if sc == nil {
					_ = raw.Close()
					return
				}
				defer sc.Close()
				_, _ = bufio.NewReader(sc).ReadString('\n')
				<-stopAll
			}(rc)
		}
	}()

	// Listener B: drops connection #1 abruptly on the initial fresh GET_HEIGHT request,
	// then accepts the resulting TOFU re-dial probe on connection #2.
	lnReset, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen B: %v", err)
	}
	var dialsReset atomic.Int32
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			rc, err := lnReset.Accept()
			if err != nil {
				return
			}
			connIdx := dialsReset.Add(1)
			wg.Add(1)
			go func(raw net.Conn, id int32) {
				defer wg.Done()
				sc, _ := UpgradeToEncryptedTransportWithConfig(raw, mockTLS)
				if sc == nil {
					_ = raw.Close()
					return
				}
				defer sc.Close()
				// Force TLS handshake completion so TOFU probe on connection #2 verifies SPKI.
				if tc, ok := sc.(*net.TCPConn); ok {
					_ = tc
				}
				br := bufio.NewReader(sc)
				for {
					_ = sc.SetReadDeadline(time.Now().Add(2 * time.Second))
					line, rErr := br.ReadString('\n')
					if rErr != nil {
						return
					}
					if id == 1 {
						// Abrupt close simulates peer crash/reset right after fresh-session request arrives
						return
					}
					var msg rawInboundMessage
					if json.Unmarshal([]byte(line), &msg) == nil && msg.Type == "GET_HEIGHT" {
						resp, _ := json.Marshal(Message{
							NetworkID: ps.GetNetworkID(),
							Type:      "HEIGHT_RESPONSE",
							Payload:   HeightResponse{Height: 0, LatestHash: core.GenesisHash, Chainwork: "1"},
						})
						_, _ = sc.Write(append(resp, '\n'))
					}
				}
			}(rc, connIdx)
		}
	}()

	defer func() {
		close(stopAll)
		_ = lnTimeout.Close()
		_ = lnReset.Close()
		wg.Wait()
	}()

	// (a) Silent timeout -> 1 dial only, no second timeout probe wait
	startTimeout := time.Now()
	_, errTimeout := ps.QueryPeer(lnTimeout.Addr().String(), Message{
		NetworkID: ps.GetNetworkID(),
		Type:      "GET_HEIGHT",
	}, 250*time.Millisecond)
	elapsedTimeout := time.Since(startTimeout)

	if errTimeout == nil {
		t.Fatal("expected timeout error from silent mock peer")
	}
	if d := dialsTimeout.Load(); d != 1 {
		t.Fatalf("expected 1 dial on timeout (no TOFU re-dial), got %d", d)
	}
	if elapsedTimeout > 480*time.Millisecond {
		t.Fatalf("timeout elapsed too long (%v > 480ms), suggesting unintended second dial wait", elapsedTimeout)
	}

	// (b) Unexpected EOF/reset on a fresh (non-reused) session -> triggers TOFU verification probe dial (dialCount == 2)
	_, errReset := ps.QueryPeer(lnReset.Addr().String(), Message{
		NetworkID: ps.GetNetworkID(),
		Type:      "GET_HEIGHT",
	}, 500*time.Millisecond)
	if errReset == nil {
		t.Fatal("expected error when fresh session closes mid-roundTrip")
	}
	if d := dialsReset.Load(); d != 2 {
		t.Fatalf("expected 2 dials (initial + TOFU probe) on unexpected EOF/reset, got %d", d)
	}
}

// TestP2P_OrphanTxsInFlightReservationAndFairEviction verifies MaxOrphanTxsPerPeer pre-verification reservation and fair top-consumer eviction:
//   - Pre-verify orphanTxInFlight reservation caps admitted orphans per peer IP at MaxOrphanTxsPerPeer (25)
//     across 60 concurrent submissions with zero leaked reservations,
//   - Invalid signatures also release in-flight slots cleanly, and
//   - Global MaxOrphanTxs (500) cap evicts top consumer's oldest entry before low-volume honest peers.
func TestP2P_OrphanTxsInFlightReservationAndFairEviction(t *testing.T) {
	t.Setenv("ARMX_DATA_DIR", t.TempDir())
	t.Setenv("ARMX_ALLOW_PRIVATE_PEERS", "1")

	bc, err := core.NewBlockchainWithStoragePath(1, filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatalf("init bc: %v", err)
	}
	defer bc.Close()

	sender := newTestAccount(t, "orp_sender")
	recip := newTestAccount(t, "orp_recip")
	bc.State.Lock.Lock()
	bc.State.Accounts[sender.AddressHex] = core.AccountState{Balance: 100_000_000 * core.CoinUnit, Nonce: 0}
	bc.State.Lock.Unlock()

	ps := NewPeerServer(getFreePort(t), bc, t.TempDir(), "ARMX-TESTNET-1")
	ps.AllowPrivatePeers = true
	defer ps.Stop()

	// 1. 60 concurrent validly-signed out-of-order nonce txs from peer-A -> capped at 25
	txs := make([]core.Transaction, 60)
	for i := 0; i < 60; i++ {
		tx := core.Transaction{
			Sender:    sender.Address,
			SenderPub: sender.PubHex,
			Recipient: recip.Address,
			Amount:    1 * core.CoinUnit,
			Fee:       25000, // Exceeds required relay fee (~11000 for ML-DSA-65 tx) so only nonce check triggers orphan path
			Nonce:     int64(i + 10),
			Type:      core.TxTypeStandard,
		}
		if err := tx.Sign(sender.PrivKey); err != nil {
			t.Fatalf("sign tx %d: %v", i, err)
		}
		txs[i] = tx
	}

	var wg sync.WaitGroup
	peerA := "198.51.100.30"
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func(tx core.Transaction) {
			defer wg.Done()
			ps.ProcessIncomingTx(tx, peerA)
		}(txs[i])
	}
	wg.Wait()

	ps.orphanLock.Lock()
	peerACount := 0
	for _, entry := range ps.orphanTxs {
		if entry.PeerIP == peerA {
			peerACount++
		}
	}
	inFlightA := ps.orphanTxInFlight[peerA]
	ps.orphanLock.Unlock()

	if peerACount != MaxOrphanTxsPerPeer {
		t.Fatalf("expected peer-A orphan count capped at %d, got %d", MaxOrphanTxsPerPeer, peerACount)
	}
	if inFlightA != 0 {
		t.Fatalf("expected 0 remaining in-flight reservations for peer-A, got %d", inFlightA)
	}

	// 2. Invalid signature releases in-flight reservation cleanly
	badTx := txs[0]
	badTx.Nonce = 999
	badTx.Signature = strings.Repeat("0", len(badTx.Signature))
	peerInvalid := "198.51.100.31"
	if ps.ProcessIncomingTx(badTx, peerInvalid) {
		t.Fatal("invalid orphan tx must not be accepted")
	}
	ps.orphanLock.Lock()
	inFlightBad := ps.orphanTxInFlight[peerInvalid]
	ps.orphanLock.Unlock()
	if inFlightBad != 0 {
		t.Fatalf("expected 0 in-flight reservations after invalid tx verify, got %d", inFlightBad)
	}

	// 3. Fair eviction at MaxOrphanTxs (500): flooder's oldest evicted while honest peer's older entry preserved
	flooder := "198.51.100.50"
	honest := "198.51.100.99"
	newPeer := "198.51.100.60"
	baseTime := time.Now().Add(-2 * time.Minute)

	ps.orphanLock.Lock()
	ps.orphanTxs = make(map[string]*orphanTxEntry, MaxOrphanTxs)
	ps.orphanTxs["honest-hash"] = &orphanTxEntry{
		Tx:      core.Transaction{Nonce: 1},
		PeerIP:  honest,
		AddedAt: baseTime.Add(-1 * time.Hour), // oldest overall, but low-volume peer (count = 1)
	}
	for i := 0; i < MaxOrphanTxs-1; i++ {
		h := fmt.Sprintf("flood-%03d", i)
		ps.orphanTxs[h] = &orphanTxEntry{
			Tx:      core.Transaction{Nonce: int64(i + 1)},
			PeerIP:  flooder,
			AddedAt: baseTime.Add(time.Duration(i) * time.Millisecond),
		}
	}
	ps.orphanLock.Unlock()

	freshTx := core.Transaction{
		Sender:    sender.Address,
		SenderPub: sender.PubHex,
		Recipient: recip.Address,
		Amount:    1 * core.CoinUnit,
		Fee:       25000,
		Nonce:     777,
		Type:      core.TxTypeStandard,
	}
	if err := freshTx.Sign(sender.PrivKey); err != nil {
		t.Fatalf("sign freshTx: %v", err)
	}
	if !ps.ProcessIncomingTx(freshTx, newPeer) {
		t.Fatal("expected valid fresh orphan tx to be admitted at global cap via fair eviction")
	}

	ps.orphanLock.Lock()
	_, honestKept := ps.orphanTxs["honest-hash"]
	_, flood0Evicted := ps.orphanTxs["flood-000"]
	totalOrphans := len(ps.orphanTxs)
	ps.orphanLock.Unlock()

	if totalOrphans != MaxOrphanTxs {
		t.Fatalf("expected global orphan count %d, got %d", MaxOrphanTxs, totalOrphans)
	}
	if !honestKept {
		t.Fatal("honest low-volume peer orphan tx was unfairly evicted instead of flooder")
	}
	if flood0Evicted {
		t.Fatal("expected top consumer flooder's oldest entry (flood-000) to be evicted")
	}
}

// TestP2P_PeriodicGCPrunesExpiredMapsConcurrently verifies periodic runtime map GC, size caps, and lazy claimTxRelay:
// pruneStaleRuntimeMaps reclaims expired/idle entries across all 9 runtime maps, preserves active entries
// (including active bans), enforces strict map size ceilings, eliminates O(N) scans in claimTxRelay,
// and executes race-free concurrently with live traffic and Stop().
func TestP2P_PeriodicGCPrunesExpiredMapsConcurrently(t *testing.T) {
	t.Setenv("ARMX_DATA_DIR", t.TempDir())
	t.Setenv("ARMX_ALLOW_PRIVATE_PEERS", "1")

	ps := NewPeerServer(getFreePort(t), nil, t.TempDir())
	ps.AllowPrivatePeers = true
	ps.RequireTLS = false
	go ps.StartServer()
	waitForServerRunning(t, ps, 3*time.Second)
	defer func() {
		if !ps.StopWithTimeout(5 * time.Second) {
			t.Fatal("PeerServer StopWithTimeout failed with GC worker active")
		}
	}()

	now := time.Now()

	// 1. Seed expired & active entries across all 9 runtime maps.
	ps.banLock.Lock()
	ps.bannedIPs["expired-ban-ip"] = now.Add(-time.Minute)
	ps.bannedIPs["active-ban-ip"] = now.Add(10 * time.Minute)
	ps.rateTrackers["expired-tracker-ip"] = &peerRateTracker{lastRefill: now.Add(-20 * time.Minute)}
	ps.rateTrackers["expired-score-ip"] = &peerRateTracker{lastRefill: now.Add(-2 * time.Hour)}
	ps.rateTrackers["active-tracker-ip"] = &peerRateTracker{lastRefill: now}
	ps.rateTrackers["active-score-ip"] = &peerRateTracker{lastRefill: now}
	ps.banScores["expired-score-ip"] = 15
	ps.banScores["missing-tracker-score-ip"] = 20
	ps.banScores["active-score-ip"] = 25
	ps.banScores["active-ban-ip"] = 100
	ps.skewStrikes["expired-skew-ip"] = []time.Time{now.Add(-20 * time.Minute)}
	ps.skewStrikes["mixed-skew-ip"] = []time.Time{now.Add(-20 * time.Minute), now.Add(-time.Minute)}
	ps.banLock.Unlock()

	ps.seenLock.Lock()
	if ps.syncCooldowns == nil {
		ps.syncCooldowns = make(map[string]time.Time)
	}
	ps.syncCooldowns["expired-sync-host"] = now.Add(-time.Minute)
	ps.syncCooldowns["active-sync-host"] = now
	ps.seenTxs["expired-tx"] = now.Add(-15 * time.Minute)
	ps.seenTxs["active-tx"] = now
	ps.seenBlks["expired-blk"] = now.Add(-45 * time.Minute)
	ps.seenBlks["active-blk"] = now
	ps.seenLock.Unlock()

	ps.reachabilityMu.Lock()
	if ps.announcedPeers == nil {
		ps.announcedPeers = make(map[string]time.Time)
	}
	ps.announcedPeers["expired-peer:19335"] = now.Add(-20 * time.Minute)
	ps.announcedPeers["active-peer:19335"] = now
	ps.reachabilityMu.Unlock()

	ps.relayLock.Lock()
	ps.relayedTxs["expired-relay-tx"] = map[string]time.Time{"peerA": now.Add(-15 * time.Minute)}
	ps.relayedTxs["partial-relay-tx"] = map[string]time.Time{
		"oldPeer":   now.Add(-15 * time.Minute),
		"freshPeer": now,
	}
	ps.relayLock.Unlock()

	// Verify claimTxRelay is lazy (re-claims expired entry without O(N) full scan over unrelated expired keys).
	ps.relayLock.Lock()
	ps.relayedTxs["lazy-target-tx"] = map[string]time.Time{"peerX": now.Add(-15 * time.Minute)}
	ps.relayLock.Unlock()
	if _, ok := ps.claimTxRelay("lazy-target-tx", "peerX"); !ok {
		t.Fatal("expected lazy expiry in claimTxRelay to allow re-claiming expired peerX attempt")
	}
	ps.relayLock.Lock()
	_, unrelatedStillPresentBeforeGC := ps.relayedTxs["expired-relay-tx"]
	ps.relayLock.Unlock()
	if !unrelatedStillPresentBeforeGC {
		t.Fatal("claimTxRelay should not perform an O(N) full-map scan on every call")
	}

	// Run periodic GC sweep now.
	ps.pruneStaleRuntimeMaps()

	ps.banLock.RLock()
	if _, ok := ps.bannedIPs["expired-ban-ip"]; ok {
		t.Fatal("expected expired-ban-ip to be pruned from bannedIPs")
	}
	if _, ok := ps.bannedIPs["active-ban-ip"]; !ok {
		t.Fatal("expected active-ban-ip to remain in bannedIPs")
	}
	if _, ok := ps.rateTrackers["expired-tracker-ip"]; ok {
		t.Fatal("expected expired-tracker-ip to be pruned from rateTrackers")
	}
	if _, ok := ps.rateTrackers["active-tracker-ip"]; !ok {
		t.Fatal("expected active-tracker-ip to remain in rateTrackers")
	}
	if _, ok := ps.banScores["expired-score-ip"]; ok {
		t.Fatal("expected idle expired-score-ip to be pruned from banScores")
	}
	if _, ok := ps.banScores["missing-tracker-score-ip"]; ok {
		t.Fatal("expected missing-tracker-score-ip to be pruned from banScores")
	}
	if ps.banScores["active-score-ip"] != 25 || ps.banScores["active-ban-ip"] != 100 {
		t.Fatal("expected active banScores entries to be preserved")
	}
	if _, ok := ps.skewStrikes["expired-skew-ip"]; ok {
		t.Fatal("expected empty expired-skew-ip to be deleted from skewStrikes")
	}
	if len(ps.skewStrikes["mixed-skew-ip"]) != 1 {
		t.Fatalf("expected 1 fresh strike in mixed-skew-ip, got %d", len(ps.skewStrikes["mixed-skew-ip"]))
	}
	ps.banLock.RUnlock()

	ps.seenLock.Lock()
	if _, ok := ps.syncCooldowns["expired-sync-host"]; ok {
		t.Fatal("expected expired-sync-host to be pruned from syncCooldowns")
	}
	if _, ok := ps.syncCooldowns["active-sync-host"]; !ok {
		t.Fatal("expected active-sync-host to remain in syncCooldowns")
	}
	if _, ok := ps.seenTxs["expired-tx"]; ok {
		t.Fatal("expected expired-tx to be pruned from seenTxs")
	}
	if _, ok := ps.seenTxs["active-tx"]; !ok {
		t.Fatal("expected active-tx to remain in seenTxs")
	}
	if _, ok := ps.seenBlks["expired-blk"]; ok {
		t.Fatal("expected expired-blk to be pruned from seenBlks")
	}
	if _, ok := ps.seenBlks["active-blk"]; !ok {
		t.Fatal("expected active-blk to remain in seenBlks")
	}
	ps.seenLock.Unlock()

	ps.reachabilityMu.Lock()
	if _, ok := ps.announcedPeers["expired-peer:19335"]; ok {
		t.Fatal("expected expired-peer:19335 to be pruned from announcedPeers")
	}
	if _, ok := ps.announcedPeers["active-peer:19335"]; !ok {
		t.Fatal("expected active-peer:19335 to remain in announcedPeers")
	}
	ps.reachabilityMu.Unlock()

	ps.relayLock.Lock()
	if _, ok := ps.relayedTxs["expired-relay-tx"]; ok {
		t.Fatal("expected expired-relay-tx to be pruned by periodic GC")
	}
	if claims := ps.relayedTxs["partial-relay-tx"]; len(claims) != 1 || claims["freshPeer"].IsZero() {
		t.Fatalf("expected only freshPeer left in partial-relay-tx, got %+v", claims)
	}
	ps.relayLock.Unlock()

	// 2. Enforce hard size caps on all bounded maps.
	capNow := time.Now()
	ps.banLock.Lock()
	for i := 0; i < maxRateTrackers+25; i++ {
		ps.rateTrackers[fmt.Sprintf("cap-tr-%d", i)] = &peerRateTracker{
			lastRefill: capNow.Add(-time.Duration(i) * time.Millisecond),
		}
	}
	for i := 0; i < maxBanScores+25; i++ {
		ps.banScores[fmt.Sprintf("cap-bs-%d", i)] = 1 + (i % 20)
	}
	ps.banLock.Unlock()

	ps.seenLock.Lock()
	for i := 0; i < maxSeenTxs+20; i++ {
		ps.seenTxs[fmt.Sprintf("cap-tx-%d", i)] = capNow.Add(-time.Duration(i) * time.Millisecond)
	}
	for i := 0; i < maxSeenBlks+20; i++ {
		ps.seenBlks[fmt.Sprintf("cap-blk-%d", i)] = capNow.Add(-time.Duration(i) * time.Millisecond)
	}
	ps.seenLock.Unlock()

	ps.relayLock.Lock()
	for i := 0; i < maxRelayedTxs+20; i++ {
		ps.relayedTxs[fmt.Sprintf("cap-relay-%d", i)] = map[string]time.Time{
			"p1": capNow.Add(-time.Duration(i) * time.Millisecond),
		}
	}
	ps.relayLock.Unlock()

	// Exercise write paths + GC cap enforcement.
	_ = ps.AllowMessage("198.51.100.200", "GET_HEIGHT")
	ps.AddBanScore("198.51.100.201", 10)
	ps.markTxSeen("cap-trigger-tx")
	ps.markBlockSeen("cap-trigger-blk")
	_, _ = ps.claimTxRelay("cap-trigger-relay", "p1")
	ps.pruneStaleRuntimeMaps()

	ps.banLock.RLock()
	if len(ps.rateTrackers) > maxRateTrackers {
		t.Fatalf("rateTrackers exceeded cap %d: got %d", maxRateTrackers, len(ps.rateTrackers))
	}
	if len(ps.banScores) > maxBanScores {
		t.Fatalf("banScores exceeded cap %d: got %d", maxBanScores, len(ps.banScores))
	}
	if _, ok := ps.bannedIPs["active-ban-ip"]; !ok {
		t.Fatal("active-ban-ip must never be evicted from bannedIPs under cap pressure")
	}
	ps.banLock.RUnlock()

	ps.seenLock.Lock()
	if len(ps.seenTxs) > maxSeenTxs {
		t.Fatalf("seenTxs exceeded cap %d: got %d", maxSeenTxs, len(ps.seenTxs))
	}
	if len(ps.seenBlks) > maxSeenBlks {
		t.Fatalf("seenBlks exceeded cap %d: got %d", maxSeenBlks, len(ps.seenBlks))
	}
	ps.seenLock.Unlock()

	ps.relayLock.Lock()
	if len(ps.relayedTxs) > maxRelayedTxs {
		t.Fatalf("relayedTxs exceeded cap %d: got %d", maxRelayedTxs, len(ps.relayedTxs))
	}
	ps.relayLock.Unlock()

	// 3. Concurrent -race stress combining GC sweeps and live map mutators.
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			ip := fmt.Sprintf("198.51.100.%d", 10+(id%8))
			for it := 0; it < 40; it++ {
				switch it % 5 {
				case 0:
					ps.pruneStaleRuntimeMaps()
				case 1:
					_ = ps.AllowMessage(ip, "GET_HEIGHT")
					ps.AddBanScore(ip, 1)
				case 2:
					ps.recordClockSkewViolation(ip)
					_ = ps.canTriggerSync(ip)
				case 3:
					txH := fmt.Sprintf("conc-tx-%d-%d", id, it)
					blkH := fmt.Sprintf("conc-blk-%d-%d", id, it)
					ps.markTxSeen(txH)
					ps.markBlockSeen(blkH)
				case 4:
					_, _ = ps.claimTxRelay(fmt.Sprintf("conc-relay-%d", id), "peer-concurrent")
					_ = ps.shouldSelfAnnounceTo(fmt.Sprintf("%s:19335", ip))
				}
			}
		}(g)
	}
	wg.Wait()
}

func startTestMockHeightPeer(t *testing.T, tlsConf *tls.Config) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen mock height peer: %v", err)
	}
	netID := core.NetworkID()
	var wg sync.WaitGroup
	stopCh := make(chan struct{})
	t.Cleanup(func() {
		close(stopCh)
		_ = ln.Close()
		wg.Wait()
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			rc, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func(raw net.Conn) {
				defer wg.Done()
				sc, _ := UpgradeToEncryptedTransportWithConfig(raw, tlsConf)
				if sc == nil {
					_ = raw.Close()
					return
				}
				defer sc.Close()
				br := bufio.NewReaderSize(sc, 64*1024)
				for {
					select {
					case <-stopCh:
						return
					default:
					}
					_ = sc.SetReadDeadline(time.Now().Add(10 * time.Second))
					line, rErr := readBoundedLine(br, MaxIncomingPacketSize)
					if rErr != nil {
						return
					}
					var msg rawInboundMessage
					if json.Unmarshal([]byte(line), &msg) != nil {
						return
					}
					if msg.Type == "GET_HEIGHT" {
						resp, _ := json.Marshal(Message{
							NetworkID: netID,
							Type:      "HEIGHT_RESPONSE",
							Payload: HeightResponse{
								Height:     1,
								LatestHash: core.GenesisHash,
								Chainwork:  "268435456",
							},
						})
						_, _ = sc.Write(append(resp, '\n'))
					}
				}
			}(rc)
		}
	}()
	return ln.Addr().String()
}

// TestP2P_OutboundSlotLRUEvictionAndNoFalsePeerEviction verifies idle outbound LRU eviction,
// heartbeat-agnostic lastAppTraffic ordering, seed preservation, and exemption of outbound-cap errors from peer eviction/blacklisting.
func TestP2P_OutboundSlotLRUEvictionAndNoFalsePeerEviction(t *testing.T) {
	t.Setenv("ARMX_DATA_DIR", t.TempDir())
	t.Setenv("ARMX_ALLOW_PRIVATE_PEERS", "1")

	tlsConf, err := EnsureNodeTLSConfig(t.TempDir())
	if err != nil {
		t.Fatalf("EnsureNodeTLSConfig: %v", err)
	}
	seedAddr := startTestMockHeightPeer(t, tlsConf)
	nonSeedActive := startTestMockHeightPeer(t, tlsConf)
	nonSeedIdle := startTestMockHeightPeer(t, tlsConf)
	nonSeedNew := startTestMockHeightPeer(t, tlsConf)

	ps := NewPeerServer(getFreePort(t), core.NewBlockchain(1), t.TempDir())
	defer ps.Blockchain.Close()
	ps.MaxPeers = 10 // maxIn=8, maxOut=2
	ps.AllowPrivatePeers = true
	ps.RequireTLS = true
	ps.P2PTOFU = false
	go ps.StartServer()
	waitForServerRunning(t, ps, 3*time.Second)
	defer ps.Stop()

	netID := ps.GetNetworkID()

	// 1. Fill both outbound slots with two non-seed sessions, then refresh only heartbeat lastUsed
	// on nonSeedIdle so it appears newer on raw socket I/O while having older lastAppTraffic.
	if _, err := ps.QueryPeer(nonSeedIdle, Message{NetworkID: netID, Type: "GET_HEIGHT"}, 3*time.Second); err != nil {
		t.Fatalf("dial nonSeedIdle: %v", err)
	}
	time.Sleep(15 * time.Millisecond)
	if _, err := ps.QueryPeer(nonSeedActive, Message{NetworkID: netID, Type: "GET_HEIGHT"}, 3*time.Second); err != nil {
		t.Fatalf("dial nonSeedActive: %v", err)
	}
	if sessIdle := ps.getExistingOutboundSession(nonSeedIdle); sessIdle != nil {
		sessIdle.touch() // Simulates DHT_PING heartbeat keeping socket lastUsed fresh
	} else {
		t.Fatal("expected pooled session for nonSeedIdle")
	}

	// Dialing a 3rd peer must evict nonSeedIdle (older lastAppTraffic) and preserve nonSeedActive.
	if _, err := ps.QueryPeer(nonSeedNew, Message{NetworkID: netID, Type: "GET_HEIGHT"}, 3*time.Second); err != nil {
		t.Fatalf("dial nonSeedNew with full outbound slots failed: %v", err)
	}
	if ps.getExistingOutboundSession(nonSeedIdle) != nil {
		t.Fatal("expected heartbeat-only nonSeedIdle to be evicted before nonSeedActive")
	}
	if ps.getExistingOutboundSession(nonSeedActive) == nil {
		t.Fatal("expected nonSeedActive to remain pooled after LRU eviction")
	}

	// 2. Verify seed sessions are preserved ahead of non-seed sessions even when the seed has older lastAppTraffic.
	ps.SetSeedPeers([]string{seedAddr})
	ps.dropOutboundSession(nonSeedNew, ps.getExistingOutboundSession(nonSeedNew))
	if _, err := ps.QueryPeer(seedAddr, Message{NetworkID: netID, Type: "GET_HEIGHT"}, 3*time.Second); err != nil {
		t.Fatalf("dial seedAddr: %v", err)
	}
	if sessSeed := ps.getExistingOutboundSession(seedAddr); sessSeed != nil {
		sessSeed.lastAppTraffic.Store(1) // Force oldest app traffic on seed
	}
	if sessActive := ps.getExistingOutboundSession(nonSeedActive); sessActive != nil {
		sessActive.touchApp()
	}
	if _, err := ps.QueryPeer(nonSeedNew, Message{NetworkID: netID, Type: "GET_HEIGHT"}, 3*time.Second); err != nil {
		t.Fatalf("dial nonSeedNew against seed + non-seed failed: %v", err)
	}
	if ps.getExistingOutboundSession(seedAddr) == nil {
		t.Fatal("expected configured seed session to be preserved ahead of non-seed session during LRU eviction")
	}
	if gotOut := atomic.LoadInt32(&ps.activeOutbound); gotOut > 2 {
		t.Fatalf("outbound slot limit exceeded: got %d > 2", gotOut)
	}

	// 3. Hold all outbound slots with raw non-pooled DialPeer sockets so eviction cannot free a slot;
	// verify SyncBestPeer and verifyAndAddInboundPeer do not evict or blacklist healthy peers.
	ps.closeOutboundSessions(nil)
	raw1, rel1, err := ps.DialPeer(nonSeedActive, 3*time.Second)
	if err != nil {
		t.Fatalf("raw DialPeer #1: %v", err)
	}
	defer func() { _ = raw1.Close(); rel1() }()
	raw2, rel2, err := ps.DialPeer(nonSeedNew, 3*time.Second)
	if err != nil {
		t.Fatalf("raw DialPeer #2: %v", err)
	}
	defer func() { _ = raw2.Close(); rel2() }()

	if _, _, err := ps.DialPeer(seedAddr, 3*time.Second); !errors.Is(err, errOutboundLimitExceeded) {
		t.Fatalf("expected errOutboundLimitExceeded when all outbound slots un-evictable, got: %v", err)
	}

	bystander := nonSeedIdle
	_ = ps.AddPeer(bystander)
	ps.verifyAndAddInboundPeer(bystander)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ps.reachabilityMu.Lock()
		probing := len(ps.probingPeers)
		ps.reachabilityMu.Unlock()
		if probing == 0 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	for i := 0; i < 4; i++ {
		ps.SyncBestPeer()
	}

	if !ps.HasPeer(bystander) {
		t.Fatal("healthy bystander peer was evicted from address book solely due to local outbound slot saturation")
	}
	if ps.isPeerUnreachable(bystander) {
		t.Fatal("healthy bystander peer was blacklisted in unreachablePeers due to local outbound slot saturation")
	}
}

// TestP2P_QueryPeerRetriesStaleReusedSessionOnIdempotentRPC verifies that QueryPeer transparently retries
// once on a fresh TOFU-verified session when a reused persistent socket was closed by the remote peer.
func TestP2P_QueryPeerRetriesStaleReusedSessionOnIdempotentRPC(t *testing.T) {
	t.Setenv("ARMX_DATA_DIR", t.TempDir())
	t.Setenv("ARMX_ALLOW_PRIVATE_PEERS", "1")

	tlsConf, err := EnsureNodeTLSConfig(t.TempDir())
	if err != nil {
		t.Fatalf("EnsureNodeTLSConfig: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	netID := core.NetworkID()
	var acceptCount atomic.Int32
	conn1Closed := make(chan struct{})
	var closeOnce sync.Once
	var wg sync.WaitGroup
	stopCh := make(chan struct{})
	defer func() {
		close(stopCh)
		_ = ln.Close()
		wg.Wait()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			rc, err := ln.Accept()
			if err != nil {
				return
			}
			connNum := acceptCount.Add(1)
			wg.Add(1)
			go func(raw net.Conn, num int32) {
				defer wg.Done()
				sc, _ := UpgradeToEncryptedTransportWithConfig(raw, tlsConf)
				if sc == nil {
					_ = raw.Close()
					return
				}
				defer sc.Close()
				br := bufio.NewReaderSize(sc, 64*1024)
				for {
					select {
					case <-stopCh:
						return
					default:
					}
					_ = sc.SetReadDeadline(time.Now().Add(5 * time.Second))
					line, rErr := readBoundedLine(br, MaxIncomingPacketSize)
					if rErr != nil {
						return
					}
					var msg rawInboundMessage
					if json.Unmarshal([]byte(line), &msg) != nil {
						return
					}
					if msg.Type == "GET_HEIGHT" {
						resp, _ := json.Marshal(Message{
							NetworkID: netID,
							Type:      "HEIGHT_RESPONSE",
							Payload: HeightResponse{
								Height:     int(num),
								LatestHash: core.GenesisHash,
								Chainwork:  "268435456",
							},
						})
						_, _ = sc.Write(append(resp, '\n'))
						if num == 1 {
							_ = sc.Close()
							closeOnce.Do(func() { close(conn1Closed) })
							return
						}
					}
				}
			}(rc, connNum)
		}
	}()

	ps := NewPeerServer(getFreePort(t), nil, t.TempDir())
	ps.AllowPrivatePeers = true
	ps.RequireTLS = true
	ps.P2PTOFU = false
	go ps.StartServer()
	waitForServerRunning(t, ps, 3*time.Second)
	defer ps.Stop()

	srvAddr := ln.Addr().String()
	resp1, err := ps.QueryPeer(srvAddr, Message{NetworkID: netID, Type: "GET_HEIGHT"}, 3*time.Second)
	if err != nil || resp1 == nil || resp1.Type != "HEIGHT_RESPONSE" {
		t.Fatalf("first QueryPeer failed: resp=%+v err=%v", resp1, err)
	}
	<-conn1Closed
	time.Sleep(50 * time.Millisecond)

	// Reuses pooled session #1 which remote already closed; must transparently reconnect once and succeed.
	resp2, err := ps.QueryPeer(srvAddr, Message{NetworkID: netID, Type: "GET_HEIGHT"}, 3*time.Second)
	if err != nil || resp2 == nil || resp2.Type != "HEIGHT_RESPONSE" {
		t.Fatalf("expected second QueryPeer on stale reused socket to transparently retry and succeed, got resp=%+v err=%v", resp2, err)
	}
	if got := acceptCount.Load(); got != 2 {
		t.Fatalf("expected exactly 2 connections (initial + single transparent retry), got %d", got)
	}
}

// TestP2P_PeersListAndDHTNodesBoundedPerMessage verifies that inbound PEERS_LIST and DHT_NODES messages
// bound processed items, cap newly persisted AddPeer writes, and limit self-announce probes to 1 per PEERS_LIST.
func TestP2P_PeersListAndDHTNodesBoundedPerMessage(t *testing.T) {
	t.Setenv("ARMX_DATA_DIR", t.TempDir())
	t.Setenv("ARMX_ALLOW_PRIVATE_PEERS", "1")

	ps := NewPeerServer(getFreePort(t), core.NewBlockchain(1), t.TempDir())
	defer ps.Blockchain.Close()
	ps.AllowPrivatePeers = true
	ps.DisableSubnetLimit = true
	ps.RequireTLS = false
	ps.P2PTOFU = false
	go ps.StartServer()
	waitForServerRunning(t, ps, 3*time.Second)
	defer ps.Stop()

	targetAddr := "127.0.0.1:" + ps.Port
	conn, _, err := DialEncryptedWithFallback(targetAddr, 3*time.Second, true)
	if err != nil {
		t.Fatalf("dial PeerServer: %v", err)
	}
	defer conn.Close()
	reader := bufio.NewReaderSize(conn, 64*1024)
	netID := ps.GetNetworkID()

	// 1. Send 10 self-announce ":port" entries (closed local ports) + 200 distinct peers in one PEERS_LIST frame.
	peersPayload := make([]string, 0, 210)
	for i := 0; i < 10; i++ {
		peersPayload = append(peersPayload, ":"+getFreePort(t))
	}
	for i := 0; i < 200; i++ {
		peersPayload = append(peersPayload, fmt.Sprintf("198.51.%d.%d:19335", (i/200)+1, (i%200)+1))
	}
	listMsg, _ := json.Marshal(Message{NetworkID: netID, Type: "PEERS_LIST", Payload: peersPayload})
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(append(listMsg, '\n')); err != nil {
		t.Fatalf("write PEERS_LIST: %v", err)
	}

	// Synchronize frame completion with GET_HEIGHT on same stream.
	heightMsg, _ := json.Marshal(Message{NetworkID: netID, Type: "GET_HEIGHT"})
	if _, err := conn.Write(append(heightMsg, '\n')); err != nil {
		t.Fatalf("write sync GET_HEIGHT #1: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("read HEIGHT_RESPONSE #1: %v", err)
	}

	afterList := len(ps.GetPeers())
	if afterList > maxNewPeersPerMessage {
		t.Fatalf("expected PEERS_LIST to add <= %d peers, got %d", maxNewPeersPerMessage, afterList)
	}

	// Wait for the single allowed async self-announce probe to settle and verify at most 1 ran.
	probeDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(probeDeadline) {
		ps.reachabilityMu.Lock()
		unreach := len(ps.unreachablePeers)
		probing := len(ps.probingPeers)
		ps.reachabilityMu.Unlock()
		if unreach >= 1 && probing == 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	ps.reachabilityMu.Lock()
	unreachCount := len(ps.unreachablePeers)
	probingCount := len(ps.probingPeers)
	ps.reachabilityMu.Unlock()
	if unreachCount != 1 || probingCount != 0 {
		t.Fatalf("expected exactly 1 self-announce probe from 10 entries, got unreachable=%d probing=%d", unreachCount, probingCount)
	}

	// 2. Send 100 DHT nodes in one DHT_NODES frame and verify bounded new peer additions.
	dhtNodes := make([]DHTNodeInfo, 0, 100)
	for i := 0; i < 100; i++ {
		dhtNodes = append(dhtNodes, DHTNodeInfo{
			IDHex:   fmt.Sprintf("%064x", i+1),
			Address: fmt.Sprintf("203.0.%d.%d:19335", (i/200)+10, (i%200)+1),
		})
	}
	dhtMsg, _ := json.Marshal(Message{NetworkID: netID, Type: "DHT_NODES", Payload: dhtNodes})
	if _, err := conn.Write(append(dhtMsg, '\n')); err != nil {
		t.Fatalf("write DHT_NODES: %v", err)
	}
	if _, err := conn.Write(append(heightMsg, '\n')); err != nil {
		t.Fatalf("write sync GET_HEIGHT #2: %v", err)
	}
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("read HEIGHT_RESPONSE #2: %v", err)
	}

	afterDHT := len(ps.GetPeers())
	addedFromDHT := afterDHT - afterList
	if addedFromDHT > maxNewPeersPerMessage {
		t.Fatalf("expected DHT_NODES to add <= %d peers, got %d", maxNewPeersPerMessage, addedFromDHT)
	}
}
