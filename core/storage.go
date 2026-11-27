package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/armaxcoincore/armax-coin/core/pqsig"
	bolt "go.etcd.io/bbolt"
	bboltErrors "go.etcd.io/bbolt/errors"
)

const DBFileName = "blockchain.db"

var (
	dbPathMu     sync.RWMutex
	customDBPath string
)

// SetDBPath explicitly sets a custom database file location
func SetDBPath(path string) {
	dbPathMu.Lock()
	customDBPath = path
	dbPathMu.Unlock()
}

// GetDBPath determines the absolute path for blockchain.db.
// Network profiles are strictly isolated into respective subfolders:
// mainnet -> <datadir>/mainnet/
// testnet -> <datadir>/testnet/
// regtest -> <datadir>/regtest/
// soak    -> <datadir>/soak/
func GetDBPath() string {
	dbPathMu.RLock()
	cPath := customDBPath
	dbPathMu.RUnlock()
	if cPath != "" {
		return cPath
	}

	netName := GetActiveNetwork().Name
	subDir := netName
	if subDir == "" {
		subDir = "mainnet"
	}

	// 1. Check environment variable override
	if envDir := os.Getenv("ARMX_DATA_DIR"); envDir != "" {
		targetDir := envDir
		if filepath.Base(envDir) != subDir {
			targetDir = filepath.Join(envDir, subDir)
		}
		_ = os.MkdirAll(targetDir, 0755)
		targetFile := filepath.Join(targetDir, DBFileName)

		// Check for legacy unisolated database in root data directory without mutating it
		if targetDir != envDir {
			legacyFile := filepath.Join(envDir, DBFileName)
			if _, err := os.Stat(legacyFile); err == nil {
				if _, errT := os.Stat(targetFile); os.IsNotExist(errT) {
					fmt.Printf("[!] Notice: Found legacy database at %s. Network '%s' now uses isolated path %s. The legacy file was left intact. Please copy or move it manually if you wish to reuse it.\n", legacyFile, netName, targetFile)
				}
			}
		}
		return targetFile
	}

	// 2. Check if blockchain.db already exists in network subfolder next to executable
	if execPath, err := os.Executable(); err == nil {
		execDir := filepath.Dir(execPath)
		targetDir := filepath.Join(execDir, subDir)
		_ = os.MkdirAll(targetDir, 0755)
		candidate := filepath.Join(targetDir, DBFileName)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		legacyCandidate := filepath.Join(execDir, DBFileName)
		if _, err := os.Stat(legacyCandidate); err == nil {
			fmt.Printf("[!] Notice: Found legacy database at %s. Network '%s' now uses isolated path %s. The legacy file was left intact. Please copy or move it manually if you wish to reuse it.\n", legacyCandidate, netName, candidate)
		}
	}

	// 3. Check if blockchain.db exists in current working directory under subDir
	baseDir := subDir
	_ = os.MkdirAll(baseDir, 0755)
	candidate := filepath.Join(baseDir, DBFileName)
	if _, err := os.Stat(candidate); err == nil {
		if abs, err := filepath.Abs(candidate); err == nil {
			return abs
		}
		return candidate
	}
	if _, err := os.Stat(DBFileName); err == nil {
		fmt.Printf("[!] Notice: Found legacy database at ./%s. Network '%s' now uses isolated path %s. The legacy file was left intact. Please copy or move it manually if you wish to reuse it.\n", DBFileName, netName, candidate)
	}

	// 4. Default: Save next to executable in subDir
	if execPath, err := os.Executable(); err == nil {
		execDir := filepath.Dir(execPath)
		targetDir := filepath.Join(execDir, subDir)
		_ = os.MkdirAll(targetDir, 0755)
		return filepath.Join(targetDir, DBFileName)
	}

	return candidate
}

