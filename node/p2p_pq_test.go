//go:build testhooks

package node

import (
	"crypto/ecdsa"
	"crypto/mldsa"
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"

	"github.com/armaxcoincore/armax-coin/core"
)

// handshakePair runs a TLS handshake between server and client configs over an in-memory pipe.
func handshakePair(t *testing.T, server, client *tls.Config) (tls.ConnectionState, error) {
	t.Helper()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	srv := tls.Server(a, server)
	errc := make(chan error, 1)
	go func() { errc <- srv.Handshake() }()
	cli := tls.Client(b, client)
	cliErr := cli.Handshake()
	if cliErr != nil {
		_ = a.Close()
	}
	srvErr := <-errc
	if cliErr != nil {
		return tls.ConnectionState{}, cliErr
	}
	if srvErr != nil {
		return tls.ConnectionState{}, srvErr
	}
	return cli.ConnectionState(), nil
}

func identityFor(t *testing.T, network string) (*tls.Config, *x509.Certificate) {
	t.Helper()
	if err := core.SetNetwork(network); err != nil {
		t.Fatalf("SetNetwork(%s): %v", network, err)
	}
	conf, _, err := InitNodeIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("InitNodeIdentity(%s): %v", network, err)
	}
	leaf, err := x509.ParseCertificate(conf.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("parse identity: %v", err)
	}
	return conf, leaf
}

// TestP2PTLS_HybridKEMOnly verifies sessions negotiate a hybrid ML-KEM group and that a peer
// offering only classical groups is refused.
func TestP2PTLS_HybridKEMOnly(t *testing.T) {
	core.LockNetworkForTesting(t)
	t.Cleanup(func() { _ = core.SetNetwork("testnet") })
	server, _ := identityFor(t, "testnet")

	cs, err := handshakePair(t, server, clientTLSConf.Clone())
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if cs.CurveID != tls.X25519MLKEM768 {
		t.Fatalf("negotiated %v, want X25519MLKEM768", cs.CurveID)
	}

	// A v1.0.12-style client (Go defaults, no CurvePreferences) still interoperates.
	legacy := &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}
	if cs, err := handshakePair(t, server, legacy); err != nil || cs.CurveID != tls.X25519MLKEM768 {
		t.Fatalf("default-config client: curve=%v err=%v", cs.CurveID, err)
	}

	classical := &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13, CurvePreferences: []tls.CurveID{tls.X25519, tls.CurveP256}}
	if _, err := handshakePair(t, server, classical); err == nil {
		t.Fatalf("classical-only client completed a handshake")
	}
}

// TestP2PTLS_IdentityKeyTypes verifies Testnet identities stay ECDSA P-256 (TOFU pins keep
// matching), new Mainnet identities are ML-DSA-65, and an ECDSA-identity peer interoperates with
// an ML-DSA-65 peer in both directions.
func TestP2PTLS_IdentityKeyTypes(t *testing.T) {
	core.LockNetworkForTesting(t)
	core.SetNetworkForTesting(true)
	t.Cleanup(func() { core.SetNetworkForTesting(false) })
	t.Cleanup(func() { _ = core.SetNetwork("testnet") })

	testnetConf, testnetLeaf := identityFor(t, "testnet")
	if _, ok := testnetLeaf.PublicKey.(*ecdsa.PublicKey); !ok {
		t.Fatalf("testnet identity key is %T, want ECDSA P-256", testnetLeaf.PublicKey)
	}

	mainDir := t.TempDir()
	if err := core.SetNetwork("mainnet"); err != nil {
		t.Fatalf("SetNetwork(mainnet): %v", err)
	}
	mainConf, _, err := InitNodeIdentity(mainDir)
	if err != nil {
		t.Fatalf("InitNodeIdentity(mainnet): %v", err)
	}
	mainLeaf, _ := x509.ParseCertificate(mainConf.Certificates[0].Certificate[0])
	if _, ok := mainLeaf.PublicKey.(*mldsa.PublicKey); !ok {
		t.Fatalf("mainnet identity key is %T, want ML-DSA-65", mainLeaf.PublicKey)
	}
	if mainLeaf.KeyUsage != x509.KeyUsageDigitalSignature {
		t.Fatalf("ML-DSA identity key usage = %v, want digital signature only", mainLeaf.KeyUsage)
	}

	// The persisted ML-DSA identity reloads unchanged.
	again, _, err := InitNodeIdentity(mainDir)
	if err != nil {
		t.Fatalf("reload ML-DSA identity: %v", err)
	}
	if string(again.Certificates[0].Certificate[0]) != string(mainConf.Certificates[0].Certificate[0]) {
		t.Fatalf("ML-DSA identity was regenerated instead of reloaded")
	}

	// ML-DSA-65 server, ECDSA client identity (mutual) and the reverse.
	client := clientTLSConf.Clone()
	client.Certificates = testnetConf.Certificates
	cs, err := handshakePair(t, mainConf, client)
	if err != nil {
		t.Fatalf("ML-DSA server <- ECDSA client: %v", err)
	}
	if _, ok := cs.PeerCertificates[0].PublicKey.(*mldsa.PublicKey); !ok {
		t.Fatalf("client did not see the ML-DSA-65 server certificate")
	}
	client2 := clientTLSConf.Clone()
	client2.Certificates = mainConf.Certificates
	if _, err := handshakePair(t, testnetConf, client2); err != nil {
		t.Fatalf("ECDSA server <- ML-DSA client: %v", err)
	}
}

// TestP2PTLS_OptInMLDSAIdentity verifies the explicit opt-in on Testnet.
func TestP2PTLS_OptInMLDSAIdentity(t *testing.T) {
	core.LockNetworkForTesting(t)
	t.Cleanup(func() { _ = core.SetNetwork("testnet") })
	t.Setenv(MLDSAIdentityEnv, "1")
	_, leaf := identityFor(t, "testnet")
	if _, ok := leaf.PublicKey.(*mldsa.PublicKey); !ok {
		t.Fatalf("opt-in identity key is %T, want ML-DSA-65", leaf.PublicKey)
	}
}

// TestP2P_InboundHandshakeRateLimit verifies the per-IP pre-TLS bucket in AllowMessage.
func TestP2P_InboundHandshakeRateLimit(t *testing.T) {
	ps := &PeerServer{}
	ip := "203.0.113.7"
	for i := 0; i < int(inboundHandshakeBurst); i++ {
		if !ps.AllowMessage(ip, inboundHandshakeMsgType) {
			t.Fatalf("attempt %d within burst refused", i+1)
		}
	}
	if ps.AllowMessage(ip, inboundHandshakeMsgType) {
		t.Fatalf("attempt beyond burst allowed")
	}
	if !ps.AllowMessage("203.0.113.8", inboundHandshakeMsgType) {
		t.Fatalf("another IP was throttled")
	}
	ps.banLock.RLock()
	score := ps.banScores[ip]
	ps.banLock.RUnlock()
	if score != 0 {
		t.Fatalf("handshake throttling added ban score %d", score)
	}
}
