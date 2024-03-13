package seth

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"slices"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
)

const (
	Priority_Ultra    = "ultra"
	Priority_Fast     = "fast"
	Priority_Standard = "standard"
	Priority_Slow     = "slow"

	Congestion_Low    = "low"
	Congestion_Medium = "medium"
	Congestion_High   = "high"
	Congestion_Ultra  = "ultra"
)

const (
	CongestionStrategy_Simple      = "simple"
	CongestionStrategy_NewestFirst = "newest_first"
)

// CalculateNetworkCongestionMetric calculates a simple congestion metric based on the last N blocks
// by averaging the trend in base fee and the gas used ratio.
func (m *Client) CalculateNetworkCongestionMetric(blocksNumber uint64, strategy string) (float64, error) {
	var getBlockData = func(bn *big.Int) (*types.Block, error) {
		cachedBlock, ok := m.BlockCache.Get(bn.Int64())
		if ok {
			return cachedBlock, nil
		}

		var timeout uint64 = uint64(blocksNumber / 100)
		if timeout < 2 {
			timeout = 2
		} else if timeout > 5 {
			timeout = 5
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
		defer cancel()
		block, err := m.Client.BlockByNumber(ctx, bn)
		if err != nil {
			return nil, err
		}
		m.BlockCache.Set(block)
		return block, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(2*time.Second))
	defer cancel()
	lastBlockNumber, err := m.Client.BlockNumber(ctx)
	if err != nil {
		return 0, err
	}

	L.Trace().Msgf("Block range for gas calculation: %d - %d", lastBlockNumber-blocksNumber, lastBlockNumber)

	lastBlock, err := getBlockData(big.NewInt(int64(lastBlockNumber)))
	if err != nil {
		return 0, err
	}

	var blocks []*types.Block
	blocks = append(blocks, lastBlock)

	channelSize := blocksNumber
	if blocksNumber > 20 {
		channelSize = 20
	}

	var wg sync.WaitGroup
	dataCh := make(chan *types.Block, channelSize)

	go func() {
		for block := range dataCh {
			blocks = append(blocks, block)
		}
	}()

	startTime := time.Now()
	for i := lastBlockNumber; i > lastBlockNumber-blocksNumber; i-- {
		if i == 1 {
			break
		}

		wg.Add(1)
		go func(bn *big.Int) {
			defer wg.Done()
			block, err := getBlockData(bn)
			if err != nil {
				L.Error().Err(err).Msgf("Failed to get block %d data", bn.Int64())
				return
			}
			dataCh <- block
		}(big.NewInt(int64(i)))
	}

	wg.Wait()
	close(dataCh)

	endTime := time.Now()
	L.Debug().Msgf("Time to fetch %d blocks: %v", blocksNumber, endTime.Sub(startTime))

	minBlockCount := int(float64(blocksNumber) * 0.8)
	if len(blocks) < minBlockCount {
		return 0, fmt.Errorf("Failed to fetch enough blocks for congestion calculation. Wanted at least %d, got %d", minBlockCount, len(blocks))
	}

	switch strategy {
	case CongestionStrategy_Simple:
		return calculateSimpleNetworkCongestionMetric(blocks), nil
	case CongestionStrategy_NewestFirst:
		return calculateNewestFirstNetworkCongestionMetric(blocks), nil
	default:
		return 0, fmt.Errorf("Unknown congestion strategy: %s", strategy)
	}
}

// average the trend and gas used ratio for a basic congestion metric
func calculateSimpleNetworkCongestionMetric(blocks []*types.Block) float64 {
	trend := calculateTrend(blocks)
	gasUsedRatio := calculateGasUsedRatio(blocks)

	congestionMetric := (trend + gasUsedRatio) / 2

	return congestionMetric
}

// calculates a congestion metric using a logarithmic function that gives more weight to most recent blocks
func calculateNewestFirstNetworkCongestionMetric(blocks []*types.Block) float64 {
	// sort blocks so that we are sure they are in ascending order
	slices.SortFunc(blocks, func(i, j *types.Block) int {
		return int(i.NumberU64() - j.NumberU64())
	})

	var weightedSum, totalWeight float64
	// Determines how quickly the weight decreases. The lower the number, the higher the weight of newer blocks.
	scaleFactor := 10.0

	// Calculate weights starting from the older to most recent blocks.
	for i, block := range blocks {
		congestion := float64(block.GasUsed()) / float64(block.GasLimit())

		// Applying a logarithmic scale for weights.
		distance := float64(len(blocks) - 1 - i)
		weight := 1.0 / math.Log10(distance+scaleFactor)

		weightedSum += congestion * weight
		totalWeight += weight
	}

	if totalWeight == 0 {
		return 0
	}
	return weightedSum / totalWeight
}

// AdjustPriorityFee adjusts the priority fee within a calculated range based on historical data, current congestion, and priority.
func (m *Client) GetSuggestedEIP1559Fees(ctx context.Context) (maxFeeCap *big.Int, adjustedTipCap *big.Int, err error) {
	var currentGasTip *big.Int
	currentGasTip, err = m.Client.SuggestGasTipCap(ctx)
	if err != nil {
		return
	}

	L.Debug().
		Str("CurrentGasTip", currentGasTip.String()).
		Msg("Current suggested gas tip")

	// Fetch the baseline historical base fee and tip for the selected priority
	var baseFee64, historicalSuggestedTip64 float64
	baseFee64, historicalSuggestedTip64, err = m.HistoricalFeeData(m.Cfg.Network.GasEstimationTxPriority)
	if err != nil {
		return
	}

	L.Debug().
		Float64("BaseFee", baseFee64).
		Float64("HistoricalSuggestedTip", historicalSuggestedTip64).
		Str("Priority", m.Cfg.Network.GasEstimationTxPriority).
		Msg("Historical fee data")

	suggestedGasTip := currentGasTip
	if big.NewInt(int64(historicalSuggestedTip64)).Cmp(suggestedGasTip) > 0 {
		suggestedGasTip = big.NewInt(int64(historicalSuggestedTip64))
	}

	// Adjust the suggestedTip based on current congestion, keeping within reasonable bounds
	var adjustmentFactor float64
	adjustmentFactor, err = getAdjustmentFactor(m.Cfg.Network.GasEstimationTxPriority)
	if err != nil {
		return
	}

	var congestionMetric float64
	congestionMetric, err = m.CalculateNetworkCongestionMetric(m.Cfg.Network.GasEstimationBlocks, CongestionStrategy_NewestFirst)
	if err != nil {
		return
	}

	congestionClassificaion := classifyCongestion(congestionMetric)

	L.Debug().
		Float64("CongestionMetric", congestionMetric).
		Str("CongestionClassificaion", congestionClassificaion).
		Msg("Calculated congestion metric")

	// Calculate adjusted tip based on congestion and priority
	congestionAdjustment := new(big.Float).Mul(big.NewFloat(congestionMetric*adjustmentFactor), new(big.Float).SetFloat64(float64(suggestedGasTip.Int64())))
	congestionAdjustmentInt, _ := congestionAdjustment.Int(nil)

	adjustedTipCap = new(big.Int).Add(suggestedGasTip, congestionAdjustmentInt)
	maxAsBig := big.NewInt(int64(m.Cfg.Network.GasEsimationMaxGasTipCap))

	// Ensure the adjusted tip does not exceed the max priority fee cap
	if adjustedTipCap.Cmp(maxAsBig) > 0 {
		L.Debug().Msg("Adjusted tip exceeds max tip cap")
		adjustedTipCap.Set(maxAsBig)
	}

	// Adjust the max fee based on the base fee, tip, and congestion-based buffer.
	var bufferPercent float64
	bufferPercent, err = getBufferPercent(congestionClassificaion)
	if err != nil {
		return
	}

	// Calculate the base max fee (without buffer) as initialBaseFee + finalTip.
	baseMaxFeeCap := new(big.Int).Add(big.NewInt(int64(baseFee64)), adjustedTipCap)

	// Calculate and apply the buffer.
	buffer := new(big.Float).Mul(new(big.Float).SetInt(baseMaxFeeCap), big.NewFloat(bufferPercent))
	bufferInt, _ := buffer.Int(nil)
	maxFeeCap = new(big.Int).Add(baseMaxFeeCap, bufferInt)

	L.Debug().
		Str("Diff", big.NewInt(0).Sub(adjustedTipCap, suggestedGasTip).String()).
		Str("Original GasTipCap", suggestedGasTip.String()).
		Str("Final GasTipCap", adjustedTipCap.String()).
		Bool("Capped", adjustedTipCap.Cmp(maxAsBig) > 0).
		Msg("Suggested EIP-1559 fees")

	L.Debug().
		Str("Diff", bufferInt.String()).
		Str("Original GasFeeCap", baseMaxFeeCap.String()).
		Str("Final GasFeeCap", maxFeeCap.String()).
		Msg("Suggested EIP-1559 fees")

	L.Debug().
		Float64("CongestionMetric", congestionMetric).
		Str("CongestionClassificaion", congestionClassificaion).
		Float64("AdjustmentFactor", adjustmentFactor).
		Str("Priority", m.Cfg.Network.GasEstimationTxPriority).
		Msg("Suggested EIP-1559 fees")

	return
}

func (m *Client) GetSuggestedLegacyFees(ctx context.Context) (adjustedGasPrice *big.Int, err error) {
	var suggestedGasPrice *big.Int
	suggestedGasPrice, err = m.Client.SuggestGasPrice(ctx)
	if err != nil {
		return
	}

	// Adjust the suggestedTip based on current congestion, keeping within reasonable bounds
	var adjustmentFactor float64
	adjustmentFactor, err = getAdjustmentFactor(m.Cfg.Network.GasEstimationTxPriority)
	if err != nil {
		return
	}

	var congestionMetric float64
	congestionMetric, err = m.CalculateNetworkCongestionMetric(m.Cfg.Network.GasEstimationBlocks, CongestionStrategy_NewestFirst)
	if err != nil {
		return
	}

	congestionClassificaion := classifyCongestion(congestionMetric)

	L.Debug().
		Float64("CongestionMetric", congestionMetric).
		Str("CongestionClassificaion", congestionClassificaion).
		Msg("Calculated congestion metric")

	// Calculate adjusted tip based on congestion and priority
	congestionAdjustment := new(big.Float).Mul(big.NewFloat(congestionMetric*adjustmentFactor), new(big.Float).SetFloat64(float64(suggestedGasPrice.Int64())))
	congestionAdjustmentInt, _ := congestionAdjustment.Int(nil)

	adjustedGasPrice = new(big.Int).Add(suggestedGasPrice, congestionAdjustmentInt)
	maxAsBig := big.NewInt(int64(m.Cfg.Network.GasEsimationMaxGasPrice))

	// Ensure the adjusted gas price does not exceed the max gas price
	if adjustedGasPrice.Cmp(maxAsBig) > 0 {
		L.Debug().Msg("Adjusted tip exceeds max tip cap")
		adjustedGasPrice.Set(maxAsBig)
	}

	// Adjust the max fee based on the base fee, tip, and congestion-based buffer.
	var bufferPercent float64
	bufferPercent, err = getBufferPercent(congestionClassificaion)
	if err != nil {
		return
	}

	// Calculate and apply the buffer.
	buffer := new(big.Float).Mul(new(big.Float).SetInt(adjustedGasPrice), big.NewFloat(bufferPercent))
	bufferInt, _ := buffer.Int(nil)
	adjustedGasPrice = new(big.Int).Add(adjustedGasPrice, bufferInt)

	L.Debug().
		Str("Diff", big.NewInt(0).Sub(adjustedGasPrice, suggestedGasPrice).String()).
		Str("Original GasPrice", suggestedGasPrice.String()).
		Str("Final GasPrice", adjustedGasPrice.String()).
		Bool("Capped", adjustedGasPrice.Cmp(maxAsBig) > 0).
		Msg("Suggested Legacy fees")

	L.Debug().
		Float64("CongestionMetric", congestionMetric).
		Str("CongestionClassificaion", congestionClassificaion).
		Float64("AdjustmentFactor", adjustmentFactor).
		Str("Priority", m.Cfg.Network.GasEstimationTxPriority).
		Msg("Suggested Legacy fees")

	return
}

func getAdjustmentFactor(priority string) (float64, error) {
	switch priority {
	case Priority_Ultra:
		return 1.5, nil
	case Priority_Fast:
		return 1.2, nil
	case Priority_Standard:
		return 1.0, nil
	case Priority_Slow:
		return 0.8, nil
	default:
		return 0, fmt.Errorf("Unknown priority: %s", priority)
	}
}

func getBufferPercent(congestionClassification string) (float64, error) {
	switch congestionClassification {
	case Congestion_Low:
		return 0.05, nil
	case Congestion_Medium:
		return 0.10, nil
	case Congestion_High:
		return 0.15, nil
	case Congestion_Ultra:
		return 0.20, nil
	default:
		return 0, fmt.Errorf("Unknown congestion classification: %s", congestionClassification)
	}
}

func classifyCongestion(congestionMetric float64) string {
	switch {
	case congestionMetric < 0.33:
		return Congestion_Low
	case congestionMetric <= 0.66:
		return Congestion_Medium
	case congestionMetric <= 0.75:
		return Congestion_High
	default:
		return Congestion_Ultra
	}
}

func (m *Client) HistoricalFeeData(priority string) (baseFee float64, historicalGasTipCap float64, err error) {
	estimator := NewGasEstimator(m)
	stats, err := estimator.Stats(m.Cfg.Network.GasEstimationBlocks, 99)
	if err != nil {
		L.Error().
			Err(err).
			Msg("Failed to get fee history. Skipping automation gas estimation")

		return
	} else {
		switch priority {
		case Priority_Ultra:
			baseFee = stats.GasPrice.Max
			historicalGasTipCap = stats.TipCap.Max
		case Priority_Fast:
			baseFee = stats.GasPrice.Perc99
			historicalGasTipCap = stats.TipCap.Perc99
		case Priority_Standard:
			baseFee = stats.GasPrice.Perc50
			historicalGasTipCap = stats.TipCap.Perc50
		case Priority_Slow:
			baseFee = stats.GasPrice.Perc25
			historicalGasTipCap = stats.TipCap.Perc25
		default:
			L.Error().
				Str("Priority", priority).
				Msg("Unknown priority. Skipping automation gas estimation")
			m.Errors = append(m.Errors, err)
		}
	}

	return baseFee, historicalGasTipCap, err
}

// CalculateTrend analyzes the change in base fee to determine congestion trend
func calculateTrend(blocks []*types.Block) float64 {
	var totalIncrease float64
	for i := 1; i < len(blocks); i++ {
		if blocks[i].BaseFee().Cmp(blocks[i-1].BaseFee()) > 0 {
			totalIncrease += 1
		}
	}
	// Normalize the increase by the number of transitions to get an average trend
	trend := totalIncrease / float64(len(blocks)-1)
	return trend
}

// CalculateGasUsedRatio averages the gas used ratio for a sense of how full blocks are
func calculateGasUsedRatio(blocks []*types.Block) float64 {
	var totalRatio float64
	for _, block := range blocks {
		ratio := float64(block.GasUsed()) / float64(block.GasLimit())
		totalRatio += ratio
	}
	averageRatio := totalRatio / float64(len(blocks))
	return averageRatio
}
