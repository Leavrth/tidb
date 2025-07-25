// Copyright 2021 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package telemetry

import (
	"context"
	"errors"
	"strconv"

	"github.com/pingcap/tidb/br/pkg/utils"
	"github.com/pingcap/tidb/pkg/config"
	"github.com/pingcap/tidb/pkg/infoschema"
	"github.com/pingcap/tidb/pkg/meta/model"
	m "github.com/pingcap/tidb/pkg/metrics"
	"github.com/pingcap/tidb/pkg/sessionctx"
	"github.com/pingcap/tidb/pkg/sessionctx/vardef"
	"github.com/pingcap/tidb/pkg/util/logutil"
	"github.com/pingcap/tidb/pkg/util/memory"
	"github.com/pingcap/tidb/pkg/util/sqlexec"
	"github.com/tikv/client-go/v2/metrics"
	"go.uber.org/zap"
)

type featureUsage struct {
	// transaction usage information
	Txn                       *TxnUsage                        `json:"txn"`
	NewClusterIndex           *NewClusterIndexUsage            `json:"newClusterIndex"`
	TemporaryTable            bool                             `json:"temporaryTable"`
	CTE                       *m.CTEUsageCounter               `json:"cte"`
	AccountLock               *m.AccountLockCounter            `json:"accountLock"`
	CachedTable               bool                             `json:"cachedTable"`
	AutoCapture               bool                             `json:"autoCapture"`
	PlacementPolicyUsage      *placementPolicyUsage            `json:"placementPolicy"`
	NonTransactionalUsage     *m.NonTransactionalStmtCounter   `json:"nonTransactional"`
	GlobalKill                bool                             `json:"globalKill"`
	MultiSchemaChange         *m.MultiSchemaChangeUsageCounter `json:"multiSchemaChange"`
	ExchangePartition         *m.ExchangePartitionUsageCounter `json:"exchangePartition"`
	TablePartition            *m.TablePartitionUsageCounter    `json:"tablePartition"`
	LogBackup                 bool                             `json:"logBackup"`
	EnablePaging              bool                             `json:"enablePaging"`
	EnableCostModelVer2       bool                             `json:"enableCostModelVer2"`
	DDLUsageCounter           *m.DDLUsageCounter               `json:"DDLUsageCounter"`
	EnableGlobalMemoryControl bool                             `json:"enableGlobalMemoryControl"`
	AutoIDNoCache             bool                             `json:"autoIDNoCache"`
	IndexMergeUsageCounter    *m.IndexMergeUsageCounter        `json:"indexMergeUsageCounter"`
	ResourceControlUsage      *resourceControlUsage            `json:"resourceControl"`
	TTLUsage                  *ttlUsageCounter                 `json:"ttlUsage"`
	StoreBatchCoprUsage       *m.StoreBatchCoprCounter         `json:"storeBatchCopr"`
}

type placementPolicyUsage struct {
	NumPlacementPolicies uint64 `json:"numPlacementPolicies"`
	NumDBWithPolicies    uint64 `json:"numDBWithPolicies"`
	NumTableWithPolicies uint64 `json:"numTableWithPolicies"`
	// The number of partitions that policies are explicitly specified.
	NumPartitionWithExplicitPolicies uint64 `json:"numPartitionWithExplicitPolicies"`
}

type resourceControlUsage struct {
	Enabled           bool   `json:"resourceControlEnabled"`
	NumResourceGroups uint64 `json:"numResourceGroups"`
}

