# Armaxcoin Documentation

This documentation directory contains official user guides, operator manuals, specifications, and verification runbooks for **Armaxcoin** (`armax-coin`).

## 1. Getting Started & User Guides

- [Project Overview & Quickstart](../README.md): High-level introduction, key features, and architecture.
- [Public Testnet Guide](TESTNET.md): Comprehensive node setup, 1-click Desktop GUI v2 (`armgui`), building from source, wallet management, transactions, and mining.
- [Mining Guide](MINING_GUIDE.md): Solo CPU mining, thread tuning, Stratum TCP/TLS setup, and Armax-ASERT-AW difficulty details.
- [Build and Compilation Guide](BUILD.md): Step-by-step toolchain and cross-compilation instructions.
- [Development Support](../SUPPORT.md): Official developer contact and voluntary contribution channels.

## 2. Technical Specifications & Architecture

- [Consensus, Cryptography & Network Specification](CONSENSUS_AND_NETWORK.md): Armax-ASERT-AW & LWMA-1 difficulty adjustment, NIST FIPS 204 ML-DSA-65 signatures, `TxBinaryV1` binary block sizing, and ArmaxHash v2 PoW specification.
- [Consensus Improvement Proposals (C1–C6)](security/consensus-proposals.md): Design history, implementation status, and Block `#20,000` Proposal C6 specification.
- [Architecture Overview](architecture/README.md): Engineering boundaries, consensus invariants, and repository taxonomy.
- [Performance & Wire Size Benchmarks](PERF.md): Wire size metrics, `TxBinaryV1` binary block accounting (`375` user ML-DSA-65 txs / 2 MB block at `#20,000`), post-quantum signature size analysis, and mempool throughput.
- [Security Disclosure Policy](../SECURITY.md): Vulnerability reporting channels, threat boundaries, and security review process.

## 3. Operations & Maintenance

- [Node Operations Guide](NODE_OPERATIONS.md): Daemon configuration flags, JSON-RPC endpoints, `armax.conf`, cookie authentication, and systemd units.
- [Testnet Backup & Reset Runbook](operations/TESTNET_BACKUP_AND_RESET_RUNBOOK.md): Procedures for database backup, disaster recovery, and testing resets.
- [Mainnet Readiness Gates](MAINNET_READINESS.md): Security evaluation, code freeze requirements, and mainnet unlock criteria.
- [Release Engineering & QA Checklist](RELEASE.md): Versioning standards, reproducible builds, SHA-256 checksum verification, and pre-tagging QA checklist.

## 4. Release History

- [Release Notes Overview (v1.0.13)](releases/RELEASE_NOTES.md): Top-level summary of the upcoming Testnet release, P2P DoS & slot isolation hardening, consensus/storage integrity, wallet/GUI/miner security, and Go `1.27.4`.
- [v1.0.13 Release Notes](releases/v1.0.13.md): P2P DoS & slot hardening, pre-snapshot storage verification, `Option 1A` / `Option 2A-i` rolling compatibility, wallet/GUI/miner security, and Track 6 adversarial & 375-tx benchmarks.
- [v1.0.12 Release Notes](releases/v1.0.12.md): Desktop Wallet GUI v2 (`armgui` `"Waiting for Sync"` & System Tray), Node Sync Gate (`HTTP 503`), integrated multi-thread CPU optimization (`armaxhash.V2JobHasher`, padded worker slots), Stratum TLS, additive Node RPC, and Go `1.27.4`.
- [v1.0.11 Release Notes](releases/v1.0.11.md): Unified Block #20,000 Activation (`C6` + `TxBinaryV1` Block Size Accounting), Adaptive C2/C6 Miner Margin, and Bounded ASERT Drift Cache.
- [v1.0.10 Release Notes](releases/v1.0.10.md): Armax-ASERT-AW DAA Consensus Fork at Block #13,335, Surge Cap & TEDR Boost, Immutable MiningJob & 3-Tier P2P Policy.
- [v1.0.9 Release Notes](releases/v1.0.9.md): Testnet Anti-Stall TEDR Fork at Block #13,293, 5.45x Zero-Alloc CPU Miner & Linux GUI Auto-Start.
- [v1.0.8 Release Notes](releases/v1.0.8.md): Desktop GUI Security, First-Time Onboarding Flow & Peer Privacy Hardening.
- [v1.0.7 Release Notes](releases/v1.0.7.md): Testnet Consensus Fork (Block 20,000), Sync & Storage Optimization, and Subsystem Hardening.
- [v1.0.6 Release Notes](releases/v1.0.6.md): Testnet Core v1.0.6 stability, security hardening, and P2P deduplication.

## 5. Community, Contributing & Preliminary Policy

- [Contributing Guidelines](../CONTRIBUTING.md): Welcoming workflow for bug reports, docs, and code Pull Requests (no GitHub Org membership required).
- [Code of Conduct](../CODE_OF_CONDUCT.md): Community standards and respectful collaboration guidelines.
- [Contributors](community/CONTRIBUTORS.md): Recognized contributors across Git history.
- [Accessibility Statement](community/ACCESSIBILITY.md): Accessibility status and feedback channels across GUI, CLI, and docs.
- [Treasury & Testnet Miner Migration Plan (Preliminary Non-Final Plan)](governance/TREASURY_MIGRATION_PLAN.md): Draft framework under ongoing evaluation (ratio and final terms remain TBA).

---

> [!NOTE]
> Internal planning revisions, historical soak test artifacts, and developmental bridge documentation have been preserved in the private engineering archive (`armax_coin_private_archive`) and are decoupled from the public Layer-1 Core distribution.
