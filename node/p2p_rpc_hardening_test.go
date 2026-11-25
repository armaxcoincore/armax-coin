package node

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/armaxcoincore/armax-coin/core"
)

func newP2PRPCHardeningBlockchain(t *testing.T) *core.Blockchain {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "p2p_rpc_hardening.db")
	bc, err := core.NewBlockchainWithStoragePath(1, dbPath)
	if err != nil {
		t.Fatalf("NewBlockchainWithStoragePath failed: %v", err)
	}
	t.Cleanup(func() { _ = bc.Close() })
	return bc
}

// TestP2P_GetMempoolPollingRateAndNoDuplicateSyncMempool verifies:
//  1. inboundConnSession.connMempoolTokens refills at 1/8s (7.5 req/min) matching per-IP limits
//     while still silently throttling excess burst frames.
//  2. SyncBestPeer does not trigger a duplicate GET_MEMPOOL broadcast.
func TestP2P_GetMempoolPollingRateAndNoDuplicateSyncMempool(t *testing.T) {
	bc := newP2PRPCHardeningBlockchain(t)
	ps := NewPeerServer(getFreePort(t), bc, t.TempDir())
	ps.DisableSubnetLimit = true
	ps.AllowPrivatePeers = true
	ps.RequireTLS = false
	ps.P2PTOFU = false

	// Part A: 16s elapsed with 0 starting tokens refills 2.0 tokens at 1/8s (vs 0.8 under old 1/20s).
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})
	quitCh := make(chan struct{})
	defer close(quitCh)

	sess := &inboundConnSession{
		conn:                  serverConn,
		remoteIP:              "198.51.100.10",
		remoteAddr:            "198.51.100.10:19335",
		p2pTOFU:               false,
		quitCh:                quitCh,
		readTimeout:           2 * time.Second,
		deadline:              time.Now().Add(10 * time.Second),
		connMempoolTokens:     0.0,
		connMempoolLastRefill: time.Now().Add(-16 * time.Second),
	}

	rawReq, err := json.Marshal(Message{
		NetworkID: ps.GetNetworkID(),
		Type:      "GET_MEMPOOL",
	})
	if err != nil {
		t.Fatalf("marshal GET_MEMPOOL: %v", err)
	}

	reader := bufio.NewReader(clientConn)
	for i := 1; i <= 2; i++ {
		doneCh := make(chan bool, 1)
		go func() {
			doneCh <- ps.processInboundFrame(sess, rawReq, nil)
		}()
		_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		line, rerr := reader.ReadString('\n')
		if rerr != nil {
			t.Fatalf("expected MEMPOOL_RESPONSE #%d under 1/8s refill, got read err: %v", i, rerr)
		}
		if ok := <-doneCh; !ok {
			t.Fatalf("expected processInboundFrame #%d to keep connection open", i)
		}
		var resp Message
		if uerr := json.Unmarshal([]byte(line), &resp); uerr != nil || resp.Type != "MEMPOOL_RESPONSE" {
			t.Fatalf("unexpected response #%d: line=%q err=%v", i, line, uerr)
		}
	}

	// Exhausted tokens immediately after the 2 requests must silently throttle a 3rd immediate burst.
	if ok := ps.processInboundFrame(sess, rawReq, nil); !ok {
		t.Fatalf("expected throttled GET_MEMPOOL to keep connection alive without drop")
	}
	_ = clientConn.SetReadDeadline(time.Now().Add(80 * time.Millisecond))
	if extra, rerr := reader.ReadString('\n'); rerr == nil {
		t.Fatalf("expected silent throttle on exhausted bucket, got unexpected frame: %q", extra)
	}

	// Part B: SyncBestPeer must query GET_HEIGHT without sending duplicate GET_MEMPOOL.
	psSync := NewPeerServer(getFreePort(t), bc, t.TempDir())
	psSync.DisableSubnetLimit = true
	psSync.AllowPrivatePeers = true
	psSync.RequireTLS = false
	psSync.P2PTOFU = false
	t.Cleanup(psSync.Stop)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	var msgMu sync.Mutex
	var receivedTypes []string
	var acceptWG sync.WaitGroup
	stopMock := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-stopMock:
		default:
			close(stopMock)
		}
		_ = ln.Close()
		acceptWG.Wait()
	})

	acceptWG.Add(1)
	go func() {
		defer acceptWG.Done()
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			acceptWG.Add(1)
			go func(conn net.Conn) {
				defer acceptWG.Done()
				defer conn.Close()
				go func() {
					<-stopMock
					_ = conn.Close()
				}()
				scanner := bufio.NewScanner(conn)
				for scanner.Scan() {
					var m Message
					if jerr := json.Unmarshal(scanner.Bytes(), &m); jerr != nil {
						return
					}
					msgMu.Lock()
					receivedTypes = append(receivedTypes, m.Type)
					msgMu.Unlock()
					if m.Type == "GET_HEIGHT" {
						reply, _ := json.Marshal(Message{
							NetworkID: psSync.GetNetworkID(),
							Type:      "HEIGHT_RESPONSE",
							Payload: HeightResponse{
								Height:     0,
								Chainwork:  "1",
								LatestHash: core.GenesisHash,
							},
						})
						_, _ = conn.Write(append(reply, '\n'))
					}
				}
			}(c)
		}
	}()

	mockAddr := ln.Addr().String()
	if err := psSync.AddPeer(mockAddr); err != nil {
		t.Fatalf("AddPeer: %v", err)
	}
	if err := psSync.RecordPeerSuccess(mockAddr, 0, psSync.GetNetworkID()); err != nil {
		t.Fatalf("RecordPeerSuccess: %v", err)
	}

	psSync.SyncBestPeer()
	time.Sleep(100 * time.Millisecond)

	close(stopMock)
	_ = ln.Close()
	acceptWG.Wait()

	msgMu.Lock()
	defer msgMu.Unlock()
	var getHeightCount, getMempoolCount int
	for _, mt := range receivedTypes {
		switch mt {
		case "GET_HEIGHT":
			getHeightCount++
		case "GET_MEMPOOL":
			getMempoolCount++
		}
	}
	if getHeightCount == 0 {
		t.Fatalf("expected SyncBestPeer to query GET_HEIGHT at least once; saw %v", receivedTypes)
	}
	if getMempoolCount != 0 {
		t.Fatalf("expected SyncBestPeer not to trigger duplicate GET_MEMPOOL, saw %d in %v", getMempoolCount, receivedTypes)
	}
}

