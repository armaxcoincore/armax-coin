# Mainnet Treasury Subsidy Split — Dual-Coinbase Consensus Specification (Proposed — Not Implemented)

**Status: PROPOSED SPECIFICATION — NOT IMPLEMENTED (`core.MainnetLaunched = false`).** This document defines the exact consensus contract for the preliminary `80% / 20%` Mainnet subsidy split described in [`../../governance/TREASURY_MIGRATION_PLAN.md`](../../governance/TREASURY_MIGRATION_PLAN.md) (`v1.3`). Current source still rejects every `SYSTEM` transaction at block index `>= 1`; nothing here is active on any network until it is implemented, independently reviewed, and ratified.

> [!IMPORTANT]
> `ARMX-TESTNET-1` is **not** affected: the split stays disabled (`-1`), Testnet blocks, transaction hashes, Genesis, and addresses (`armx1…`) remain byte-identical to `v1.0.12`, and active Testnet nodes stay rolling-compatible.

## 1. Scope and Parameters

| Parameter | Value | Location (planned) |
|---|---|---|
| Base subsidy | `60 ARMX` (`InitialReward`, halving every `1,050,000` blocks) | `core/blockchain.go` (unchanged) |
| Treasury share | `treasuryShare = (allowedBlockReward * 20) / 100` (integer `int64`) | `core/state.go` |
| Miner base share | `minerBaseShare = allowedBlockReward - treasuryShare` | `core/state.go` |
| Split window | Blocks `#1..TreasurySplitEndHeight` | `core/network.go` |
| `TreasurySplitEndHeight` | Mainnet `108000`; Regtest short local window (for example `200`); Testnet & Soak `-1` (disabled) | `NetworkProfile` |
| `TreasuryAddress` | Mainnet: published `ax1…` `M-of-N` ML-DSA-65 multisig address; Regtest: test-only fixture multisig | `NetworkProfile` |
| Active predicate | `IsTreasurySplitActive(h) = TreasurySplitEndHeight >= 1 && 1 <= h && h <= TreasurySplitEndHeight` (always `false` at `h = 0`) | `core/network.go` |

With `allowedBlockReward = 6,000,000,000` Armoids (`60 ARMX`), `treasuryShare = 1,200,000,000` (`12 ARMX`) and `minerBaseShare = 4,800,000,000` (`48 ARMX`). No halving occurs inside `#1..#108,000`, so `108,000 × 12 = 1,296,000 ARMX` exactly.

### 1.1 Activation Policy

Mainnet has no chain yet, so the rule applies from genesis: **no activation block and no hard fork are required**. This mirrors `MainnetProfile.BlockBinarySizeActivationHeight = 0`. Enabling the split on `ARMX-TESTNET-1` would be a coordinated hard fork with a new activation height ahead of the tip and requires separate explicit authorization.

### 1.2 Address Prefix (Per-Network HRP)

| Network | Bech32 HRP | Zero-payload (burn) address |
|---|---|---|
| `ARMX-MAINNET-1` | `ax` | `ax1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqyum580` |
| `ARMX-TESTNET-1` | `armx` (unchanged; bound into Testnet transaction hashes and Genesis) | `armx1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqplwcdd` |
| `ARMX-REGTEST-1` | `axrt` | `axrt1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqa862k7` |
| `ARMX-SOAK-1` | `axsk` | `axsk1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqpactgh` |

`TreasuryAddress` must use the HRP of its network. Address validation rejects any address whose HRP does not match the active network.

## 2. Block Layout While the Split Is Active

When `IsTreasurySplitActive(b.Index)` is `true`, a valid block has `len(b.Transactions) >= 2` and:

| Index | Role | Required fields |
|---|---|---|
| `tx[0]` | Miner coinbase | `Sender == "SYSTEM"` (exact), `Signature == "REWARD_SIG"`, `Fee == 0`, `SenderPub == ""`, `Nonce == int64(b.Index)`, `SigAlg ∈ {0, 0x01}`, `Type ∈ {"", "STANDARD"}`, no HTLC/extended fields, `0 <= Amount <= minerBaseShare + totalFees`, valid non-`SYSTEM` recipient |
| `tx[1]` | Treasury coinbase | `Sender == "SYSTEM"` (exact), `Recipient == TreasuryAddress` (canonical comparison), `Signature == "TREASURY_REWARD_SIG"`, `Amount == treasuryShare` (exact, never less), `Fee == 0`, `SenderPub == ""`, `Nonce == int64(b.Index)`, `SigAlg ∈ {0, 0x01}`, `Type ∈ {"", "STANDARD"}`, no HTLC/extended fields |
| `tx[2..]` | User transactions | Existing rules; any `SYSTEM` sender or recipient is rejected |

