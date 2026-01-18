package liquidator

import (
	"context"
	"crypto/ecdsa"
	"log"
	"math/big"
	"path/filepath"
	"sync"
	"time"

	"github.com/CSWellesSun/hypermevlib/mev"
	"github.com/CSWellesSun/hypermevlib/utils"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

const (
	MaxFeeDataRecords = 4
	JobLength         = 10
	NumEOAs           = 8
)

// Job represents a liquidation job
type Job struct {
	PriceDeviation *mev.PriceDeviation `json:"priceDeviation"`
	BlockNumber    uint64              `json:"blockNumber"`
	BlocksSent     int                 `json:"blocksSent"`
	SpamTxs        []SpamTx            `json:"spamTxs"`
	StartTime      time.Time           `json:"startTime"`
	EndTime        time.Time           `json:"endTime"`
	Finished       bool                `json:"finished"`
}

// LiquidatorSystem is the main liquidator system
type LiquidatorSystem struct {
	Client             *ethclient.Client
	FeeDataRecords     []FeeDataRecord // Recent records (max 4)
	AllFeeDataRecords  []FeeDataRecord // All records for saving
	FeeDataMutex       sync.Mutex
	LogChan            chan types.Log
	PriceFeedChan      chan *mev.PriceFeed
	PriceDeviationChan chan *mev.PriceDeviation
	ErrorChan          chan error
	CurrentJob         *Job
	JobMutex           sync.Mutex
	SubscribeTokens    []string
	LastBlockNumber    uint64
	LastBaseFee        *big.Int

	// EOA management
	SpamEOAs        []common.Address
	SpamPrivateKeys []*ecdsa.PrivateKey
	SpamNonces      map[common.Address]uint64
	NonceMutex      sync.Mutex
	CurEOAIndex     int
}

// NewLiquidatorSystem creates a new liquidator system
func NewLiquidatorSystem(client *ethclient.Client, subscribeTokens []string, mnemonic string) (*LiquidatorSystem, error) {
	// Get private keys from mnemonic
	privateKeys, err := utils.GetPrivateKeysFromMnemonic(mnemonic, NumEOAs)
	if err != nil {
		return nil, err
	}

	// Get addresses from private keys
	eoas := make([]common.Address, NumEOAs)
	for i, pk := range privateKeys {
		eoas[i] = utils.GetAddress(pk)
	}

	log.Printf("Initialized %d EOAs for spam transactions", NumEOAs)
	for i, eoa := range eoas {
		log.Printf("EOA %d: %s", i, eoa.Hex())
	}

	return &LiquidatorSystem{
		Client:             client,
		FeeDataRecords:     make([]FeeDataRecord, 0, MaxFeeDataRecords),
		AllFeeDataRecords:  make([]FeeDataRecord, 0),
		LogChan:            make(chan types.Log, 100),
		PriceFeedChan:      make(chan *mev.PriceFeed, 100),
		PriceDeviationChan: make(chan *mev.PriceDeviation, 100),
		ErrorChan:          make(chan error, 100),
		SubscribeTokens:    subscribeTokens,
		SpamEOAs:           eoas,
		SpamPrivateKeys:    privateKeys,
		SpamNonces:         make(map[common.Address]uint64),
		CurEOAIndex:        0,
	}, nil
}

// InitializeNonces initializes nonces for all EOAs
func (ls *LiquidatorSystem) InitializeNonces() error {
	ls.NonceMutex.Lock()
	defer ls.NonceMutex.Unlock()

	for _, eoa := range ls.SpamEOAs {
		nonce, err := ls.Client.PendingNonceAt(context.Background(), eoa)
		if err != nil {
			return err
		}
		ls.SpamNonces[eoa] = nonce
		log.Printf("Initialized nonce for %s: %d", eoa.Hex(), nonce)
	}
	return nil
}

// GetNextEOA returns the next EOA in round-robin fashion
func (ls *LiquidatorSystem) GetNextEOA() (common.Address, *ecdsa.PrivateKey, uint64) {
	ls.NonceMutex.Lock()
	defer ls.NonceMutex.Unlock()

	eoa := ls.SpamEOAs[ls.CurEOAIndex]
	pk := ls.SpamPrivateKeys[ls.CurEOAIndex]
	nonce := ls.SpamNonces[eoa]

	// Increment nonce for next use
	ls.SpamNonces[eoa] = nonce + 1

	// Move to next EOA
	ls.CurEOAIndex = (ls.CurEOAIndex + 1) % NumEOAs

	return eoa, pk, nonce
}

// UpdateFeeData fetches and updates fee data records
func (ls *LiquidatorSystem) UpdateFeeData() {
	ls.FeeDataMutex.Lock()
	defer ls.FeeDataMutex.Unlock()

	record := FeeDataRecord{
		Timestamp: time.Now(),
	}

	// Get fee history
	feeHistory, err := GetPendingFeeHistory(ls.Client)
	if err != nil {
		record.FeeHistoryError = err.Error()
	} else {
		record.FeeHistory = feeHistory
	}

	// Get max priority fee
	maxPriorityFee, err := GetMaxPriorityFee(ls.Client)
	if err != nil {
		record.MaxPriorityError = err.Error()
	} else {
		record.MaxPriorityFee = maxPriorityFee
	}

	// Maintain max 4 recent records
	if len(ls.FeeDataRecords) >= MaxFeeDataRecords {
		ls.FeeDataRecords = ls.FeeDataRecords[1:]
	}
	ls.FeeDataRecords = append(ls.FeeDataRecords, record)

	// Also append to all records for saving
	ls.AllFeeDataRecords = append(ls.AllFeeDataRecords, record)

	log.Printf("Fee data updated: records=%d, allRecords=%d, maxPriorityFee=%v", len(ls.FeeDataRecords), len(ls.AllFeeDataRecords), maxPriorityFee)
}

// FetchBlockAndLogs fetches the latest block and checks for Redstone logs
func (ls *LiquidatorSystem) FetchBlockAndLogs() (*types.Block, []types.Log, error) {
	// Get latest block number
	blockNumber, err := ls.Client.BlockNumber(context.Background())
	if err != nil {
		return nil, nil, err
	}

	// Skip if same block
	if blockNumber == ls.LastBlockNumber {
		return nil, nil, nil
	}
	ls.LastBlockNumber = blockNumber

	// Get block
	block, err := ls.Client.BlockByNumber(context.Background(), new(big.Int).SetUint64(blockNumber))
	if err != nil {
		return nil, nil, err
	}

	// Update LastBaseFee from block
	if block.BaseFee() != nil {
		ls.LastBaseFee = block.BaseFee()
		log.Printf("Block %d baseFee: %s", blockNumber, ls.LastBaseFee.String())
	}

	// Query logs for REDSTONE_VALUEUPDATE_EVENT_TOPIC
	query := ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(blockNumber),
		ToBlock:   new(big.Int).SetUint64(blockNumber),
		Topics:    [][]common.Hash{{common.HexToHash(mev.REDSTONE_VALUEUPDATE_EVENT_TOPIC)}},
	}

	logs, err := ls.Client.FilterLogs(context.Background(), query)
	if err != nil {
		return block, nil, err
	}

	return block, logs, nil
}