// TestP2P_QueryPeerNonTimeoutTOFUProbeDoesNotPoolIdleSocket verifies:
// 1) A timed-out DHT_PING heartbeat on a pooled session does not dial a second retry socket.
// 2) Failed/non-timeout TOFU verification closes any probe socket cleanly and leaks zero outbound slots.
func TestP2P_QueryPeerNonTimeoutTOFUProbeDoesNotPoolIdleSocket(t *testing.T) {
	bc := newP2PRPCHardeningBlockchain(t)

	srvPS := NewPeerServer(getFreePort(t), bc, t.TempDir())
	clientPS := NewPeerServer(getFreePort(t), bc, t.TempDir())
	clientPS.DisableSubnetLimit = true
	clientPS.AllowPrivatePeers = true
	clientPS.RequireTLS = true
	clientPS.P2PTOFU = true
	clientPS.MaxOutboundPeers = 3
	t.Cleanup(clientPS.Stop)

	netID := clientPS.GetNetworkID()

	// Part A: Pooled session stalls on DHT_PING -> no second dial attempt.
	rawLn1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen 1: %v", err)
	}
	tlsLn1 := tls.NewListener(rawLn1, srvPS.tlsConfig)

	var accepted1 atomic.Int32
	stop1 := make(chan struct{})
	var stopOnce1 sync.Once
	var wg1 sync.WaitGroup
	close1 := func() {
		stopOnce1.Do(func() { close(stop1) })
		_ = tlsLn1.Close()
	}
	t.Cleanup(func() {
		close1()
		wg1.Wait()
	})

	wg1.Add(1)
	go func() {
		defer wg1.Done()
		for {
			c, aerr := tlsLn1.Accept()
			if aerr != nil {
				return
			}
			accepted1.Add(1)
			wg1.Add(1)
			go func(conn net.Conn) {
				defer wg1.Done()
				defer conn.Close()
				go func() {
					<-stop1
					_ = conn.Close()
				}()
				scanner := bufio.NewScanner(conn)
				for scanner.Scan() {
					var m Message
					if jerr := json.Unmarshal(scanner.Bytes(), &m); jerr != nil {
						return
					}
					if m.Type == "GET_HEIGHT" {
						reply, _ := json.Marshal(Message{
							NetworkID: netID,
							Type:      "HEIGHT_RESPONSE",
							Payload: HeightResponse{
								Height:     0,
								Chainwork:  "1",
								LatestHash: core.GenesisHash,
							},
						})
						_, _ = conn.Write(append(reply, '\n'))
					} else if m.Type == "DHT_PING" {
						// Silently drop response to simulate i/o timeout without closing.
						<-stop1
						return
					}
				}
			}(c)
		}
	}()

	addr1 := tlsLn1.Addr().String()
	resp, qerr := clientPS.QueryPeer(addr1, Message{NetworkID: netID, Type: "GET_HEIGHT"}, 2*time.Second)
	if qerr != nil || resp == nil || resp.Type != "HEIGHT_RESPONSE" {
		t.Fatalf("initial GET_HEIGHT QueryPeer failed: resp=%v err=%v", resp, qerr)
	}
	if clientPS.getExistingOutboundSession(addr1) == nil {
		t.Fatalf("expected pooled outbound session for %s after GET_HEIGHT", addr1)
	}

	if _, pingErr := clientPS.QueryPeer(addr1, Message{NetworkID: netID, Type: "DHT_PING"}, 120*time.Millisecond); pingErr == nil {
		t.Fatalf("expected stalled DHT_PING QueryPeer to return timeout error")
	}
	if got := accepted1.Load(); got != 1 {
		t.Fatalf("expected DHT_PING timeout not to open a second retry connection, accepted=%d", got)
	}
	if clientPS.getExistingOutboundSession(addr1) != nil {
		t.Fatalf("expected timed-out pooled session to be evicted after DHT_PING failure")
	}
	close1()
	wg1.Wait()

	// Part B: Non-timeout failure on reused socket does not pool an un-handshaked socket or leak slots.
	rawLn2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen 2: %v", err)
	}
	tlsLn2 := tls.NewListener(rawLn2, srvPS.tlsConfig)

	var accepted2 atomic.Int32
	probeEOFCh := make(chan error, 4)
	stop2 := make(chan struct{})
	var stopOnce2 sync.Once
	var wg2 sync.WaitGroup
	close2 := func() {
		stopOnce2.Do(func() { close(stop2) })
		_ = tlsLn2.Close()
	}
	t.Cleanup(func() {
		close2()
		wg2.Wait()
	})

	wg2.Add(1)
	go func() {
		defer wg2.Done()
		for {
			c, aerr := tlsLn2.Accept()
			if aerr != nil {
				return
			}
			n := accepted2.Add(1)
			wg2.Add(1)
			go func(conn net.Conn, seq int32) {
				defer wg2.Done()
				defer conn.Close()
				go func() {
					<-stop2
					_ = conn.Close()
				}()
				if seq == 1 {
					scanner := bufio.NewScanner(conn)
					if scanner.Scan() {
						var m Message
						if json.Unmarshal(scanner.Bytes(), &m) == nil && m.Type == "GET_HEIGHT" {
							reply, _ := json.Marshal(Message{
								NetworkID: netID,
								Type:      "HEIGHT_RESPONSE",
								Payload: HeightResponse{
									Height:     0,
									Chainwork:  "1",
									LatestHash: core.GenesisHash,
								},
							})
							_, _ = conn.Write(append(reply, '\n'))
						}
					}
					// Wait for next request then close immediately with EOF (non-timeout failure).
					_ = scanner.Scan()
					return
				}
				// Subsequent retry/probe conn completes TLS handshake; fail GET_HEIGHT/first frame and observe close.
				if tc, ok := conn.(*tls.Conn); ok {
					_ = tc.Handshake()
				}
				_ = conn.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
				buf := make([]byte, 256)
				_, rerr := conn.Read(buf)
				if rerr == nil {
					_ = conn.Close()
					probeEOFCh <- errors.New("retry closed cleanly after read")
					return
				}
				probeEOFCh <- rerr
			}(c, n)
		}
	}()

	addr2 := tlsLn2.Addr().String()
	if _, qerr := clientPS.QueryPeer(addr2, Message{NetworkID: netID, Type: "GET_HEIGHT"}, 2*time.Second); qerr != nil {
		t.Fatalf("initial GET_HEIGHT on peer 2 failed: %v", qerr)
	}
	if _, qerr := clientPS.QueryPeer(addr2, Message{NetworkID: netID, Type: "GET_MEMPOOL"}, 250*time.Millisecond); qerr == nil {
		t.Fatalf("expected GET_MEMPOOL against closing peer 2 to fail")
	}
	select {
	case rerr := <-probeEOFCh:
		var netErr net.Error
		if errors.As(rerr, &netErr) && netErr.Timeout() {
			t.Fatalf("expected retry/TOFU verification socket not to linger idle until timeout: %v", rerr)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("timed out waiting for peer 2 second connection observation")
	}
	if clientPS.getExistingOutboundSession(addr2) != nil {
		t.Fatalf("expected failed peer 2 session not to remain pooled")
	}

	// Verify all MaxOutboundPeers (3) slots are cleanly un-leaked and reservable.
	var releases []func()
	for i := 0; i < clientPS.MaxOutboundPeers; i++ {
		rel, ok := clientPS.tryReserveOutboundSlot()
		if !ok {
			t.Fatalf("outbound slot leak detected: only reserved %d of %d slots", i, clientPS.MaxOutboundPeers)
		}
		releases = append(releases, rel)
	}
	for _, rel := range releases {
		rel()
	}

	close2()
	wg2.Wait()
}

