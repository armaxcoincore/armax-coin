package pqsig

import (
	"bytes"
	"compress/gzip"
	"crypto"
	"crypto/mldsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

// Vendored ML-DSA-65 vector files and their pinned SHA-256 digests. The digests make
// any accidental or silent edit to the consensus-critical vector data fail loudly.
// Provenance and licensing are documented in testdata/README.md.
const (
	acvpKeyGenFile   = "acvp_mldsa65_keygen.json.gz"
	acvpKeyGenSHA256 = "7f497bd83bd41325756879b88d21fc172d69c6813429f3bc86253839227800eb"

	acvpSigVerFile   = "acvp_mldsa65_sigver_pure.json.gz"
	acvpSigVerSHA256 = "e6c2964d0995a37ab56d201db1b159984461b08d3805933da910e61a30c7b684"

	wycheproofVerifyFile   = "wycheproof_mldsa65_verify.json.gz"
	wycheproofVerifySHA256 = "1243dc6c3f72c1e27348f1cbd5a3d555e9d54caa5a01a966fcaccfe2cc3ece26"

	wycheproofSignSeedFile   = "wycheproof_mldsa65_sign_seed.json.gz"
	wycheproofSignSeedSHA256 = "b08da9cddff1e28ce3108c0aecac9c2f803c814e5502c3c05c7c36925435a49c"
)

// loadVectorFile reads a gzip-compressed JSON vector file from testdata, checks it
// against its pinned SHA-256 digest, and decodes it into v.
func loadVectorFile(t *testing.T, name, wantSHA256 string, v any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != wantSHA256 {
		t.Fatalf("%s digest mismatch: got %s, want %s", name, got, wantSHA256)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gunzip %s: %v", name, err)
	}
	defer zr.Close()
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip %s: %v", name, err)
	}
	dec := json.NewDecoder(bytes.NewReader(plain))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("invalid hex %q: %v", s, err)
	}
	return b
}

// wycheproofShouldPass maps a Wycheproof result to a strict accept/reject decision.
// "acceptable" is rejected outright: a consensus verifier must not have cases whose
// outcome is left to the implementation.
func wycheproofShouldPass(t *testing.T, result string) bool {
	t.Helper()
	switch result {
	case "valid":
		return true
	case "invalid":
		return false
	default:
		t.Fatalf("unsupported Wycheproof result %q; consensus needs a fixed decision", result)
		return false
	}
}

type acvpKeyGenCase struct {
	TcID int    `json:"tcId"`
	Seed string `json:"seed"`
	PK   string `json:"pk"`
}

// TestPQSig_ACVPKeyGen checks FIPS 204 ML-DSA-65 key generation (seed -> public key)
// against the NIST ACVP keyGen answers, through the GenerateFromSeed path wallets use.
func TestPQSig_ACVPKeyGen(t *testing.T) {
	var cases []acvpKeyGenCase
	loadVectorFile(t, acvpKeyGenFile, acvpKeyGenSHA256, &cases)
	if len(cases) != 25 {
		t.Fatalf("expected 25 ACVP keyGen cases, got %d", len(cases))
	}
	for _, tc := range cases {
		t.Run("tcId="+strconv.Itoa(tc.TcID), func(t *testing.T) {
			seedBytes := mustHex(t, tc.Seed)
			if len(seedBytes) != SeedSize {
				t.Fatalf("seed length %d, want %d", len(seedBytes), SeedSize)
			}
			var seed [32]byte
			copy(seed[:], seedBytes)
			sk, err := GenerateFromSeed(seed)
			if err != nil {
				t.Fatalf("GenerateFromSeed: %v", err)
			}
			if !bytes.Equal(sk.PublicKeyBytes(), mustHex(t, tc.PK)) {
				t.Fatalf("public key does not match NIST ACVP answer")
			}
		})
	}
}

type acvpSigVerCase struct {
	TcID       int    `json:"tcId"`
	TestPassed bool   `json:"testPassed"`
	Reason     string `json:"reason"`
	PK         string `json:"pk"`
	Message    string `json:"message"`
	Context    string `json:"context"`
	Signature  string `json:"signature"`
}

