package main

import (
	"strings"

	"github.com/armaxcoincore/armax-coin/core"
	"github.com/armaxcoincore/armax-coin/wallet"
)

// FeeEstimateResponse exposes protocol-accurate fee tiers computed from core.MinRelayFee and tx size.
type FeeEstimateResponse struct {
	MinRelayFeePerKB  int64  `json:"min_relay_fee_per_kb"`
	EstimatedTxBytes  int    `json:"estimated_tx_bytes"`
	MinFeeArmoids     int64  `json:"min_fee_armoids"`
	MinFeeFormatted   string `json:"min_fee_formatted"`
	EconomyArmoids    int64  `json:"economy_armoids"`
	EconomyFormatted  string `json:"economy_formatted"`
	StandardArmoids   int64  `json:"standard_armoids"`
	StandardFormatted string `json:"standard_formatted"`
	PriorityArmoids   int64  `json:"priority_armoids"`
	PriorityFormatted string `json:"priority_formatted"`
	MempoolTxCount    int    `json:"mempool_tx_count"`
	Ticker            string `json:"ticker"`
}

func roundUpFeeStep(val, step int64) int64 {
	if step <= 0 {
		return val
	}
	return ((val + step - 1) / step) * step
}

func isBinarySizeActiveForNetwork(netID string, nextBlockHeight int) bool {
	trimmed := strings.TrimSpace(netID)
	if trimmed == "" || strings.EqualFold(trimmed, core.GetActiveNetwork().NetworkID) {
		return core.IsMempoolBinaryFeeActive(nextBlockHeight)
	}
	if prof, ok := core.ProfileForNetworkID(trimmed); ok {
		return prof.MempoolBinaryFeeActivationHeight >= 0 && int64(nextBlockHeight) >= prof.MempoolBinaryFeeActivationHeight
	}
	return core.IsMempoolBinaryFeeActive(nextBlockHeight)
}

// ComputeGUIFeeEstimateAtHeight calculates guaranteed-valid Economy/Standard/Priority fees
// for nextBlockHeight using canonical TxBinaryV1 weight when active or legacy JSON size prior to activation [HIGH-T4-02].
func ComputeGUIFeeEstimateAtHeight(w *wallet.Wallet, recipient string, amount int64, mempoolCount int, netID string, nextBlockHeight int) FeeEstimateResponse {
	binaryActive := isBinarySizeActiveForNetwork(netID, nextBlockHeight)
	estBytes := 10800
	if binaryActive {
		estBytes = 5320
	}
	minFee := core.CalculateMinMempoolFee(estBytes)

	if w != nil && w.PrivateKey != nil {
		dummy := core.Transaction{
			Sender:    w.Address,
			SenderPub: w.PublicKey,
			Recipient: recipient,
			Amount:    amount,
			Fee:       minFee,
			Nonce:     0,
		}
		if dummy.Sign(w.PrivateKey) == nil {
			if binaryActive {
				if binSz := dummy.BinarySize(); binSz > 0 {
					estBytes = binSz
				} else {
					estBytes = dummy.Size()
				}
			} else {
				estBytes = dummy.Size()
			}
			minFee = core.CalculateMinMempoolFee(estBytes)
			if !binaryActive && dummy.Fee != minFee {
				dummy.Fee = minFee
				if dummy.Sign(w.PrivateKey) == nil {
					estBytes = dummy.Size()
					if exactMin := core.CalculateMinMempoolFee(estBytes); exactMin > minFee {
						minFee = exactMin
					}
				}
			}
		}
	}

	step := core.MinRelayFee
	if step <= 0 {
		step = 1000
	}
	if mempoolCount < 0 {
		mempoolCount = 0
	}
	congestionSteps := int64(mempoolCount / 50)

	economy := minFee
	standard := roundUpFeeStep(minFee*3/2, step) + congestionSteps*step
	if standard <= economy {
		standard = economy + step
	}
	priority := minFee*2 + congestionSteps*2*step
	if priority <= standard {
		priority = standard + step
	}

	return FeeEstimateResponse{
		MinRelayFeePerKB:  step,
		EstimatedTxBytes:  estBytes,
		MinFeeArmoids:     minFee,
		MinFeeFormatted:   core.FormatArmxDecimal(minFee),
		EconomyArmoids:    economy,
		EconomyFormatted:  core.FormatArmxDecimal(economy),
		StandardArmoids:   standard,
		StandardFormatted: core.FormatArmxDecimal(standard),
		PriorityArmoids:   priority,
		PriorityFormatted: core.FormatArmxDecimal(priority),
		MempoolTxCount:    mempoolCount,
		Ticker:            core.TickerForNetworkID(netID),
	}
}

// ComputeGUIFeeEstimate calculates guaranteed-valid Economy/Standard/Priority fees using
// core.MinRelayFee (1,000 Armoids/KB) and height-0 network rules.
func ComputeGUIFeeEstimate(w *wallet.Wallet, recipient string, amount int64, mempoolCount int, netID string) FeeEstimateResponse {
	return ComputeGUIFeeEstimateAtHeight(w, recipient, amount, mempoolCount, netID, 0)
}
