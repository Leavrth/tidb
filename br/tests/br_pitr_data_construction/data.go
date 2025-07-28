// Copyright 2025 PingCAP, Inc.
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

package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"flag"
	"fmt"

	"github.com/gogo/protobuf/proto"
	"github.com/klauspost/compress/zstd"
	"github.com/pingcap/errors"
	backuppb "github.com/pingcap/kvproto/pkg/brpb"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/log"
	"github.com/pingcap/tidb/br/pkg/metautil"
	"github.com/pingcap/tidb/br/pkg/storage"
	"github.com/pingcap/tidb/br/pkg/stream"
	"go.uber.org/zap"
)

func writeBackupLock(ctx context.Context, stg storage.ExternalStorage, prefix string) error {
	return stg.WriteFile(ctx, fmt.Sprintf("%s/%s", prefix, metautil.LockFile),
		[]byte("DO NOT DELETE\n"+
			"This file exists to remind other backup jobs won't use this path"))
}

func writeFullBackupMeta(ctx context.Context, stg storage.ExternalStorage, prefix string) error {
	metaFile := &backuppb.MetaFile{
		Schemas: []*backuppb.Schema{
			{
				Db:    []byte("{\"id\":2,\"db_name\":{\"O\":\"test\",\"L\":\"test\"},\"charset\":\"utf8mb4\",\"collate\":\"utf8mb4_bin\",\"Deprecated\":{},\"state\":5,\"policy_ref_info\":null}"),
				Table: []byte("{\"id\":110,\"name\":{\"O\":\"t1\",\"L\":\"t1\"},\"charset\":\"utf8mb4\",\"collate\":\"utf8mb4_bin\",\"cols\":[{\"id\":1,\"name\":{\"O\":\"id\",\"L\":\"id\"},\"offset\":0,\"origin_default\":null,\"origin_default_bit\":null,\"default\":null,\"default_bit\":null,\"default_is_expr\":false,\"generated_expr_string\":\"\",\"generated_stored\":false,\"dependences\":null,\"type\":{\"Tp\":3,\"Flag\":0,\"Flen\":11,\"Decimal\":0,\"Charset\":\"binary\",\"Collate\":\"binary\",\"Elems\":null,\"ElemsIsBinaryLit\":null,\"Array\":false},\"state\":5,\"comment\":\"\",\"hidden\":false,\"change_state_info\":null,\"version\":2}],\"index_info\":[],\"constraint_info\":null,\"fk_info\":null,\"state\":5,\"pk_is_handle\":false,\"is_common_handle\":false,\"common_handle_version\":0,\"comment\":\"\",\"auto_inc_id\":30001,\"auto_id_cache\":0,\"auto_rand_id\":0,\"max_col_id\":1,\"max_idx_id\":0,\"max_fk_id\":0,\"max_cst_id\":0,\"update_timestamp\":459177310191091722,\"ShardRowIDBits\":0,\"max_shard_row_id_bits\":0,\"auto_random_bits\":0,\"auto_random_range_bits\":0,\"pre_split_regions\":0,\"partition\":null,\"compression\":\"\",\"view\":null,\"sequence\":null,\"Lock\":null,\"version\":5,\"tiflash_replica\":null,\"is_columnar\":false,\"temp_table_type\":0,\"cache_table_status\":0,\"policy_ref_info\":null,\"stats_options\":null,\"exchange_partition_info\":null,\"ttl_info\":null,\"revision\":1}"),
			},
		},
	}
	data, err := proto.Marshal(metaFile)
	if err != nil {
		return err
	}
	if err := stg.WriteFile(ctx, fmt.Sprintf("%s/backupmeta.schema.000000001", prefix), data); err != nil {
		return err
	}
	checksum := sha256.Sum256(data)

	backupMeta := &backuppb.BackupMeta{
		ClusterId:      7523160737852614324,
		ClusterVersion: "9.0.0-beta.2",
		BrVersion:      "BR\nRelease Version: v9.0.0-beta.2.pre-50-gd31c573\nGit Commit Hash: d31c573cf2fc54bd91db358a543c3c6f6dde7ed6\nGit Branch: HEAD\nGo Version: go1.23.10\nUTC Build Time: 2025-07-04 00:55:39\nRace Enabled: false",
		Version:        1,
		EndVersion:     459177124841914369,
		SchemaIndex: &backuppb.MetaFile{
			MetaFiles: []*backuppb.File{
				{
					Name:   "backupmeta.schema.000000001",
					Sha256: checksum[:],
					Size_:  uint64(len(data)),
				},
			},
		},
		Ddls:                 []byte("[]"),
		NewCollationsEnabled: "True",
	}
	backupMeta.BackupSize = uint64(len(data) + backupMeta.Size())

	data, err = proto.Marshal(backupMeta)
	if err != nil {
		return err
	}
	return stg.WriteFile(ctx, fmt.Sprintf("%s/backupmeta", prefix), data)
}

