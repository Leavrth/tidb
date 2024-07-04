// Copyright 2022 PingCAP, Inc. Licensed under Apache-2.0.

package rawkv_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/br/pkg/conn"
	berrors "github.com/pingcap/tidb/br/pkg/errors"
	"github.com/pingcap/tidb/br/pkg/gluetidb"
	rawclient "github.com/pingcap/tidb/br/pkg/restore/internal/rawkv"
	"github.com/pingcap/tidb/br/pkg/stream"
	"github.com/pingcap/tidb/br/pkg/task"
	ddlutil "github.com/pingcap/tidb/pkg/ddl/util"
	"github.com/pingcap/tidb/pkg/domain"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/meta"
	"github.com/pingcap/tidb/pkg/parser/model"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util/codec"
	"github.com/stretchr/testify/require"
	"github.com/tikv/client-go/v2/config"
	"github.com/tikv/client-go/v2/rawkv"
	"google.golang.org/grpc/keepalive"
)

// fakeRawkvClient is a mock for rawkv.client
type fakeRawkvClient struct {
	rawkv.Client
	kvs []kv.Entry
}

func newFakeRawkvClient() *fakeRawkvClient {
	return &fakeRawkvClient{
		kvs: make([]kv.Entry, 0),
	}
}

func (f *fakeRawkvClient) BatchPut(
	ctx context.Context,
	keys [][]byte,
	values [][]byte,
	options ...rawkv.RawOption,
) error {
	if len(keys) != len(values) {
		return errors.Annotate(berrors.ErrInvalidArgument,
			"the length of keys don't equal the length of values")
	}

	for i := 0; i < len(keys); i++ {
		entry := kv.Entry{
			Key:   keys[i],
			Value: values[i],
		}
		f.kvs = append(f.kvs, entry)
	}
	return nil
}

func (f *fakeRawkvClient) Close() error {
	return nil
}

func TestRawKVBatchClient(t *testing.T) {
	fakeRawkvClient := newFakeRawkvClient()
	batchCount := 3
	rawkvBatchClient := rawclient.NewRawKVBatchClient(fakeRawkvClient, batchCount)
	defer rawkvBatchClient.Close()

	rawkvBatchClient.SetColumnFamily("default")

	kvs := []kv.Entry{
		{Key: codec.EncodeUintDesc([]byte("key1"), 1), Value: []byte("v1")},
		{Key: codec.EncodeUintDesc([]byte("key2"), 2), Value: []byte("v2")},
		{Key: codec.EncodeUintDesc([]byte("key3"), 3), Value: []byte("v3")},
		{Key: codec.EncodeUintDesc([]byte("key4"), 4), Value: []byte("v4")},
		{Key: codec.EncodeUintDesc([]byte("key5"), 5), Value: []byte("v5")},
	}

	for i := 0; i < batchCount; i++ {
		require.Equal(t, 0, len(fakeRawkvClient.kvs))
		err := rawkvBatchClient.Put(context.TODO(), kvs[i].Key, kvs[i].Value, uint64(i+1))
		require.Nil(t, err)
	}
	require.Equal(t, batchCount, len(fakeRawkvClient.kvs))

	for i := batchCount; i < len(kvs); i++ {
		err := rawkvBatchClient.Put(context.TODO(), kvs[i].Key, kvs[i].Value, uint64(i+1))
		require.Nil(t, err)
	}
	require.Equal(t, batchCount, len(fakeRawkvClient.kvs))
	err := rawkvBatchClient.PutRest(context.TODO())
	require.Nil(t, err)
	sort.Slice(fakeRawkvClient.kvs, func(i, j int) bool {
		return bytes.Compare(fakeRawkvClient.kvs[i].Key, fakeRawkvClient.kvs[j].Key) < 0
	})
	require.Equal(t, kvs, fakeRawkvClient.kvs)
}