func getFeatureUsage(ctx context.Context, sctx sessionctx.Context) (*featureUsage, error) {
	var usage featureUsage
	var err error
	usage.NewClusterIndex, err = getClusterIndexUsageInfo(ctx, sctx)
	if err != nil {
		logutil.BgLogger().Info("Failed to get feature usage", zap.Error(err))
		return nil, err
	}

	// transaction related feature
	usage.Txn = getTxnUsageInfo(sctx)

	usage.CTE = getCTEUsageInfo()

	usage.AccountLock = getAccountLockUsageInfo()

	usage.MultiSchemaChange = getMultiSchemaChangeUsageInfo()

	usage.ExchangePartition = getExchangePartitionUsageInfo()

	usage.TablePartition = getTablePartitionUsageInfo()

	usage.AutoCapture = getAutoCaptureUsageInfo(sctx)

	collectFeatureUsageFromInfoschema(sctx, &usage)

	usage.NonTransactionalUsage = getNonTransactionalUsage()

	usage.GlobalKill = getGlobalKillUsageInfo()

	usage.LogBackup = getLogBackupUsageInfo(sctx)

	usage.EnablePaging = getPagingUsageInfo(sctx)

	usage.EnableCostModelVer2 = getCostModelVer2UsageInfo(sctx)

	usage.DDLUsageCounter = getDDLUsageInfo(sctx)

	usage.EnableGlobalMemoryControl = getGlobalMemoryControl()

	usage.IndexMergeUsageCounter = getIndexMergeUsageInfo()

	usage.TTLUsage = getTTLUsageInfo(ctx, sctx)

	usage.StoreBatchCoprUsage = getStoreBatchUsage(sctx)

	return &usage, nil
}

// collectFeatureUsageFromInfoschema updates the usage for temporary table, cached table, placement policies and resource groups.
func collectFeatureUsageFromInfoschema(ctx sessionctx.Context, usage *featureUsage) {
	if usage.PlacementPolicyUsage == nil {
		usage.PlacementPolicyUsage = &placementPolicyUsage{}
	}

	is := GetDomainInfoSchema(ctx)
	for _, dbInfo := range is.AllSchemas() {
		if dbInfo.PlacementPolicyRef != nil {
			usage.PlacementPolicyUsage.NumDBWithPolicies++
		}

		tblInfos, err := is.SchemaTableInfos(context.Background(), dbInfo.Name)
		if err != nil {
			return
		}
		for _, tbInfo := range tblInfos {
			if tbInfo.TempTableType != model.TempTableNone {
				usage.TemporaryTable = true
			}
			if tbInfo.TableCacheStatusType != model.TableCacheStatusDisable {
				usage.CachedTable = true
			}
			if tbInfo.PlacementPolicyRef != nil {
				usage.PlacementPolicyUsage.NumTableWithPolicies++
			}
			if tbInfo.AutoIDCache == 1 {
				usage.AutoIDNoCache = true
			}
			partitions := tbInfo.GetPartitionInfo()
			if partitions == nil {
				continue
			}
			for _, partitionInfo := range partitions.Definitions {
				if partitionInfo.PlacementPolicyRef != nil {
					usage.PlacementPolicyUsage.NumPartitionWithExplicitPolicies++
				}
			}
		}
	}
	usage.PlacementPolicyUsage.NumPlacementPolicies += uint64(len(is.AllPlacementPolicies()))

	if usage.ResourceControlUsage == nil {
		usage.ResourceControlUsage = &resourceControlUsage{}
	}
	usage.ResourceControlUsage.NumResourceGroups = uint64(len(is.AllResourceGroups()))
	usage.ResourceControlUsage.Enabled = vardef.EnableResourceControl.Load()
}

// GetDomainInfoSchema is used by the telemetry package to get the latest schema information
// while avoiding circle dependency with domain package.
var GetDomainInfoSchema func(sessionctx.Context) infoschema.InfoSchema

// TableClusteredInfo records the usage info of clusterindex of each table
// CLUSTERED, NON_CLUSTERED, NA
type TableClusteredInfo struct {
	IsClustered   bool   `json:"isClustered"`   // True means CLUSTERED, False means NON_CLUSTERED
	ClusterPKType string `json:"clusterPKType"` // INT means clustered PK type is int
	// NON_INT means clustered PK type is not int
	// NA means this field is no meaningful information
}

