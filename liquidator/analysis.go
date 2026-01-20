package liquidator

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"sort"

	"github.com/CSWellesSun/hypermevlib/mev"
	"github.com/CSWellesSun/hypermevlib/utils"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

func AnalyseRecall(client *ethclient.Client) {
	// Get mnemonic from environment variable
	mnemonic := os.Getenv("MNEMONIC")
	if mnemonic == "" {
		log.Fatal("MNEMONIC environment variable is not set")
	}

	// Get number of EOAs
	numEOAs := 10 // Default, can be made configurable
	if numEOAsStr := os.Getenv("NUM_EOAS"); numEOAsStr != "" {
		fmt.Sscanf(numEOAsStr, "%d", &numEOAs)
	}

	// Derive EOA addresses from mnemonic
	privateKeys, err := utils.GetPrivateKeysFromMnemonic(mnemonic, numEOAs)
	if err != nil {
		log.Fatalf("Error getting private keys from mnemonic: %v", err)
	}

	ourEOAs := make(map[common.Address]bool)
	for _, pk := range privateKeys {
		eoa := utils.GetAddress(pk)
		ourEOAs[eoa] = true
		log.Printf("Our EOA: %s", eoa.Hex())
	}

	// Read all job files from 20260119 onwards
	jobFiles, err := findJobFiles("data", "20260119")
	if err != nil {
		log.Fatalf("Error finding job files: %v", err)
	}

	log.Printf("Found %d job files", len(jobFiles))

	// Analyze each job separately
	for idx, jobFile := range jobFiles {
		log.Printf("\n=== Analyzing job %d/%d: %s ===", idx+1, len(jobFiles), jobFile)

		job, err := readJobFile(jobFile)
		if err != nil {
			log.Printf("Error reading job file %s: %v", jobFile, err)
			continue
		}

		if len(job.SpamTxs) == 0 {
			log.Printf("No spam transactions in this job, skipping")
			continue
		}

		// Collect blockNumberSend values for this job
		var blockNumbers []uint64
		for _, spamTx := range job.SpamTxs {
			blockNumbers = append(blockNumbers, spamTx.BlockNumberSend)
		}

		// Find min and max block numbers for this job
		sort.Slice(blockNumbers, func(i, j int) bool {
			return blockNumbers[i] < blockNumbers[j]
		})

		minBlock := blockNumbers[0]
		maxBlock := blockNumbers[len(blockNumbers)-1] + 5

		log.Printf("Job block range: %d to %d (original max: %d)", minBlock, maxBlock, blockNumbers[len(blockNumbers)-1])

		// Query logs for REDSTONE_VALUEUPDATE_EVENT_TOPIC in this job's range
		query := ethereum.FilterQuery{
			FromBlock: new(big.Int).SetUint64(minBlock),
			ToBlock:   new(big.Int).SetUint64(maxBlock),
			Topics:    [][]common.Hash{{common.HexToHash(mev.REDSTONE_VALUEUPDATE_EVENT_TOPIC)}},
		}

		logs, err := client.FilterLogs(context.Background(), query)
		if err != nil {
			log.Printf("Error filtering logs for job: %v", err)
			continue
		}

		if len(logs) == 0 {
			log.Printf("No value update events found in this job's range")
			continue
		}

		log.Printf("Found %d value update events in this job's range", len(logs))

		// Group logs by block number
		blockToLogs := make(map[uint64][]types.Log)
		for _, logEntry := range logs {
			blockToLogs[logEntry.BlockNumber] = append(blockToLogs[logEntry.BlockNumber], logEntry)
		}

		// For each block with value update events
		for blockNumber, logsInBlock := range blockToLogs {
			log.Printf("\n  --- Analyzing block %d ---", blockNumber)

			// Get the block
			block, err := client.BlockByNumber(context.Background(), new(big.Int).SetUint64(blockNumber))
			if err != nil {
				log.Printf("  Error getting block %d: %v", blockNumber, err)
				continue
			}

			// Get all value update transactions from logs
			valueUpdateTxHashes := make(map[common.Hash]bool)
			for _, logEntry := range logsInBlock {
				valueUpdateTxHashes[logEntry.TxHash] = true
			}

			log.Printf("  Found %d value update event(s) in block %d", len(valueUpdateTxHashes), blockNumber)

			// Get value update transactions and their tip caps
			valueUpdateTipCaps := make(map[string]common.Hash) // tipCap -> txHash
			var ourTxs []*types.Transaction

			for _, tx := range block.Transactions() {
				sender, err := mev.Sender(tx)
				if err != nil {
					continue
				}

				// Check if this is a value update transaction
				if valueUpdateTxHashes[tx.Hash()] {
					tipCap := tx.GasTipCap()
					if tipCap != nil {
						tipCapStr := tipCap.String()
						valueUpdateTipCaps[tipCapStr] = tx.Hash()
						log.Printf("  Value update tx %s: tipCap=%s, feeCap=%s",
							tx.Hash().Hex(), tipCapStr, tx.GasFeeCap().String())
					}
				}

				// Check if this is our transaction
				if ourEOAs[sender] {
					ourTxs = append(ourTxs, tx)
				}
			}

			if len(ourTxs) == 0 {
				log.Printf("  No transactions from our EOAs in block %d", blockNumber)
				continue
			}

			log.Printf("  Found %d transactions from our EOAs in block %d", len(ourTxs), blockNumber)

			// Check if any of our transactions have the same tip cap as value update transactions
			hasMatch := false
			for _, tx := range ourTxs {
				tipCap := tx.GasTipCap()
				if tipCap != nil {
					tipCapStr := tipCap.String()
					log.Printf("    Our tx %s: tipCap=%s, feeCap=%s, nonce=%d",
						tx.Hash().Hex(), tipCapStr, tx.GasFeeCap().String(), tx.Nonce())

					if valueUpdateTxHash, found := valueUpdateTipCaps[tipCapStr]; found {
						hasMatch = true
						log.Printf("    ✓ MATCH! Our tx has same tip cap as value update tx %s", valueUpdateTxHash.Hex())
					}
				}
			}

			if !hasMatch {
				log.Printf("  ✗ No match found - none of our transactions have the same tip cap as value update transactions")
			}
		}
	}

	log.Printf("\n=== Analysis complete ===")
}