// TestPQSig_ACVPSigVer checks ML-DSA-65 verification (external interface, pure, with
// context, as used by ARMX-TX-V1) against NIST ACVP sigVer pass/fail answers.
func TestPQSig_ACVPSigVer(t *testing.T) {
	var cases []acvpSigVerCase
	loadVectorFile(t, acvpSigVerFile, acvpSigVerSHA256, &cases)
	if len(cases) != 15 {
		t.Fatalf("expected 15 ACVP sigVer cases, got %d", len(cases))
	}
	passed := 0
	for _, tc := range cases {
		if tc.TestPassed {
			passed++
		}
		t.Run("tcId="+strconv.Itoa(tc.TcID), func(t *testing.T) {
			opts := &mldsa.Options{Context: string(mustHex(t, tc.Context))}
			got := verifyWithOptions(mustHex(t, tc.PK), mustHex(t, tc.Message), mustHex(t, tc.Signature), opts)
			if got != tc.TestPassed {
				t.Fatalf("verify=%v, NIST expects %v (%s)", got, tc.TestPassed, tc.Reason)
			}
		})
	}
	if passed != 3 {
		t.Fatalf("expected 3 passing ACVP sigVer cases, got %d", passed)
	}
}

type wycheproofVerifyFileData struct {
	Algorithm        string            `json:"algorithm"`
	GeneratorVersion string            `json:"generatorVersion"`
	Header           []string          `json:"header"`
	Notes            json.RawMessage   `json:"notes"`
	NumberOfTests    int               `json:"numberOfTests"`
	Schema           string            `json:"schema"`
	TestGroups       []wycheproofGroup `json:"testGroups"`
}

type wycheproofGroup struct {
	Type        string               `json:"type"`
	PublicKey   *string              `json:"publicKey"`
	PrivateSeed string               `json:"privateSeed"`
	Source      json.RawMessage      `json:"source"`
	Tests       []wycheproofTestCase `json:"tests"`
}

type wycheproofTestCase struct {
	TcID    int      `json:"tcId"`
	Comment string   `json:"comment"`
	Msg     *string  `json:"msg"`
	Ctx     *string  `json:"ctx"`
	Mu      *string  `json:"mu"`
	Sig     string   `json:"sig"`
	Result  string   `json:"result"`
	Flags   []string `json:"flags"`
}

func (tc wycheproofTestCase) name() string {
	return "tcId=" + strconv.Itoa(tc.TcID) + "/" + tc.Comment
}

func (tc wycheproofTestCase) options(t *testing.T) *mldsa.Options {
	t.Helper()
	opts := &mldsa.Options{}
	if tc.Ctx != nil {
		opts.Context = string(mustHex(t, *tc.Ctx))
	}
	return opts
}

// TestPQSig_WycheproofVerify runs every Wycheproof ML-DSA-65 verification vector
// (bit flips, wrong lengths, non-canonical hints, infinity-norm violations, invalid
// contexts, zero keys) through the consensus verification path. A difference in
// accept/reject behaviour between toolchains would split the chain, so every case
// must match exactly.
func TestPQSig_WycheproofVerify(t *testing.T) {
	var data wycheproofVerifyFileData
	loadVectorFile(t, wycheproofVerifyFile, wycheproofVerifySHA256, &data)
	if data.Algorithm != "ML-DSA-65" {
		t.Fatalf("unexpected algorithm %q", data.Algorithm)
	}

	total, valid := 0, 0
	for _, tg := range data.TestGroups {
		if tg.PublicKey == nil {
			t.Fatalf("verify group without public key")
		}
		pub := mustHex(t, *tg.PublicKey)
		for _, tc := range tg.Tests {
			total++
			want := wycheproofShouldPass(t, tc.Result)
			if want {
				valid++
			}
			if tc.Msg == nil {
				t.Fatalf("tcId %d: verify vector without message", tc.TcID)
			}
			t.Run(tc.name(), func(t *testing.T) {
				got := verifyWithOptions(pub, mustHex(t, *tc.Msg), mustHex(t, tc.Sig), tc.options(t))
				if got != want {
					t.Fatalf("verify=%v, Wycheproof expects %v (flags %v)", got, want, tc.Flags)
				}
			})
		}
	}
	if total != data.NumberOfTests || total != 210 || valid != 79 {
		t.Fatalf("vector count drift: total=%d (header %d), valid=%d; want 210/79", total, data.NumberOfTests, valid)
	}
}