// BackupAndResetDatabase safely archives the active blockchain database file into a timestamped backup
// and removes the active file, allowing clean resynchronization from Genesis [Phase 4].
func BackupAndResetDatabase() (string, error) {
	dbPath := GetDBPath()
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return "", nil // No database file exists
	}
	backupPath := fmt.Sprintf("%s.bak.%d", dbPath, time.Now().Unix())
	if err := os.Rename(dbPath, backupPath); err != nil {
		return "", fmt.Errorf("failed to backup existing database file '%s' to '%s': %w", dbPath, backupPath, err)
	}
	return backupPath, nil
}

// SaveToFile safely persists the blockchain to bbolt store [C-25]. It re-verifies every height
// and is used where a full consistency pass is wanted (startup, shutdown).
func (bc *Blockchain) SaveToFile() error {
	return bc.saveToFileFrom(0)
}

// saveToFileFrom persists heights >= startHeight. Heights below it must be unchanged since the last
// save (ResolveFork guarantees this for everything at or below the fork ancestor). If the store does
// not already hold a matching block and chainwork at startHeight-1, it falls back to a full pass.
func (bc *Blockchain) saveToFileFrom(startHeight int) error {
	db, err := bc.getStoreDB()
	if err != nil {
		return fmt.Errorf("failed to get bbolt store: %w", err)
	}

	return db.Update(func(tx *bolt.Tx) error {
		bktB := tx.Bucket(bktBlocks)
		bktH := tx.Bucket(bktHashIndex)
		bktT := tx.Bucket(bktTxIndex)
		bktW := tx.Bucket(bktChainwork)
		bktS := tx.Bucket(bktSnapshots)
		bktU := tx.Bucket(bktUndo)
		bktM := tx.Bucket(bktMeta)

		runningWork := big.NewInt(0)
		two256 := new(big.Int).Lsh(big.NewInt(1), 256)
		reorgStartHeight := -1

		if startHeight > len(bc.Chain) {
			startHeight = 0
		}
		if startHeight > 0 {
			prev := &bc.Chain[startHeight-1]
			pBytes := uint64ToBytes(uint64(prev.Index))
			storedWork := bktW.Get(pBytes)
			storedIdx := bktH.Get([]byte(prev.Hash))
			if len(storedWork) == 0 || !bytes.Equal(storedIdx, pBytes) {
				startHeight = 0
			} else {
				runningWork.SetBytes(storedWork)
			}
		}

		// Two-phase store update: delete all overridden/pruned block hashes and tx index entries
		// BEFORE writing new blocks/tx indices so transactions that shift to an earlier height
		// during reorganization are never deleted from bktTxIndex by later height cleanup.
		needWrite := make([]bool, len(bc.Chain)-startHeight)
		for i := startHeight; i < len(bc.Chain); i++ {
			b := &bc.Chain[i]
			hBytes := uint64ToBytes(uint64(b.Index))

			existing := bktB.Get(hBytes)
			if existing == nil {
				needWrite[i-startHeight] = true
				continue
			}
			var oldB Block
			if err := unmarshalRecord(existing, &oldB); err != nil || oldB.Hash != b.Hash {
				needWrite[i-startHeight] = true
				if reorgStartHeight == -1 {
					reorgStartHeight = b.Index
				}
				if oldB.Hash != "" {
					_ = bktH.Delete([]byte(oldB.Hash))
				}
				for _, oldTx := range oldB.Transactions {
					_ = bktT.Delete([]byte(oldTx.HashHex()))
				}
				if bktS != nil {
					_ = bktS.Delete(hBytes)
				}
				if bktU != nil {
					_ = bktU.Delete(hBytes)
				}
			}
		}

		if bktM != nil && len(bc.Chain) > 0 {
			// Prune any orphaned heights above the current chain tip before re-indexing newTip blocks
			newTip := uint64(bc.Chain[len(bc.Chain)-1].Index)
			if oldTipBytes := bktM.Get(keyTipHeight); oldTipBytes != nil {
				oldTip := bytesToUint64(oldTipBytes)
				for h := newTip + 1; h <= oldTip; h++ {
					hBytes := uint64ToBytes(h)
					if oldData := bktB.Get(hBytes); oldData != nil {
						var oldB Block
						if unmarshalRecord(oldData, &oldB) == nil {
							if oldB.Hash != "" {
								_ = bktH.Delete([]byte(oldB.Hash))
							}
							for _, oldTx := range oldB.Transactions {
								_ = bktT.Delete([]byte(oldTx.HashHex()))
							}
						}
						_ = bktB.Delete(hBytes)
					}
					_ = bktW.Delete(hBytes)
					if bktS != nil {
						_ = bktS.Delete(hBytes)
					}
					if bktU != nil {
						_ = bktU.Delete(hBytes)
					}
				}
			}
		}

		for i := startHeight; i < len(bc.Chain); i++ {
			b := &bc.Chain[i]
			hBytes := uint64ToBytes(uint64(b.Index))

			target := b.GetTarget()
			if target == nil || target.Sign() <= 0 {
				target = DifficultyToTarget(1)
			}
			divisor := new(big.Int).Add(target, big.NewInt(1))
			work := new(big.Int).Div(two256, divisor)
			runningWork.Add(runningWork, work)

			if needWrite[i-startHeight] {
				bData, err := json.Marshal(b)
				if err != nil {
					return err
				}
				if err := bktB.Put(hBytes, encodeRecord(bData)); err != nil {
					return err
				}
				if err := bktH.Put([]byte(b.Hash), hBytes); err != nil {
					return err
				}
				for txIdx, t := range b.Transactions {
					loc := encodeTxLocation(uint64(b.Index), uint64(txIdx))
					if err := bktT.Put([]byte(t.HashHex()), loc); err != nil {
						return err
					}
				}
				if err := bktW.Put(hBytes, append([]byte(nil), runningWork.Bytes()...)); err != nil {
					return err
				}
				if bktU != nil {
					bc.undoLock.RLock()
					if bc.undoHistory != nil {
						if u, ok := bc.undoHistory[b.Index]; ok && u != nil {
							if uData, err := json.Marshal(u); err == nil {
								_ = bktU.Put(hBytes, encodeRecord(uData))
							}
						}
					}
					bc.undoLock.RUnlock()
				}
			}

			// Only persist bc.State snapshot at the exact chain tip height (bc.State represents tip state)
			if i == len(bc.Chain)-1 && b.Index%SnapshotInterval == 0 && bc.State != nil && bktS != nil {
				sData, err := json.Marshal(bc.State)
				if err != nil {
					return fmt.Errorf("failed to marshal state snapshot: %w", err)
				}
				if err := bktS.Put(hBytes, encodeRecord(sData)); err != nil {
					return err
				}
			} else if reorgStartHeight != -1 && b.Index >= reorgStartHeight && i != len(bc.Chain)-1 && bktS != nil {
				_ = bktS.Delete(hBytes)
			}
		}

		if bktM != nil {
			if len(bc.Chain) > 0 {
				newTip := uint64(bc.Chain[len(bc.Chain)-1].Index)
				tipBytes := uint64ToBytes(newTip)
				_ = bktM.Put(keyTipHeight, tipBytes)
			}
			_ = bktM.Put(keySchemaVersion, uint64ToBytes(CurrentSchemaVersion))
		}
		return nil
	})
}

