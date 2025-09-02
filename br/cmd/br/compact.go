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
	"github.com/pingcap/tidb/br/pkg/storage"
	"github.com/pingcap/tidb/br/pkg/stream"
	"github.com/pingcap/tidb/br/pkg/task"
	tidbutil "github.com/pingcap/tidb/pkg/util"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
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
			totalSize := uint64(0)
			if err = stream.FastUnmarshalMetaData(ctx, s, lastSnapshotBackupTs, untilTs, cfg.MetadataDownloadBatchSize, func(path string, rawMetaData []byte) error {
				meta := &backuppb.Metadata{}
				if err := meta.Unmarshal(rawMetaData); err != nil {
					return errors.Trace(err)
				}
				for _, fgs := range meta.FileGroups {
					for _, f := range fgs.DataFilesInfo {
						if f.MinTs <= untilTs && f.MaxTs >= lastSnapshotBackupTs {
							atomic.AddUint64(&totalSize, f.Length)
						}
					}
				}
				return nil
			}); err != nil {
				return errors.Trace(err)
			}
			compactionSize := uint64(0)
			if err := walkCompactions(ctx, s, cfg.MetadataDownloadBatchSize, func(lfs *backuppb.LogFileSubcompaction) {
				if lfs.Meta.InputMinTs <= untilTs && lfs.Meta.InputMaxTs >= lastSnapshotBackupTs {
					atomic.AddUint64(&compactionSize, lfs.Meta.Size_)
				}
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

func walkCompactions(ctx context.Context, s storage.ExternalStorage, concurrency uint, fn func(*backuppb.LogFileSubcompaction)) (retErr error) {
	ext := stream.MigrationExtension(s)
	migs, err := ext.Load(ctx)
	if err != nil {
		return errors.Trace(err)
	}
	workerpool := tidbutil.NewWorkerPool(concurrency, "read compaction metadata")
	eg, ectx := errgroup.WithContext(ctx)
	defer func() {
		if err := eg.Wait(); err != nil {
			retErr = err
		}
	}()
	for _, mig := range migs.ListAll() {
		for _, compaction := range mig.Compactions {
			if err := s.WalkDir(ectx, &storage.WalkOption{SubDir: compaction.Artifacts}, func(path string, size int64) error {
				if ectx.Err() != nil {
					return errors.Trace(ectx.Err())
				}
				workerpool.ApplyOnErrorGroup(eg, func() error {
					data, err := s.ReadFile(ectx, path)
					if err != nil {
						return errors.Trace(err)
					}
					subCompactions := &backuppb.LogFileSubcompactions{}
					if err := subCompactions.Unmarshal(data); err != nil {
						return errors.Trace(err)
					}
					for _, subCompaction := range subCompactions.Subcompactions {
						fn(subCompaction)
					}
					return nil
				})
				return nil
			}); err != nil {
				return errors.Trace(err)
			}
		}
	}
	return nil
}
