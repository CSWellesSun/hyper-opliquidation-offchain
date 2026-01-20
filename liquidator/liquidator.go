package liquidator

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"log"
	"math/big"
	"path/filepath"
	"strings"
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

// LastBlockInfo holds the last block number and base fee with a lock
type LastBlockInfo struct {
	BlockNumber uint64
	BaseFee     *big.Int
	sync.RWMutex
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
	Oracle2Job         map[string]*Job // key is TokenName
	SubscribeTokens    []string
	LastBlock          *LastBlockInfo

	TgBotToken      string
	TgChatIdForInfo string

	// EOA management
	SpamEOAs        []common.Address
	SpamPrivateKeys []*ecdsa.PrivateKey
	SpamNonces      map[common.Address]*EOANonceInfo
	NoncesMutex     sync.Mutex
	CurEOAIndex     int
}

// NewLiquidatorSystem creates a new liquidator system
func NewLiquidatorSystem(
	client *ethclient.Client, subscribeTokens []string, mnemonic string, numEOAs int,
	tgBotToken string, tgChatIdForInfo string,
) (*LiquidatorSystem, error) {
	// Get private keys from mnemonic
	privateKeys, err := utils.GetPrivateKeysFromMnemonic(mnemonic, numEOAs)
	if err != nil {
		return nil, err
	}

	// Get addresses from private keys
	eoas := make([]common.Address, numEOAs)
	for i, pk := range privateKeys {
		eoas[i] = utils.GetAddress(pk)
	}

	log.Printf("Initialized %d EOAs for spam transactions", numEOAs)
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
		Oracle2Job:         make(map[string]*Job),
		SubscribeTokens:    subscribeTokens,
		LastBlock:          &LastBlockInfo{},
		SpamEOAs:           eoas,
		SpamPrivateKeys:    privateKeys,
		SpamNonces:         make(map[common.Address]*EOANonceInfo),
		CurEOAIndex:        0,
		TgBotToken:         tgBotToken,
		TgChatIdForInfo:    tgChatIdForInfo,
	}, nil
}

// InitializeNonces initializes nonces for all EOAs
func (ls *LiquidatorSystem) InitializeNonces() error {
	// Get current block number
	blockNumber, err := ls.Client.BlockNumber(context.Background())
	if err != nil {
		return err
	}

	ls.NoncesMutex.Lock()
	defer ls.NoncesMutex.Unlock()

	for _, eoa := range ls.SpamEOAs {
		nonce, err := ls.Client.PendingNonceAt(context.Background(), eoa)
		if err != nil {
			return err
		}
		ls.SpamNonces[eoa] = &EOANonceInfo{
			RealNonce:      nonce,
			RealNonceBlock: blockNumber,
			SendNonce:      nonce,
			SendNonceBlock: blockNumber,
		}
		log.Printf("Initialized nonce for %s: nonce=%d, block=%d", eoa.Hex(), nonce, blockNumber)
	}
	return nil
}

// GetNextEOA returns the next EOA in round-robin fashion
func (ls *LiquidatorSystem) GetNextEOA() (common.Address, *ecdsa.PrivateKey, uint64) {
	ls.NoncesMutex.Lock()
	defer ls.NoncesMutex.Unlock()

	eoa := ls.SpamEOAs[ls.CurEOAIndex]
	pk := ls.SpamPrivateKeys[ls.CurEOAIndex]
	nonceInfo := ls.SpamNonces[eoa]

	// Check if SendNonceBlock is 10+ blocks behind RealNonceBlock
	if nonceInfo.RealNonceBlock >= nonceInfo.SendNonceBlock+10 {
		log.Printf("Syncing nonce for %s: SendNonce %d->%d, SendNonceBlock %d->%d (RealNonceBlock=%d)",
			eoa.Hex(), nonceInfo.SendNonce, nonceInfo.RealNonce,
			nonceInfo.SendNonceBlock, nonceInfo.RealNonceBlock, nonceInfo.RealNonceBlock)
		nonceInfo.SendNonce = nonceInfo.RealNonce
		nonceInfo.SendNonceBlock = nonceInfo.RealNonceBlock
	}

	// Get current nonce to send
	nonce := nonceInfo.SendNonce

	// Increment SendNonce for next use
	nonceInfo.SendNonce++

	// Update SendNonceBlock to current block
	ls.LastBlock.RLock()
	currentBlock := ls.LastBlock.BlockNumber
	ls.LastBlock.RUnlock()
	nonceInfo.SendNonceBlock = currentBlock

	// Move to next EOA
	ls.CurEOAIndex = (ls.CurEOAIndex + 1) % len(ls.SpamEOAs)

	return eoa, pk, nonce
}

