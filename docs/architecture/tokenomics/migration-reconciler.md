# `armreconcile` — Offline Testnet-to-Mainnet Migration Reconciler

> **Status:** tooling only. It applies whatever policy governance publishes; it does not decide
> eligibility, caps, the cutoff height, or any conversion ratio, and it cannot enable Mainnet.

`armreconcile` turns a final Testnet snapshot into a deterministic, Merkle-committed migration
manifest, and plans staged treasury tranches on the target network. Anyone with a synced node can
rerun it and must get a byte-identical manifest.

## Safety properties

- **Read-only.** Node databases are opened with bbolt `ReadOnly` and a timeout. A running node holds
  an exclusive lock, so stop the node or point the tool at a copy. The tool never broadcasts.
- **Chain integrity.** Before use, every block is checked for schema version, Genesis of the active
  network, height continuity, parent linkage, and Merkle root. Proof of work is not re-verified, so
  the manifest records `cutoff_block_hash` and `tip_hash` for comparison with your own node.
- **Finality.** A snapshot is refused unless `tip >= cutoff + 720` (`MaxReorganizationDepth`).
  Coinbase maturity is a spendability rule and is not finality.
- **Mainnet stays locked.** Planning a Mainnet tranche uses the normal `-network mainnet` profile
  selection, which fails while `MainnetLaunched = false`.
- **Domain-separated signatures.** Claims and pool ledgers are ML-DSA-65 signatures under their own
  FIPS 204 contexts (`ARMX-MIGRATION-V1`, `ARMX-POOL-LEDGER-V1`). The transaction context
  `ARMX-TX-V1` is refused for them, so an off-chain signature can never authorize a spend and a
  transaction signature can never act as a claim.
- **No local paths in output.** Errors recorded in the published manifest name only file base names.

## Inputs

### Policy (`policy.json`)

| Field | Meaning |
|---|---|
| `source_network_id` / `target_network_id` | e.g. `ARMX-TESTNET-1` → `ARMX-MAINNET-1`; must differ |
| `cutoff_height` | announced `H_cutoff`; rewards above it never count |
| `wallet_cap_armoids` | per-address cap; only the surplus is clipped |
| `program_cap_armoids` | program total; above it every entitlement is scaled pro rata |
| `excluded_addresses` | Layer 1 developer/test wallets (`0`) |
| `pending_addresses` | ownership still being verified (escrowed) |
| `faucet_addresses` | faucet wallets (`0`) |
| `burn_addresses` | burn addresses (`0`); the zero-payload burn address is always added |
| `pools[]` | pool hot wallet `address` and the `operator_public_keys` trusted to sign its ledgers |

Unknown fields are rejected, and an address may appear in only one list. All amounts are source
network Armoids. The manifest embeds the normalized policy and its SHA-256.

### Pool ledger exports (`-ledgers DIR`)

A pool ledger lists on-chain payouts from the pool hot wallet, sorted by `(height, tx_hash)`.
The operator signs the SHA-256 of its canonical JSON (`ledger-sign`). Every payout at or below the
cutoff must match a canonical transaction exactly (hash, height, sender, recipient, amount), or the
run fails. Overlapping weekly and final exports are fine if they agree. Total verified payouts of a
pool may not exceed the pool's own coinbase rewards up to the cutoff (the backing ceiling).

### Claims (`-claims DIR`)

A claim binds an eligible Testnet address to a **new** target address (`claim-sign`). The signed
message is:

```
"ARMX-MIGRATION-V1" 0x00 target_network_id 0x00 target_address 0x00 uint64_be(cutoff_height)
```

Malformed or invalid claims are listed under `rejected_claims` with a reason; they never block the
run. Identical repeated bindings count once. Different bindings for one source address put it in
`escrow_claim_conflict`.

## Accounting

Only canonical mining acquisition events count: coinbase rewards (`solo`) and verified pool
payouts (`pool`). Ordinary transfers, including unlisted pool sends and faucet payouts, are recorded
as `transfer_in_armoids` and never create entitlement.

| Category | Rule | Entitlement |
|---|---|---|
| `eligible_solo` / `eligible_pool` / `eligible_solo_pool` | unlisted address with mining events | counted |
| `pending_verification` | listed in `pending_addresses` | computed and escrowed |
| `dev_excluded` | listed in `excluded_addresses` | `0` |
| `pool_hot_wallet` | a policy pool address | `0` |
| `transfer_only_faucet` | faucet, burn, or no mining events | `0` |

1. `capped = min(solo + pool, wallet_cap)`, `excess = eligible − capped`.
2. If `Σ capped` over eligible and pending entries exceeds `program_cap`, then
   `entitlement = floor(capped × program_cap / Σ capped)` in integer math. The unassigned rounding
   remainder is reported. Pending entries reserve capacity, so resolving them never overdraws the
   program.
3. Status is `claimed`, `unclaimed`, `escrow_claim_conflict`, `escrow_pending_verification`, or `none`.

`entries_merkle_root` is the RFC 6962 Merkle root (`0x00` leaf / `0x01` node prefixes) over the
canonical JSON of each entry, sorted by source address. `manifest-verify` recomputes the policy
digest, the categories against the policy, the caps and scaling, the totals, and the root, without
needing a database.

## Staged tranches

`tranche` reads the target network database and the manifest:

- Funding counts only treasury coinbases (block `tx[1]`, `SYSTEM`, `TREASURY_REWARD_SIG`, paid to
  `-treasury`) at least 720 blocks below the tip.
- Every treasury spend already on chain, including fees, is subtracted. What a target already
  received from the treasury counts as paid.
- Each claimed target is owed `floor(Σ entitlement × target_total / program_cap)`. `target_total` is
  the governance-approved program size in target Armoids; the tool assumes no ratio.
- If the budget does not cover everything still owed, each target gets the same fraction of its
  remainder, rounded down.

The plan only proposes payments. Treasury multisig signers still create and co-sign each spend.

## Commands

```
armreconcile ledger-sign     -in ledger.json -keystore pool.json -out signed.json
armreconcile ledger-verify   -ledger signed.json -policy policy.json -db blockchain.db
armreconcile claim-sign      -keystore wallet.json -target-address ax1... -cutoff H -out claim.json
armreconcile claim-verify    -claim claim.json -cutoff H
armreconcile reconcile       -db blockchain.db -policy policy.json -ledgers DIR -claims DIR -out manifest.json
armreconcile manifest-verify -manifest manifest.json
armreconcile tranche         -network mainnet -db blockchain.db -manifest manifest.json \
                             -treasury ax1... -target-total ARMOIDS [-budget ARMOIDS] -out plan.json
```

Keystore passphrases come from `ARMX_WALLET_PASSPHRASE` or a no-echo prompt. Never share
mnemonics, keystores, or passphrases to prove ownership. Sign a claim locally instead.
