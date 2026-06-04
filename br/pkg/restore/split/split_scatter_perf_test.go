// Copyright 2026 PingCAP, Inc. Licensed under Apache-2.0.

package split

import (
	"bytes"
	"context"
	"crypto/tls"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pingcap/kvproto/pkg/metapb"
	connutil "github.com/pingcap/tidb/br/pkg/conn/util"
	"github.com/pingcap/tidb/br/pkg/pdutil"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/tablecodec"
	tidbutil "github.com/pingcap/tidb/pkg/util"
	"github.com/stretchr/testify/require"
	pd "github.com/tikv/pd/client"
)

const (
	splitScatterBenchPDEnv              = "BR_SPLIT_SCATTER_BENCH_PD"
	splitScatterBenchRegionsPerStoreEnv = "BR_SPLIT_SCATTER_BENCH_REGIONS_PER_STORE"
	splitScatterBenchTableIDEnv         = "BR_SPLIT_SCATTER_BENCH_TABLE_ID"
	splitScatterBenchConcurrencyEnv     = "BR_SPLIT_SCATTER_BENCH_CONCURRENCY"
	splitScatterBenchBatchKeysEnv       = "BR_SPLIT_SCATTER_BENCH_BATCH_KEYS"
	splitScatterBenchTimeoutEnv         = "BR_SPLIT_SCATTER_BENCH_TIMEOUT"
	splitScatterBenchCAEnv              = "BR_SPLIT_SCATTER_BENCH_CA"
	splitScatterBenchCertEnv            = "BR_SPLIT_SCATTER_BENCH_CERT"
	splitScatterBenchKeyEnv             = "BR_SPLIT_SCATTER_BENCH_KEY"

	defaultSplitScatterBenchRegionsPerStore = 100_000
	defaultSplitScatterBenchBatchKeys       = 4096
	defaultSplitScatterBenchTableIDBase     = int64(9_000_000_000_000_000_000)
)

func TestMakeSplitScatterBenchKeys(t *testing.T) {
	keys := makeSplitScatterBenchKeys(42, 8)

	require.Len(t, keys, 8)
	require.True(t, slices.IsSortedFunc(keys, bytes.Compare))
	require.True(t, bytes.HasPrefix(keys[0], tablecodec.EncodeTablePrefix(42)))
	for i := 1; i < len(keys); i++ {
		require.NotEqual(t, keys[i-1], keys[i])
	}
}

func TestSplitAndScatterPerfWithPD(t *testing.T) {
	pdAddrs := splitScatterBenchPDAddrs(t)
	if len(pdAddrs) == 0 {
		t.Skipf("set %s to run the split&scatter performance test against a real cluster", splitScatterBenchPDEnv)
	}

	timeout := parseDurationEnv(t, splitScatterBenchTimeoutEnv, 0)
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	start := time.Now()
	tlsConf, securityOption := splitScatterBenchTLS(t)
	controller, err := pdutil.NewPdController(ctx, pdAddrs, tlsConf, securityOption)
	require.NoError(t, err)
	defer controller.Close()

	stores, err := connutil.GetAllTiKVStores(ctx, controller.GetPDClient(), connutil.SkipTiFlash)
	require.NoError(t, err)
	stores = filterUpStores(stores)
	require.NotEmpty(t, stores, "PD has no live TiKV stores")

	regionsPerStore := parsePositiveIntEnv(
		t,
		splitScatterBenchRegionsPerStoreEnv,
		defaultSplitScatterBenchRegionsPerStore,
	)
	splitKeyCount := regionsPerStore * len(stores)
	tableID := splitScatterBenchTableID(t)
	splitConcurrency := parsePositiveIntEnv(
		t,
		splitScatterBenchConcurrencyEnv,
		len(stores)+1,
	)
	splitBatchKeys := parsePositiveIntEnv(
		t,
		splitScatterBenchBatchKeysEnv,
		defaultSplitScatterBenchBatchKeys,
	)

	keyBuildStart := time.Now()
	splitKeys := makeSplitScatterBenchKeys(tableID, splitKeyCount)
	keyBuildDuration := time.Since(keyBuildStart)
	require.True(t, slices.IsSortedFunc(splitKeys, bytes.Compare))

	t.Logf(
		"split&scatter bench starts: pd=%s liveStores=%d regionsPerStore=%d splitKeys=%d tableID=%d splitConcurrency=%d splitBatchKeys=%d keyBuild=%s setup=%s",
		strings.Join(pdAddrs, ","),
		len(stores),
		regionsPerStore,
		len(splitKeys),
		tableID,
		splitConcurrency,
		splitBatchKeys,
		keyBuildDuration,
		time.Since(start),
	)

	splitCli := NewClient(
		controller.GetPDClient(),
		controller.GetPDHTTPClient(),
		tlsConf,
		splitBatchKeys,
		splitConcurrency,
	)
	t.Logf("BR scatter decision: needScatter=%t", splitCli.(*pdClient).needScatter(ctx))
	splitter := NewRegionSplitter(splitCli)

	restoreSchedulers := pauseSplitScatterBenchSchedulers(t, ctx, controller)
	defer func() {
		restoreCtx := ctx
		if restoreCtx.Err() != nil {
			restoreCtx = context.Background()
		}
		require.NoError(t, restoreSchedulers(restoreCtx))
	}()

	splitStart := time.Now()
	require.NoError(t, splitter.ExecuteSortedKeys(ctx, splitKeys))
	splitDuration := time.Since(splitStart)

	t.Logf(
		"split&scatter bench finished: splitKeys=%d liveStores=%d duration=%s total=%s",
		len(splitKeys),
		len(stores),
		splitDuration,
		time.Since(start),
	)
}

