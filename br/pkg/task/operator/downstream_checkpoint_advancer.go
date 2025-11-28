// Copyright 2025 PingCAP, Inc. Licensed under Apache-2.0.
package operator

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/docker/go-units"
	"github.com/google/btree"
	"github.com/pingcap/errors"
	backuppb "github.com/pingcap/kvproto/pkg/brpb"
	"github.com/pingcap/log"
	berrors "github.com/pingcap/tidb/br/pkg/errors"
	"github.com/pingcap/tidb/br/pkg/metautil"
	"github.com/pingcap/tidb/br/pkg/restore"
	"github.com/pingcap/tidb/br/pkg/storage"
	"github.com/pingcap/tidb/br/pkg/streamhelper"
	"github.com/pingcap/tidb/br/pkg/task"
	"github.com/pingcap/tidb/pkg/util/redact"
	pd "github.com/tikv/pd/client"
	"github.com/tikv/pd/client/opt"
	"github.com/tikv/pd/client/pkg/caller"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

const downstreamCheckpointTsPath = "v1/global_checkpoint/synced.ts"

type GlobalCheckpointPdtsoPair struct {
	pdtso            uint64
	globalCheckpoint uint64
}

// Less impls btree.Item.
func (lhs *GlobalCheckpointPdtsoPair) Less(rhs *GlobalCheckpointPdtsoPair) bool {
	return lhs.pdtso < rhs.pdtso
}

type GlobalCheckpointPdtsoTree struct {
	*btree.BTreeG[*GlobalCheckpointPdtsoPair]
}

func NewGlobalCheckpointPdtsoTree() *GlobalCheckpointPdtsoTree {
	return &GlobalCheckpointPdtsoTree{
		BTreeG: btree.NewG(32, (*GlobalCheckpointPdtsoPair).Less),
	}
}

func getStorage(ctx context.Context, storageName string) (storage.ExternalStorage, error) {
	u, err := storage.ParseBackend(storageName, &storage.BackendOptions{})
	if err != nil {
		return nil, errors.Trace(err)
	}
	s, err := storage.New(ctx, u, &storage.ExternalStorageOptions{})
	if err != nil {
		return nil, errors.Annotate(err, "create storage failed")
	}
	return s, nil
}

func getStartTsFromBackupmeta(ctx context.Context, us storage.ExternalStorage) (uint64, error) {
	metaData, err := us.ReadFile(ctx, metautil.MetaFile)
	if err != nil {
		return 0, errors.Trace(err)
	}
	backupMeta := &backuppb.BackupMeta{}
	if err = backupMeta.Unmarshal(metaData); err != nil {
		return 0, errors.Trace(err)
	}
	if backupMeta.GetEndVersion() > 0 {
		return 0, errors.Annotate(berrors.ErrStorageUnknown,
			"the storage has been used for full backup")
	}
	return backupMeta.GetStartVersion(), nil
}

func dialEtcd(ctx context.Context, pd []string, tlsConf *task.TLSConfig) (*clientv3.Client, error) {
	var (
		tlsConfig *tls.Config
		err       error
	)
	if tlsConf.IsEnabled() {
		tlsConfig, err = tlsConf.ToTLSConfig()
		if err != nil {
			return nil, errors.Trace(err)
		}
	}
	log.Info("trying to connect to etcd", zap.Strings("addr", pd))
	etcdCLI, err := clientv3.New(clientv3.Config{
		TLS:              tlsConfig,
		Endpoints:        pd,
		AutoSyncInterval: 30 * time.Second,
		DialTimeout:      5 * time.Second,
		DialOptions: []grpc.DialOption{
			grpc.WithKeepaliveParams(keepalive.ClientParameters{
				Time:                10 * time.Second,
				Timeout:             3 * time.Second,
				PermitWithoutStream: false,
			}),
			grpc.WithBlock(),
			grpc.WithReturnConnectionError(),
		},
		Context: ctx,
	})
	if err != nil {
		return nil, err
	}
	return etcdCLI, nil
}

func RunDownstreamCheckpointAdvancer(ctx context.Context, cfg *DownstreamCheckpointAdvancerConfig) error {
	advancer, err := NewAdvancer(ctx, cfg)
	if err != nil {
		return errors.Trace(err)
	}
	return advancer.AdvancerLoop(ctx)
}

type Advancer struct {
	us         storage.ExternalStorage
	ds         storage.ExternalStorage
	etcdCLI    *clientv3.Client
	pdClient   pd.Client
	taskName   string
	sortedTree *GlobalCheckpointPdtsoTree

	downstreamCheckpointTs uint64
}

