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
	"fmt"
	"sync/atomic"

	"github.com/pingcap/errors"
	backuppb "github.com/pingcap/kvproto/pkg/brpb"
	"github.com/pingcap/log"
	logclient "github.com/pingcap/tidb/br/pkg/restore/log_client"
	"github.com/pingcap/tidb/br/pkg/storage"
	"github.com/pingcap/tidb/br/pkg/stream"
	"github.com/pingcap/tidb/br/pkg/task"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

// compactAnalyzeCommand returns a compact debug subcommand
func compactAnalyzeCommand() *cobra.Command {
	command := &cobra.Command{
		Use:          "compact",
		Short:        "compact debug tasks",
		SilenceUsage: true,
	}

	command.AddCommand(
		newCompactRatioCalculatedCommand(),
	)

	return command
}

func newCompactRatioCalculatedCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "ratio",
		Short: "calculate compaction ratio",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithCancel(GetDefaultContext())
			defer cancel()

			untilTs, err := cmd.Flags().GetUint64("until-ts")
			if err != nil {
				return errors.Trace(err)
			}
			lastSnapshotBackupTs, err := cmd.Flags().GetUint64("last-snapshot-backup-ts")
			if err != nil {
				return errors.Trace(err)
			}
			var cfg task.Config
			if err = cfg.ParseFromFlags(cmd.Flags()); err != nil {
				return errors.Trace(err)
			}

			_, s, err := task.GetStorage(ctx, cfg.Storage, &cfg)
			if err != nil {
				return errors.Trace(err)
			}
			skipmap, err := getSkipMapFromMigration(ctx, s)
			if err != nil {
				return errors.Trace(err)
			}
			totalSize := uint64(0)
			compactionSize := uint64(0)
			if err = stream.FastUnmarshalMetaData(ctx, s, lastSnapshotBackupTs, untilTs, cfg.MetadataDownloadBatchSize, func(path string, rawMetaData []byte) error {
				meta := &backuppb.Metadata{}
				if err := meta.Unmarshal(rawMetaData); err != nil {
					return errors.Trace(err)
				}
				for _, fgs := range meta.FileGroups {
					for _, f := range fgs.DataFilesInfo {
						if f.MinTs <= untilTs && f.MaxTs >= lastSnapshotBackupTs {
							atomic.AddUint64(&totalSize, f.Length)
							if skipmap.NeedSkip(path, fgs.Path, f.RangeOffset) {
								atomic.AddUint64(&compactionSize, f.Length)
							}
						}
					}
				}
				return nil
			}); err != nil {
				return errors.Trace(err)
			}
			log.Info("compaction ratio",
				zap.Uint64("compaction size", compactionSize),
				zap.Uint64("total size", totalSize),
				zap.String("ratio", fmt.Sprintf("%.2f", float32(compactionSize)/float32(totalSize))),
			)
			return nil
		},
	}

	flags := command.Flags()
	flags.Uint64("until-ts", 0, "calculate compaction ratio with upper bound ts")
	flags.Uint64("last-snapshot-backup-ts", 0, "calculate compaction ratio with lower bound ts")
	return command
}

func getSkipMapFromMigration(ctx context.Context, s storage.ExternalStorage) (*logclient.MetaSkipMapWrapper, error) {
	skipmap := logclient.NewMetaSkipMapWrapper()
	ext := stream.MigrationExtension(s)
	migs, err := ext.Load(ctx)
	if err != nil {
		return nil, errors.Trace(err)
	}
	for _, mig := range migs.ListAll() {
		for _, editMeta := range mig.EditMeta {
			skipmap.UpdateSkipMap(editMeta)
		}
	}
	return skipmap, nil
}
