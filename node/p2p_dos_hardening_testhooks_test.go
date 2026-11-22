//go:build testhooks

package node

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/armaxcoincore/armax-coin/core"
)

// TestP2P_SyncBatchByteLimit_BoundaryOvershootAndProgress verifies that RequestChainSync enforces MaxSyncBatchBytes (8 MiB):
// RequestChainSync flushes commit batches cleanly via ResolveFork once cumulative canonical blk.Size()
// reaches MaxSyncBatchBytes (8 MiB), preserving exact height continuity without skipping or duplicating blocks.
func TestP2P_SyncBatchByteLimit_BoundaryOvershootAndProgress(t *testing.T) {
	t.Setenv("ARMX_DATA_DIR", t.TempDir())
	t.Setenv("ARMX_ALLOW_PRIVATE_PEERS", "1")
	core.SetTargetOverrideForTesting(core.DifficultyToTarget(1))
	defer core.SetTargetOverrideForTesting(nil)

	srvDir := t.TempDir()
	bcSrv, err := core.NewBlockchainWithStoragePath(1, filepath.Join(srvDir, "srv.db"))
	if err != nil {
		t.Fatalf("init bcSrv: %v", err)
	}
	defer bcSrv.Close()

	senderAcc := newTestAccount(t, "sync_batch_sender")
	recipAcc := newTestAccount(t, "sync_batch_recip")
	fundAccount(bcSrv, senderAcc.Address, 10_000_000*core.CoinUnit)

	// Construct 6 valid diff-1 blocks (#1..#6), each containing 160 signed ML-DSA-65 transactions
	// (~10.78 KB each <= core.MaxTxSize 20,000 B, total ~1.73 MB/block <= core.MaxBlockSize 2,000,000 B).
	// Chunk responses return 1 block per chunk (< 2 MB MaxBlockRangeResponseBytes), so:
	//   - After 4 blocks (~6.92 MB < 8 MiB): stays buffered
	//   - After 5 blocks (~8.65 MB >= 8 MiB MaxSyncBatchBytes): RequestChainSync flushes batch #1 (#1..#5)
	//   - Batch #2 fetches remaining #6 (~1.73 MB) and resolves cleanly to tip #6.
	bcSrv.Lock.Lock()
	prev := bcSrv.Chain[0]
	var nextNonce int64
	for h := 1; h <= 6; h++ {
		const txsPerBlock = 160
		txs := make([]core.Transaction, 0, txsPerBlock+1)
		cb := core.Transaction{
			Sender:    "SYSTEM",
			Recipient: core.GenesisMiner,
			Amount:    core.InitialReward,
			Nonce:     int64(h),
			Signature: "REWARD_SIG",
		}
		txs = append(txs, cb)
		var totalFees int64
		for i := 0; i < txsPerBlock; i++ {
			nextNonce++
			tx := core.Transaction{
				Sender:    senderAcc.Address,
				SenderPub: senderAcc.PubHex,
				Recipient: recipAcc.Address,
				Amount:    1 * core.CoinUnit,
				Fee:       25000,
				Nonce:     nextNonce,
				Type:      core.TxTypeStandard,
			}
			if err := tx.Sign(senderAcc.PrivKey); err != nil {
				t.Fatalf("sign tx h=%d i=%d: %v", h, i, err)
			}
			totalFees += tx.Fee
			txs = append(txs, tx)
		}
		txs[0].Amount += totalFees

		blk := *core.NewBlock(txs, prev.Hash, h, core.GenesisMiner, core.DifficultyToTarget(1))
		blk.Timestamp = prev.Timestamp + int64(core.TargetBlockTimeSeconds)
		for {
			calcHash, err := blk.CalculateHashWithError()
			if err != nil {
				t.Fatalf("hash block #%d: %v", h, err)
			}
			blk.Hash = calcHash
			if core.HashMeetsTarget(blk.Hash, blk.GetTarget()) {
				break
			}
			blk.Nonce++
		}
		if sz := blk.Size(); sz <= 1_680_000 || sz > core.MaxBlockSize {
			bcSrv.Lock.Unlock()
			t.Fatalf("block #%d size %d outside expected target window (1680000..%d]", h, sz, core.MaxBlockSize)
		}
		bcSrv.Chain = append(bcSrv.Chain, blk)
		prev = blk
	}
	bcSrv.InvalidateChainworkCache()
	tipCW := bcSrv.GetTotalChainworkUnsafe().String()
	bcSrv.Lock.Unlock()

	mockTLS, err := EnsureNodeTLSConfig(srvDir)
	if err != nil {
		t.Fatalf("EnsureNodeTLSConfig: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	var rangeCalls atomic.Int32
	var wg sync.WaitGroup
	stopCh := make(chan struct{})
	defer func() {
		close(stopCh)
		_ = ln.Close()
		wg.Wait()
	}()

	netID := core.NetworkID()
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
				sc, _ := UpgradeToEncryptedTransportWithConfig(raw, mockTLS)
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
					// Keep mock peer idle timeout generous enough for -race ResolveFork validation across 8 MiB batches.
					_ = sc.SetReadDeadline(time.Now().Add(30 * time.Second))
					line, rErr := readBoundedLine(br, MaxIncomingPacketSize)
					if rErr != nil {
						return
					}
					var msg rawInboundMessage
					if json.Unmarshal([]byte(line), &msg) != nil {
						return
					}
					switch msg.Type {
					case "GET_HEIGHT":
						resp, _ := json.Marshal(Message{
							NetworkID: netID,
							Type:      "HEIGHT_RESPONSE",
							Payload:   HeightResponse{Height: 6, LatestHash: prev.Hash, Chainwork: tipCW},
						})
						_, _ = sc.Write(append(resp, '\n'))
					case "GET_FORKPOINT":
						resp, _ := json.Marshal(Message{
							NetworkID: netID,
							Type:      "FORKPOINT_RESPONSE",
							Payload:   ForkpointResponse{AncestorHeight: 0, AncestorHash: bcSrv.Chain[0].Hash},
						})
						_, _ = sc.Write(append(resp, '\n'))
					case "GET_BLOCK_RANGE":
						var req BlockRangeRequest
						_ = json.Unmarshal(msg.Payload, &req)
						rangeCalls.Add(1)
						// Respect MaxBlockRangeResponseBytes (~2 MB): return 1 block (~1.73 MB) per chunk.
						var blks []core.Block
						if req.FromIndex >= 1 && req.FromIndex < len(bcSrv.Chain) {
							blks = []core.Block{bcSrv.Chain[req.FromIndex]}
						}
						resp, _ := json.Marshal(Message{
							NetworkID: netID,
							Type:      "BLOCK_RANGE_RESPONSE",
							Payload:   BlockRangeResponse{FromIndex: req.FromIndex, ToIndex: req.FromIndex, Blocks: blks},
						})
						_, _ = sc.Write(append(resp, '\n'))
					}
				}
			}(rc)
		}
	}()

	cliDir := t.TempDir()
	bcCli, err := core.NewBlockchainWithStoragePath(1, filepath.Join(cliDir, "cli.db"))
	if err != nil {
		t.Fatalf("init bcCli: %v", err)
	}
	defer bcCli.Close()
	fundAccount(bcCli, senderAcc.Address, 10_000_000*core.CoinUnit)

	psCli := NewPeerServer(getFreePort(t), bcCli, cliDir, netID)
	psCli.AllowPrivatePeers = true
	psCli.RequireTLS = false
	defer psCli.Stop()

	psCli.RequestChainSync(ln.Addr().String())

	bcCli.Lock.RLock()
	gotLen := len(bcCli.Chain)
	gotTipHash := bcCli.Chain[gotLen-1].Hash
	bcCli.Lock.RUnlock()

	if gotLen != 7 {
		t.Fatalf("expected synced chain length 7 (height #6), got %d", gotLen)
	}
	if gotTipHash != prev.Hash {
		t.Fatalf("tip hash mismatch: got %s, want %s", gotTipHash, prev.Hash)
	}
	if calls := rangeCalls.Load(); calls != 6 {
		t.Fatalf("expected 6 exact sequential single-block chunks (flush at >=8 MiB then remaining), got %d", calls)
	}
}
