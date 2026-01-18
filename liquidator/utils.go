package liquidator

import (
	"context"
	"errors"
	"log"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

func GetPendingFeeHistory(client *ethclient.Client) (*ethereum.FeeHistory, error) {
	feeHistory, err := client.FeeHistory(context.Background(), 2, new(big.Int).SetInt64(rpc.PendingBlockNumber.Int64()), []float64{0.5})
	if err != nil {
		log.Println("Error getting fee history", err)
		return nil, err
	}
	if len(feeHistory.Reward) == 0 {
		return nil, errors.New("no fee history found")
	}
	if len(feeHistory.Reward[0]) == 0 {
		return nil, errors.New("no fee history found")
	}
	return feeHistory, nil
}

func GetMaxPriorityFee(client *ethclient.Client) (*big.Int, error) {
	maxPriorityFee, err := client.SuggestGasTipCap(context.Background())
	if err != nil {
		return nil, err
	}
	return maxPriorityFee, nil
}

// GetMaxReward returns the maximum reward from a 2D array of rewards
// Returns big.NewInt(0) if no rewards found or all rewards are negative
func GetMaxReward(rewards [][]*big.Int) *big.Int {
	maxReward := big.NewInt(0)

	for _, row := range rewards {
		for _, reward := range row {
			if reward != nil && reward.Cmp(maxReward) > 0 {
				maxReward = reward
			}
		}
	}

	return maxReward
}
