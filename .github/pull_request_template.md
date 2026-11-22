> [!NOTE]
> **Docs / Typo / Small Non-Code PRs:** You only need a concise description below (and `Fixes #...` if applicable). Feel free to skip the Consensus and Go code checklists—automated CI validates repository hygiene automatically.

## Description
Provide a concise summary of the changes in this pull request and why they help.

Fixes #(issue)

## Type of Change
Please check the options that are relevant:
- [ ] **Bug fix** (non-breaking change which fixes an issue)
- [ ] **New feature** (non-breaking change which adds functionality)
- [ ] **Breaking change** (fix or feature that alters existing functionality or wire format)
- [ ] **Consensus rule change** (affects block validation, ArmaxHash, Armax-ASERT-AW/LWMA, `TxBinaryV1`, or transaction format)
- [ ] **Documentation / Chore** (updates to docs, typos, CI, or build configurations)

## Subsystem / Component *(Optional)*
- [ ] `core/` (Blockchain state, block validation, transactions, crypto/ML-DSA)
- [ ] `armaxhash/` (ArmaxHash v1: Argon2id and BLAKE2b; v2: domain-separated chained BLAKE2b + prefix cache)
- [ ] `node/` (P2P mesh, sync gate, TLS 1.3/TOFU transport, RPC daemon)
- [ ] `miner/` (Multi-threaded CPU mining engine & Stratum TCP/TLS client)
- [ ] `wallet/` & `cli/` (CLI tools: `armwallet`, `armsend`, multi-wallet keystore)
- [ ] `gui/` (Desktop GUI v2: WebView2, system tray, multi-wallet & CPU miner)
- [ ] Docs, Scripts, or Workflows

## Consensus & Network Compatibility Checklist
*(Skip for documentation-only or non-protocol PRs; required when touching `core/`, `node/`, or `armaxhash/`)*
- [ ] Changes preserve deterministic block and transaction evaluation across platforms.
- [ ] Wire format changes maintain backwards compatibility or define an explicit version bump.
- [ ] No sensitive keys, test mnemonic seeds, or private credentials are included.

## Quality & Verification Checklist
*(Skip Go build/race items for docs-only PRs; CI also runs full checks automatically)*
- [ ] Code compiles cleanly on Go 1.27.4+ (`go build ./...`).
- [ ] Relevant unit/integration tests pass locally (`go test -tags testhooks -race ./...`).
- [ ] Added unit or integration test cases covering new code behavior (if applicable).
- [ ] Code/docs follow existing conventions (`go fmt` / valid Markdown links) with clean commit messages (`Signed-off-by` DCO).