// NewClusterIndexUsage records the clustered index usage info of all the tables.
type NewClusterIndexUsage struct {
	// The number of user's tables with clustered index enabled.
	NumClusteredTables uint64 `json:"numClusteredTables"`
	// The number of user's tables.
	NumTotalTables uint64 `json:"numTotalTables"`
}

// getClusterIndexUsageInfo gets the ClusterIndex usage information. It's exported for future test.
func getClusterIndexUsageInfo(ctx context.Context, sctx sessionctx.Context) (ncu *NewClusterIndexUsage, err error) {
	var newUsage NewClusterIndexUsage
	exec := sctx.(sqlexec.RestrictedSQLExecutor)

	// query INFORMATION_SCHEMA.tables to get the latest table information about ClusterIndex
	rows, _, err := exec.ExecRestrictedSQL(ctx, nil, `
		SELECT TIDB_PK_TYPE
		FROM information_schema.tables
		WHERE table_schema not in ('INFORMATION_SCHEMA', 'METRICS_SCHEMA', 'PERFORMANCE_SCHEMA', 'mysql')`)
	if err != nil {
		return nil, err
	}

	defer func() {
		if r := recover(); r != nil {
			switch x := r.(type) {
			case string:
				err = errors.New(x)
			case error:
				err = x
			default:
				err = errors.New("unknown failure")
			}
		}
	}()

	err = sctx.RefreshTxnCtx(ctx)
	if err != nil {
		return nil, err
	}

	// check ClusterIndex information for each table
	// row: 0 = TIDB_PK_TYPE
	for _, row := range rows {
		if row.Len() < 1 {
			continue
		}
		if row.GetString(0) == "CLUSTERED" {
			newUsage.NumClusteredTables++
		}
	}
	newUsage.NumTotalTables = uint64(len(rows))
	return &newUsage, nil
}

// TxnUsage records the usage info of transaction related features, including
// async-commit, 1PC and counters of transactions committed with different protocols.
type TxnUsage struct {
	AsyncCommitUsed           bool                      `json:"asyncCommitUsed"`
	OnePCUsed                 bool                      `json:"onePCUsed"`
	TxnCommitCounter          metrics.TxnCommitCounter  `json:"txnCommitCounter"`
	MutationCheckerUsed       bool                      `json:"mutationCheckerUsed"`
	AssertionLevel            string                    `json:"assertionLevel"`
	RcCheckTS                 bool                      `json:"rcCheckTS"`
	RCWriteCheckTS            bool                      `json:"rcWriteCheckTS"`
	FairLocking               bool                      `json:"fairLocking"`
	SavepointCounter          int64                     `json:"SavepointCounter"`
	LazyUniqueCheckSetCounter int64                     `json:"lazyUniqueCheckSetCounter"`
	FairLockingUsageCounter   m.FairLockingUsageCounter `json:"FairLockingUsageCounter"`
}

var initialTxnCommitCounter metrics.TxnCommitCounter
var initialCTECounter m.CTEUsageCounter
var initialAccountLockCounter m.AccountLockCounter
var initialNonTransactionalCounter m.NonTransactionalStmtCounter
var initialMultiSchemaChangeCounter m.MultiSchemaChangeUsageCounter
var initialExchangePartitionCounter m.ExchangePartitionUsageCounter
var initialTablePartitionCounter m.TablePartitionUsageCounter
var initialSavepointStmtCounter int64
var initialLazyPessimisticUniqueCheckSetCount int64
var initialDDLUsageCounter m.DDLUsageCounter
var initialIndexMergeCounter m.IndexMergeUsageCounter
var initialStoreBatchCoprCounter m.StoreBatchCoprCounter
var initialFairLockingUsageCounter m.FairLockingUsageCounter