// CreateJob creates a new job from price deviation
func (ls *LiquidatorSystem) CreateJob(pd *mev.PriceDeviation, blockNumber uint64) {
	ls.JobMutex.Lock()
	defer ls.JobMutex.Unlock()

	if ls.CurrentJob != nil && !ls.CurrentJob.Finished {
		log.Printf("Job already in progress, skipping")
		return
	}

	ls.CurrentJob = &Job{
		PriceDeviation: pd,
		BlockNumber:    blockNumber,
		BlocksSent:     0,
		SpamTxs:        make([]SpamTx, 0),
		StartTime:      time.Now(),
		Finished:       false,
	}
	log.Printf("Job created for token %s at block %d", pd.Token, blockNumber)
}

// ProcessJob processes the current job, sending transactions each block
func (ls *LiquidatorSystem) ProcessJob() {
	ls.JobMutex.Lock()
	defer ls.JobMutex.Unlock()

	if ls.CurrentJob == nil || ls.CurrentJob.Finished {
		return
	}

	// Get fee data for calculating tip/fee caps
	ls.FeeDataMutex.Lock()
	feeDataRecords := make([]FeeDataRecord, len(ls.FeeDataRecords))
	copy(feeDataRecords, ls.FeeDataRecords)
	ls.FeeDataMutex.Unlock()

	if len(feeDataRecords) == 0 {
		log.Printf("No fee data available, skipping")
		return
	}

	if ls.LastBaseFee == nil {
		log.Printf("No baseFee available, skipping")
		return
	}

	// feeCap = baseFee * 2 + tipCap
	baseFee := ls.LastBaseFee
	baseFeeDouble := new(big.Int).Mul(baseFee, big.NewInt(2))

	// Send len(FeeDataRecords)*2 transactions (maxReward and maxPriority for each record)
	txCount := len(feeDataRecords) * 2
	log.Printf("Sending %d transactions for block %d (feeDataRecords=%d, baseFee=%s)", txCount, ls.LastBlockNumber, len(feeDataRecords), baseFee.String())

	for i, record := range feeDataRecords {
		if record.FeeHistory == nil || record.MaxPriorityFee == nil {
			log.Printf("Skipping fee record %d: missing data", i)
			continue
		}

		// Get max reward from 2D rewards array
		maxRewardTip := GetMaxReward(record.FeeHistory.Reward)

		// Skip if maxRewardTip is 0
		if maxRewardTip.Cmp(big.NewInt(0)) > 0 {
			// feeCap = baseFee * 2 + tipCap
			maxRewardFeeCap := new(big.Int).Add(baseFeeDouble, maxRewardTip)
			// Send maxReward tx (nextBaseFee = nil)
			ls.sendSpamTx(maxRewardTip, maxRewardFeeCap, "maxReward")
		} else {
			log.Printf("Skipping maxReward tx: tipCap is 0")
		}

		// Send maxPriority tx (use MaxPriorityFee)
		maxPriorityTip := record.MaxPriorityFee

		// Skip if maxPriorityTip is 0
		if maxPriorityTip.Cmp(big.NewInt(0)) > 0 {
			// feeCap = baseFee * 2 + tipCap
			maxPriorityFeeCap := new(big.Int).Add(baseFeeDouble, maxPriorityTip)
			// Send maxPriority tx (nextBaseFee = nil)
			ls.sendSpamTx(maxPriorityTip, maxPriorityFeeCap, "maxPriority")
		} else {
			log.Printf("Skipping maxPriority tx: tipCap is 0")
		}
	}

	// Increment blocks sent
	ls.CurrentJob.BlocksSent++
	log.Printf("Job progress: sent block %d/%d for token %s",
		ls.CurrentJob.BlocksSent, JobLength, ls.CurrentJob.PriceDeviation.Token)

	// Mark job as finished after JobLength blocks
	if ls.CurrentJob.BlocksSent >= JobLength {
		ls.CurrentJob.Finished = true
		ls.CurrentJob.EndTime = time.Now()
		ls.SaveJobToJSON()
	}
}

