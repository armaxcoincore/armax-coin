# ML-DSA-65 Multisig Custody (`sig_alg` `0x02`)

This document specifies the M-of-N multisig account type used for treasury custody. It is the
normative reference for `core/multisig.go`.

## 1. Scope and activation

| Network | `MultisigActivationHeight` | Behaviour |
|---|---|---|
| `ARMX-MAINNET-1` | `1` | Multisig spends accepted from block `#1` (Genesis carries no user transactions). |
| `ARMX-REGTEST-1` | `1` | Same as Mainnet, for local end-to-end tests. |
| `ARMX-TESTNET-1` | `-1` | Rejected. `v1.0.12` Testnet nodes cannot parse `sig_alg` `0x02`. |
| `ARMX-SOAK-1` | `-1` | Rejected. |

A value of zero or below disables multisig. A profile that leaves the field unset therefore fails
closed.

Activation is checked in three places: mempool admission (against the next height), block template
selection, and block validation (including pre-snapshot replay).

## 2. Bounds

- `2 <= M <= 3` and `M <= N <= 4`. 1-of-N custody is not accepted.
- 4-of-4 is rejected. Its spend would be about 21.1 KB, above `MaxTxSize = 20,000` bytes.
- Approximate `TxBinaryV1` sizes:

| Spend | Approximate `TxBinaryV1` size |
|---|---|
| 2-of-3 | 12.5 KB |
| 3-of-3 | 15.9 KB |
| 3-of-4 | 17.8 KB |

## 3. Descriptor and address

```
descriptor = M (1 byte) || N (1 byte) || pubkey_1 || ... || pubkey_N
address    = Bech32(network HRP, SHA-256(0x02 || descriptor))
```

The public keys are 1,952-byte ML-DSA-65 keys, distinct and in strictly ascending byte order.

The descriptor encoding is canonical: unsorted keys, duplicate keys, a wrong length, or an M or N
out of range are rejected. Each key set and threshold therefore has exactly one address.

The address prefix `0x02` separates multisig addresses from single-key addresses
(`SHA-256(0x01 || pubkey)`). A multisig address never equals any cosigner's own address. Two
thresholds over the same keys give different addresses.

## 4. Spend encoding

A spend reuses the existing transaction fields, so the JSON schema is unchanged:

| Field | Value |
|---|---|
| `sig_alg` | `2` |
| `sender_pub` | lowercase hex of the descriptor |
| `signature` | lowercase hex of the witness: `bitmap (1 byte) || M signatures` |
| `sender` | the descriptor address |

Witness rules:

- Bit `i` of the bitmap marks key `i` as a signer.
- The witness must have exactly `M` bits set, and no bit at or above `N`.
- The signatures are in ascending key order.

In `TxBinaryV1`, the descriptor and the witness are each written as a `uint16`-length-prefixed field
in place of the fixed-size key and signature. Lowercase hex is required on every network, so a
decoded spend has the same txid.

## 5. Signing and verification

Every cosigner signs the ordinary transaction hash with the `ARMX-TX-V1` ML-DSA-65 context. The hash
commits to the network ID, to `sig_alg`, and to the full descriptor. As a result, a cosigner
signature cannot be replayed as:

- a single-key spend;
- a spend from another multisig account; or
- a spend on another network.

Verification checks every signature named by the bitmap.

## 6. Size and fee

Multisig spends are always sized by `TxBinaryV1`, both for `MaxTxSize` and for the relay fee, even
on profiles that still use JSON sizing. There is no legacy JSON sizing to preserve, and the hex JSON
form is roughly twice as large.

A block sized in JSON still counts the spend's JSON size toward `MaxBlockSize`.

The `/tx/new` request body limit is `2 * MaxTxSize + 16 KiB`, so the JSON form of the largest spend
fits.

## 7. Air-gapped wallet flow (`armwallet multisig`)

1. `pubkey`: each cosigner exports its public key. The key is derived from the decrypted keystore,
   never from its unencrypted metadata.
2. `create`: builds the descriptor file (network, M, N, keys, descriptor, address).
3. `propose`: builds an unsigned spend file (network, transaction, `tx_hash`). The nonce is given
   explicitly. The fee defaults to the minimum relay fee for the exact projected size.
4. `sign`: each cosigner signs offline after reviewing a summary (network, recipient, amount, fee,
   nonce, txid).
5. `combine`: aggregates the shares. With more than `M` shares it uses the lowest key indices, so
   the result is deterministic. Every share is verified first.
6. `broadcast`: submits the final transaction. It does so only if the daemon reports the active
   network in `X-Network-ID`.

Every file records its network. A file from another network, a recipient from another network, or
a file whose transaction no longer matches its `tx_hash` is refused before signing.

**Key rotation and quorum recovery:** create a new descriptor with the replacement keys. Then spend
the whole balance of the old account to the new address, signed by the old quorum.