func NewAdvancer(ctx context.Context, cfg *DownstreamCheckpointAdvancerConfig) (*Advancer, error) {
	us, err := getStorage(ctx, cfg.upstreamStorage)
	if err != nil {
		return nil, errors.Trace(err)
	}
	ds, err := getStorage(ctx, cfg.downstreamStorage)
	if err != nil {
		return nil, errors.Trace(err)
	}
	etcdCLI, err := dialEtcd(ctx, cfg.pd, cfg.tlsConfig)
	if err != nil {
		return nil, errors.Trace(err)
	}
	securityOption := pd.SecurityOption{}
	if cfg.tlsConfig.IsEnabled() {
		securityOption.CAPath = cfg.tlsConfig.CA
		securityOption.CertPath = cfg.tlsConfig.Cert
		securityOption.KeyPath = cfg.tlsConfig.Key
	}
	pdClient, err := pd.NewClientWithContext(
		ctx, caller.Component("br-pd-controller"), cfg.pd, securityOption,
		opt.WithGRPCDialOptions(
			grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(128*units.MiB)),
			grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(128*units.MiB)),
		),
		// If the time too short, we may scatter a region many times, because
		// the interface `ScatterRegions` may time out.
		opt.WithCustomTimeoutOption(60*time.Second),
	)
	if err != nil {
		return nil, errors.Trace(err)
	}
	var downstreamCheckpointTs uint64 = 0
	exists, err := ds.FileExists(ctx, downstreamCheckpointTsPath)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if exists {
		data, err := ds.ReadFile(ctx, downstreamCheckpointTsPath)
		if err != nil {
			return nil, errors.Trace(err)
		}
		downstreamCheckpointTs = binary.LittleEndian.Uint64(data)
	}
	return &Advancer{
		us:         us,
		ds:         ds,
		etcdCLI:    etcdCLI,
		pdClient:   pdClient,
		taskName:   cfg.taskName,
		sortedTree: NewGlobalCheckpointPdtsoTree(),

		downstreamCheckpointTs: downstreamCheckpointTs,
	}, nil
}

func parseGlobalCheckpointTs(value []byte) (uint64, error) {
	if len(value) != 8 {
		return 0, errors.Annotatef(berrors.ErrPiTRMalformedMetadata,
			"the global checkpoint isn't 64bits (it is %d bytes, value = %s)",
			len(value),
			redact.Key(value))
	}
	return binary.BigEndian.Uint64(value), nil
}

func (adv *Advancer) getGlobalCheckpointWatcher(ctx context.Context) (uint64, clientv3.WatchChan, error) {
	resp, err := adv.etcdCLI.KV.Get(ctx, streamhelper.GlobalCheckpointOf(adv.taskName))
	if err != nil {
		return 0, nil, errors.Trace(err)
	}
	globalCheckpointTs := uint64(0)
	if len(resp.Kvs) > 0 {
		globalCheckpointTs, err = parseGlobalCheckpointTs(resp.Kvs[0].Value)
		if err != nil {
			return 0, nil, errors.Trace(err)
		}
	}
	checkpointCh := adv.etcdCLI.Watcher.Watch(
		ctx,
		streamhelper.GlobalCheckpointOf(adv.taskName),
		clientv3.WithRev(resp.Header.GetRevision()),
	)
	return globalCheckpointTs, checkpointCh, nil
}

func handleResponse(resp clientv3.WatchResponse) (uint64, error) {
	globalCheckpoint := uint64(0)
	if err := resp.Err(); err != nil {
		return 0, err
	}
	for _, event := range resp.Events {
		ts, err := parseGlobalCheckpointTs(event.Kv.Value)
		if err != nil {
			return 0, errors.Trace(err)
		}
		globalCheckpoint = max(globalCheckpoint, ts)
	}
	return globalCheckpoint, nil
}

func (adv *Advancer) generatePair(ctx context.Context, globalCheckpointTs uint64) error {
	pdtso, err := restore.GetTSWithRetry(ctx, adv.pdClient)
	if err != nil {
		return errors.Trace(err)
	}
	log.Info("generate the global checkpoint pd tso pair", zap.Uint64("pd tso", pdtso), zap.Uint64("global checkpoint ts", globalCheckpointTs))
	adv.sortedTree.ReplaceOrInsert(&GlobalCheckpointPdtsoPair{
		pdtso:            pdtso,
		globalCheckpoint: globalCheckpointTs,
	})
	return nil
}

func (adv *Advancer) waitGlobalCheckpointAdvance(ctx context.Context, checkpointCh clientv3.WatchChan, curGlobalCheckpointTs uint64) (uint64, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case resp, ok := <-checkpointCh:
		if !ok {
			return 0, errors.Errorf("channel closed")
		}
		ts, err := handleResponse(resp)
		if err != nil {
			return 0, errors.Trace(err)
		}
		if curGlobalCheckpointTs < ts {
			curGlobalCheckpointTs = ts
			if err := adv.generatePair(ctx, ts); err != nil {
				return 0, errors.Trace(err)
			}
		}
	}
	return curGlobalCheckpointTs, nil
}