// sendSpamTx sends a spam transaction using SendTransferSelf in a goroutine
// nextBaseFee is set to nil
func (ls *LiquidatorSystem) sendSpamTx(tipCap, feeCap *big.Int, txType string) {
	eoa, pk, nonce := ls.GetNextEOA()
	blockNumber := ls.LastBlockNumber

	go func() {
		tx, err := mev.SendTransferSelf(eoa, nonce, pk, nil, tipCap, feeCap, ls.Client)
		if err != nil {
			log.Printf("Error sending %s tx from %s: %v", txType, eoa.Hex(), err)
			return
		}

		spamTx := SpamTx{
			TxHash:          tx.Hash(),
			From:            eoa,
			TipCap:          tipCap,
			FeeCap:          feeCap,
			Nonce:           nonce,
			BlockNumberSend: blockNumber,
			SendTime:        time.Now(),
			TxType:          txType,
		}

		ls.JobMutex.Lock()
		if ls.CurrentJob != nil {
			ls.CurrentJob.SpamTxs = append(ls.CurrentJob.SpamTxs, spamTx)
		}
		ls.JobMutex.Unlock()

		log.Printf("Sent %s tx: hash=%s, from=%s, nonce=%d, tipCap=%s, feeCap=%s",
			txType, tx.Hash().Hex(), eoa.Hex(), nonce, tipCap.String(), feeCap.String())
	}()
}