- `totalFees` is the sum of `Fee` over `tx[2..]`; `100%` of fees go to the miner through `tx[0]`.
- A miner may under-claim `tx[0]` (existing behavior); `tx[1]` may not be under- or over-paid.
- `tx[0]` and `tx[1]` share `Nonce == int64(b.Index)` but differ in `Recipient`/`Amount`/`Signature`, so their transaction hashes differ even when the miner's address equals `TreasuryAddress`.

When the split is **not** active (Mainnet `#0`, Mainnet `#108,001+`, all of Testnet and Soak), the current rule is unchanged: only `tx[0]` may be `SYSTEM`, and any `SYSTEM` transaction at index `>= 1` is rejected.

## 3. Maturity, Finality, and Supply Accounting

- **Maturity:** `tx[1]` is recorded as an immature reward exactly like `tx[0]` and becomes spendable after `CoinbaseMaturity` (`100` on Mainnet, `1` on Regtest).
- **Operational finality (policy, not consensus):** Treasury disbursements, including staged migration tranches, use only Treasury coinbases with `>= 720` confirmations (`MaxReorganizationDepth`, `~24` hours).
- **Supply accounting:** the minted subsidy of a block is `max(0, tx[0].Amount - totalFees)` plus `tx[1].Amount` when the split is active, capped at `allowedBlockReward`. Incremental supply tracking and full state rebuilds must use the same formula so circulating supply, the reward schedule, and the `MaxSupply` cap stay consistent across apply, rebuild, and reorg.
- **Reorgs:** both coinbases are dropped (never returned to the mempool) when a block is disconnected; behavior must be identical across the `#108,000 -> #108,001` boundary.

## 4. Block Capacity and Size

- `MaxBlockTransactions = 1000` includes both coinbases, so at most `998` user transactions fit while the split is active (`999` otherwise).
- `tx[1]` is counted in `TxBinaryV1` block size accounting like any other transaction; Mainnet uses `TxBinaryV1` sizing from genesis.

## 5. Mining Interfaces

### 5.1 `/getblocktemplate`

Existing fields keep their names and meaning for Testnet. `coinbasevalue` always means the maximum value of `tx[0]` (`minerBaseShare + totalFees` while the split is active). Additive fields:

| Field | Type | Meaning |
|---|---|---|
| `treasury_active` | bool | `IsTreasurySplitActive(height)` |
| `treasury_address` | string | `TreasuryAddress` (empty when inactive) |
| `treasury_value` | int64 Armoids | `treasuryShare` (`0` when inactive) |

When `treasury_active` is `true`, the block assembler must place `tx[0]`, then `tx[1]`, then the template transactions, and the Merkle root must cover all of them in that order.

### 5.2 Miners and Pools

`armminer`, the GUI miner, and `/mine` add `tx[1]` whenever `treasury_active` is `true`. External Stratum pool software must do the same; a pool that omits `tx[1]` produces invalid Mainnet blocks during `#1..#108,000`. This is documented for operators in [`../../MINING_GUIDE.md`](../../MINING_GUIDE.md) once implemented.

## 6. Test Requirements

- Boundary heights `#0`, `#1`, `#108,000`, `#108,001` (and the Regtest equivalents).
- Reject: missing `tx[1]`, wrong amount, wrong recipient, wrong sentinel, non-zero fee, non-empty `SenderPub`, wrong nonce, invalid `SigAlg`/`Type`/HTLC fields, `SYSTEM` at index `>= 2`, and `tx[1]` on Testnet/Soak.
- Supply parity: incremental supply equals full rebuild supply after mining and after a reorg across `#108,000 -> #108,001`.
- `b.Miner == TreasuryAddress` edge case.
- Testnet regression: Genesis hash, transaction hashes, and Merkle roots identical to `v1.0.12`.