func TestRawKVBatchClientDuplicated(t *testing.T) {
	fakeRawkvClient := newFakeRawkvClient()
	batchCount := 3
	rawkvBatchClient := rawclient.NewRawKVBatchClient(fakeRawkvClient, batchCount)
	defer rawkvBatchClient.Close()

	rawkvBatchClient.SetColumnFamily("default")

	kvs := []kv.Entry{
		{Key: codec.EncodeUintDesc([]byte("key1"), 1), Value: []byte("v1")},
		{Key: codec.EncodeUintDesc([]byte("key1"), 2), Value: []byte("v2")},
		{Key: codec.EncodeUintDesc([]byte("key3"), 3), Value: []byte("v3")},
		{Key: codec.EncodeUintDesc([]byte("key4"), 4), Value: []byte("v4")},
		{Key: codec.EncodeUintDesc([]byte("key4"), 5), Value: []byte("v5")},
	}

	expectedKvs := []kv.Entry{
		// we keep the large ts entry, and we only make sure there is no duplicated entry in a batch.
		// which is 3. so the duplicated key4 not in a batch will have two versions finally.
		{Key: codec.EncodeUintDesc([]byte("key1"), 2), Value: []byte("v2")},
		{Key: codec.EncodeUintDesc([]byte("key3"), 3), Value: []byte("v3")},
		{Key: codec.EncodeUintDesc([]byte("key4"), 5), Value: []byte("v5")},
		{Key: codec.EncodeUintDesc([]byte("key4"), 4), Value: []byte("v4")},
	}

	for i := 0; i < batchCount; i++ {
		require.Equal(t, 0, len(fakeRawkvClient.kvs))
		err := rawkvBatchClient.Put(context.TODO(), kvs[i].Key, kvs[i].Value, uint64(i+1))
		require.Nil(t, err)
	}
	// There only two different keys which doesn't send to kv.
	require.Equal(t, 0, len(fakeRawkvClient.kvs))

	for i := batchCount; i < 5; i++ {
		err := rawkvBatchClient.Put(context.TODO(), kvs[i].Key, kvs[i].Value, uint64(i+1))
		require.Nil(t, err)
		require.Equal(t, batchCount, len(fakeRawkvClient.kvs))
	}

	err := rawkvBatchClient.PutRest(context.TODO())
	require.Nil(t, err)
	sort.Slice(fakeRawkvClient.kvs, func(i, j int) bool {
		return bytes.Compare(fakeRawkvClient.kvs[i].Key, fakeRawkvClient.kvs[j].Key) < 0
	})
	require.Equal(t, expectedKvs, fakeRawkvClient.kvs)
}

func generateTableWriteCfKV(dbID, tableID int64, startTs, commitTs uint64) (key, value []byte) {
	rawMetaKey := &stream.RawMetaKey{
		Key:   meta.DBkey(dbID),
		Field: meta.TableKey(tableID),
		Ts:    commitTs,
	}

	rawWriteCFValue := stream.FakeRawWriteCFValue(startTs)
	return rawMetaKey.EncodeMetaKey(), rawWriteCFValue.EncodeTo()
}

func generateTableDefaultCfKV(dbID, tableID int64, startTs uint64, tableInfo *model.TableInfo) (key, value []byte) {
	rawMetaKey := &stream.RawMetaKey{
		Key:   meta.DBkey(dbID),
		Field: meta.TableKey(tableID),
		Ts:    startTs,
	}

	rawDefaultCFValue, err := json.Marshal(tableInfo)
	if err != nil {
		log.Panic(err)
	}

	return rawMetaKey.EncodeMetaKey(), rawDefaultCFValue
}

func generateDBWriteCfKV(dbID int64, startTs, commitTs uint64) (key, value []byte) {
	rawMetaKey := &stream.RawMetaKey{
		Key:   []byte("DBs"),
		Field: meta.DBkey(dbID),
		Ts:    commitTs,
	}

	rawWriteCFValue := stream.FakeRawWriteCFValue(startTs)
	return rawMetaKey.EncodeMetaKey(), rawWriteCFValue.EncodeTo()
}

func generateDBDefaultCfKV(dbID int64, startTs uint64, dbInfo *model.DBInfo) (key, value []byte) {
	rawMetaKey := &stream.RawMetaKey{
		Key:   []byte("DBs"),
		Field: meta.DBkey(dbID),
		Ts:    startTs,
	}

	rawDefaultCFValue, err := json.Marshal(dbInfo)
	if err != nil {
		log.Panic(err)
	}

	return rawMetaKey.EncodeMetaKey(), rawDefaultCFValue
}

type tableGenerator struct {
	rawBatchCli *rawclient.RawKVBatchClient

	startTs  uint64
	commitTs uint64
}

func getIntType() types.FieldType {
	intType := types.FieldType{}
	intType.SetType(3)
	intType.SetFlag(4099) // primary key
	intType.SetFlen(11)
	intType.SetCharset("binary")
	intType.SetCollate("binary")
	return intType
}

func getCharType() types.FieldType {
	charType := types.FieldType{}
	charType.SetType(254)
	charType.SetFlag(0)
	charType.SetFlen(20)
	charType.SetCharset("utf8mb4")
	charType.SetCollate("utf8mb4_bin")
	return charType
}