// getTxnUsageInfo gets the usage info of transaction related features. It's exported for tests.
func getTxnUsageInfo(ctx sessionctx.Context) *TxnUsage {
	asyncCommitUsed := false
	if val, err := ctx.GetSessionVars().GetGlobalSystemVar(context.Background(), vardef.TiDBEnableAsyncCommit); err == nil {
		asyncCommitUsed = val == vardef.On
	}
	onePCUsed := false
	if val, err := ctx.GetSessionVars().GetGlobalSystemVar(context.Background(), vardef.TiDBEnable1PC); err == nil {
		onePCUsed = val == vardef.On
	}
	curr := metrics.GetTxnCommitCounter()
	diff := curr.Sub(initialTxnCommitCounter)
	mutationCheckerUsed := false
	if val, err := ctx.GetSessionVars().GetGlobalSystemVar(context.Background(), vardef.TiDBEnableMutationChecker); err == nil {
		mutationCheckerUsed = val == vardef.On
	}
	assertionUsed := ""
	if val, err := ctx.GetSessionVars().GetGlobalSystemVar(context.Background(), vardef.TiDBTxnAssertionLevel); err == nil {
		assertionUsed = val
	}
	rcCheckTSUsed := false
	if val, err := ctx.GetSessionVars().GetGlobalSystemVar(context.Background(), vardef.TiDBRCReadCheckTS); err == nil {
		rcCheckTSUsed = val == vardef.On
	}
	rcWriteCheckTSUsed := false
	if val, err := ctx.GetSessionVars().GetGlobalSystemVar(context.Background(), vardef.TiDBRCWriteCheckTs); err == nil {
		rcWriteCheckTSUsed = val == vardef.On
	}
	fairLockingUsed := false
	if val, err := ctx.GetSessionVars().GetGlobalSystemVar(context.Background(), vardef.TiDBPessimisticTransactionFairLocking); err == nil {
		fairLockingUsed = val == vardef.On
	}

	currSavepointCount := m.GetSavepointStmtCounter()
	diffSavepointCount := currSavepointCount - initialSavepointStmtCounter

	currLazyUniqueCheckSetCount := m.GetLazyPessimisticUniqueCheckSetCounter()
	diffLazyUniqueCheckSetCount := currLazyUniqueCheckSetCount - initialLazyPessimisticUniqueCheckSetCount

	currFairLockingUsageCounter := m.GetFairLockingUsageCounter()
	diffFairLockingUsageCounter := currFairLockingUsageCounter.Sub(initialFairLockingUsageCounter)

	return &TxnUsage{asyncCommitUsed, onePCUsed, diff,
		mutationCheckerUsed, assertionUsed, rcCheckTSUsed, rcWriteCheckTSUsed,
		fairLockingUsed, diffSavepointCount, diffLazyUniqueCheckSetCount, diffFairLockingUsageCounter,
	}
}

func postReportTxnUsage() {
	initialTxnCommitCounter = metrics.GetTxnCommitCounter()
}

func postReportCTEUsage() {
	initialCTECounter = m.GetCTECounter()
}

func postReportAccountLockUsage() {
	initialAccountLockCounter = m.GetAccountLockCounter()
}

// PostSavepointCount exports for testing.
func PostSavepointCount() {
	initialSavepointStmtCounter = m.GetSavepointStmtCounter()
}

func postReportLazyPessimisticUniqueCheckSetCount() {
	initialLazyPessimisticUniqueCheckSetCount = m.GetLazyPessimisticUniqueCheckSetCounter()
}

func postReportFairLockingUsageCounter() {
	initialFairLockingUsageCounter = m.GetFairLockingUsageCounter()
}

// getCTEUsageInfo gets the CTE usages.
func getCTEUsageInfo() *m.CTEUsageCounter {
	curr := m.GetCTECounter()
	diff := curr.Sub(initialCTECounter)
	return &diff
}

// getAccountLockUsageInfo gets the AccountLock usages.
func getAccountLockUsageInfo() *m.AccountLockCounter {
	curr := m.GetAccountLockCounter()
	diff := curr.Sub(initialAccountLockCounter)
	return &diff
}

func postReportMultiSchemaChangeUsage() {
	initialMultiSchemaChangeCounter = m.GetMultiSchemaCounter()
}