type wycheproofSignSeedFileData struct {
	Algorithm     string            `json:"algorithm"`
	Header        []string          `json:"header"`
	Notes         json.RawMessage   `json:"notes"`
	NumberOfTests int               `json:"numberOfTests"`
	Schema        string            `json:"schema"`
	TestGroups    []wycheproofGroup `json:"testGroups"`
}

// signDeterministic signs with the FIPS 204 deterministic variant (rnd = 0^32) so the
// result can be compared byte for byte. Production Sign is hedged and cannot be.
func signDeterministic(sk *PrivateKey, msg []byte, opts crypto.SignerOpts) ([]byte, error) {
	return sk.inner.SignDeterministic(msg, opts)
}

// TestPQSig_WycheproofSignSeed checks deterministic ML-DSA-65 signing byte for byte
// against Wycheproof seed-based vectors, and that each seed derives the expected
// public key. Randomized vectors were removed from the vendored file because the
// public API cannot inject rnd.
func TestPQSig_WycheproofSignSeed(t *testing.T) {
	var data wycheproofSignSeedFileData
	loadVectorFile(t, wycheproofSignSeedFile, wycheproofSignSeedSHA256, &data)
	if data.Algorithm != "ML-DSA-65" {
		t.Fatalf("unexpected algorithm %q", data.Algorithm)
	}

	total := 0
	for _, tg := range data.TestGroups {
		seedBytes := mustHex(t, tg.PrivateSeed)
		for _, tc := range tg.Tests {
			total++
			if slices.Contains(tc.Flags, "Randomized") {
				t.Fatalf("tcId %d: randomized vector must not be vendored", tc.TcID)
			}
			want := wycheproofShouldPass(t, tc.Result)
			t.Run(tc.name(), func(t *testing.T) {
				if len(seedBytes) != SeedSize {
					if _, err := mldsa.NewPrivateKey(params, seedBytes); err == nil {
						t.Fatalf("seed of %d bytes accepted", len(seedBytes))
					}
					if want {
						t.Fatalf("vector expects success with a %d-byte seed", len(seedBytes))
					}
					return
				}
				var seed [32]byte
				copy(seed[:], seedBytes)
				sk, err := GenerateFromSeed(seed)
				if err != nil {
					t.Fatalf("GenerateFromSeed: %v", err)
				}
				pub := sk.PublicKeyBytes()
				if tg.PublicKey != nil && !bytes.Equal(pub, mustHex(t, *tg.PublicKey)) {
					t.Fatalf("public key mismatch")
				}

				var sig []byte
				if tc.Msg != nil {
					sig, err = signDeterministic(sk, mustHex(t, *tc.Msg), tc.options(t))
				} else if tc.Mu != nil {
					sig, err = signDeterministic(sk, mustHex(t, *tc.Mu), crypto.MLDSAMu)
				} else {
					t.Fatalf("vector has neither msg nor mu")
				}
				if err != nil {
					if want {
						t.Fatalf("sign: %v", err)
					}
					return
				}
				if !want {
					t.Fatalf("sign unexpectedly succeeded")
				}
				if !bytes.Equal(sig, mustHex(t, tc.Sig)) {
					t.Fatalf("deterministic signature differs from Wycheproof answer")
				}
				if tc.Msg != nil && !verifyWithOptions(pub, mustHex(t, *tc.Msg), sig, tc.options(t)) {
					t.Fatalf("own signature failed verification")
				}
			})
		}
	}
	if total != 104 {
		t.Fatalf("vector count drift: got %d deterministic sign vectors, want 104", total)
	}
}