// SaveJobToJSON saves the finished job to a JSON file
func (ls *LiquidatorSystem) SaveJobToJSON() {
	if ls.CurrentJob == nil {
		return
	}

	// Create filename with timestamp
	now := ls.CurrentJob.StartTime
	date := now.Format("20060102")
	hourStr := now.Format("15")
	filename := filepath.Join("data", date, hourStr, now.Format("150405")+"_job.json")

	if err := utils.WriteToJson(ls.CurrentJob, filename); err != nil {
		log.Printf("Error saving job: %v", err)
		return
	}

	log.Printf("Job saved to %s (token=%s, blocksSent=%d, txsSent=%d, duration=%v)",
		filename, ls.CurrentJob.PriceDeviation.Token, ls.CurrentJob.BlocksSent,
		len(ls.CurrentJob.SpamTxs), ls.CurrentJob.EndTime.Sub(ls.CurrentJob.StartTime))
}

// StartFeeDataSaver starts a goroutine that saves AllFeeDataRecords every 10 minutes
func (ls *LiquidatorSystem) StartFeeDataSaver() {
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()

		for range ticker.C {
			ls.SaveFeeDataToJSON()
		}
	}()
}

// SaveFeeDataToJSON saves all fee data records to a JSON file
func (ls *LiquidatorSystem) SaveFeeDataToJSON() {
	ls.FeeDataMutex.Lock()
	// Copy records to avoid holding lock during file write
	records := make([]FeeDataRecord, len(ls.AllFeeDataRecords))
	copy(records, ls.AllFeeDataRecords)
	// Clear the records after copying
	ls.AllFeeDataRecords = make([]FeeDataRecord, 0)
	ls.FeeDataMutex.Unlock()

	if len(records) == 0 {
		log.Printf("No fee data records to save")
		return
	}

	// Create filename with timestamp
	now := time.Now()
	date := now.Format("20060102")
	hourStr := now.Format("15")
	filename := filepath.Join("data", date, hourStr, now.Format("150405")+"_fee_data.json")

	if err := utils.WriteToJson(records, filename); err != nil {
		log.Printf("Error saving fee data: %v", err)
		return
	}

	log.Printf("Fee data saved to %s (records=%d)", filename, len(records))
}

// Start starts the liquidator system
func (ls *LiquidatorSystem) Start() {
	log.Printf("Starting liquidator system with tokens: %v", ls.SubscribeTokens)

	// Initialize nonces
	if err := ls.InitializeNonces(); err != nil {
		log.Fatalf("Failed to initialize nonces: %v", err)
	}

	// Start fee data saver (saves every 10 minutes)
	ls.StartFeeDataSaver()

	// Start price feed monitoring
	mev.StartPriceFeed(ls.Client, ls.SubscribeTokens, ls.LogChan, ls.PriceFeedChan, ls.ErrorChan)
	mev.StartPythPriceFeedForDeviation(ls.Client, ls.SubscribeTokens, ls.PriceFeedChan, ls.PriceDeviationChan, ls.ErrorChan, nil)

	// Main loop - runs every second at 0ms
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Wait for next second boundary (0ms)
			now := time.Now()
			sleepDuration := time.Duration(1000-now.Nanosecond()/1000000) * time.Millisecond
			if sleepDuration > 0 && sleepDuration < time.Second {
				time.Sleep(sleepDuration)
			}

			log.Printf("=== Tick at %s ===", time.Now().Format("15:04:05.000"))

			// Step 1: Update fee data
			ls.UpdateFeeData()

			// Step 2: Fetch block and logs
			block, logs, err := ls.FetchBlockAndLogs()
			if err != nil {
				log.Printf("Error fetching block: %v", err)
				continue
			}

			if block != nil {
				log.Printf("Block %d fetched, logs count: %d", block.NumberU64(), len(logs))
			}

			// Step 3: Send logs to logChan for StartPriceFeed to process
			for _, logEntry := range logs {
				select {
				case ls.LogChan <- logEntry:
					log.Printf("Log sent to channel: tx=%s", logEntry.TxHash.Hex())
				default:
					log.Printf("LogChan full, dropping log")
				}
			}

			// Step 4: Process any existing job
			ls.ProcessJob()

		case pd := <-ls.PriceDeviationChan:
			log.Printf("Price deviation detected: token=%s, deviation=%v", pd.Token, pd.Deviation)
			ls.CreateJob(pd, ls.LastBlockNumber)
			ls.ProcessJob()

		case err := <-ls.ErrorChan:
			log.Printf("Error: %v", err)
		}
	}
}