// LoadFromFile loads the blockchain from the persistent bbolt store and validates integrity [C-14, C-23, C-25, P2-6]
func LoadFromFile() (*Blockchain, bool) {
	bc, found, err := loadBlockchainFromPath(GetDBPath())
	if err != nil {
		fmt.Printf("[-] WARNING: Could not load blockchain: %v\n", err)
	}
	return bc, found
}

func loadBlockchainFromPath(dbPath string) (*Blockchain, bool, error) {

	// If DB file doesn't exist, return false to trigger genesis initialization
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("stat database %q: %w", dbPath, err)
	}

	db, err := OpenStoreReadOnly(dbPath)
	if err != nil {
		// Quarantine only when bbolt identifies the file as an invalid database.
		// Other errors (for example, permissions or a lock timeout) should not
		// cause an otherwise valid database to be renamed.
		if errors.Is(err, bboltErrors.ErrInvalid) {
			if quarantineErr := MarkDBInvalid(dbPath); quarantineErr != nil {
				return nil, false, fmt.Errorf(
					"open database %q: %w (failed to quarantine database: %v)",
					dbPath, err, quarantineErr,
				)
			}
		}

		return nil, false, fmt.Errorf("open database %q: %w", dbPath, err)
	}

	var tipHeight uint64
	var blocks []Block

	err = db.View(func(tx *bolt.Tx) error {
		bktM := tx.Bucket(bktMeta)
		if bktM == nil {
			return fmt.Errorf("meta bucket missing")
		}

		// Enforce schema version: reject legacy ECDSA or outdated databases [P2-6]
		verBytes := bktM.Get(keySchemaVersion)
		if verBytes == nil || bytesToUint64(verBytes) < CurrentSchemaVersion {
			var oldVer uint64
			if verBytes != nil {
				oldVer = bytesToUint64(verBytes)
			}
			return fmt.Errorf("incompatible database schema version: got v%d, required v%d (legacy database rejected)", oldVer, CurrentSchemaVersion)
		}

		tipBytes := bktM.Get(keyTipHeight)
		if tipBytes == nil {
			return fmt.Errorf("tip height missing")
		}
		tipHeight = bytesToUint64(tipBytes)

		bktB := tx.Bucket(bktBlocks)
		if bktB == nil {
			return fmt.Errorf("blocks bucket missing")
		}

		blocks = make([]Block, 0, tipHeight+1)
		for h := uint64(0); h <= tipHeight; h++ {
			hBytes := uint64ToBytes(h)
			bData := bktB.Get(hBytes)
			if bData == nil {
				return fmt.Errorf("block at height %d missing", h)
			}
			var b Block
			if err := unmarshalRecord(bData, &b); err != nil {
				return fmt.Errorf("failed to unmarshal block %d: %w", h, err)
			}
			blocks = append(blocks, b)
		}
		return nil
	})

	if err != nil || len(blocks) == 0 {
		_ = db.Close()

		if err != nil {
			if strings.Contains(err.Error(), "incompatible database schema version") {
				if quarantineErr := MarkDBInvalid(dbPath); quarantineErr != nil {
					return nil, false, fmt.Errorf(
						"read blockchain from %q: %w; quarantine failed: %v",
						dbPath,
						err,
						quarantineErr,
					)
				}
			}

			return nil, false, fmt.Errorf(
				"read blockchain from %q: %w",
				dbPath,
				err,
			)
		}

		return nil, false, fmt.Errorf(
			"blockchain database %q contains no blocks",
			dbPath,
		)
	}

	// [C-23] Strictly validate Genesis block against active network rules
	if err := ValidateGenesisBlock(&blocks[0]); err != nil {
		_ = db.Close()
		gen := GetGenesisBlock()
		return nil, false, fmt.Errorf("stored database genesis mismatch: database genesis hash %s does not match active network %s genesis hash %s: %w (source database left unmodified)", blocks[0].Hash, GetActiveNetwork().Name, gen.Hash, err)
	}
	blocks[0] = GetGenesisBlock()

	// Locate nearest state snapshot <= tipHeight [C-25]
	var snapshotHeight uint64 = 0
	var stateMgr *StateManager

	_ = db.View(func(tx *bolt.Tx) error {
		bktS := tx.Bucket(bktSnapshots)
		if bktS == nil {
			return nil
		}
		// Check from highest potential snapshot down
		nearestSnap := (tipHeight / SnapshotInterval) * SnapshotInterval
		for s := nearestSnap; ; s -= SnapshotInterval {
			sBytes := uint64ToBytes(s)
			sData := bktS.Get(sBytes)
			if sData != nil {
				var sm StateManager
				if err := unmarshalRecord(sData, &sm); err == nil {
					snapshotHeight = s
					stateMgr = &sm
					break
				}
			}
			if s < SnapshotInterval {
				break
			}
		}
		return nil
	})
	_ = db.Close()

	mat := GetActiveNetwork().CoinbaseMaturity
	if mat <= 0 {
		mat = CoinbaseMaturity
	}

	// Initialize state & validate continuity/snapshot integrity
	if stateMgr != nil {
		var continuityErr error
		var snapSupply int64 = 0
		var snapBalanceSum int64 = 0
		expectedAccounts := make(map[string]AccountState, len(stateMgr.Accounts))

		for h := uint64(0); h <= snapshotHeight; h++ {
			b := &blocks[h]
			if h > 0 {
				if err := validateBlockContext(b, &blocks[h-1], blocks[:h], true); err != nil {
					continuityErr = fmt.Errorf("block #%d linkage validation failed: %w", h, err)
					break
				}
			}

			maxReward := CalculateBlockRewardGivenSupply(int(h), snapSupply)
			netDelta, accErr := applyAndVerifyPreSnapshotBlock(b, expectedAccounts, maxReward, mat)
			if accErr != nil {
				continuityErr = accErr
				break
			}
			minted := BlockMintedSubsidy(b, maxReward)
			snapSupply += minted
			if snapSupply > MaxSupply {
				snapSupply = MaxSupply
			}
			snapBalanceSum += netDelta
		}
		if continuityErr != nil {
			_ = MarkDBInvalid(dbPath)
			return nil, false, fmt.Errorf("stored database continuity check failed before snapshot #%d: %w", snapshotHeight, continuityErr)
		}

		if stateMgr.Accounts == nil {
			stateMgr.Accounts = make(map[string]AccountState)
		}
		stateMgr.CoinbaseMaturity = mat

		if snapErr := verifyLoadedSnapshotIntegrity(stateMgr, expectedAccounts, snapBalanceSum, int(snapshotHeight), mat); snapErr != nil {
			fmt.Printf("[-] WARNING: State snapshot at height #%d failed integrity check (%v); rebuilding state from Genesis\n", snapshotHeight, snapErr)
			stateMgr = nil
			snapshotHeight = 0
		}
	}

	if stateMgr == nil {
		stateMgr = NewStateManager()
		stateMgr.CoinbaseMaturity = mat
		if err := stateMgr.VerifyAndApplyBlock(&blocks[0], InitialReward, mat); err != nil {
			_ = MarkDBInvalid(dbPath)
			return nil, false, fmt.Errorf("genesis state application failed: %w", err)
		}
		snapshotHeight = 0
	}

	// Calculate running supply up to snapshotHeight using actual minted subsidy [HIGH-04]
	var runningSupply int64 = 0
	for h := uint64(0); h <= snapshotHeight; h++ {
		b := &blocks[h]
		maxReward := CalculateBlockRewardGivenSupply(int(h), runningSupply)
		minted := BlockMintedSubsidy(b, maxReward)
		runningSupply += minted
		if runningSupply > MaxSupply {
			runningSupply = MaxSupply
		}
	}

	// Replay and verify blocks from snapshotHeight + 1 up to tipHeight [C-25]
	for h := snapshotHeight + 1; h <= tipHeight; h++ {
		curr := &blocks[h]
		prev := &blocks[h-1]

		if err := validateBlockContext(curr, prev, blocks[:h], true); err != nil {
			MarkDBInvalid(dbPath)
			return nil, false, fmt.Errorf("block #%d linkage validation failed: %w", h, err)
		}

		reward := CalculateBlockRewardGivenSupply(curr.Index, runningSupply)
		if err := stateMgr.VerifyAndApplyBlock(curr, reward, mat); err != nil {
			MarkDBInvalid(dbPath)
			return nil, false, fmt.Errorf("block #%d state execution failed: %w", h, err)
		}
		minted := BlockMintedSubsidy(curr, reward)
		runningSupply += minted
		if runningSupply > MaxSupply {
			runningSupply = MaxSupply
		}
	}

	var totalLoadedTx int64 = 0
	for _, b := range blocks {
		totalLoadedTx += int64(len(b.Transactions))
	}

	bc := &Blockchain{
		Chain:            blocks,
		Difficulty:       TargetToDifficulty(blocks[len(blocks)-1].GetTarget()),
		Mempool:          []Transaction{},
		BlockReward:      CalculateBlockRewardGivenSupply(int(tipHeight+1), runningSupply),
		State:            stateMgr,
		StoragePath:      dbPath,
		undoHistory:      make(map[int]*BlockUndoRecord),
		cachedSupply:     runningSupply,
		cachedTotalTx:    totalLoadedTx,
		cachedWork:       CalculateChainwork(blocks),
		CoinbaseMaturity: mat,
	}

	fmt.Printf("[OK] Blockchain database loaded and verified from: %s (%d blocks)\n", dbPath, len(bc.Chain))
	return bc, true, nil
}

