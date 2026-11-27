# Protocol Treasury Accumulation & Open-Launch Subsidy Split Plan (`Preliminary Non-Final`)

**Status: PRELIMINARY DRAFT — NOT FINAL (Aligned with [`TREASURY_MIGRATION_PLAN.md`](../../governance/TREASURY_MIGRATION_PLAN.md) `v1.3`; Mainnet is locked [`core.MainnetLaunched = false`] and has not launched.)**

> [!IMPORTANT]
> This is a **preliminary, non-final planning document** subject to revision and separate Founder approval. Nothing here authorizes Mainnet launch, consensus changes, Treasury transfers, or token commitments.

---

## 1. Two-Stage Open-Launch Model (`Preliminary Proposal`)

To eliminate pre-public private mining while keeping Mainnet readiness isolated from real value, this preliminary plan separates staging verification from public launch:

- **Stage 1 — Separate Private Staging Testing First (`Zero Real ARMX`):** All stability, soak, adversarial, and [`MAINNET_READINESS.md`](../../MAINNET_READINESS.md) verification runs execute first on an isolated staging Network ID & genesis before `ARMX-MAINNET-1` Block `#1` is ever mined.
- **Stage 2 — Open Public Mainnet from Block `#1` with `80% / 20%` Subsidy Split (`Preliminary`, Subject to Approval):** Once Stage 1 completes, `ARMX-MAINNET-1` opens directly to public P2P miners at **Block `#1`** (`0` premine, `0` pre-public private blocks). Across **Blocks `#1` through `#108,000`**, consensus splits each `60 ARMX` block subsidy deterministically:
  - **Blocks `#1..#108,000` (`108,000` blocks):** **`48 ARMX` (`80%`) + `100%` of transaction fees to the miner** (`5,184,000 ARMX` miner subsidies), and **`12 ARMX` (`20%`, `0` fees) to the fixed protocol Treasury address** (`108,000 × 12 = 1,296,000 ARMX` exact).
  - **Blocks `#108,001..#1,049,999`:** Treasury split terminates permanently (`0 ARMX`); miners receive the full **`60 ARMX` (`100%`) + `100%` of transaction fees** until the first halving (`#1,050,000`).
  - **Emission & Duration Math (`120s` Target Block Time):** `86,400 / 120 = 720` expected blocks/day; `108,000 / 720 = 150` expected days (`~5` months); `1,296,000 ARMX` represents **`~1.03%`** (`1.029%`) of `126,000,000 ARMX` max supply and **`~8.22%`** of the `15,768,000 ARMX` theoretical Year-1 issuance (`20%` of the `6,480,000 ARMX` emitted across `#1..#108,000`; actual wall-clock duration varies under PoW and `Armax-ASERT-AW`, while the consensus cutoff is strictly Block `#108,000`).
  - **Public Mainnet Opening:** `TBA` (open directly to all public miners from Block `#1` under the `80% / 20%` subsidy split on `#1..#108,000` and `100%` miner reward from `#108,001` onward).
  - **Transparency & Staged Migration:** Receiving Treasury address, custody controls, and dual-coinbase consensus rules will be published and reviewed before launch, alongside a public read-only Explorer tracking Treasury inflows and **staged-tranche Testnet miner migration payouts** as Treasury coinbases become spendable (`CoinbaseMaturity = 100`) and reach `>= 720` confirmations (`MaxReorganizationDepth`) through Block `#108,000`. The proposed consensus contract is specified in [`mainnet-treasury-subsidy-split.md`](../consensus/mainnet-treasury-subsidy-split.md).

---

## 2. Liquidity, Allocation Ceilings & Bridge Status

- Liquidity, vesting custody, and cross-chain bridge designs are **not finalized** and will be announced with full technical details before any public Mainnet deployment.
- For the conservative non-proportional allocation ceilings under this `1,296,000 ARMX` preliminary scenario (`100k` Core Dev, `200k` Community Contributors, up to `500k` Testnet Miner Migration [`5,000,000 tARMX` / `500k` per-wallet cap, disbursed in staged tranches as Treasury coinbases mature], `200k` conditional Future Bridge/Cross-Chain Reserve, `50k` Operations, and `246k` [`18.98%`] Uncommitted Treasury Reserve), see [`TREASURY_MIGRATION_PLAN.md`](../../governance/TREASURY_MIGRATION_PLAN.md).
- No commitments regarding specific decentralized exchanges, AMM pools, lockup structures, or bridge mechanisms are finalized at this time.
- Any future cross-chain bridge or wrapped-asset contract must pass open independent security review and adversarial testing before holding real funds (formal commercial reviews remain optional as funding permits).

---

## 3. Risks & Criticism

- **Temporary Protocol Split Criticism:** Although zero blocks are mined privately before public opening, routing `20%` (`12 ARMX/block`) of block subsidies to a protocol Treasury across Blocks `#1..#108,000` (`~150` days) reduces miner base rewards from `60` to `48 ARMX` during the first ~5 months and may be criticized as a temporary protocol tax.
- **Consensus & Ecosystem Implementation Requirement:** Unlike private mining, an open-launch `80%/20%` split requires implementing, testing, and independently reviewing dual-coinbase validation (`core/`), `/getblocktemplate` (`node/`), `armminer` (`miner/`), and external Stratum pool compatibility before Mainnet launch freeze.
- **Staged Cash-Flow Dependency:** Because Treasury funds accrue gradually at `12 ARMX/block` (`~8,640 ARMX/day` at target cadence, spendable after `100`-block maturity and scheduled only after `>= 720` confirmations), Testnet migration payouts and operational budgets must be staged to match final on-chain Treasury balances rather than assuming Day-1 lump-sum availability.
- **Hashrate Response (`Armax-ASERT-AW`, `τ = 3600 s`):** The split applies from Block `#1`, so there is no sudden reward cut; difficulty bootstraps against the `48 ARMX` + fees opening reward. The only step is `+25%` (`48 -> 60 ARMX`) at Block `#108,001`; absorbing a matching `±25%` hashrate step needs about `τ · log2(1.25) ≈ 1,159 s` of cumulative schedule drift, so difficulty re-converges within hours. See [`TREASURY_MIGRATION_PLAN.md`](../../governance/TREASURY_MIGRATION_PLAN.md) Section `9`.
