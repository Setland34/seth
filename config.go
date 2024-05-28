package seth

import (
	"crypto/ecdsa"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/pelletier/go-toml/v2"
	"github.com/pkg/errors"
)

const (
	ErrReadSethConfig         = "failed to read TOML config for seth"
	ErrReadKeyFileConfig      = "failed to read TOML keyfile config"
	ErrUnmarshalSethConfig    = "failed to unmarshal TOML config for seth"
	ErrUnmarshalKeyFileConfig = "failed to unmarshal TOML keyfile config for seth"
	ErrEmptyRootPrivateKey    = "no private keys were set, set %s=..."

	GETH  = "Geth"
	ANVIL = "Anvil"

	CONFIG_FILE_ENV_VAR    = "SETH_CONFIG_PATH"
	KEYFILE_BASE64_ENV_VAR = "SETH_KEYFILE_BASE64"
	KEYFILE_PATH_ENV_VAR   = "SETH_KEYFILE_PATH"

	ROOT_PRIVATE_KEY_ENV_VAR = "SETH_ROOT_PRIVATE_KEY"
	NETWORK_ENV_VAR          = "SETH_NETWORK"
	CHAIN_ID_ENV_VAR         = "SETH_CHAIN_ID"
	URL_ENV_VAR              = "SETH_URL"

	DefaultNetworkName = "Default"
	DefaultChainID     = "-1"
)

type KeyFileSource string

const (
	KeyFileSourceBase64EnvVar KeyFileSource = "base64_env"
	KeyFileSourceFile         KeyFileSource = "file"
)

type Config struct {
	// internal fields
	RevertedTransactionsFile string
	ephemeral                bool

	// external fields
	KeyFileSource                 KeyFileSource     `toml:"keyfile_source"`
	KeyFilePath                   string            `toml:"keyfile_path"`
	EphemeralAddrs                *int64            `toml:"ephemeral_addresses_number"`
	RootKeyFundsBuffer            *int64            `toml:"root_key_funds_buffer"`
	ABIDir                        string            `toml:"abi_dir"`
	BINDir                        string            `toml:"bin_dir"`
	ContractMapFile               string            `toml:"contract_map_file"`
	SaveDeployedContractsMap      bool              `toml:"save_deployed_contracts_map"`
	Network                       *Network          `toml:"network"`
	Networks                      []*Network        `toml:"networks"`
	NonceManager                  *NonceManagerCfg  `toml:"nonce_manager"`
	TracingLevel                  string            `toml:"tracing_level"`
	TraceToJson                   bool              `toml:"trace_to_json"`
	PendingNonceProtectionEnabled bool              `toml:"pending_nonce_protection_enabled"`
	ConfigDir                     string            `toml:"abs_path"`
	ExperimentsEnabled            []string          `toml:"experiments_enabled"`
	CheckRpcHealthOnStart         bool              `toml:"check_rpc_health_on_start"`
	BlockStatsConfig              *BlockStatsConfig `toml:"block_stats"`
}

type NonceManagerCfg struct {
	KeySyncRateLimitSec int       `toml:"key_sync_rate_limit_per_sec"`
	KeySyncTimeout      *Duration `toml:"key_sync_timeout"`
	KeySyncRetries      uint      `toml:"key_sync_retries"`
	KeySyncRetryDelay   *Duration `toml:"key_sync_retry_delay"`
}

type Network struct {
	Name                         string    `toml:"name"`
	ChainID                      string    `toml:"chain_id"`
	URLs                         []string  `toml:"urls_secret"`
	EIP1559DynamicFees           bool      `toml:"eip_1559_dynamic_fees"`
	GasPrice                     int64     `toml:"gas_price"`
	GasFeeCap                    int64     `toml:"gas_fee_cap"`
	GasTipCap                    int64     `toml:"gas_tip_cap"`
	GasLimit                     uint64    `toml:"gas_limit"`
	TxnTimeout                   *Duration `toml:"transaction_timeout"`
	TransferGasFee               int64     `toml:"transfer_gas_fee"`
	PrivateKeys                  []string  `toml:"private_keys_secret"`
	GasPriceEstimationEnabled    bool      `toml:"gas_price_estimation_enabled"`
	GasPriceEstimationBlocks     uint64    `toml:"gas_price_estimation_blocks"`
	GasPriceEstimationTxPriority string    `toml:"gas_price_estimation_tx_priority"`
}