// TestP2P_BackgroundMempoolAndGossipSkipUnreachableAndStalePeers verifies:
// 1) SyncMempool and RebroadcastMempool skip unreachable and stale peers without dialing them.
// 2) RequestChainSync does not increment peer failure count on errOutboundLimitExceeded.
func TestP2P_BackgroundMempoolAndGossipSkipUnreachableAndStalePeers(t *testing.T) {
	bc := newP2PRPCHardeningBlockchain(t)
	ps := NewPeerServer(getFreePort(t), bc, t.TempDir())
	ps.DisableSubnetLimit = true
	ps.AllowPrivatePeers = true
	ps.RequireTLS = false
	ps.P2PTOFU = false
	ps.MaxOutboundPeers = 2
	t.Cleanup(ps.Stop)

	lnUnreach, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen unreach: %v", err)
	}
	defer lnUnreach.Close()
	lnStale, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen stale: %v", err)
	}
	defer lnStale.Close()

	var dialUnreach, dialStale atomic.Int32
	serveCount := func(ln net.Listener, cnt *atomic.Int32) {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			cnt.Add(1)
			_ = c.Close()
		}
	}
	go serveCount(lnUnreach, &dialUnreach)
	go serveCount(lnStale, &dialStale)

	addrUnreach := lnUnreach.Addr().String()
	addrStale := lnStale.Addr().String()

	if err := ps.AddPeer(addrUnreach); err != nil {
		t.Fatalf("AddPeer unreach: %v", err)
	}
	if err := ps.RecordPeerSuccess(addrUnreach, 0, ps.GetNetworkID()); err != nil {
		t.Fatalf("RecordPeerSuccess unreach: %v", err)
	}
	ps.markPeerUnreachable(addrUnreach, 10*time.Minute)

	if err := ps.AddPeer(addrStale); err != nil {
		t.Fatalf("AddPeer stale: %v", err)
	}
	if err := ps.RecordPeerSuccess(addrStale, 0, ps.GetNetworkID()); err != nil {
		t.Fatalf("RecordPeerSuccess stale: %v", err)
	}
	ps.Lock.Lock()
	if pi := ps.triedPeers[addrStale]; pi != nil {
		pi.LastSeen = time.Now().Add(-3 * time.Hour)
		pi.Failures = 1
	}
	if pi := ps.newPeers[addrStale]; pi != nil {
		pi.LastSeen = time.Now().Add(-3 * time.Hour)
		pi.Failures = 1
	}
	ps.Lock.Unlock()

	if !ps.isPeerUnreachable(addrUnreach) {
		t.Fatalf("expected %s to be marked unreachable", addrUnreach)
	}
	if !ps.isPeerStale(addrStale) {
		t.Fatalf("expected %s with LastSeen=-3h and Failures=1 to report stale", addrStale)
	}

	ps.SyncMempool()
	ps.RebroadcastMempool()
	time.Sleep(80 * time.Millisecond)

	if dialUnreach.Load() != 0 {
		t.Fatalf("expected SyncMempool/RebroadcastMempool to skip unreachable peer, got %d dials", dialUnreach.Load())
	}
	if dialStale.Load() != 0 {
		t.Fatalf("expected SyncMempool/RebroadcastMempool to skip stale peer, got %d dials", dialStale.Load())
	}

	// Verify RequestChainSync does not record a peer failure when local outbound slots are saturated.
	lnSyncPeer, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen sync peer: %v", err)
	}
	defer lnSyncPeer.Close()
	addrSync := lnSyncPeer.Addr().String()
	if err := ps.AddPeer(addrSync); err != nil {
		t.Fatalf("AddPeer sync: %v", err)
	}
	if err := ps.RecordPeerSuccess(addrSync, 0, ps.GetNetworkID()); err != nil {
		t.Fatalf("RecordPeerSuccess sync: %v", err)
	}

	ps.Lock.RLock()
	failBefore := ps.triedPeers[addrSync].Failures
	ps.Lock.RUnlock()

	var slotReleases []func()
	for i := 0; i < ps.MaxOutboundPeers; i++ {
		rel, ok := ps.tryReserveOutboundSlot()
		if !ok {
			t.Fatalf("failed to saturate outbound slot #%d", i)
		}
		slotReleases = append(slotReleases, rel)
	}
	ps.RequestChainSync(addrSync)
	for _, rel := range slotReleases {
		rel()
	}

	ps.Lock.RLock()
	failAfter := ps.triedPeers[addrSync].Failures
	ps.Lock.RUnlock()
	if failAfter != failBefore {
		t.Fatalf("expected RequestChainSync on errOutboundLimitExceeded not to penalize Failures (before=%d, after=%d)", failBefore, failAfter)
	}
}