// findJobFiles finds all job files from a given date onwards
func findJobFiles(dataDir string, fromDate string) ([]string, error) {
	var jobFiles []string

	err := filepath.Walk(dataDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip if it's a directory
		if info.IsDir() {
			return nil
		}

		// Check if it's a job file (not fee_data.json)
		if filepath.Ext(path) == ".json" && filepath.Base(path) != "fee_data.json" && !contains(filepath.Base(path), "fee_data") {
			// Extract date from path (e.g., data/20260119/...)
			pathParts := filepath.SplitList(path)
			if len(pathParts) == 0 {
				pathParts = filepath.SplitList(filepath.ToSlash(path))
			}
			if len(pathParts) == 0 {
				// Try a different approach
				dir := filepath.Dir(path)
				for dir != "." && dir != "/" && dir != dataDir {
					base := filepath.Base(dir)
					if len(base) == 8 && base >= fromDate {
						jobFiles = append(jobFiles, path)
						return nil
					}
					dir = filepath.Dir(dir)
				}
			} else {
				for _, part := range pathParts {
					if len(part) == 8 && part >= fromDate {
						jobFiles = append(jobFiles, path)
						return nil
					}
				}
			}

			// Alternative: just check if path contains a date >= fromDate
			if contains(path, "/"+fromDate) || containsDateAfter(path, fromDate) {
				jobFiles = append(jobFiles, path)
			}
		}

		return nil
	})

	return jobFiles, err
}

// containsDateAfter checks if path contains a date (YYYYMMDD format) >= fromDate
func containsDateAfter(path string, fromDate string) bool {
	// Look for patterns like /20260119/, /20260120/, etc.
	for i := 0; i <= len(path)-9; i++ {
		if (i == 0 || path[i-1] == '/') && len(path) > i+8 {
			candidate := path[i : i+8]
			// Check if it's a valid date format and >= fromDate
			if isDateFormat(candidate) && candidate >= fromDate {
				return true
			}
		}
	}
	return false
}

// isDateFormat checks if a string looks like YYYYMMDD
func isDateFormat(s string) bool {
	if len(s) != 8 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// contains checks if a string contains a substring
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// readJobFile reads a job file and returns the Job struct
func readJobFile(filename string) (*Job, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	var job Job
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, err
	}

	return &job, nil
}
