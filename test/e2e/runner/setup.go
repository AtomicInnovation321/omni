package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	halocmd "github.com/omni-network/omni/halo/cmd"
	"github.com/omni-network/omni/lib/errors"
	"github.com/omni-network/omni/lib/log"
	"github.com/omni-network/omni/lib/netconf"

	"github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/p2p"
	"github.com/cometbft/cometbft/privval"
	e2e "github.com/cometbft/cometbft/test/e2e/pkg"
	"github.com/cometbft/cometbft/test/e2e/pkg/infra"
	"github.com/cometbft/cometbft/types"

	"github.com/BurntSushi/toml"
)

const (
	AppAddressTCP  = "tcp://127.0.0.1:30000"
	AppAddressUNIX = "unix:///var/run/app.sock"

	PrivvalAddressTCP     = "tcp://0.0.0.0:27559"
	PrivvalAddressUNIX    = "unix:///var/run/privval.sock"
	PrivvalKeyFile        = "config/priv_validator_key.json"
	PrivvalStateFile      = "data/priv_validator_state.json"
	PrivvalDummyKeyFile   = "config/dummy_validator_key.json"
	PrivvalDummyStateFile = "data/dummy_validator_state.json"
	NetworkConfigFile     = "config/network.json"
)

// Setup sets up the testnet configuration.
func Setup(ctx context.Context, testnet *e2e.Testnet, infp infra.Provider) error {
	log.Info(ctx, "Setup testnet", "dir", testnet.Dir)

	if err := os.MkdirAll(testnet.Dir, os.ModePerm); err != nil {
		return errors.Wrap(err, "mkdir")
	}

	if err := infp.Setup(); err != nil {
		return errors.Wrap(err, "setup provider")
	}

	genesis, err := MakeGenesis(testnet)
	if err != nil {
		return err
	}

	for _, node := range testnet.Nodes {
		nodeDir := filepath.Join(testnet.Dir, node.Name)

		dirs := []string{
			filepath.Join(nodeDir, "config"),
			filepath.Join(nodeDir, "data"),
		}
		for _, dir := range dirs {
			err := os.MkdirAll(dir, 0o755)
			if err != nil {
				return errors.Wrap(err, "make dir")
			}
		}

		cfg, err := MakeConfig(node, nodeDir)
		if err != nil {
			return err
		}
		config.WriteConfigFile(filepath.Join(nodeDir, "config", "config.toml"), cfg) // panics

		appCfg, err := MakeHaloConfig(node)
		if err != nil {
			return err
		}
		err = os.WriteFile(filepath.Join(nodeDir, "config", "halo.toml"), appCfg, 0o644)
		if err != nil {
			return errors.Wrap(err, "write halo config")
		}

		err = genesis.SaveAs(filepath.Join(nodeDir, "config", "genesis.json"))
		if err != nil {
			return errors.Wrap(err, "write genesis")
		}

		err = (&p2p.NodeKey{PrivKey: node.NodeKey}).SaveAs(filepath.Join(nodeDir, "config", "node_key.json"))
		if err != nil {
			return err
		}

		(privval.NewFilePV(node.PrivvalKey,
			filepath.Join(nodeDir, PrivvalKeyFile),
			filepath.Join(nodeDir, PrivvalStateFile),
		)).Save()

		if err := writeNetworkConfig(defaultNetwork, filepath.Join(nodeDir, NetworkConfigFile)); err != nil {
			return errors.Wrap(err, "write network config")
		}

		// Initialize the node's data directory (with noop logger since it is noisy).
		initCfg := halocmd.InitConfig{HomeDir: nodeDir, Network: netconf.Simnet}
		if err := halocmd.InitFiles(log.WithNoopLogger(ctx), initCfg); err != nil {
			return errors.Wrap(err, "init files")
		}
	}

	if testnet.Prometheus {
		if err := testnet.WritePrometheusConfig(); err != nil {
			return errors.Wrap(err, "write prom config")
		}
	}

	return nil
}