// TestP2PRPC_CorruptNodeTLSCertAndAuthLockoutHardening verifies:
// 1) NewPeerServer fails closed (GetPeerStoreInitErr != nil && IsPeerStoreCorrupted) when p2ptls.pem is corrupt.
// 2) isAuthorizedMiner enforces globalAuthLockout BEFORE validating X-Mine-Key.
// 3) ValidateRPCBind rejects partial TLS cert/key pairs even for loopback binds.
func TestP2PRPC_CorruptNodeTLSCertAndAuthLockoutHardening(t *testing.T) {
	// 1. Corrupt p2ptls.pem fail-closed in NewPeerServer.
	bc := newP2PRPCHardeningBlockchain(t)
	corruptDir := t.TempDir()
	tlsPath := GetNodeTLSCertPath(corruptDir)
	if err := os.MkdirAll(filepath.Dir(tlsPath), 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(tlsPath, []byte("corrupt-non-pem-certificate-bytes"), 0600); err != nil {
		t.Fatalf("write corrupt p2ptls.pem: %v", err)
	}
	psCorrupt := NewPeerServer(getFreePort(t), bc, corruptDir)
	if psCorrupt.GetPeerStoreInitErr() == nil || !psCorrupt.IsPeerStoreCorrupted() {
		t.Fatalf("expected NewPeerServer with corrupt p2ptls.pem to fail closed (initErr=%v, corrupted=%v)",
			psCorrupt.GetPeerStoreInitErr(), psCorrupt.IsPeerStoreCorrupted())
	}

	// 2. Locked-out IP cannot bypass lockout with valid X-Mine-Key.
	t.Setenv("ARMX_DATA_DIR", t.TempDir())
	ResetRPCAuthForTesting()
	t.Cleanup(ResetRPCAuthForTesting)

	if err := InitRPCAuth("rpcadmin", "correct-password-123"); err != nil {
		t.Fatalf("InitRPCAuth: %v", err)
	}

	lockedIP := "198.51.100.77"
	badAuth := base64.StdEncoding.EncodeToString([]byte("rpcadmin:wrong-password"))
	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:19332/status", nil)
		req.RemoteAddr = lockedIP + ":" + strconv.Itoa(40000+i)
		req.Header.Set("Authorization", "Basic "+badAuth)
		if Authenticate(req) {
			t.Fatalf("expected invalid Basic Auth attempt #%d to fail", i+1)
		}
	}
	if !globalAuthLockout.isLocked(lockedIP) {
		t.Fatalf("expected IP %s to be locked out after 5 failed auth attempts", lockedIP)
	}

	srvCfg := &ServerConfig{MineKey: "super-secret-mine-key"}
	reqLocked, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:19332/mine", nil)
	reqLocked.RemoteAddr = lockedIP + ":49999"
	reqLocked.Header.Set("X-Mine-Key", srvCfg.MineKey)
	if isAuthorizedMiner(reqLocked, srvCfg) {
		t.Fatalf("expected locked-out IP to be denied by isAuthorizedMiner even with valid X-Mine-Key")
	}

	reqClean, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:19332/mine", nil)
	reqClean.RemoteAddr = "198.51.100.78:49999"
	reqClean.Header.Set("X-Mine-Key", srvCfg.MineKey)
	if !isAuthorizedMiner(reqClean, srvCfg) {
		t.Fatalf("expected non-locked IP with valid X-Mine-Key to be authorized")
	}

	// 3. ValidateRPCBind rejects half-configured TLS pair on loopback.
	cfgCertOnly := DefaultConfig()
	cfgCertOnly.RPCBind = "127.0.0.1"
	cfgCertOnly.RPCUser = "u"
	cfgCertOnly.RPCPassword = "p"
	cfgCertOnly.RPCTLSCert = filepath.Join(t.TempDir(), "rpc.crt")
	cfgCertOnly.RPCTLSKey = ""
	if err := ValidateRPCBind(cfgCertOnly); err == nil {
		t.Fatalf("expected ValidateRPCBind to reject RPCTLSCert without RPCTLSKey on loopback")
	}

	cfgKeyOnly := DefaultConfig()
	cfgKeyOnly.RPCBind = "127.0.0.1"
	cfgKeyOnly.RPCUser = "u"
	cfgKeyOnly.RPCPassword = "p"
	cfgKeyOnly.RPCTLSCert = ""
	cfgKeyOnly.RPCTLSKey = filepath.Join(t.TempDir(), "rpc.key")
	if err := ValidateRPCBind(cfgKeyOnly); err == nil {
		t.Fatalf("expected ValidateRPCBind to reject RPCTLSKey without RPCTLSCert on loopback")
	}

	cfgLoopClean := DefaultConfig()
	cfgLoopClean.RPCBind = "127.0.0.1"
	cfgLoopClean.RPCUser = "u"
	cfgLoopClean.RPCPassword = "p"
	cfgLoopClean.RPCTLSCert = ""
	cfgLoopClean.RPCTLSKey = ""
	if err := ValidateRPCBind(cfgLoopClean); err != nil {
		t.Fatalf("expected loopback RPC bind with empty TLS pair to pass ValidateRPCBind, got: %v", err)
	}
}