func writeLogBackupMeta(ctx context.Context, stg storage.ExternalStorage, prefix string) error {
	backupMeta := &backuppb.BackupMeta{
		ClusterId:      7523160737852614324,
		ClusterVersion: "9.0.0-beta.2",
		Version:        1,
		StartVersion:   459177118642470914,
		Ddls:           []byte("[]"),
	}
	backupMeta.BackupSize = uint64(backupMeta.Size())
	data, err := proto.Marshal(backupMeta)
	if err != nil {
		return err
	}
	return stg.WriteFile(ctx, fmt.Sprintf("%s/backupmeta", prefix), data)
}

func writeLogBackupGlobalCheckpoint(ctx context.Context, stg storage.ExternalStorage, prefix string) error {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], 459177232937254913)
	return stg.WriteFile(ctx, fmt.Sprintf("%s/v1/global_checkpoint/1.ts", prefix), buf[:])
}

func compressDataWithZSTD(data []byte) ([]byte, error) {
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return encoder.EncodeAll(data, nil), nil
}

func writePutLogData(ctx context.Context, stg storage.ExternalStorage, prefix string) error {
	// encodedKey: [116,128,0,0,0,0,0,0,255,110,95,114,128,0,0,0,0,255,0,0,1,0,0,0,0,0,250], ts: [249,160,172,213,86,243,255,253] 459177129704947714
	putKey := []byte{116, 128, 0, 0, 0, 0, 0, 0, 255, 110, 95, 114, 128, 0, 0, 0, 0, 255, 0, 0, 1, 0, 0, 0, 0, 0, 250, 249, 160, 172, 213, 86, 243, 255, 253}
	// writeType: 80, startTs: [129,128,176,200,170,229,212,175,6] 459177129704947713, shortValue: [flag: 118, len: 10, val: [128, 0, 1, 0, 0, 0, 1, 1, 0, 1]]
	putWriteCFValue := []byte{80, 129, 128, 176, 200, 170, 229, 212, 175, 6, 118, 10, 128, 0, 1, 0, 0, 0, 1, 1, 0, 1}
	putEntry := stream.EncodeKVEntry(putKey, putWriteCFValue)
	putChecksum := sha256.Sum256(putEntry)
	putRawLength := uint64(len(putEntry))
	putEntry, err := compressDataWithZSTD(putEntry)
	if err != nil {
		return errors.Trace(err)
	}
	putLength := uint64(len(putEntry))
	if err := stg.WriteFile(ctx, fmt.Sprintf("%s/%s", prefix, "v1/20250704/09/4/459177129704947714-46e67c37-dd25-4aa8-9248-8d60733b657e.log"), putEntry); err != nil {
		return errors.Trace(err)
	}

	// encodedKey: [116,128,0,0,0,0,0,0,255,110,95,114,128,0,0,0,0,255,0,0,1,0,0,0,0,0,250], ts: [249,160,172,199,92,11,255,253] 459177189749030914
	dropKey := []byte{116, 128, 0, 0, 0, 0, 0, 0, 255, 110, 95, 114, 128, 0, 0, 0, 0, 255, 0, 0, 1, 0, 0, 0, 0, 0, 250, 249, 160, 172, 199, 92, 11, 255, 253}
	// writeType: 68, startTs: [129,128,208,159,138,231,212,175,6] 459177189749030913
	dropWriteCFValue := []byte{68, 129, 128, 208, 159, 138, 231, 212, 175, 6}
	dropEntry := stream.EncodeKVEntry(dropKey, dropWriteCFValue)
	dropChecksum := sha256.Sum256(dropEntry)
	dropRawLength := uint64(len(dropEntry))
	dropEntry, err = compressDataWithZSTD(dropEntry)
	if err != nil {
		return errors.Trace(err)
	}
	dropLength := uint64(len(dropEntry))
	if err := stg.WriteFile(ctx, fmt.Sprintf("%s/%s", prefix, "v1/20250704/09/4/459177189749030914-6012bd8d-f6d1-4dfa-adc8-591956e71786.log"), dropEntry); err != nil {
		return errors.Trace(err)
	}

	metadata := &backuppb.Metadata{
		FileGroups: []*backuppb.DataFileGroup{
			{
				Path: "v1/20250704/09/4/459177189749030914-6012bd8d-f6d1-4dfa-adc8-591956e71786.log",
				DataFilesInfo: []*backuppb.DataFileInfo{
					{
						Sha256:                dropChecksum[:],
						NumberOfEntries:       1,
						MinTs:                 459177189749030914,
						MaxTs:                 459177189749030914,
						ResolvedTs:            459177188346560515,
						RegionId:              26,
						StartKey:              []byte{116, 128, 0, 0, 0, 0, 0, 0, 255, 110, 95, 114, 128, 0, 0, 0, 0, 255, 0, 0, 1, 0, 0, 0, 0, 0, 250, 249, 160, 172, 199, 92, 11, 255, 253},
						EndKey:                []byte{116, 128, 0, 0, 0, 0, 0, 0, 255, 110, 95, 114, 128, 0, 0, 0, 0, 255, 0, 0, 1, 0, 0, 0, 0, 0, 250, 249, 160, 172, 199, 92, 11, 255, 253},
						Cf:                    "write",
						Type:                  backuppb.FileType_Put,
						IsMeta:                false,
						TableId:               110,
						Length:                dropRawLength,
						MinBeginTsInDefaultCf: 459177189749030913,
						RangeOffset:           0,
						RangeLength:           dropLength,
						CompressionType:       backuppb.CompressionType_ZSTD,
						Crc64Xor:              10600011882420382856,
						RegionStartKey:        []byte{116, 128, 0, 0, 0, 0, 0, 0, 255, 110, 0, 0, 0, 0, 0, 0, 0, 248},
						RegionEndKey:          []byte{116, 128, 0, 255, 255, 255, 255, 255, 255, 248, 0, 0, 0, 0, 0, 0, 0, 248},
						RegionEpoch:           []*metapb.RegionEpoch{{ConfVer: 5, Version: 65}},
					},
				},
				MinTs:         459177189749030914,
				MaxTs:         459177189749030914,
				MinResolvedTs: 459177188346560515,
				Length:        dropLength,
			},
			{
				Path: "v1/20250704/09/4/459177129704947714-46e67c37-dd25-4aa8-9248-8d60733b657e.log",
				DataFilesInfo: []*backuppb.DataFileInfo{
					{
						Sha256:                putChecksum[:],
						NumberOfEntries:       1,
						MinTs:                 459177129704947714,
						MaxTs:                 459177129704947714,
						ResolvedTs:            459177128053440513,
						RegionId:              26,
						StartKey:              []byte{116, 128, 0, 0, 0, 0, 0, 0, 255, 110, 95, 114, 128, 0, 0, 0, 0, 255, 0, 0, 1, 0, 0, 0, 0, 0, 250, 249, 160, 172, 213, 86, 243, 255, 253},
						EndKey:                []byte{116, 128, 0, 0, 0, 0, 0, 0, 255, 110, 95, 114, 128, 0, 0, 0, 0, 255, 0, 0, 1, 0, 0, 0, 0, 0, 250, 249, 160, 172, 213, 86, 243, 255, 253},
						Cf:                    "write",
						Type:                  backuppb.FileType_Put,
						IsMeta:                false,
						TableId:               110,
						Length:                putRawLength,
						MinBeginTsInDefaultCf: 459177129704947713,
						RangeOffset:           0,
						RangeLength:           putLength,
						CompressionType:       backuppb.CompressionType_ZSTD,
						Crc64Xor:              17742263206069239029,
						RegionStartKey:        []byte{116, 128, 0, 0, 0, 0, 0, 0, 255, 110, 0, 0, 0, 0, 0, 0, 0, 248},
						RegionEndKey:          []byte{116, 128, 0, 255, 255, 255, 255, 255, 255, 248, 0, 0, 0, 0, 0, 0, 0, 248},
						RegionEpoch:           []*metapb.RegionEpoch{{ConfVer: 5, Version: 65}},
					},
				},
				MinTs:         459177129704947714,
				MaxTs:         459177129704947714,
				MinResolvedTs: 459177128053440513,
				Length:        putLength,
			},
		},
		StoreId:     4,
		ResolvedTs:  459177128053440513,
		MaxTs:       459177189749030914,
		MinTs:       459177129704947714,
		MetaVersion: backuppb.MetaVersion_V2,
	}
	data, err := proto.Marshal(metadata)
	if err != nil {
		return err
	}
	return stg.WriteFile(ctx, fmt.Sprintf("%s/%s", prefix, "v1/backupmeta/459177128053440513-e157b01c-d166-47ca-bfaf-73290c6fbb06.meta"), data)
}