// applyAndVerifyPreSnapshotBlock deterministically replays block accounting (balances, nonces, and
// immature reward queues) and verifies transaction signatures and coinbase invariants so snapshot loading
// validates full block + state integrity across blocks[0..snapshotHeight].
func applyAndVerifyPreSnapshotBlock(b *Block, expected map[string]AccountState, allowedReward int64, maturity int) (int64, error) {
	if b == nil || len(b.Transactions) == 0 {
		return 0, fmt.Errorf("block has no transactions")
	}

	firstUserTx := FirstUserTxIndex(b.Index)
	if len(b.Transactions) < firstUserTx {
		return 0, fmt.Errorf("block #%d is missing the tx[1] treasury coinbase", b.Index)
	}
	var blockFees int64 = 0
	for idx := firstUserTx; idx < len(b.Transactions); idx++ {
		tx := b.Transactions[idx]
		if strings.EqualFold(tx.Sender, "SYSTEM") || strings.EqualFold(tx.Recipient, "SYSTEM") {
			return 0, fmt.Errorf("block #%d tx #%d invalid SYSTEM sender/recipient", b.Index, idx)
		}
		if tx.Amount <= 0 || tx.Amount > MaxSupply || tx.Fee < 0 || tx.Fee > MaxSupply || tx.Amount > math.MaxInt64-tx.Fee {
			return 0, fmt.Errorf("block #%d tx #%d amount/fee out of bounds", b.Index, idx)
		}
		senderHex, err := AddressToHexCached(tx.Sender)
		if err != nil {
			return 0, fmt.Errorf("block #%d tx #%d invalid sender address: %w", b.Index, idx, err)
		}
		recipHex, err := AddressToHexCached(tx.Recipient)
		if err != nil {
			return 0, fmt.Errorf("block #%d tx #%d invalid recipient address: %w", b.Index, idx, err)
		}
		if strings.EqualFold(senderHex, recipHex) {
			return 0, fmt.Errorf("block #%d tx #%d self-transfer rejected", b.Index, idx)
		}
		if err := checkMultisigActivation(&tx, b.Index); err != nil {
			return 0, fmt.Errorf("block #%d tx #%d %v", b.Index, idx, err)
		}
		if !tx.Verify() {
			return 0, fmt.Errorf("block #%d tx #%d cryptographic signature verification failed", b.Index, idx)
		}

		senderAcc := cloneAccountState(expected[senderHex])
		if tx.Nonce != senderAcc.Nonce+1 {
			return 0, fmt.Errorf("block #%d tx #%d nonce mismatch: expected %d, got %d", b.Index, idx, senderAcc.Nonce+1, tx.Nonce)
		}
		totalCost := tx.Amount + tx.Fee
		if senderAcc.Spendable(b.Index, maturity) < totalCost {
			return 0, fmt.Errorf("block #%d tx #%d insufficient spendable balance", b.Index, idx)
		}
		if blockFees > MaxSupply-tx.Fee {
			return 0, fmt.Errorf("block #%d fee sum out of bounds", b.Index)
		}
		blockFees += tx.Fee

		senderAcc.Balance -= totalCost
		senderAcc.Nonce = tx.Nonce
		senderAcc = senderAcc.PruneMatured(b.Index, maturity)
		expected[senderHex] = senderAcc

		recipAcc := cloneAccountState(expected[recipHex])
		if recipAcc.Balance > MaxSupply-tx.Amount {
			return 0, fmt.Errorf("block #%d tx #%d recipient balance exceeds MaxSupply", b.Index, idx)
		}
		recipAcc.Balance += tx.Amount
		expected[recipHex] = recipAcc
	}

	cb := b.Transactions[0]
	if cb.Sender != "SYSTEM" || cb.Fee != 0 || cb.Nonce != int64(b.Index) || cb.SenderPub != "" ||
		(cb.Type != "" && cb.Type != TxTypeStandard) || cb.Signature != "REWARD_SIG" || cb.Amount < 0 {
		return 0, fmt.Errorf("block #%d invalid coinbase fields", b.Index)
	}
	if isStrictCoinbaseConsensusActive(NetworkID()) {
		if (cb.SigAlg != 0 && cb.SigAlg != pqsig.SigAlgMLDSA65) || cb.HashLock != "" || cb.TimeLock != 0 || cb.Preimage != "" {
			return 0, fmt.Errorf("block #%d coinbase must not carry invalid sig_alg or HTLC/extended fields", b.Index)
		}
	}
	if strings.EqualFold(cb.Recipient, "SYSTEM") || !ValidateAddress(cb.Recipient) {
		return 0, fmt.Errorf("block #%d invalid coinbase recipient %q", b.Index, cb.Recipient)
	}
	var treasuryShare int64
	if firstUserTx == 2 {
		treasuryShare = TreasuryShare(allowedReward)
	}
	if allowedReward < 0 || allowedReward > math.MaxInt64-blockFees || cb.Amount > allowedReward-treasuryShare+blockFees {
		return 0, fmt.Errorf("block #%d coinbase reward exceeds allowed %d + fees %d", b.Index, allowedReward, blockFees)
	}
	minerHex, err := AddressToHexCached(cb.Recipient)
	if err != nil {
		return 0, fmt.Errorf("block #%d invalid coinbase recipient: %w", b.Index, err)
	}
	minerAcc := cloneAccountState(expected[minerHex])
	if minerAcc.Balance > MaxSupply-cb.Amount {
		return 0, fmt.Errorf("block #%d miner balance exceeds MaxSupply", b.Index)
	}
	minerAcc.Balance += cb.Amount
	if maturity > 0 && cb.Amount > 0 {
		minerAcc.Immature = append(minerAcc.Immature, ImmatureReward{
			Amount: cb.Amount,
			Height: b.Index,
		})
	}
	minerAcc = minerAcc.PruneMatured(b.Index, maturity)
	expected[minerHex] = minerAcc

	if firstUserTx == 2 {
		tt := b.Transactions[1]
		if err := validateTreasuryCoinbase(&tt, b.Index, allowedReward, ValidateAddress); err != nil {
			return 0, err
		}
		treasuryHex, err := AddressToHexCached(tt.Recipient)
		if err != nil {
			return 0, fmt.Errorf("block #%d invalid treasury coinbase recipient: %w", b.Index, err)
		}
		treasuryAcc := cloneAccountState(expected[treasuryHex])
		if treasuryAcc.Balance > MaxSupply-tt.Amount {
			return 0, fmt.Errorf("block #%d treasury balance exceeds MaxSupply", b.Index)
		}
		treasuryAcc.Balance += tt.Amount
		if maturity > 0 && tt.Amount > 0 {
			treasuryAcc.Immature = append(treasuryAcc.Immature, ImmatureReward{Amount: tt.Amount, Height: b.Index})
		}
		treasuryAcc = treasuryAcc.PruneMatured(b.Index, maturity)
		expected[treasuryHex] = treasuryAcc
		return cb.Amount - blockFees + tt.Amount, nil
	}
	return cb.Amount - blockFees, nil
}