// updateRealNonces updates real nonces for all EOAs from block transactions
func (ls *LiquidatorSystem) updateRealNonces(block *types.Block) {
	ls.NoncesMutex.Lock()
	defer ls.NoncesMutex.Unlock()

	blockNumber := block.NumberU64()

	// Track the highest nonce seen for each EOA in this block
	eoaNonceMap := make(map[common.Address]uint64)

	// Iterate through all transactions in the block
	for _, tx := range block.Transactions() {
		sender, err := mev.Sender(tx)
		if err != nil {
			continue
		}

		// Check if this transaction is from one of our EOAs
		_, exists := ls.SpamNonces[sender]
		if !exists {
			continue
		}

		txNonce := tx.Nonce()
		// Track the highest nonce for this EOA
		if prevNonce, ok := eoaNonceMap[sender]; !ok || txNonce > prevNonce {
			eoaNonceMap[sender] = txNonce
		}
	}

	// Update RealNonce to the next nonce after the highest one seen in this block
	for eoa, highestNonce := range eoaNonceMap {
		nonceInfo := ls.SpamNonces[eoa]
		nextNonce := highestNonce + 1
		if nonceInfo.RealNonce != nextNonce {
			log.Printf("Real nonce updated for %s: %d->%d at block %d (tx nonce: %d)",
				eoa.Hex(), nonceInfo.RealNonce, nextNonce, blockNumber, highestNonce)
		}
		nonceInfo.RealNonce = nextNonce
	}
	for eoa := range ls.SpamNonces {
		nonceInfo := ls.SpamNonces[eoa]
		nonceInfo.RealNonceBlock = blockNumber
	}
}

