package managed

import (
	"context"
	"fmt"
	"time"

	"github.com/smartcontractkit/libocr/offchainreporting/internal/config"
	"github.com/smartcontractkit/libocr/offchainreporting/internal/protocol"
	"github.com/smartcontractkit/libocr/offchainreporting/internal/shim"
	"github.com/smartcontractkit/libocr/offchainreporting/loghelper"
	"github.com/smartcontractkit/libocr/offchainreporting/types"
	"github.com/smartcontractkit/libocr/subprocesses"
)

func RunManagedOracle(
	ctx context.Context,

	bootstrappers []string,
	configTracker types.ContractConfigTracker,
	contractTransmitter types.ContractTransmitter,
	database types.Database,
	datasource types.DataSource,
	localConfig types.LocalConfig,
	logger types.Logger,
	monitoringEndpoint types.MonitoringEndpoint,
	netEndpointFactory types.BinaryNetworkEndpointFactory,
	privateKeys types.PrivateKeys,
) {
	mo := managedOracleState{
		ctx: ctx,

		bootstrappers:       bootstrappers,
		configTracker:       configTracker,
		contractTransmitter: contractTransmitter,
		database:            database,
		datasource:          datasource,
		localConfig:         localConfig,
		logger:              logger,
		monitoringEndpoint:  monitoringEndpoint,
		netEndpointFactory:  netEndpointFactory,
		privateKeys:         privateKeys,
	}
	mo.run()
}




type managedOracleState struct {
	ctx context.Context

	bootstrappers       []string
	config              config.SharedConfig
	configTracker       types.ContractConfigTracker
	contractTransmitter types.ContractTransmitter
	database            types.Database
	datasource          types.DataSource
	localConfig         types.LocalConfig
	logger              types.Logger
	monitoringEndpoint  types.MonitoringEndpoint
	netEndpointFactory  types.BinaryNetworkEndpointFactory
	privateKeys         types.PrivateKeys

	netEndpoint        *shim.SerializingEndpoint
	oracleCancel       context.CancelFunc
	oracleSubprocesses subprocesses.Subprocesses
	otherSubprocesses  subprocesses.Subprocesses
}

func (mo *managedOracleState) run() {
	
	
	{
		var cc *types.ContractConfig
		ok := mo.otherSubprocesses.BlockForAtMost(
			mo.ctx,
			mo.localConfig.DatabaseTimeout,
			func(ctx context.Context) {
				cc = loadConfigFromDatabase(ctx, mo.database, mo.logger)
			},
		)
		if !ok {
			mo.logger.Error("ManagedOracle: database timed out while attempting to restore configuration", types.LogFields{
				"timeout": mo.localConfig.DatabaseTimeout,
			})
		} else if cc != nil {
			mo.configChanged(*cc)
		}
	}

	chNewConfig := make(chan types.ContractConfig, 5)
	mo.otherSubprocesses.Go(func() {
		TrackConfig(mo.ctx, mo.configTracker, mo.localConfig, mo.logger, chNewConfig)
	})

	mo.otherSubprocesses.Go(func() {
		collectGarbage(mo.ctx, mo.database, mo.localConfig, mo.logger)
	})

	for {
		select {
		case change := <-chNewConfig:
			mo.logger.Info("ManagedOracle: switching between configs", types.LogFields{
				"oldConfigDigest": mo.config.ConfigDigest.Hex(),
				"newConfigDigest": change.ConfigDigest.Hex(),
			})
			mo.configChanged(change)
		case <-mo.ctx.Done():
			mo.logger.Info("ManagedOracle: winding down", nil)
			mo.closeOracle()
			mo.otherSubprocesses.Wait()
			mo.logger.Info("ManagedOracle: exiting", nil)
			return 
		}
	}
}

func (mo *managedOracleState) closeOracle() {
	if mo.oracleCancel != nil {
		mo.oracleCancel()
		mo.oracleSubprocesses.Wait()
		err := mo.netEndpoint.Close()
		if err != nil {
			mo.logger.Error("ManagedOracle: error while closing BinaryNetworkEndpoint", types.LogFields{
				"error": err,
			})
			
		}
		mo.oracleCancel = nil
		mo.netEndpoint = nil
	}
}

func (mo *managedOracleState) configChanged(contractConfig types.ContractConfig) {
	
	mo.closeOracle()

	
	var err error
	var oid types.OracleID
	mo.config, oid, err = config.SharedConfigFromContractConfig(
		contractConfig,
		mo.privateKeys,
		mo.netEndpointFactory.PeerID(),
		mo.contractTransmitter.FromAddress(),
	)
	if err != nil {
		mo.logger.Error("ManagedOracle: error while updating config", types.LogFields{
			"error": err,
		})
		return
	}

	
	peerIDs := []string{}
	for _, identity := range mo.config.OracleIdentities {
		peerIDs = append(peerIDs, identity.PeerID)
	}

	childLogger := loghelper.MakeLoggerWithContext(mo.logger, types.LogFields{
		"configDigest": fmt.Sprintf("%x", mo.config.ConfigDigest),
		"oid":          oid,
	})

	binNetEndpoint, err := mo.netEndpointFactory.MakeEndpoint(mo.config.ConfigDigest, peerIDs,
		mo.bootstrappers, mo.config.F, computeTokenBucketRefillRate(mo.config.PublicConfig),
		computeTokenBucketSize())
	if err != nil {
		mo.logger.Error("ManagedOracle: error during MakeEndpoint", types.LogFields{
			"error":         err,
			"configDigest":  mo.config.ConfigDigest,
			"peerIDs":       peerIDs,
			"bootstrappers": mo.bootstrappers,
		})
		return
	}

	netEndpoint := shim.NewSerializingEndpoint(binNetEndpoint, childLogger)

	if err := netEndpoint.Start(); err != nil {
		mo.logger.Error("ManagedOracle: error during netEndpoint.Start()", types.LogFields{
			"error":        err,
			"configDigest": mo.config.ConfigDigest,
		})
		return
	}

	mo.netEndpoint = netEndpoint
	oracleCtx, oracleCancel := context.WithCancel(mo.ctx)
	mo.oracleCancel = oracleCancel
	mo.oracleSubprocesses.Go(func() {
		defer oracleCancel()
		protocol.RunOracle(
			oracleCtx,
			mo.config,
			mo.contractTransmitter,
			mo.database,
			mo.datasource,
			oid,
			mo.privateKeys,
			mo.localConfig,
			childLogger,
			mo.monitoringEndpoint,
			mo.netEndpoint,
		)
	})

	childCtx, childCancel := context.WithTimeout(mo.ctx, mo.localConfig.DatabaseTimeout)
	defer childCancel()
	if err := mo.database.WriteConfig(childCtx, contractConfig); err != nil {
		mo.logger.Error("ManagedOracle: error writing new config to database", types.LogFields{
			"configDigest": mo.config.ConfigDigest,
			"config":       contractConfig,
			"error":        err,
		})
	}
}


































func computeTokenBucketRefillRate(cfg config.PublicConfig) float64 {
	return (1.0*float64(time.Second)/float64(cfg.DeltaResend) +
		1.0*float64(time.Second)/float64(cfg.DeltaProgress) +
		1.0*float64(time.Second)/float64(cfg.DeltaRound) +
		3.0*float64(time.Second)/float64(cfg.DeltaRound) +
		2.0*float64(time.Second)/float64(cfg.DeltaRound)) * 2.0
}

func computeTokenBucketSize() int {
	return (2 + 6) * 2
}
