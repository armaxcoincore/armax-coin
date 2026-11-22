# Contributing to Armaxcoin (`armax-coin`)

Thank you for your interest in contributing to Armaxcoin. **Armaxcoin is an open-source community project**, not a closed organization. Whether you are fixing a typo, reporting a bug, testing a release, running a node/miner, or hardening post-quantum consensus code, **everyone is welcome to participate without formal membership or prior appointment**.

See something that can be improved? **Join the discussion, report it, or send a Pull Request.**

At the same time, because Armaxcoin is an experimental Layer-1 Proof-of-Work blockchain incorporating NIST FIPS 204 ML-DSA-65 signatures and ArmaxHash v2, code affecting consensus, cryptography, P2P security, or wallets follows strict testing and review safeguards.

---

## 1. Code of Conduct

All contributors and community participants are expected to adhere to our [Code of Conduct](CODE_OF_CONDUCT.md). Please treat all participants with respect, professionalism, and constructive collaboration.

---

## 2. Experimental Status & Security Boundaries

- **Experimental Software:** Armaxcoin is currently deployed in an active open testnet phase (`ARMX-TESTNET-1`) for public verification and protocol stabilization.
- **No Financial Guarantees:** Testnet coins (`tARMX`) have no current market value and carry no financial guarantee or investment return. Verified community testnet mining rewards are planned for a future Mainnet migration program with non-final, TBA conversion terms.
- **Vulnerability Reporting:** Do **NOT** open public GitHub issues for security vulnerabilities, consensus bypasses, or denial-of-service vectors. Follow the responsible disclosure procedures defined in [SECURITY.md](SECURITY.md).

---

## 3. Getting Started

*(Required only if you plan to build and test Go code locally. For documentation, typo, or template PRs, feel free to skip local Go setup and edit directly via fork + Pull Request.)*

### Prerequisites

- **Go:** Version **1.27 or higher** (strict requirement due to standard library `crypto/mldsa` post-quantum support).
- **Git:** Standard Git client.
- **Operating Systems Supported:**
  - Linux (`amd64`, `arm64`)
  - Windows 10/11 / Windows Server (`amd64`)
  - macOS Apple Silicon (`arm64`)
- **Linters & Tooling (Optional locally, run automatically in CI):**
  - `staticcheck` (pinned version v0.8.2)
  - `govulncheck` (pinned version v1.9.0)

### Clone & Build

```bash
git clone https://github.com/armaxcoincore/armax-coin.git
cd armax-coin

# Verify Go toolchain
go version

# Compile all binaries
go build ./...

# Run the test suite
go test -count=1 ./...
```

---

## 4. Development Workflow & Guidelines

### Lightweight Contribution Workflow

1. **Explore the project and existing docs:** Start with [`README.md`](README.md) and [`docs/README.md`](docs/README.md) to see how things work today.
2. **Discuss larger changes when useful:** For major features or protocol ideas, open an issue or GitHub Discussion first so maintainers can check scope and compatibility early. *(Formal pre-approval is not required for bug fixes or documentation improvements.)*
3. **Open an Issue for reproducible bugs:** Use the bug report template with logs, version, and steps to reproduce *(never post security exploits publicly)*.
4. **Fork and submit a Pull Request:** Create a focused topic branch in your fork, add tests if modifying code, sign off your commits (`git commit -s`), and open a PR against `main`.
5. **Automated CI runs checks:** GitHub Actions runs build, test, race detection, linting, and hygiene checks automatically on every PR.
6. **Proportional maintainer review & merge:** Maintainers review proportionally to change risk, share feedback, and merge accepted changes.

### Branching Strategy

1. Fork the repository on GitHub (no upstream `Write` access or GitHub Org membership is needed).
2. Create a focused topic branch from `main`:
   ```bash
   git checkout -b feature/my-feature-name
   ```
3. Keep commits atomic, well-scoped, and accompanied by tests when modifying Go code.

### Commit Conventions