func (tg *tableGenerator) getTableInfo(dbID, tableID int64) *model.TableInfo {
	return &model.TableInfo{
		ID:      tableID,
		Name:    model.NewCIStr(fmt.Sprintf("tbl-%d", tableID)),
		Charset: "utf8mb4",
		Collate: "utf8mb4_bin",
		Columns: []*model.ColumnInfo{
			{ID: 1, Name: model.NewCIStr("id"), Offset: 0, FieldType: getIntType(), State: model.StatePublic, Version: 2},
			{ID: 2, Name: model.NewCIStr("a"), Offset: 1, FieldType: getCharType(), State: model.StatePublic, Version: 2},
			{ID: 3, Name: model.NewCIStr("b"), Offset: 2, FieldType: getCharType(), State: model.StatePublic, Version: 2},
			{ID: 4, Name: model.NewCIStr("c"), Offset: 3, FieldType: getCharType(), State: model.StatePublic, Version: 2},
			{ID: 5, Name: model.NewCIStr("d"), Offset: 4, FieldType: getCharType(), State: model.StatePublic, Version: 2},
			{ID: 6, Name: model.NewCIStr("e"), Offset: 5, FieldType: getCharType(), State: model.StatePublic, Version: 2},
			{ID: 7, Name: model.NewCIStr("f"), Offset: 6, FieldType: getCharType(), State: model.StatePublic, Version: 2},
			{ID: 8, Name: model.NewCIStr("g"), Offset: 7, FieldType: getCharType(), State: model.StatePublic, Version: 2},
			{ID: 9, Name: model.NewCIStr("h"), Offset: 8, FieldType: getCharType(), State: model.StatePublic, Version: 2},
		},
		State:      model.StatePublic,
		PKIsHandle: true,
		UpdateTS:   tg.commitTs,
		Version:    5,
		DBID:       dbID,
	}
}

func (tg *tableGenerator) putDefaultTable(ctx context.Context, dbID, tableID int64) error {
	tableInfo := tg.getTableInfo(dbID, tableID)
	defaultKey, defaultValue := generateTableDefaultCfKV(dbID, tableID, tg.startTs, tableInfo)
	return tg.rawBatchCli.Put(ctx, defaultKey, defaultValue, tg.startTs)
}

func (tg *tableGenerator) putWriteTable(ctx context.Context, dbID, tableID int64) error {
	writeKey, writeValue := generateTableWriteCfKV(dbID, tableID, tg.startTs, tg.commitTs)
	return tg.rawBatchCli.Put(ctx, writeKey, writeValue, tg.commitTs)
}

func (tg *tableGenerator) getDBInfo(dbID int64) *model.DBInfo {
	return &model.DBInfo{
		ID:      dbID,
		Name:    model.NewCIStr(fmt.Sprintf("db-%d", dbID)),
		Charset: "utf8mb4",
		Collate: "utf8mb4_bin",
		State:   model.StatePublic,
	}
}

func (tg *tableGenerator) putDefaultDB(ctx context.Context, dbID int64) error {
	dbInfo := tg.getDBInfo(dbID)
	defaultKey, defaultValue := generateDBDefaultCfKV(dbID, tg.startTs, dbInfo)
	return tg.rawBatchCli.Put(ctx, defaultKey, defaultValue, tg.startTs)
}

func (tg *tableGenerator) putWriteDB(ctx context.Context, dbID int64) error {
	writeKey, writeValue := generateDBWriteCfKV(dbID, tg.startTs, tg.commitTs)
	return tg.rawBatchCli.Put(ctx, writeKey, writeValue, tg.commitTs)
}

func updateSchemaVersion(ctx context.Context, t *testing.T, dom *domain.Domain) {
	storage := dom.Store()
	var schemaVersion int64

	ctx = kv.WithInternalSourceType(ctx, kv.InternalTxnBR)
	err := kv.RunInNewTxn(
		ctx,
		storage,
		true,
		func(ctx context.Context, txn kv.Transaction) error {
			t := meta.NewMeta(txn)
			var e error
			// To trigger full-reload instead of diff-reload, we need to increase the schema version
			// by at least `domain.LoadSchemaDiffVersionGapThreshold`.
			schemaVersion, e = t.GenSchemaVersions(64 + domain.LoadSchemaDiffVersionGapThreshold)
			if e != nil {
				return e
			}
			// add the diff key so that the domain won't retry to reload the schemas with `schemaVersion` frequently.
			return t.SetSchemaDiff(&model.SchemaDiff{
				Version:             schemaVersion,
				Type:                model.ActionNone,
				SchemaID:            -1,
				TableID:             -1,
				RegenerateSchemaMap: true,
			})
		},
	)
	require.NoError(t, err)

	ver := strconv.FormatInt(schemaVersion, 10)
	err = ddlutil.PutKVToEtcd(
		ctx,
		dom.GetEtcdClient(),
		math.MaxInt,
		ddlutil.DDLGlobalSchemaVersion,
		ver,
	)
	require.NoError(t, err)
}

