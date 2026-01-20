package main

import (
	"context"
	"log"
	"math/big"
	"os"
	"strconv"

	"hyper-opliquidation-offchain/liquidator"

	"github.com/CSWellesSun/hypermevlib/mev"
	"github.com/CSWellesSun/hypermevlib/utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/joho/godotenv"
)

func init() {
	err := godotenv.Load()
	if err != nil {
		log.Fatal(err)
	}

	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("Usage: go run main.go <command>")
	}

	switch os.Args[1] {
	case "liquidate":
		// go run main.go liquidate
		client, err := mev.NewClient(os.Getenv("HYPE_RPC_URL"))
		if err != nil {
			log.Fatal(err)
		}

		mnemonic := os.Getenv("MNEMONIC")
		if mnemonic == "" {
			log.Fatal("MNEMONIC environment variable is not set")
		}

		// Subscribe tokens for price monitoring
		subscribeTokens := []string{
			"BTC / USD",
			"ETH / USD",
			"HYPE / USD",
			"UBTC / USD",
			"kHYPE / USD",
			"stHYPE / USD",
			"XAUt / USD",
		}

		numEOAs, err := strconv.Atoi(os.Getenv("NUM_EOAS"))
		if err != nil {
			log.Fatal(err)
		}

		ls, err := liquidator.NewLiquidatorSystem(
			client,
			subscribeTokens,
			mnemonic,
			numEOAs,
			os.Getenv("TELEGRAM_BOT_TOKEN"),
			os.Getenv("TELEGRAM_CHAT_ID_FOR_INFO"),
		)
		if err != nil {
			log.Fatalf("Error creating liquidator system: %v", err)
		}
		ls.Start()

	case "split":
		// go run main.go split <splitAddressAmount> <targetAmount> <sendValue>
		client, err := mev.NewClient(os.Getenv("HYPE_RPC_URL"))
		if err != nil {
			log.Fatal(err)
		}
		splitAddressAmount, err := strconv.Atoi(os.Args[2])
		if err != nil {
			log.Fatal(err)
		}
		targetAmount, err := strconv.ParseFloat(os.Args[3], 64)
		if err != nil {
			log.Fatal(err)
		}
		sendValue, err := strconv.ParseFloat(os.Args[4], 64)
		if err != nil {
			log.Fatal(err)
		}
		pk, err := utils.GetPrivateKey(os.Getenv("HYPE_REPLENISH_PK"))
		if err != nil {
			log.Fatal("Error getting private key")
		}
		eoa := utils.GetAddress(pk)
		splitPks, err := utils.GetPrivateKeysFromMnemonic(os.Getenv("MNEMONIC"), splitAddressAmount)
		if err != nil {
			log.Fatal("Error getting private keys from mnemonic")
		}
		splitAddresses := make([]common.Address, splitAddressAmount)
		for i := range splitPks {
			splitAddresses[i] = utils.GetAddress(splitPks[i])
		}
		nonce, err := client.NonceAt(context.Background(), eoa, nil)
		if err != nil {
			log.Fatal("Error getting pending nonce")
		}
		for i := 0; i < len(splitAddresses); i += 20 {
			end := i + 20
			if end > len(splitAddresses) {
				end = len(splitAddresses)
			}
			splitAddressesBatch := splitAddresses[i:end]
			if i != 0 {
				sendValue = 0
			}
			tx, err := mev.CallSplitContract(
				eoa,
				nonce,
				pk,
				common.HexToAddress(os.Getenv("SPLIT_CONTRACT_ADDRESS")),
				splitAddressesBatch,
				utils.ToWei(big.NewFloat(targetAmount), "ether"),
				utils.ToWei(big.NewFloat(sendValue), "ether"),
				client,
			)
			if err != nil {
				log.Fatalf("Error calling split contract: %v", err)
			}
			log.Printf("Split contract call sent: %s", tx.Hash().Hex())
			nonce++
		}
	default:
		log.Fatalf("Unknown command: %s", os.Args[1])
	}
}
