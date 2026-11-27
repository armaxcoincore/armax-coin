package pqsig

import (
	"crypto/sha256"
	"testing"
)

func contextTestKey(t *testing.T) *PrivateKey {
	t.Helper()
	sk, err := GenerateFromSeed(sha256.Sum256([]byte("pqsig-context-test")))
	if err != nil {
		t.Fatal(err)
	}
	return sk
}

func TestSignWithContext_DomainSeparation(t *testing.T) {
	sk := contextTestKey(t)
	pub := sk.PublicKeyBytes()
	msg := []byte("off-chain statement")

	sig, err := sk.SignWithContext(msg, "ARMX-MIGRATION-V1")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyWithContext(pub, msg, sig, "ARMX-MIGRATION-V1") {
		t.Fatal("signature must verify under its own context")
	}
	if VerifyWithContext(pub, msg, sig, "ARMX-POOL-LEDGER-V1") {
		t.Fatal("signature must not verify under another application context")
	}
	if Verify(pub, msg, sig) {
		t.Fatal("an application-context signature must not verify as a transaction signature")
	}

	txSig, err := sk.Sign(msg)
	if err != nil {
		t.Fatal(err)
	}
	if VerifyWithContext(pub, msg, txSig, "ARMX-MIGRATION-V1") {
		t.Fatal("a transaction signature must not verify as an off-chain statement")
	}
}

func TestSignWithContext_RejectsReservedAndInvalidContexts(t *testing.T) {
	sk := contextTestKey(t)
	pub := sk.PublicKeyBytes()
	msg := []byte("m")
	long := string(make([]byte, 256))
	for _, ctx := range []string{"", ContextString, long} {
		if _, err := sk.SignWithContext(msg, ctx); err == nil {
			t.Fatalf("context of length %d (reserved=%v) must be refused", len(ctx), ctx == ContextString)
		}
	}
	txSig, err := sk.Sign(msg)
	if err != nil {
		t.Fatal(err)
	}
	if VerifyWithContext(pub, msg, txSig, ContextString) {
		t.Fatal("VerifyWithContext must refuse the transaction context")
	}
}