func TestXxx(t *testing.T) {
	ctx := context.Background()
	mgr, err := task.NewMgr(ctx, &gluetidb.Glue{}, []string{"127.0.0.1:2379"}, task.TLSConfig{}, keepalive.ClientParameters{}, false, true, conn.NormalVersionChecker)
	require.NoError(t, err)
	rawCli, err := rawclient.NewRawkvClient(ctx, []string{"127.0.0.1:2379"}, config.Security{})
	require.NoError(t, err)
	rawBatchCli := rawclient.NewRawKVBatchClient(rawCli, 128)

	startTime := time.Now()
	emptyDBCount := int64(60000)
	dbCount := int64(40000)
	tableCountPerDB := int64(25)
	startId := int64(0)
	startTs := uint64(0)
	commitTs := uint64(0)
	ctx = kv.WithInternalSourceType(ctx, kv.InternalTxnBR)
	err = kv.RunInNewTxn(ctx, mgr.GetDomain().Store(), true, func(_ context.Context, txn kv.Transaction) error {
		startTs = txn.StartTS()
		m := meta.NewMeta(txn)
		currentId, err := m.GetGlobalID()
		if err != nil {
			return err
		}
		startId = currentId
		_, err = m.AdvanceGlobalIDs(int(emptyDBCount + dbCount + tableCountPerDB*dbCount + int64(100000)))
		if err != nil {
			return err
		}
		return nil
	})
	require.NoError(t, err)
	err = kv.RunInNewTxn(ctx, mgr.GetDomain().Store(), true, func(_ context.Context, txn kv.Transaction) error {
		commitTs = txn.StartTS()
		return nil
	})
	require.NoError(t, err)
	tg := &tableGenerator{rawBatchCli: rawBatchCli, startTs: startTs, commitTs: commitTs}
	ctx = context.Background()

	// create some empty database
	{
		// default cf
		rawBatchCli.SetColumnFamily("default")
		for i := int64(0); i < emptyDBCount; i += 1 {
			dbID := i + startId + 1
			err = tg.putDefaultDB(ctx, dbID)
			require.NoError(t, err)
		}
		err = rawBatchCli.PutRest(ctx)
		require.NoError(t, err)
		// write cf
		rawBatchCli.SetColumnFamily("write")
		for i := int64(0); i < emptyDBCount; i += 1 {
			dbID := i + startId + 1
			err = tg.putWriteDB(ctx, dbID)
			require.NoError(t, err)
		}
		err = rawBatchCli.PutRest(ctx)
		require.NoError(t, err)
	}

	startId = startId + emptyDBCount
	// create db
	{
		// default cf
		rawBatchCli.SetColumnFamily("default")
		for i := int64(0); i < dbCount; i += 1 {
			dbID := i + startId + 1
			err = tg.putDefaultDB(ctx, dbID)
			require.NoError(t, err)
		}
		err = rawBatchCli.PutRest(ctx)
		require.NoError(t, err)
		// write cf
		rawBatchCli.SetColumnFamily("write")
		for i := int64(0); i < dbCount; i += 1 {
			dbID := i + startId + 1
			err = tg.putWriteDB(ctx, dbID)
			require.NoError(t, err)
		}
		err = rawBatchCli.PutRest(ctx)
		require.NoError(t, err)
	}

	tableStartId := startId + dbCount
	// create table
	{
		// default cf
		tableID := tableStartId
		rawBatchCli.SetColumnFamily("default")
		///*
		dbID := startId + 1
		for j := int64(0); j < dbCount*tableCountPerDB; j += 1 {
			tableID = tableID + 1
			err = tg.putDefaultTable(ctx, dbID, tableID)
			require.NoError(t, err)
		}
		/*/
		for i := int64(0); i < dbCount; i += 1 {
			dbID := i + startId + 1
			for j := int64(0); j < tableCountPerDB; j += 1 {
				tableID = tableID + 1
				err = tg.putDefaultTable(ctx, dbID, tableID)
				require.NoError(t, err)
			}
		}
		/**/
		err = rawBatchCli.PutRest(ctx)
		require.NoError(t, err)
		// write cf
		tableID = tableStartId
		rawBatchCli.SetColumnFamily("write")
		//* *
		dbID = startId + 1
		for j := int64(0); j < dbCount*tableCountPerDB; j += 1 {
			tableID = tableID + 1
			err = tg.putWriteTable(ctx, dbID, tableID)
			require.NoError(t, err)
		}
		/*/
		for i := int64(0); i < dbCount; i += 1 {
			dbID := i + startId + 1
			for j := int64(0); j < tableCountPerDB; j += 1 {
				tableID = tableID + 1
				err = tg.putWriteTable(ctx, dbID, tableID)
				require.NoError(t, err)
			}
		}
		/**/
		err = rawBatchCli.PutRest(ctx)
		require.NoError(t, err)
	}

	rawBatchCli.Close()
	updateSchemaVersion(ctx, t, mgr.GetDomain())
	require.Equal(t, time.Since(startTime), 0)
}
