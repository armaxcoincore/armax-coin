package node

import (
	"fmt"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/armaxcoincore/armax-coin/core"
)

// bestClaim evaluates the claim table the same way GetSyncStatus does.
func bestClaim(p *PeerServer, localHeight int, localCW *big.Int, tipTs int64, now time.Time) int {
	p.syncStateMu.Lock()
	defer p.syncStateMu.Unlock()
	return p.bestClaimedPeerHeightLocked(localHeight, localCW, tipTs, now)
}

func TestPeerTipClaims_HonestClaimAheadHoldsGate(t *testing.T) {
	p := &PeerServer{}
	now := time.Now()
	localCW := big.NewInt(1000)

	p.recordPeerTipClaim("203.0.113.10:19334", 110, big.NewInt(2000))
	if got := bestClaim(p, 100, localCW, now.Unix(), now); got != 110 {
		t.Fatalf("best claimed height = %d, want 110", got)
	}
}

func TestPeerTipClaims_AbsurdHeightIsIgnored(t *testing.T) {
	p := &PeerServer{}
	now := time.Now()
	localCW := big.NewInt(1000)
	huge := new(big.Int).Lsh(big.NewInt(1), 255)

	p.recordPeerTipClaim("198.51.100.7:19334", math.MaxInt32, huge)
	if got := bestClaim(p, 100, localCW, now.Unix(), now); got != 0 {
		t.Fatalf("absurd claim must not hold the sync gate, got best=%d", got)
	}
}

func TestPeerTipClaims_LowerChainworkClaimIsIgnored(t *testing.T) {
	p := &PeerServer{}
	now := time.Now()
	localCW := big.NewInt(5000)

	// High height but less chainwork than the local chain: not ahead, must not block mining.
	p.recordPeerTipClaim("198.51.100.8:19334", 150, big.NewInt(10))
	if got := bestClaim(p, 100, localCW, now.Unix(), now); got != 0 {
		t.Fatalf("lower-chainwork claim must be ignored, got best=%d", got)
	}
}

func TestPeerTipClaims_ExpiredClaimIsPruned(t *testing.T) {
	p := &PeerServer{}
	now := time.Now()
	localCW := big.NewInt(1000)

	p.recordPeerTipClaim("203.0.113.11:19334", 120, big.NewInt(2000))
	later := now.Add(peerTipClaimTTL + time.Second)
	if got := bestClaim(p, 100, localCW, later.Unix(), later); got != 0 {
		t.Fatalf("expired claim must not hold the sync gate, got best=%d", got)
	}
	if n := len(p.peerTipClaims); n != 0 {
		t.Fatalf("expired claim must be pruned, table size=%d", n)
	}
}

func TestPeerTipClaims_DroppedAfterFailedDelivery(t *testing.T) {
	p := &PeerServer{}
	now := time.Now()
	localCW := big.NewInt(1000)

	p.recordPeerTipClaim("198.51.100.9:19334", 140, big.NewInt(9000))
	if got := bestClaim(p, 100, localCW, now.Unix(), now); got != 140 {
		t.Fatalf("precondition: best=%d, want 140", got)
	}
	p.dropPeerTipClaim("198.51.100.9:19334")
	if got := bestClaim(p, 100, localCW, now.Unix(), now); got != 0 {
		t.Fatalf("dropped claim must release the sync gate, got best=%d", got)
	}
}

func TestPeerTipClaims_GroupedPerHostAcrossPorts(t *testing.T) {
	p := &PeerServer{}
	p.recordPeerTipClaim("198.51.100.20:19334", 130, big.NewInt(9000))
	p.recordPeerTipClaim("198.51.100.20:29334", 105, big.NewInt(9000))
	if n := len(p.peerTipClaims); n != 1 {
		t.Fatalf("claims from one host must share a slot, table size=%d", n)
	}
	now := time.Now()
	if got := bestClaim(p, 100, big.NewInt(1000), now.Unix(), now); got != 105 {
		t.Fatalf("latest claim from the host must replace the earlier one, got best=%d", got)
	}
}

func TestPeerTipClaims_TableIsBounded(t *testing.T) {
	p := &PeerServer{}
	for i := 0; i < maxPeerTipClaims+50; i++ {
		host := fmt.Sprintf("10.%d.%d.%d:19334", (i>>16)&0xff, (i>>8)&0xff, i&0xff)
		p.recordPeerTipClaim(host, 101, big.NewInt(2000))
	}
	if n := len(p.peerTipClaims); n > maxPeerTipClaims {
		t.Fatalf("claim table exceeded cap: %d > %d", n, maxPeerTipClaims)
	}
}

func TestPeerTipClaims_MarkSyncCaughtUpDoesNotRaiseClaims(t *testing.T) {
	p := &PeerServer{}
	p.markSyncCaughtUp()
	now := time.Now()
	if got := bestClaim(p, 100, big.NewInt(1000), now.Unix(), now); got != 0 {
		t.Fatalf("markSyncCaughtUp must not create a height claim, got best=%d", got)
	}
	if !p.initialSyncDone {
		t.Fatal("markSyncCaughtUp must mark initial sync as done")
	}
}

func TestMaxPlausiblePeerLead_AllowsInitialBlockDownload(t *testing.T) {
	now := time.Now()
	// A node still at a tip from 30 days ago must accept the honest network height.
	tipTs := now.Add(-30 * 24 * time.Hour).Unix()
	honestLead := int((30 * 24 * time.Hour).Seconds()) / int(core.TargetBlockTimeSeconds)
	if lead := maxPlausiblePeerLead(tipTs, now); lead < honestLead {
		t.Fatalf("plausible lead %d must cover the honest lead %d", lead, honestLead)
	}
	// A fresh tip only tolerates the fixed margin.
	if lead := maxPlausiblePeerLead(now.Unix(), now); lead != peerTipClaimBaseLead {
		t.Fatalf("fresh-tip lead = %d, want %d", lead, peerTipClaimBaseLead)
	}
	// Future-dated local tips never produce a negative lead.
	if lead := maxPlausiblePeerLead(now.Add(time.Hour).Unix(), now); lead != peerTipClaimBaseLead {
		t.Fatalf("future-tip lead = %d, want %d", lead, peerTipClaimBaseLead)
	}
}