func (adv *Advancer) waitDownstreamFileSynced(ctx context.Context, filename string) error {
	for {
		exists, err := adv.ds.FileExists(ctx, filename)
		if err != nil {
			return errors.Trace(err)
		}
		if exists {
			return nil
		}
		log.Info("the file has not been synced yet", zap.String("filename", filename))
		time.Sleep(3 * time.Second)
	}
}

func (adv *Advancer) waitDownstreamFilesSynced(ctx context.Context, prefixTs uint64) error {
	subPrefix := fmt.Sprintf("v1/backupmeta/%09X", prefixTs>>28)
	paths := make([]string, 0)
	adv.us.WalkDir(ctx, &storage.WalkOption{ObjPrefix: subPrefix}, func(path string, size int64) error {
		log.Info("track metadata", zap.String("filename", path))
		paths = append(paths, path)
		return nil
	})
	for _, path := range paths {
		data, err := adv.us.ReadFile(ctx, path)
		if err != nil {
			return errors.Trace(err)
		}
		m := &backuppb.Metadata{}
		err = m.Unmarshal(data)
		if err != nil {
			return errors.Trace(err)
		}
		for _, gs := range m.FileGroups {
			if err := adv.waitDownstreamFileSynced(ctx, gs.Path); err != nil {
				return errors.Trace(err)
			}
		}
		if err := adv.waitDownstreamFileSynced(ctx, path); err != nil {
			return nil
		}
	}
	return nil
}

func (adv *Advancer) tryAdvanceDownstreamCheckpoint(ctx context.Context, prefixTs uint64) error {
	ts := prefixTs | TsFill
	downstreamTs := uint64(0)
	for {
		pair, ok := adv.sortedTree.Min()
		if !ok {
			return errors.Errorf("no pair found")
		}
		if pair.pdtso <= ts {
			downstreamTs = max(downstreamTs, pair.globalCheckpoint)
			adv.sortedTree.DeleteMin()
		} else {
			break
		}
	}
	if adv.downstreamCheckpointTs < downstreamTs {
		log.Info("downstream checkpoint is advanced",
			zap.Uint64("global checkpoint ts", adv.downstreamCheckpointTs),
			zap.Duration("now gap", time.Since(time.UnixMilli(int64(downstreamTs>>18)))),
			zap.Duration("max gap", time.Since(time.UnixMilli(int64(adv.downstreamCheckpointTs>>18)))),
		)
		adv.downstreamCheckpointTs = downstreamTs
		return adv.ds.WriteFile(ctx, downstreamCheckpointTsPath, binary.LittleEndian.AppendUint64(nil, adv.downstreamCheckpointTs))
	}
	return nil
}

const (
	TsStep uint64 = 1 << 28
	TsFill uint64 = 0xFFFFFFF
	TsMask uint64 = ^uint64(TsFill)
)

func (adv *Advancer) AdvancerLoop(ctx context.Context) error {
	startTs, err := getStartTsFromBackupmeta(ctx, adv.us)
	if err != nil {
		return errors.Trace(err)
	}
	startTs = max(startTs, adv.downstreamCheckpointTs)
	prefixTs := startTs & TsMask
	globalCheckpointTs := startTs

	ts, checkpointCh, err := adv.getGlobalCheckpointWatcher(ctx)
	if err != nil {
		return errors.Trace(err)
	}
	if globalCheckpointTs < ts {
		globalCheckpointTs = ts
		if err := adv.generatePair(ctx, ts); err != nil {
			return errors.Trace(err)
		}
	}
	log.Info("start advancer loop", zap.String("start prefix ts", fmt.Sprintf("%016X", prefixTs)), zap.String("global checkpoint ts", fmt.Sprintf("%016X", globalCheckpointTs)))
	for {
		for prefixTs < (globalCheckpointTs & TsMask) {
			if err := adv.waitDownstreamFilesSynced(ctx, prefixTs); err != nil {
				return errors.Trace(err)
			}
			if err := adv.tryAdvanceDownstreamCheckpoint(ctx, prefixTs); err != nil {
				return errors.Trace(err)
			}
			prefixTs += TsStep
		}
		globalCheckpointTs, err = adv.waitGlobalCheckpointAdvance(ctx, checkpointCh, globalCheckpointTs)
		if err != nil {
			return errors.Trace(err)
		}
		log.Info("global checkpoint ts is advanced", zap.String("start prefix ts", fmt.Sprintf("%016X", prefixTs)), zap.String("global checkpoint ts", fmt.Sprintf("%016X", globalCheckpointTs)))
	}
}