// verifyLoadedSnapshotIntegrity checks account invariants, total balance conservation,
// and exact per-account balance, nonce, and immature-queue parity against expectedAccounts.
func verifyLoadedSnapshotIntegrity(sm *StateManager, expectedAccounts map[string]AccountState, expectedBalanceSum int64, snapshotHeight int, maturity int) error {
	if sm == nil || sm.Accounts == nil {
		return errors.New("snapshot state manager missing")
	}
	if expectedBalanceSum < 0 || expectedBalanceSum > MaxSupply {
		return fmt.Errorf("expected balance sum %d out of supply bounds", expectedBalanceSum)
	}

	var totalBalances int64 = 0
	for addrHex, acc := range sm.Accounts {
		if acc.Balance < 0 || acc.Balance > MaxSupply {
			return fmt.Errorf("account %s invalid balance %d", addrHex, acc.Balance)
		}
		if acc.Nonce < 0 {
			return fmt.Errorf("account %s negative nonce %d", addrHex, acc.Nonce)
		}
		for _, entry := range acc.Immature {
			if entry.Amount < 0 || entry.Height < 0 || entry.Height > snapshotHeight {
				return fmt.Errorf("account %s invalid immature entry (amount=%d height=%d snapshot=%d)", addrHex, entry.Amount, entry.Height, snapshotHeight)
			}
		}
		if imm := acc.ImmatureTotal(snapshotHeight, maturity); imm < 0 || imm > acc.Balance {
			return fmt.Errorf("account %s immature total %d exceeds balance %d", addrHex, imm, acc.Balance)
		}
		if acc.Balance > MaxSupply-totalBalances {
			return fmt.Errorf("snapshot balance sum overflow past MaxSupply at account %s", addrHex)
		}
		totalBalances += acc.Balance
	}

	if totalBalances != expectedBalanceSum {
		return fmt.Errorf("snapshot total balance %d mismatches expected chain balance %d", totalBalances, expectedBalanceSum)
	}

	if expectedAccounts != nil {
		for addrHex, acc := range sm.Accounts {
			expAcc, ok := expectedAccounts[addrHex]
			if !ok {
				if acc.Balance != 0 || acc.Nonce != 0 || len(acc.Immature) != 0 {
					return fmt.Errorf("snapshot contains unexpected non-zero account %s", addrHex)
				}
				continue
			}
			if acc.Balance != expAcc.Balance {
				return fmt.Errorf("snapshot account %s balance %d mismatches expected %d", addrHex, acc.Balance, expAcc.Balance)
			}
			if acc.Nonce != expAcc.Nonce {
				return fmt.Errorf("snapshot account %s nonce %d mismatches expected %d", addrHex, acc.Nonce, expAcc.Nonce)
			}
			if len(acc.Immature) != len(expAcc.Immature) {
				return fmt.Errorf("snapshot account %s immature len %d mismatches expected %d", addrHex, len(acc.Immature), len(expAcc.Immature))
			}
			for i := range acc.Immature {
				if acc.Immature[i] != expAcc.Immature[i] {
					return fmt.Errorf("snapshot account %s immature[%d] %+v mismatches expected %+v", addrHex, i, acc.Immature[i], expAcc.Immature[i])
				}
			}
		}
		for addrHex, expAcc := range expectedAccounts {
			if expAcc.Balance == 0 && expAcc.Nonce == 0 && len(expAcc.Immature) == 0 {
				continue
			}
			if _, ok := sm.Accounts[addrHex]; !ok {
				return fmt.Errorf("snapshot missing expected account %s", addrHex)
			}
		}
	}
	return nil
}