func getMultiSchemaChangeUsageInfo() *m.MultiSchemaChangeUsageCounter {
	curr := m.GetMultiSchemaCounter()
	diff := curr.Sub(initialMultiSchemaChangeCounter)
	return &diff
}

func postReportExchangePartitionUsage() {
	initialExchangePartitionCounter = m.GetExchangePartitionCounter()
}

func getExchangePartitionUsageInfo() *m.ExchangePartitionUsageCounter {
	curr := m.GetExchangePartitionCounter()
	diff := curr.Sub(initialExchangePartitionCounter)
	return &diff
}

func postReportTablePartitionUsage() {
	initialTablePartitionCounter = m.ResetTablePartitionCounter(initialTablePartitionCounter)
}

func postReportDDLUsage() {
	initialDDLUsageCounter = m.GetDDLUsageCounter()
}

func getTablePartitionUsageInfo() *m.TablePartitionUsageCounter {
	curr := m.GetTablePartitionCounter()
	diff := curr.Cal(initialTablePartitionCounter)
	return &diff
}

// getAutoCaptureUsageInfo gets the 'Auto Capture' usage
func getAutoCaptureUsageInfo(ctx sessionctx.Context) bool {
	if val, err := ctx.GetSessionVars().GetGlobalSystemVar(context.Background(), vardef.TiDBCapturePlanBaseline); err == nil {
		return val == vardef.On
	}
	return false
}

func getNonTransactionalUsage() *m.NonTransactionalStmtCounter {
	curr := m.GetNonTransactionalStmtCounter()
	diff := curr.Sub(initialNonTransactionalCounter)
	return &diff
}

func postReportNonTransactionalCounter() {
	initialNonTransactionalCounter = m.GetNonTransactionalStmtCounter()
}

func getGlobalKillUsageInfo() bool {
	return config.GetGlobalConfig().EnableGlobalKill
}

func getLogBackupUsageInfo(ctx sessionctx.Context) bool {
	return utils.IsLogBackupInUse(ctx)
}

func getCostModelVer2UsageInfo(ctx sessionctx.Context) bool {
	return ctx.GetSessionVars().CostModelVersion == 2
}

// getPagingUsageInfo gets the value of system vardef `tidb_enable_paging`.
// This vardef is set to true as default since v6.2.0. We want to know many
// users set it to false manually.
func getPagingUsageInfo(ctx sessionctx.Context) bool {
	return ctx.GetSessionVars().EnablePaging
}

func getDDLUsageInfo(ctx sessionctx.Context) *m.DDLUsageCounter {
	curr := m.GetDDLUsageCounter()
	diff := curr.Sub(initialDDLUsageCounter)
	isEnable, err := ctx.GetSessionVars().GlobalVarsAccessor.GetGlobalSysVar("tidb_enable_metadata_lock")
	if err == nil {
		diff.MetadataLockUsed = isEnable == "ON"
	}
	return &diff
}

func getGlobalMemoryControl() bool {
	return memory.ServerMemoryLimit.Load() > 0
}

func postReportIndexMergeUsage() {
	initialIndexMergeCounter = m.GetIndexMergeCounter()
}

func getIndexMergeUsageInfo() *m.IndexMergeUsageCounter {
	curr := m.GetIndexMergeCounter()
	diff := curr.Sub(initialIndexMergeCounter)
	return &diff
}

func getStoreBatchUsage(ctx sessionctx.Context) *m.StoreBatchCoprCounter {
	curr := m.GetStoreBatchCoprCounter()
	diff := curr.Sub(initialStoreBatchCoprCounter)
	if val, err := ctx.GetSessionVars().GetGlobalSystemVar(context.Background(), vardef.TiDBStoreBatchSize); err == nil {
		if batchSize, err := strconv.Atoi(val); err == nil {
			diff.BatchSize = batchSize
		}
	}
	return &diff
}

func postStoreBatchUsage() {
	initialStoreBatchCoprCounter = m.GetStoreBatchCoprCounter()
}