// TestP2P_InboundDisconnectReasonObservability verifies:
// 1) classifyInboundDisconnectReason distinguishes remote_eof, handshake_timeout, idle_timeout,
//    peer_reset, local_closed, frame_too_large, and payload_budget_exhausted.
// 2) handleConnectionWithRelease logs reason, duration, msgs, active_before, and active (post-decrement)
//    when P2P debug logging is enabled.
func TestP2P_InboundDisconnectReasonObservability(t *testing.T) {
	timeoutErr := os.ErrDeadlineExceeded
	if got := classifyInboundDisconnectReason(io.EOF, AuthenticatedIdleTimeout, 3); got != "remote_eof" {
		t.Fatalf("expected remote_eof for io.EOF, got %q", got)
	}
	if got := classifyInboundDisconnectReason(timeoutErr, InboundHandshakeTimeout, 0); got != "handshake_timeout" {
		t.Fatalf("expected handshake_timeout for initial 5s timeout, got %q", got)
	}
	if got := classifyInboundDisconnectReason(timeoutErr, AuthenticatedIdleTimeout, 2); got != "idle_timeout" {
		t.Fatalf("expected idle_timeout for authenticated 90s timeout, got %q", got)
	}
	if got := classifyInboundDisconnectReason(errors.New("read tcp: connection reset by peer"), AuthenticatedIdleTimeout, 1); got != "peer_reset" {
		t.Fatalf("expected peer_reset for connection reset, got %q", got)
	}
	if got := classifyInboundDisconnectReason(net.ErrClosed, AuthenticatedIdleTimeout, 1); got != "local_closed" {
		t.Fatalf("expected local_closed for net.ErrClosed, got %q", got)
	}
	if got := classifyInboundDisconnectReason(errors.New("message size exceeds hard limit of 8388608 bytes"), AuthenticatedIdleTimeout, 1); got != "frame_too_large" {
		t.Fatalf("expected frame_too_large, got %q", got)
	}
	if got := classifyInboundDisconnectReason(errLargePayloadBudgetExhausted, AuthenticatedIdleTimeout, 1); got != "payload_budget_exhausted" {
		t.Fatalf("expected payload_budget_exhausted, got %q", got)
	}

	t.Setenv("ARMX_P2P_DEBUG", "1")
	var logBuf bytes.Buffer
	logger := core.GetGlobalLogger()
	prevLevel := logger.GetLevel()
	logger.SetLevel(core.LogLevelDebug)
	logger.SetCustomWriter(&logBuf)
	t.Cleanup(func() {
		logger.SetCustomWriter(nil)
		logger.SetLevel(prevLevel)
	})

	bc := newP2PRPCHardeningBlockchain(t)
	ps := NewPeerServer(getFreePort(t), bc, t.TempDir())
	ps.DisableSubnetLimit = true
	ps.AllowPrivatePeers = true
	ps.RequireTLS = false
	ps.P2PTOFU = false
	t.Cleanup(ps.Stop)

	releaseSlot, _, ok := ps.tryReserveInboundSlot("198.51.100.0", false)
	if !ok {
		t.Fatalf("tryReserveInboundSlot failed")
	}

	clientConn, serverConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ps.handleConnectionWithRelease(serverConn, "198.51.100.0", releaseSlot)
	}()

	req, err := json.Marshal(Message{
		NetworkID: ps.GetNetworkID(),
		Type:      "GET_HEIGHT",
	})
	if err != nil {
		t.Fatalf("marshal GET_HEIGHT: %v", err)
	}
	reader := bufio.NewReader(clientConn)
	_ = clientConn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := clientConn.Write(append(req, '\n')); err != nil {
		t.Fatalf("write GET_HEIGHT: %v", err)
	}
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("read HEIGHT_RESPONSE: %v", err)
	}
	_ = clientConn.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleConnectionWithRelease did not exit after client close")
	}

	logOutput := logBuf.String()
	for _, want := range []string{
		"[P2P][CONN] inbound closed",
		"reason=remote_eof",
		"duration=",
		"msgs=1",
		"active_before=1",
		"active=0",
	} {
		if !strings.Contains(logOutput, want) {
			t.Fatalf("expected inbound closed debug log to contain %q, got:\n%s", want, logOutput)
		}
	}
}