// ReadConfig reads the TOML config file from location specified by env var "SETH_CONFIG_PATH" and returns a Config struct
func ReadConfig() (*Config, error) {
	cfgPath := os.Getenv(CONFIG_FILE_ENV_VAR)
	if cfgPath == "" {
		return nil, errors.New(ErrEmptyConfigPath)
	}
	var cfg *Config
	d, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, errors.Wrap(err, ErrReadSethConfig)
	}
	err = toml.Unmarshal(d, &cfg)
	if err != nil {
		return nil, errors.Wrap(err, ErrUnmarshalSethConfig)
	}
	absPath, err := filepath.Abs(cfgPath)
	if err != nil {
		return nil, err
	}
	cfg.ConfigDir = filepath.Dir(absPath)
	snet := os.Getenv(NETWORK_ENV_VAR)
	if snet != "" {
		for _, n := range cfg.Networks {
			if n.Name == snet {
				cfg.Network = n
				break
			}
		}
		if cfg.Network == nil {
			return nil, fmt.Errorf("network %s not defined in the TOML file", snet)
		}
	} else {
		chainId := os.Getenv(CHAIN_ID_ENV_VAR)
		url := os.Getenv(URL_ENV_VAR)

		if chainId == "" || url == "" {
			return nil, fmt.Errorf("network not selected, set %s=... or %s=... and %s=..., check TOML config for available networks", NETWORK_ENV_VAR, CHAIN_ID_ENV_VAR, URL_ENV_VAR)
		}

		//look for default network
		for _, n := range cfg.Networks {
			if n.Name == DefaultNetworkName {
				cfg.Network = n
				cfg.Network.Name = DefaultNetworkName
				cfg.Network.ChainID = chainId
				cfg.Network.URLs = []string{url}
				break
			}
		}

		if cfg.Network == nil {
			return nil, fmt.Errorf("default network not defined in the TOML file")
		}
	}

	rootPrivateKey := os.Getenv(ROOT_PRIVATE_KEY_ENV_VAR)
	if rootPrivateKey == "" {
		return nil, errors.Errorf(ErrEmptyRootPrivateKey, ROOT_PRIVATE_KEY_ENV_VAR)
	} else {
		cfg.Network.PrivateKeys = append(cfg.Network.PrivateKeys, rootPrivateKey)
	}
	L.Trace().Interface("Config", cfg).Msg("Parsed seth config")
	return cfg, nil
}

// ParseKeys parses private keys from the config
func (c *Config) ParseKeys() ([]common.Address, []*ecdsa.PrivateKey, error) {
	addresses := make([]common.Address, 0)
	privKeys := make([]*ecdsa.PrivateKey, 0)
	for _, k := range c.Network.PrivateKeys {
		privateKey, err := crypto.HexToECDSA(k)
		if err != nil {
			return nil, nil, err
		}
		publicKey := privateKey.Public()
		publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
		if !ok {
			return nil, nil, err
		}
		pubKeyAddress := crypto.PubkeyToAddress(*publicKeyECDSA)
		addresses = append(addresses, pubKeyAddress)
		privKeys = append(privKeys, privateKey)
	}
	return addresses, privKeys, nil
}

// IsSimulatedNetwork returns true if the network is simulated (i.e. Geth or Anvil)
func (c *Config) IsSimulatedNetwork() bool {
	networkName := strings.ToLower(c.Network.Name)
	return networkName == strings.ToLower(GETH) || networkName == strings.ToLower(ANVIL)
}

// GenerateContractMapFileName generates a file name for the contract map
func (c *Config) GenerateContractMapFileName() string {
	networkName := strings.ToLower(c.Network.Name)
	now := time.Now().Format("2006-01-02-15-04-05")
	return fmt.Sprintf(ContractMapFilePattern, networkName, now)
}

// ShoulSaveDeployedContractMap returns true if the contract map should be saved (i.e. not a simulated network and functionality is enabled)
func (c *Config) ShoulSaveDeployedContractMap() bool {
	return !c.IsSimulatedNetwork() && c.SaveDeployedContractsMap
}

func readKeyFileConfig(cfg *Config) error {
	if cfg.KeyFileSource == "" {
		return nil
	}

	var err error
	var kf *KeyFile
	var kfd []byte

	if cfg.KeyFileSource == KeyFileSourceFile {
		if _, err := os.Stat(cfg.KeyFilePath); os.IsNotExist(err) {
			return nil
		}
		kfd, err = os.ReadFile(cfg.KeyFilePath)
		if err != nil {
			return errors.Wrap(err, ErrReadKeyFileConfig)
		}
		L.Debug().Msgf("Found keyfile file '%s' found", cfg.KeyFilePath)
	} else {
		keyFileEncoded, isSet := os.LookupEnv(KEYFILE_BASE64_ENV_VAR)
		if isSet && keyFileEncoded != "" {
			L.Debug().Msgf("Found base64 keyfile environment variable '%s' found", KEYFILE_BASE64_ENV_VAR)
			kfd, err = base64.StdEncoding.DecodeString(keyFileEncoded)
			if err != nil {
				return err
			}
		}
	}

	err = toml.Unmarshal(kfd, &kf)
	if err != nil {
		return errors.Wrap(err, ErrUnmarshalKeyFileConfig)
	}
	for _, pk := range kf.Keys {
		cfg.Network.PrivateKeys = append(cfg.Network.PrivateKeys, pk.PrivateKey)
	}

	return nil
}

func (c *Config) setEphemeralAddrs() {
	if c.EphemeralAddrs == nil {
		c.EphemeralAddrs = &ZeroInt64
	}

	if *c.EphemeralAddrs == 0 {
		c.ephemeral = false
	} else {
		c.ephemeral = true
	}

	if c.RootKeyFundsBuffer == nil {
		c.RootKeyFundsBuffer = &ZeroInt64
	}
}

const (
	Experiment_SlowFundsReturn    = "slow_funds_return"
	Experiment_Eip1559FeeEqualier = "eip_1559_fee_equalizer"
)

func (c *Config) IsExperimentEnabled(experiment string) bool {
	for _, e := range c.ExperimentsEnabled {
		if e == experiment {
			return true
		}
	}
	return false
}

// GetMaxConcurrency returns the maximum number of concurrent transactions. Root key is excluded from the count.
func (c *Config) GetMaxConcurrency() int {
	if c.ephemeral {
		return int(*c.EphemeralAddrs)
	}

	return len(c.Network.PrivateKeys) - 1
}