func pauseSplitScatterBenchSchedulers(
	t *testing.T,
	ctx context.Context,
	controller *pdutil.PdController,
) pdutil.UndoFunc {
	t.Helper()

	restoreSchedulers, cfg, err := controller.RemoveSchedulersWithConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	t.Logf(
		"PD schedulers paused: schedulers=%v configKeys=%d",
		cfg.Schedulers,
		len(cfg.ScheduleCfg),
	)
	return restoreSchedulers
}

func makeSplitScatterBenchKeys(tableID int64, splitKeyCount int) [][]byte {
	keys := make([][]byte, 0, splitKeyCount)
	for i := 0; i < splitKeyCount; i++ {
		keys = append(keys, tablecodec.EncodeRowKeyWithHandle(tableID, kv.IntHandle(i)))
	}
	return keys
}

func splitScatterBenchPDAddrs(t *testing.T) []string {
	t.Helper()

	raw := strings.TrimSpace(os.Getenv(splitScatterBenchPDEnv))
	if raw == "" {
		return nil
	}

	addrs := strings.Split(raw, ",")
	ret := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		addr = strings.TrimSpace(addr)
		if addr != "" {
			ret = append(ret, addr)
		}
	}
	require.NotEmpty(t, ret, "%s only contains empty PD addresses", splitScatterBenchPDEnv)
	return ret
}

func splitScatterBenchTLS(t *testing.T) (*tls.Config, pd.SecurityOption) {
	t.Helper()

	caPath := strings.TrimSpace(os.Getenv(splitScatterBenchCAEnv))
	certPath := strings.TrimSpace(os.Getenv(splitScatterBenchCertEnv))
	keyPath := strings.TrimSpace(os.Getenv(splitScatterBenchKeyEnv))
	require.True(
		t,
		certPath == "" && keyPath == "" || certPath != "" && keyPath != "",
		"%s and %s must be set together",
		splitScatterBenchCertEnv,
		splitScatterBenchKeyEnv,
	)

	tlsConf, err := tidbutil.ToTLSConfig(caPath, certPath, keyPath)
	require.NoError(t, err)
	return tlsConf, pd.SecurityOption{
		CAPath:   caPath,
		CertPath: certPath,
		KeyPath:  keyPath,
	}
}

func splitScatterBenchTableID(t *testing.T) int64 {
	t.Helper()

	raw := strings.TrimSpace(os.Getenv(splitScatterBenchTableIDEnv))
	if raw == "" {
		return defaultSplitScatterBenchTableIDBase + time.Now().Unix()%1_000_000
	}

	tableID, err := strconv.ParseInt(raw, 10, 64)
	require.NoError(t, err)
	require.Positive(t, tableID)
	return tableID
}

func parsePositiveIntEnv(t *testing.T, name string, defaultValue int) int {
	t.Helper()

	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return defaultValue
	}

	value, err := strconv.Atoi(raw)
	require.NoError(t, err)
	require.Positive(t, value, "%s must be positive", name)
	return value
}

func parseDurationEnv(t *testing.T, name string, defaultValue time.Duration) time.Duration {
	t.Helper()

	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return defaultValue
	}

	value, err := time.ParseDuration(raw)
	require.NoError(t, err)
	require.Positive(t, value, "%s must be positive", name)
	return value
}

func filterUpStores(stores []*metapb.Store) []*metapb.Store {
	j := 0
	for _, store := range stores {
		if store.GetState() == metapb.StoreState_Up {
			stores[j] = store
			j++
		}
	}
	return stores[:j]
}