// MakeGenesis generates a genesis document.
func MakeGenesis(testnet *e2e.Testnet) (types.GenesisDoc, error) {
	genesis := types.GenesisDoc{
		GenesisTime:     time.Now(),
		ChainID:         testnet.Name,
		ConsensusParams: halocmd.DefaultConsensusParams(),
		InitialHeight:   testnet.InitialHeight,
	}
	// set the app version to 1
	genesis.ConsensusParams.Version.App = 1
	genesis.ConsensusParams.Evidence.MaxAgeNumBlocks = e2e.EvidenceAgeHeight
	genesis.ConsensusParams.Evidence.MaxAgeDuration = e2e.EvidenceAgeTime
	for validator, power := range testnet.Validators {
		genesis.Validators = append(genesis.Validators, types.GenesisValidator{
			Name:    validator.Name,
			Address: validator.PrivvalKey.PubKey().Address(),
			PubKey:  validator.PrivvalKey.PubKey(),
			Power:   power,
		})
	}
	// The validator set will be sorted internally by CometBFT ranked by power,
	// but we sort it here as well so that all genesis files are identical.
	sort.Slice(genesis.Validators, func(i, j int) bool {
		return strings.Compare(genesis.Validators[i].Name, genesis.Validators[j].Name) == -1
	})
	if len(testnet.InitialState) > 0 {
		appState, err := json.Marshal(testnet.InitialState)
		if err != nil {
			return genesis, errors.Wrap(err, "marshal initial state")
		}
		genesis.AppState = appState
	}

	if err := genesis.ValidateAndComplete(); err != nil {
		return genesis, errors.Wrap(err, "validate genesis")
	}

	return genesis, nil
}

// MakeConfig generates a CometBFT config for a node.
//
//nolint:lll // CometBFT super long names :(
func MakeConfig(node *e2e.Node, nodeDir string) (*config.Config, error) {
	cfg := halocmd.DefaultCometConfig(nodeDir)
	cfg.Moniker = node.Name
	cfg.ProxyApp = AppAddressTCP
	cfg.RPC.ListenAddress = "tcp://0.0.0.0:26657"
	cfg.RPC.PprofListenAddress = ":6060"
	cfg.P2P.ExternalAddress = fmt.Sprintf("tcp://%v", node.AddressP2P(false))
	cfg.P2P.AddrBookStrict = false
	cfg.DBBackend = node.Database
	cfg.StateSync.DiscoveryTime = 5 * time.Second
	cfg.BlockSync.Version = node.BlockSyncVersion
	cfg.Mempool.ExperimentalMaxGossipConnectionsToNonPersistentPeers = int(node.Testnet.ExperimentalMaxGossipConnectionsToNonPersistentPeers)
	cfg.Mempool.ExperimentalMaxGossipConnectionsToPersistentPeers = int(node.Testnet.ExperimentalMaxGossipConnectionsToPersistentPeers)

	switch node.ABCIProtocol {
	case e2e.ProtocolUNIX:
		cfg.ProxyApp = AppAddressUNIX
	case e2e.ProtocolTCP:
		cfg.ProxyApp = AppAddressTCP
	case e2e.ProtocolGRPC:
		cfg.ProxyApp = AppAddressTCP
		cfg.ABCI = "grpc"
	case e2e.ProtocolBuiltin, e2e.ProtocolBuiltinConnSync:
		cfg.ProxyApp = ""
		cfg.ABCI = ""
	default:
		return nil, errors.New("unexpected ABCI protocol")
	}

	// CometBFT errors if it does not have a privval key set up, regardless of whether
	// it's actually needed (e.g. for remote KMS or non-validators). We set up a dummy
	// key here by default, and use the real key for actual validators that should use
	// the file privval.
	cfg.PrivValidatorListenAddr = ""
	cfg.PrivValidatorKey = PrivvalDummyKeyFile
	cfg.PrivValidatorState = PrivvalDummyStateFile

	switch node.Mode {
	case e2e.ModeValidator:
		switch node.PrivvalProtocol {
		case e2e.ProtocolFile:
			cfg.PrivValidatorKey = PrivvalKeyFile
			cfg.PrivValidatorState = PrivvalStateFile
		case e2e.ProtocolUNIX:
			cfg.PrivValidatorListenAddr = PrivvalAddressUNIX
		case e2e.ProtocolTCP:
			cfg.PrivValidatorListenAddr = PrivvalAddressTCP
		default:
			return nil, errors.New("unexpected privval protocol")
		}
	case e2e.ModeSeed:
		cfg.P2P.SeedMode = true
		cfg.P2P.PexReactor = true
	case e2e.ModeFull, e2e.ModeLight:
		// Don't need to do anything, since we're using a dummy privval key by default.
	default:
		return nil, errors.New("unexpected mode")
	}

	if node.StateSync {
		cfg.StateSync.Enable = true
		cfg.StateSync.RPCServers = []string{}
		for _, peer := range node.Testnet.ArchiveNodes() {
			if peer.Name == node.Name {
				continue
			}
			cfg.StateSync.RPCServers = append(cfg.StateSync.RPCServers, peer.AddressRPC())
		}
		if len(cfg.StateSync.RPCServers) < 2 {
			return nil, errors.New("unable to find 2 suitable state sync RPC servers")
		}
	}

	cfg.P2P.Seeds = ""
	for _, seed := range node.Seeds {
		if len(cfg.P2P.Seeds) > 0 {
			cfg.P2P.Seeds += ","
		}
		cfg.P2P.Seeds += seed.AddressP2P(true)
	}
	cfg.P2P.PersistentPeers = ""
	for _, peer := range node.PersistentPeers {
		if len(cfg.P2P.PersistentPeers) > 0 {
			cfg.P2P.PersistentPeers += ","
		}
		cfg.P2P.PersistentPeers += peer.AddressP2P(true)
	}

	if node.Prometheus {
		cfg.Instrumentation.Prometheus = true
	}

	return &cfg, nil
}