// FetchFeeDataAndProcess fetches fee data concurrently and calls ProcessJob immediately
func (ls *LiquidatorSystem) FetchFeeDataAndProcess() {
	record := FeeDataRecord{
		Timestamp: time.Now(),
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// Fetch fee history concurrently
	go func() {
		defer wg.Done()
		feeHistory, err := GetPendingFeeHistory(ls.Client)
		if err != nil {
			record.FeeHistoryError = err.Error()
		} else {
			record.FeeHistory = feeHistory
		}
	}()

	// Fetch max priority fee concurrently
	go func() {
		defer wg.Done()
		maxPriorityFee, err := GetMaxPriorityFee(ls.Client)
		if err != nil {
			record.MaxPriorityError = err.Error()
		} else {
			record.MaxPriorityFee = maxPriorityFee
		}
	}()

	// Wait for both to complete
	wg.Wait()

	// Update fee data records
	ls.FeeDataMutex.Lock()
	// Maintain max 4 recent records
	if len(ls.FeeDataRecords) >= MaxFeeDataRecords {
		ls.FeeDataRecords = ls.FeeDataRecords[1:]
	}
	ls.FeeDataRecords = append(ls.FeeDataRecords, record)
	// Also append to all records for saving
	ls.AllFeeDataRecords = append(ls.AllFeeDataRecords, record)
	ls.FeeDataMutex.Unlock()

	log.Printf("Fee data updated: records=%d, allRecords=%d, maxPriorityFee=%v, maxReward=%v", len(ls.FeeDataRecords), len(ls.AllFeeDataRecords), record.MaxPriorityFee, GetMaxReward(record.FeeHistory.Reward))

	// Immediately process job after getting fee data
	ls.ProcessJob()
}

// FetchBlockAndLogs fetches the latest block and checks for Redstone logs
func (ls *LiquidatorSystem) FetchBlockAndLogs() (*types.Block, []types.Log, error) {
	// Get latest block number
	blockNumber, err := ls.Client.BlockNumber(context.Background())
	if err != nil {
		return nil, nil, err
	}

	// Skip if same block
	ls.LastBlock.RLock()
	lastBlockNumber := ls.LastBlock.BlockNumber
	ls.LastBlock.RUnlock()

	if blockNumber == lastBlockNumber {
		return nil, nil, nil
	}

	// Get block
	block, err := ls.Client.BlockByNumber(context.Background(), new(big.Int).SetUint64(blockNumber))
	if err != nil {
		return nil, nil, err
	}

	// Update LastBlock with lock
	ls.LastBlock.Lock()
	ls.LastBlock.BlockNumber = blockNumber
	if block.BaseFee() != nil {
		ls.LastBlock.BaseFee = block.BaseFee()
		log.Printf("Block %d baseFee: %s", blockNumber, ls.LastBlock.BaseFee.String())
	}
	ls.LastBlock.Unlock()

	// Update real nonces for all EOAs from block transactions
	ls.updateRealNonces(block)

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
	token := pd.Token

	// Check if job already exists for this token
	if existingJob, ok := ls.Oracle2Job[token]; ok && !existingJob.Finished {
		log.Printf("Job already exists for token %s, skipping", token)
		return
	}

	ls.Oracle2Job[token] = &Job{
		PriceDeviation: pd,
		BlockNumber:    blockNumber,
		BlocksSent:     0,
		SpamTxs:        make([]SpamTx, 0),
		StartTime:      time.Now(),
		Finished:       false,
	}
	log.Printf("Job created for token %s at block %d", token, blockNumber)
}

// ProcessJob processes all active jobs, sending transactions once and adding to all active jobs
func (ls *LiquidatorSystem) ProcessJob() {
	// Collect active jobs
	activeTokens := make([]string, 0)
	for token, job := range ls.Oracle2Job {
		if !job.Finished {
			activeTokens = append(activeTokens, token)
		}
	}

	if len(activeTokens) == 0 {
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

	// Get LastBlock info with lock
	ls.LastBlock.RLock()
	baseFee := ls.LastBlock.BaseFee
	lastBlockNumber := ls.LastBlock.BlockNumber
	ls.LastBlock.RUnlock()

	if baseFee == nil {
		log.Printf("No baseFee available, skipping")
		return
	}

	// feeCap = baseFee * 2 + tipCap
	baseFeeDouble := new(big.Int).Mul(baseFee, big.NewInt(2))

	// Send len(FeeDataRecords)*2 transactions once (maxReward and maxPriority for each record)
	txCount := len(feeDataRecords) * 2
	log.Printf("Sending %d transactions at block %d for %d active jobs (feeDataRecords=%d, baseFee=%s)",
		txCount, lastBlockNumber, len(activeTokens), len(feeDataRecords), baseFee.String())

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
			// Send maxReward tx and add to all active jobs
			ls.sendSpamTxToActiveJobs(activeTokens, maxRewardTip, maxRewardFeeCap, "maxReward", lastBlockNumber)
		} else {
			log.Printf("Skipping maxReward tx: tipCap is 0")
		}

		// Send maxPriority tx (use MaxPriorityFee)
		maxPriorityTip := record.MaxPriorityFee

		// Skip if maxPriorityTip is 0
		if maxPriorityTip.Cmp(big.NewInt(0)) > 0 {
			// feeCap = baseFee * 2 + tipCap
			maxPriorityFeeCap := new(big.Int).Add(baseFeeDouble, maxPriorityTip)
			// Send maxPriority tx and add to all active jobs
			ls.sendSpamTxToActiveJobs(activeTokens, maxPriorityTip, maxPriorityFeeCap, "maxPriority", lastBlockNumber)
		} else {
			log.Printf("Skipping maxPriority tx: tipCap is 0")
		}
	}

	// Update all active jobs
	for _, token := range activeTokens {
		job := ls.Oracle2Job[token]
		job.BlocksSent++
		log.Printf("Job progress for %s: sent block %d/%d", token, job.BlocksSent, JobLength)

		// Mark job as finished after JobLength blocks
		if job.BlocksSent >= JobLength {
			job.Finished = true
			job.EndTime = time.Now()
			ls.saveJobToJSON(token, job)
		}
	}
}