func prepareFullBackupData(ctx context.Context, stg storage.ExternalStorage) error {
	// full/backup.lock
	// full/backupmeta
	// full/backupmeta.schema.000000001
	if err := writeBackupLock(ctx, stg, "full"); err != nil {
		return errors.Trace(err)
	}
	if err := writeFullBackupMeta(ctx, stg, "full"); err != nil {
		return errors.Trace(err)
	}
	return nil
}

func prepareLogBackupData(ctx context.Context, stg storage.ExternalStorage) error {
	// log/backup.lock
	// log/backupmeta
	// log/v1/global_checkpoint/1.ts
	// log/v1/backupmeta/459177128053440513-e157b01c-d166-47ca-bfaf-73290c6fbb06.meta
	// log/v1/20250704/09/4/459177129704947714-46e67c37-dd25-4aa8-9248-8d60733b657e.log
	// log/v1/20250704/09/4/459177189749030914-6012bd8d-f6d1-4dfa-adc8-591956e71786.log
	if err := writeBackupLock(ctx, stg, "log"); err != nil {
		return errors.Trace(err)
	}
	if err := writeLogBackupMeta(ctx, stg, "log"); err != nil {
		return errors.Trace(err)
	}
	if err := writeLogBackupGlobalCheckpoint(ctx, stg, "log"); err != nil {
		return errors.Trace(err)
	}
	if err := writePutLogData(ctx, stg, "log"); err != nil {
		return errors.Trace(err)
	}
	return nil
}

var path = flag.String("storage", "", "storage url to save mock pitr data")

func main() {
	flag.Parse()

	ctx := context.Background()
	backend, err := storage.ParseBackend(*path, nil)
	if err != nil {
		log.Panic("failed to parse backend", zap.String("path", *path), zap.Error(err))
	}
	stg, err := storage.New(ctx, backend, nil)
	if err != nil {
		log.Panic("failed to create storage", zap.Error(err))
	}
	if err := prepareFullBackupData(ctx, stg); err != nil {
		log.Panic("failed to preapre full backup data", zap.Error(err))
	}
	if err := prepareLogBackupData(ctx, stg); err != nil {
		log.Panic("failed to prepare log backup data", zap.Error(err))
	}
	log.Info("create mock pitr data successfully")
}
