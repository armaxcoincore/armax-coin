# ML-DSA-65 test vectors

Vendored known-answer and edge-case vectors used by `core/pqsig/pqsig_vectors_test.go`.
They run in CI on every build so that Armax consensus does not depend only on the Go
toolchain's own testing of `crypto/mldsa`: if a future toolchain changes how ML-DSA-65
keys are derived or which signatures it accepts, these tests fail instead of nodes
built with different toolchains disagreeing about block validity.

Each file is gzip-compressed JSON (compact, sorted keys, gzip `mtime=0`). Its SHA-256
digest is pinned in the test, so any edit to the data is detected.

| File | Source | Cases | Covers |
|---|---|---:|---|
| `acvp_mldsa65_keygen.json.gz` | NIST ACVP-Server `ML-DSA-keyGen-FIPS204`, test group `tgId=2` (ML-DSA-65) | 25 | seed -> public key |
| `acvp_mldsa65_sigver_pure.json.gz` | NIST ACVP-Server `ML-DSA-sigVer-FIPS204`, test group `tgId=3` (ML-DSA-65, external interface, pure, with context) | 15 | verify pass/fail (modified z, hint, commitment, message) |
| `wycheproof_mldsa65_verify.json.gz` | C2SP Wycheproof `testvectors_v1/mldsa_65_verify_test.json` | 210 | verify edge cases (lengths, hints, norms, contexts, zero keys) |
| `wycheproof_mldsa65_sign_seed.json.gz` | C2SP Wycheproof `testvectors_v1/mldsa_65_sign_seed_test.json` | 104 | deterministic signing byte for byte, seed -> public key |

## Provenance

- NIST ACVP-Server: <https://github.com/usnistgov/ACVP-Server>, commit
  `975de31eb83d87039ec88934fdc47d8c312b892d`, files
  `gen-val/json-files/<name>/internalProjection.json`.
- C2SP Wycheproof: <https://github.com/C2SP/wycheproof>, Go module version
  `v0.0.0-20260625212325-ee7b4f7e6119` (the version pinned by the Go 1.27 standard
  library's own Wycheproof tests).

## Modifications (2026-11-26)

- NIST ACVP data: kept only the ML-DSA-65 test group listed above; kept only the fields
  `tcId`, `seed`, `pk` (keyGen) and `tcId`, `testPassed`, `reason`, `pk`, `message`,
  `context`, `signature` (sigVer); re-encoded as compact JSON and gzip-compressed.
  ACVP sigGen vectors are not included because they provide expanded private keys,
  which the public `crypto/mldsa` API cannot load; deterministic signing is covered by
  the Wycheproof seed-based vectors instead.
- Wycheproof data: removed `publicKeyDer` (verify) and `privateKeyPkcs8` (sign) fields;
  removed sign vectors flagged `Randomized`, which need an injected `rnd` that the
  public API does not expose; re-encoded as compact JSON and gzip-compressed.

## Licenses

- NIST ACVP data. Source: National Institute of Standards and Technology (NIST),
  ACVP-Server. Notice reproduced as required:

  > NIST-developed software is provided by NIST as a public service. You may use, copy, and distribute copies of the software in any medium, provided that you keep intact this entire notice. You may improve, modify, and create derivative works of the software or any portion of the software, and you may copy and distribute such modifications or works. Modified works should carry a notice stating that you changed the software and should note the date and nature of any such change. Please explicitly acknowledge the National Institute of Standards and Technology as the source of the software.
  >
  > NIST-developed software is expressly provided "AS IS." NIST MAKES NO WARRANTY OF ANY KIND, EXPRESS, IMPLIED, IN FACT, OR ARISING BY OPERATION OF LAW, INCLUDING, WITHOUT LIMITATION, THE IMPLIED WARRANTY OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE, NON-INFRINGEMENT, AND DATA ACCURACY. NIST NEITHER REPRESENTS NOR WARRANTS THAT THE OPERATION OF THE SOFTWARE WILL BE UNINTERRUPTED OR ERROR-FREE, OR THAT ANY DEFECTS WILL BE CORRECTED. NIST DOES NOT WARRANT OR MAKE ANY REPRESENTATIONS REGARDING THE USE OF THE SOFTWARE OR THE RESULTS THEREOF, INCLUDING BUT NOT LIMITED TO THE CORRECTNESS, ACCURACY, RELIABILITY, OR USEFULNESS OF THE SOFTWARE.
  >
  > You are solely responsible for determining the appropriateness of using and distributing the software and you assume all risks associated with its use, including but not limited to the risks and costs of program errors, compliance with applicable laws, damage to or loss of data, programs or equipment, and the unavailability or interruption of operation. This software is not intended to be used in any situation where a failure could cause risk of injury or damage to property. The software developed by NIST employees is not subject to copyright protection within the United States.

- Wycheproof vectors are licensed under the Apache License, Version 2.0:
  <https://github.com/C2SP/wycheproof/blob/main/LICENSE>.