// sendSpamTxToActiveJobs sends a spam transaction and adds it to all active jobs
// nextBaseFee is set to nil
// Note: This function assumes JobMutex is already held by caller (ProcessJob)
func (ls *LiquidatorSystem) sendSpamTxToActiveJobs(activeTokens []string, tipCap, feeCap *big.Int, txType string, blockNumber uint64) {
	eoa, pk, nonce := ls.GetNextEOA()

	// Get signed transaction synchronously
	tx, err := mev.GetTransferSelfTransaction(eoa, nonce, pk, nil, tipCap, feeCap, ls.Client)
	if err != nil {
		log.Printf("Error creating %s tx from %s: %v", txType, eoa.Hex(), err)
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

	// Add the tx to all active jobs (JobMutex is already held by caller)
	for _, token := range activeTokens {
		if job, ok := ls.Oracle2Job[token]; ok && job != nil && !job.Finished {
			job.SpamTxs = append(job.SpamTxs, spamTx)
		}
	}

	log.Printf("Created %s tx: hash=%s, from=%s, nonce=%d, tipCap=%s, feeCap=%s (added to %d jobs)",
		txType, tx.Hash().Hex(), eoa.Hex(), nonce, tipCap.String(), feeCap.String(), len(activeTokens))

	// Send transaction asynchronously
	go func() {
		if err := ls.Client.SendTransaction(context.Background(), tx); err != nil {
			log.Printf("Error sending %s tx %s: %v", txType, tx.Hash().Hex(), err)
			return
		}
		log.Printf("Sent %s tx: hash=%s", txType, tx.Hash().Hex())
	}()
}

// saveJobToJSON saves the finished job to a JSON file
func (ls *LiquidatorSystem) saveJobToJSON(token string, job *Job) {
	if job == nil {
		return
	}

	// Create filename with timestamp
	now := job.StartTime
	date := now.Format("20060102")
	hourStr := now.Format("15")
	// Include token in filename (replace unsafe chars for safe filename)
	safeToken := token
	safeToken = strings.ReplaceAll(safeToken, " / ", "_")
	safeToken = filepath.Base(safeToken) // Remove path separators
	filename := filepath.Join("data", date, hourStr, now.Format("150405")+"_"+safeToken+"_job.json")

	if err := utils.WriteToJson(job, filename); err != nil {
		log.Printf("Error saving job for %s: %v", token, err)
		return
	}

	log.Printf("Job saved to %s (token=%s, blocksSent=%d, txsSent=%d, duration=%v)",
		filename, token, job.BlocksSent, len(job.SpamTxs), job.EndTime.Sub(job.StartTime))
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

// StartBlockFetcher starts a goroutine that fetches blocks and logs independently at 0ms
func (ls *LiquidatorSystem) StartBlockFetcher() {
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			// Wait for next second boundary (0ms)
			now := time.Now()
			sleepDuration := time.Duration(1000-now.Nanosecond()/1000000) * time.Millisecond
			if sleepDuration > 0 && sleepDuration < time.Second {
				time.Sleep(sleepDuration)
			}

			go func() {
				// Fetch block and logs
				block, logs, err := ls.FetchBlockAndLogs()
				if err != nil {
					log.Printf("Error fetching block: %v", err)
					return
				}

				if block != nil {
					log.Printf("Block %d fetched, logs count: %d", block.NumberU64(), len(logs))
				}

				// Send logs to logChan for StartPriceFeed to process
				for _, logEntry := range logs {
					select {
					case ls.LogChan <- logEntry:
						log.Printf("Log sent to channel: tx=%s", logEntry.TxHash.Hex())
					default:
						log.Printf("LogChan full, dropping log")
					}
				}
			}()
		}
	}()
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

	// Start block fetcher (independent goroutine)
	ls.StartBlockFetcher()

	priceFeedChan := make(chan *mev.PriceFeed, 100)

	// Start price feed monitoring
	mev.StartPriceFeed(ls.Client, ls.SubscribeTokens, ls.LogChan, priceFeedChan, ls.ErrorChan)
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

			// At 0ms: Fetch fee data concurrently and immediately process job
			ls.FetchFeeDataAndProcess()

		case pd := <-ls.PriceDeviationChan:
			log.Printf("Price deviation detected: token=%s, deviation=%v", pd.Token, pd.Deviation)
			ls.LastBlock.RLock()
			blockNum := ls.LastBlock.BlockNumber
			ls.LastBlock.RUnlock()
			ls.CreateJob(pd, blockNum)

		case priceFeed := <-priceFeedChan:
			hasActiveJob := false
			for _, job := range ls.Oracle2Job {
				if !job.Finished {
					hasActiveJob = true
					break
				}
			}
			if !hasActiveJob {
				log.Printf("[HYPE-LIQUIDATOR] No active job found for price feed: token=%s", priceFeed.Token)
				ls.NotifyInfo(fmt.Sprintf("[HYPE-LIQUIDATOR] No active job found for price feed: token=%s", priceFeed.Token))
			} else {
				log.Printf("[HYPE-LIQUIDATOR] Active job found for price feed: token=%s", priceFeed.Token)
				ls.NotifyInfo(fmt.Sprintf("[HYPE-LIQUIDATOR] Active job found for price feed: token=%s", priceFeed.Token))
			}
			ls.PriceFeedChan <- priceFeed

		case err := <-ls.ErrorChan:
			log.Printf("Error: %v", err)
		}
	}
}
