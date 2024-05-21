package seth

import (
	"context"
	"fmt"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/pelletier/go-toml/v2"
	"go.uber.org/ratelimit"
	"golang.org/x/sync/errgroup"
	"math"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"
)

type BlockStatsConfig struct {
	RPCRateLimit int `toml:"rpc_requests_per_second_limit"`
}

func (cfg *BlockStatsConfig) Validate() error {
	if cfg.RPCRateLimit == 0 {
		cfg.RPCRateLimit = 3
	}
	return nil
}

// BlockStats is a block stats calculator
type BlockStats struct {
	Limiter ratelimit.Limiter
	Client  *Client
}

// NewBlockStats creates a new instance of BlockStats
func NewBlockStats(c *Client) (*BlockStats, error) {
	return &BlockStats{
		Limiter: ratelimit.New(c.Cfg.BlockStatsConfig.RPCRateLimit, ratelimit.WithoutSlack),
		Client:  c,
	}, nil
}

// Stats fetches and logs the blocks' statistics from startBlock to endBlock
func (cs *BlockStats) Stats(startBlock *big.Int, endBlock *big.Int) error {
	// Get the latest block number if endBlock is nil or if startBlock is negative
	var latestBlockNumber *big.Int
	if endBlock == nil || startBlock.Sign() < 0 {
		header, err := cs.Client.Client.HeaderByNumber(context.Background(), nil)
		if err != nil {
			return fmt.Errorf("failed to get the latest block header: %v", err)
		}
		latestBlockNumber = header.Number
	}

	// Handle case where startBlock is negative
	if startBlock.Sign() < 0 {
		startBlock = new(big.Int).Add(latestBlockNumber, startBlock)
	}

	if endBlock.Int64() == 0 {
		endBlock = latestBlockNumber
	}
	if endBlock != nil && startBlock.Int64() > endBlock.Int64() {
		return fmt.Errorf("start block is less than the end block")
	}
	L.Info().
		Int64("EndBlock", endBlock.Int64()).
		Int64("StartBlock", startBlock.Int64()).
		Msg("Calculating stats for blocks interval")

	blocks := make([]*types.Block, 0)
	blockMu := &sync.Mutex{}
	eg := &errgroup.Group{}
	for bn := startBlock.Int64(); bn <= endBlock.Int64(); bn++ {
		bn := bn
		eg.Go(func() error {
			cs.Limiter.Take()
			block, err := cs.Client.Client.BlockByNumber(context.Background(), big.NewInt(bn))
			if err != nil {
				// invalid blocks on some networks, ignore them for now
				if strings.Contains(err.Error(), "value overflows uint256") {
					return nil
				} else if strings.Contains(err.Error(), "transaction type not supported") {
					return nil
				}
				return err
			}
			blockMu.Lock()
			blocks = append(blocks, block)
			blockMu.Unlock()
			L.Debug().
				Uint64("BlockNumber", block.Number().Uint64()).
				Uint64("Size", block.Size()).
				Uint64("GasUsed", block.GasUsed()).
				Uint64("GasLimit", block.GasLimit()).
				Int("Transactions", len(block.Transactions())).
				Msg("Block info")
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return err
	}
	sort.SliceStable(blocks, func(i, j int) bool {
		return blocks[i].Number().Int64() < blocks[j].Number().Int64()
	})
	return cs.CalculateBlockDurations(blocks)
}

// CalculateBlockDurations calculates and logs the duration, TPS, gas used, and gas limit between each consecutive block
func (cs *BlockStats) CalculateBlockDurations(blocks []*types.Block) error {
	var (
		durations          []time.Duration
		tpsValues          []float64
		gasUsedValues      []uint64
		gasLimitValues     []uint64
		blockBaseFeeValues []uint64
	)
	totalDuration := time.Duration(0)
	totalTransactions := 0
	totalGasUsed := uint64(0)
	totalGasLimit := uint64(0)
	totalBaseFee := uint64(0)

	for i := 1; i < len(blocks); i++ {
		duration := time.Unix(int64(blocks[i].Time()), 0).Sub(time.Unix(int64(blocks[i-1].Time()), 0))
		durations = append(durations, duration)
		totalDuration += duration

		transactions := len(blocks[i].Transactions())
		totalTransactions += transactions

		gasUsed := blocks[i].GasUsed()
		gasLimit := blocks[i].GasLimit()
		blockBaseFee := blocks[i].BaseFee().Uint64()
		gasUsedValues = append(gasUsedValues, gasUsed)
		gasLimitValues = append(gasLimitValues, gasLimit)
		blockBaseFeeValues = append(blockBaseFeeValues, blockBaseFee)
		totalGasUsed += gasUsed
		totalGasLimit += gasLimit
		totalBaseFee += blockBaseFee

		var tps float64
		if duration.Seconds() > 0 {
			tps = float64(transactions) / duration.Seconds()
		} else {
			tps = 0
		}
		tpsValues = append(tpsValues, tps)

		L.Debug().
			Uint64("BlockNumber", blocks[i].Number().Uint64()).
			Str("Duration", duration.String()).
			Float64("TPS", tps).
			Uint64("BlockGasFee", blocks[i].BaseFee().Uint64()).
			Uint64("BlockGasTip", blocks[i].BaseFee().Uint64()).
			Uint64("GasUsed", gasUsed).
			Uint64("GasLimit", gasLimit).
			Msg("Block calculated info")
	}

	// Calculate average TPS, duration, gas used, and gas limit
	averageTPS := float64(totalTransactions) / totalDuration.Seconds()
	averageDuration := totalDuration / time.Duration(len(durations))
	averageGasUsed := totalGasUsed / uint64(len(gasUsedValues))
	averageGasLimit := totalGasLimit / uint64(len(gasLimitValues))
	averageBlockBaseFee := totalBaseFee / uint64(len(blockBaseFeeValues))

	// Calculate 95th percentile TPS, duration, gas used, and gas limit
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	sort.Float64s(tpsValues)
	sort.Slice(gasUsedValues, func(i, j int) bool { return gasUsedValues[i] < gasUsedValues[j] })
	sort.Slice(gasLimitValues, func(i, j int) bool { return gasLimitValues[i] < gasLimitValues[j] })
	sort.Slice(blockBaseFeeValues, func(i, j int) bool { return blockBaseFeeValues[i] < blockBaseFeeValues[j] })

	index95 := int(0.95 * float64(len(durations)))

	percentile95Duration := durations[index95]
	percentile95TPS := tpsValues[index95]
	percentile95GasUsed := gasUsedValues[index95]
	percentile95GasLimit := gasLimitValues[index95]
	percentile95BlockBaseFee := blockBaseFeeValues[index95]

	L.Debug().
		Float64("AverageTPS", averageTPS).
		Dur("AvgBlockDuration", averageDuration).
		Uint64("AvgBlockGasUsed", averageGasUsed).
		Uint64("AvgBlockGasLimit", averageGasLimit).
		Uint64("AvgBlockBaseFee", averageBlockBaseFee).
		Dur("95thBlockDuration", percentile95Duration).
		Float64("95thTPS", percentile95TPS).
		Uint64("95thBlockGasUsed", percentile95GasUsed).
		Uint64("95thBlockGasLimit", percentile95GasLimit).
		Uint64("95thBlockBaseFee", percentile95BlockBaseFee).
		Float64("RequiredGasBumpPercentage", calculateRatioPercentage(percentile95BlockBaseFee, averageBlockBaseFee)).
		Msg("Summary")

	type stats struct {
		Perc95TPS           float64 `toml:"perc_95_tps"`
		Perc95BlockDuration string  `toml:"perc_95_block_duration"`
		Perc95BlockGasUsed  uint64  `toml:"perc_95_block_gas_used"`
		Perc95BlockGasLimit uint64  `toml:"perc_95_block_gas_limit"`
		Perc95BlockBaseFee  uint64  `toml:"perc_95_block_base_fee"`
		AvgTPS              float64 `toml:"avg_tps"`
		AvgBlockDuration    string  `toml:"avg_block_duration"`
		AvgBlockGasUsed     uint64  `toml:"avg_block_gas_used"`
		AvgBlockGasLimit    uint64  `toml:"avg_block_gas_limit"`
		AvgBlockBaseFee     uint64  `toml:"avg_block_base_fee"`
	}

	type performanceTestStats struct {
		Duration                 string  `toml:"duration"`
		GasInitialValue          uint64  `toml:"avg_block_gas_base_fee_initial_value"`
		GasBaseFeeBumpPercentage string  `toml:"avg_block_gas_base_fee_bump_percentage"`
		GasUsagePercentage       string  `toml:"avg_block_gas_usage_percentage"`
		TPSStable                float64 `toml:"avg_tps"`
		TPSMax                   float64 `toml:"max_tps"`
	}

	tomlCfg := stats{
		Perc95TPS:           percentile95TPS,
		Perc95BlockDuration: percentile95Duration.String(),
		Perc95BlockGasUsed:  percentile95GasUsed,
		Perc95BlockGasLimit: percentile95GasLimit,
		Perc95BlockBaseFee:  percentile95BlockBaseFee,
		AvgTPS:              averageTPS,
		AvgBlockDuration:    averageDuration.String(),
		AvgBlockGasUsed:     averageGasUsed,
		AvgBlockGasLimit:    averageGasLimit,
		AvgBlockBaseFee:     averageBlockBaseFee,
	}

	var bumpMsg string
	bump := calculateRatioPercentage(percentile95BlockBaseFee, averageBlockBaseFee)
	if bump == 100.0 {
		bumpMsg = fmt.Sprintf("%.2f%% (no bump required)", bump)
	} else {
		bumpMsg = fmt.Sprintf("%.2f%% (multiply)", bump)
	}
	var blockGasUsagePercentageMsg string
	blockGasUsagePerc := calculateRatioPercentage(averageGasUsed, averageGasLimit)
	if blockGasUsagePerc >= 100 {
		blockGasUsagePercentageMsg = fmt.Sprintf("%.8f%% gas used (network is congested)", blockGasUsagePerc)
	} else {
		blockGasUsagePercentageMsg = fmt.Sprintf("%.8f%% gas used (no congestion)", blockGasUsagePerc)
	}

	perfStats := performanceTestStats{
		Duration:                 totalDuration.String(),
		GasInitialValue:          averageBlockBaseFee,
		TPSStable:                math.Ceil(averageTPS),
		TPSMax:                   math.Ceil(percentile95TPS),
		GasUsagePercentage:       blockGasUsagePercentageMsg,
		GasBaseFeeBumpPercentage: bumpMsg,
	}

	marshalled, err := toml.Marshal(tomlCfg)
	if err != nil {
		return err
	}
	L.Info().Msgf("Stats:\n%s", string(marshalled))

	marshalled, err = toml.Marshal(perfStats)
	if err != nil {
		return err
	}
	L.Info().Msgf("Recommended performance/chaos test parameters:\n%s", string(marshalled))
	return nil
}

// calculateRatioPercentage calculates the ratio between two uint64 values and returns it as a percentage
func calculateRatioPercentage(value1, value2 uint64) float64 {
	if value2 == 0 {
		return 0.0
	}
	ratio := float64(value1) / float64(value2) * 100
	return ratio
}