// MakeHaloConfig generates an ABCI application config for a node.
func MakeHaloConfig(node *e2e.Node) ([]byte, error) {
	cfg := map[string]interface{}{
		"chain_id":               node.Testnet.Name,
		"dir":                    "data/app",
		"listen":                 AppAddressUNIX,
		"mode":                   node.Mode,
		"protocol":               "socket",
		"persist_interval":       node.PersistInterval,
		"snapshot_interval":      node.SnapshotInterval,
		"retain_blocks":          node.RetainBlocks,
		"key_type":               node.PrivvalKey.Type(),
		"prepare_proposal_delay": node.Testnet.PrepareProposalDelay,
		"process_proposal_delay": node.Testnet.ProcessProposalDelay,
		"check_tx_delay":         node.Testnet.CheckTxDelay,
		"vote_extension_delay":   node.Testnet.VoteExtensionDelay,
		"finalize_block_delay":   node.Testnet.FinalizeBlockDelay,
	}
	switch node.ABCIProtocol {
	case e2e.ProtocolUNIX:
		cfg["listen"] = AppAddressUNIX
	case e2e.ProtocolTCP:
		cfg["listen"] = AppAddressTCP
	case e2e.ProtocolGRPC:
		cfg["listen"] = AppAddressTCP
		cfg["protocol"] = "grpc"
	case e2e.ProtocolBuiltin, e2e.ProtocolBuiltinConnSync:
		delete(cfg, "listen")
		cfg["protocol"] = string(node.ABCIProtocol)
	default:
		return nil, errors.New("unexpected abci protocol")
	}
	if node.Mode == e2e.ModeValidator {
		switch node.PrivvalProtocol {
		case e2e.ProtocolFile:
		case e2e.ProtocolTCP:
			cfg["privval_server"] = PrivvalAddressTCP
			cfg["privval_key"] = PrivvalKeyFile
			cfg["privval_state"] = PrivvalStateFile
		case e2e.ProtocolUNIX:
			cfg["privval_server"] = PrivvalAddressUNIX
			cfg["privval_key"] = PrivvalKeyFile
			cfg["privval_state"] = PrivvalStateFile
		default:
			return nil, errors.New("unexpected validator mode")
		}
	}

	if len(node.Testnet.ValidatorUpdates) > 0 {
		validatorUpdates := map[string]map[string]int64{}
		for height, validators := range node.Testnet.ValidatorUpdates {
			updateVals := map[string]int64{}
			for node, power := range validators {
				updateVals[base64.StdEncoding.EncodeToString(node.PrivvalKey.PubKey().Bytes())] = power
			}
			validatorUpdates[strconv.FormatInt(height, 10)] = updateVals
		}
		cfg["validator_update"] = validatorUpdates
	}

	var buf bytes.Buffer
	err := toml.NewEncoder(&buf).Encode(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "encode config")
	}

	return buf.Bytes(), nil
}

// UpdateConfigStateSync updates the state sync config for a node.
func UpdateConfigStateSync(node *e2e.Node, height int64, hash []byte) error {
	cfgPath := filepath.Join(node.Testnet.Dir, node.Name, "config", "config.toml")

	// FIXME Apparently there's no function to simply load a config file without
	// involving the entire Viper apparatus, so we'll just resort to regexps.
	bz, err := os.ReadFile(cfgPath)
	if err != nil {
		return errors.Wrap(err, "read config")
	}
	bz = regexp.MustCompile(`(?m)^trust_height =.*`).ReplaceAll(bz, []byte(fmt.Sprintf(`trust_height = %v`, height)))
	bz = regexp.MustCompile(`(?m)^trust_hash =.*`).ReplaceAll(bz, []byte(fmt.Sprintf(`trust_hash = "%X"`, hash)))

	if err := os.WriteFile(cfgPath, bz, 0o644); err != nil {
		return errors.Wrap(err, "write config")
	}

	return nil
}

// writeNetworkConfig writes the network config (adjusted for intra-docker networking) to the given path.
func writeNetworkConfig(network netconf.Network, path string) error {
	// Clone the network since we need to change the RPC URLs for intra-docker networking.
	clone := netconf.Network{
		Name:   network.Name,
		Chains: slices.Clone(network.Chains),
	}

	for i, chain := range clone.Chains {
		clone.Chains[i].RPCURL = fmt.Sprintf("http://%v:8545", chain.Name)
	}

	return netconf.Save(clone, path)
}