Commit messages should be written clearly in English following [Conventional Commits](https://www.conventionalcommits.org/) where practical:

- `feat:` A new feature or user-facing functionality.
- `fix:` A bug fix or defect resolution.
- `docs:` Documentation additions or corrections.
- `refactor:` Code refactoring without behavioral changes.
- `test:` Adding or improving test suites.
- `build:` Build tooling, packaging scripts, or dependency changes.
- `ci:` GitHub Actions workflow modifications.
- `security:` Security hardening or vulnerability remediations.

Example:
```text
feat(miner): add thread auto-detection based on runtime.NumCPU
fix(p2p): prevent goroutine leak on handshake timeout
docs(mining): clarify ArmaxHash memory requirements
```

### Consensus & Determinism Rules

Any change to packages `core/`, `armaxhash/`, or consensus-critical logic in `node/` must satisfy:

1. **Strict Determinism:** No non-deterministic iteration order over Go maps when calculating hashes, Merkle trees, block headers, or transaction lists.
2. **Network Profile Isolation:** Changes must never hardcode mainnet or testnet constants outside of `core/network.go`.
3. **No Breaking Wire Changes:** P2P wire formats and RPC JSON schemas must maintain backward compatibility or be versioned explicitly.
4. **Zero Float Usage in Economics:** All financial calculations (balances, rewards, fees, totals) must strictly use integer Armoids (`1 ARMX = 100,000,000 Armoids`) with 64-bit integer arithmetic. Floating-point types are strictly forbidden in consensus paths.
5. **No Blind Renames:** Do not rename protocol identifiers, wire commands, network IDs (`ARMX-TESTNET-1`), database keys, or serialization tags.

---

## 5. Verification Gates

Verification expectations match the scope of your change:

- **Documentation / Typo / Template PRs:** Confirm relative links are accurate and follow `markdown` conventions—**you do not need to run full local Go test suites or install linters**, as automated CI validates repository hygiene.
- **Go Code / Build / CI Changes:** Run the relevant verification checks locally before submitting (CI executes the full suite across platforms):

```bash
# 1. Formatting
gofmt -l .

# 2. Go Vet (including test hooks)
go vet -tags testhooks ./...

# 3. Comprehensive Unit & Integration Tests
go test -tags testhooks -count=1 -timeout 300s ./...

# 4. Personal Data & Repository Hygiene Gate
go test -v -run TestNoPersonalDataInTree .

# 5. Static Analysis (if installed locally)
staticcheck -tags testhooks ./...

# 6. Known Vulnerability Check (if installed locally)
govulncheck ./...
```

---

## 6. Pull Request Review Process

1. **Automated CI:** Every PR triggers `.github/workflows/ci.yml` (running `go vet`, race detection, Linux/Windows cross-compilation, static analysis, and hygiene tests) alongside CodeQL checks.
2. **Proportional Review by Risk:** Maintainers calibrate review rigor to the real impact of a change rather than blocking simple fixes behind heavy processes:

   | Risk Level | Typical Examples | Review Expectations |
   |---|---|---|
   | **Low** | Typos, documentation clarity, README updates, template cleanup | Automated CI + maintainer review |
   | **Medium** | Explorer integration, GUI UX, tooling scripts, non-consensus CLI/API updates | Automated CI + reproducible tests + technical review |
   | **High** | Consensus (`core/`), PoW (`armaxhash/`), difficulty adjustment, transaction validation, cryptography/ML-DSA-65, wallet security, P2P TLS/TOFU, security-critical CI/CD | Automated CI + rigorous/adversarial tests + independent technical/security review for Mainnet readiness ([`docs/MAINNET_READINESS.md`](docs/MAINNET_READINESS.md)) |

   *(Risk classification is primarily a maintainer responsibility; contributors may share a note if helpful without filling out extra governance forms. Self-review is never represented as independent security review.)*
3. **Review Criteria (Code PRs):** Correctness, deterministic behavior, negative-path/edge-case test coverage, absence of data races/resource leaks, and preservation of security boundaries.
4. **Maintainer Merge:** Maintainers merge PRs once CI and proportional review checks pass.

---

## 7. Developer Certificate of Origin (DCO) & Licensing

All contributions to Armaxcoin are submitted under the terms of the project's [MIT License](LICENSE). By submitting a pull request or patch, you certify that you have the legal right to submit the contribution under this license, conforming to the Developer Certificate of Origin (DCO):

```text
Developer Certificate of Origin
Version 1.1

By making a contribution to this project, I certify that:

(a) The contribution was created in whole or in part by me and I
    have the right to submit it under the open source license
    indicated in the file; or

(b) The contribution is based upon previous work that, to the best
    of my knowledge, is covered under an appropriate open source
    license and I have the right under that license to submit that
    work with modifications, whether created in whole or in part
    by me, under the same open source license; or

(c) The contribution was provided directly to me by some other
    person who certified (a), (b) or (c) and I have not modified
    it.

(d) I understand and agree that this project and the contribution
    are public and that a record of the contribution (including all
    personal information I submit with it, including my sign-off) is
    maintained indefinitely and may be redistributed consistent with
    this project or the open source license(s) involved.
```

Please add a `Signed-off-by:` trailer to each commit message (e.g. `git commit -s`).

---

## 8. Contributor Recognition

Contributions to Armaxcoin are entirely voluntary. Contributors are credited in [docs/community/CONTRIBUTORS.md](docs/community/CONTRIBUTORS.md), GitHub contributor history, official release notes, and public community announcements where appropriate.

> [!IMPORTANT]
> **No Financial or Token Compensation:**
> Armaxcoin is an experimental Layer-1 open-source protocol on public testnet (`ARMX-TESTNET-1`). Testnet coins (`tARMX`) have no current market value and are not an investment offer. While verified external testnet mining rewards are planned for a preliminary Mainnet migration program (final ratio and eligibility terms TBA), code contributions themselves remain voluntary with no guaranteed tokens, airdrops, or financial bounties.

