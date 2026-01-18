package liquidator

import (
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
)

// FeeDataRecord represents a single data point collected every second
type FeeDataRecord struct {
	Timestamp        time.Time            `json:"timestamp"`
	FeeHistory       *ethereum.FeeHistory `json:"feeHistory,omitempty"`
	FeeHistoryError  string               `json:"feeHistoryError,omitempty"`
	MaxPriorityFee   *big.Int             `json:"maxPriorityFee,omitempty"`
	MaxPriorityError string               `json:"maxPriorityError,omitempty"`
}

// SpamTx represents a spam transaction
type SpamTx struct {
	TxHash          common.Hash    `json:"txHash"`
	From            common.Address `json:"from"`
	TipCap          *big.Int       `json:"tipCap"`
	FeeCap          *big.Int       `json:"feeCap"`
	Nonce           uint64         `json:"nonce"`
	BlockNumberSend uint64         `json:"blockNumberSend"`
	SendTime        time.Time      `json:"sendTime"`
	TxType          string         `json:"txType"` // "maxReward" or "maxPriority"
}
