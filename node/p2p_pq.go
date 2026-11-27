package node

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/tls"
	"os"

	"github.com/armaxcoincore/armax-coin/core"
)

// Post-quantum P2P transport.
//
// Key exchange: on Mainnet every P2P TLS 1.3 session uses a hybrid ML-KEM key exchange only, so
// a Mainnet peer that offers no ML-KEM group cannot complete a TLS handshake. Every other network
// keeps Go's defaults, which already prefer X25519MLKEM768, so running Testnet peers (including
// ones started with GODEBUG=tlsmlkem=0) see no behaviour change.
//
// The KEM lock applies inside a TLS session only. While RequireTLS and P2PTOFU are both off, a
// failed handshake can still fall back to plaintext JSON, so this is not full downgrade
// protection; enable -requiretls or -p2p-tofu for that.
//
// Identity: new Mainnet node identities are ML-DSA-65 certificates (TLS scheme 0x0905). Testnet
// identities stay ECDSA P-256 so existing TOFU pins (peers.json, node/seed_pins.go) keep
// matching; ARMX_P2P_MLDSA_IDENTITY=1 opts a new identity into ML-DSA-65 on other networks.
// Existing identity files are always loaded as they are and never rotated automatically.

// MLDSAIdentityEnv opts a newly generated P2P identity into ML-DSA-65 outside Mainnet.
const MLDSAIdentityEnv = "ARMX_P2P_MLDSA_IDENTITY"

// P2PCurvePreferences returns the key exchanges accepted for P2P TLS: hybrid ML-KEM only on
// Mainnet, and nil (Go's defaults) on every other network.
func P2PCurvePreferences(mainnet bool) []tls.CurveID {
	if !mainnet {
		return nil
	}
	return []tls.CurveID{tls.X25519MLKEM768, tls.SecP256r1MLKEM768}
}

// useMLDSAIdentity reports whether a newly generated identity should be ML-DSA-65.
func useMLDSAIdentity() bool {
	return core.GetActiveNetwork().IsMainnet || os.Getenv(MLDSAIdentityEnv) == "1"
}

// newNodeIdentityKey generates the private key for a new P2P identity certificate.
func newNodeIdentityKey() (crypto.Signer, error) {
	if useMLDSAIdentity() {
		return mldsa.GenerateKey(mldsa.MLDSA65())
	}
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}
