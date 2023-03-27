package checkpoint

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/pingcap/errors"
	backuppb "github.com/pingcap/kvproto/pkg/brpb"
	"github.com/pingcap/log"
	"github.com/pingcap/tidb/br/pkg/logutil"
	"github.com/pingcap/tidb/br/pkg/metautil"
	"github.com/pingcap/tidb/br/pkg/storage"
	"github.com/pingcap/tidb/br/pkg/summary"
	"go.uber.org/zap"
)

type LogDataFileInfo struct {
	*backuppb.DataFileInfo

	MetaDataGroupName   string
	OffsetInMetaGroup   int
	OffsetInMergedGroup int
}

type LogGroupValue struct {
	Goff int
	Foff int
}

type LogCheckpointMessage struct {
	// start-key of the origin range
	GroupKey string

	Group *LogGroupValue
}

// A Checkpoint Range File is like this:
//
//    ChecksumData
// +----------------+           RangeGroupData                      RangeGroups
// |    DureTime    |     +--------------------------+ encrypted  +-------------+
// | RangeGroupData-+---> | RangeGroupsEncriptedData-+----------> |   GroupKey  |
// | RangeGroupData |     | Checksum                 |            |    Range    |
// |      ...       |     | CipherIv                 |            |     ...     |
// | RangeGroupData |     | Size                     |            |    Range    |
// +----------------+     +--------------------------+            +-------------+

type LogRangeGroups struct {
	GroupKey string           `json:"group-key"`
	Groups   []*LogGroupValue `json:"groups"`
}

type LogCheckpointRunner struct {
	meta map[string]*LogRangeGroups

	storage storage.ExternalStorage
	cipher  *backuppb.CipherInfo

	appendCh chan *LogCheckpointMessage
	metaCh   chan map[string]*LogRangeGroups
	errCh    chan error

	wg sync.WaitGroup
}

func StartLogCheckpointRunner(ctx context.Context, storage storage.ExternalStorage, cipher *backuppb.CipherInfo) (*LogCheckpointRunner, error) {
	runner := &LogCheckpointRunner{
		meta: make(map[string]*LogRangeGroups),

		storage: storage,
		cipher:  cipher,

		appendCh: make(chan *LogCheckpointMessage),
		metaCh:   make(chan map[string]*LogRangeGroups),
		errCh:    make(chan error, 1),
	}

	runner.startCheckpointLoop(ctx, tickDurationForFlush, tickDurationForLock)
	return runner, nil
}
func (r *LogCheckpointRunner) Append(
	ctx context.Context,
	groupKey string,
	goff int,
	foff int,
) error {
	select {
	case <-ctx.Done():
		return nil
	case err := <-r.errCh:
		return err
	case r.appendCh <- &LogCheckpointMessage{
		GroupKey: groupKey,
		Group: &LogGroupValue{
			Goff: goff,
			Foff: foff,
		},
	}:
		return nil
	}
}

// Note: Cannot be parallel with `Append` function
func (r *LogCheckpointRunner) WaitForFinish(ctx context.Context) {
	// can not append anymore
	close(r.appendCh)
	// wait the range flusher exit
	r.wg.Wait()
}

// Send the meta to the flush goroutine, and reset the CheckpointRunner's meta
func (r *LogCheckpointRunner) flushMeta(ctx context.Context, errCh chan error) error {
	meta := r.meta
	r.meta = make(map[string]*LogRangeGroups)
	// do flush
	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	case r.metaCh <- meta:
	}
	return nil
}

// start a goroutine to flush the meta, which is sent from `checkpoint looper`, to the external storage
func (r *LogCheckpointRunner) startCheckpointRunner(ctx context.Context, wg *sync.WaitGroup) chan error {
	errCh := make(chan error, 1)
	wg.Add(1)
	flushWorker := func(ctx context.Context, errCh chan error) {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case meta, ok := <-r.metaCh:
				if !ok {
					log.Info("stop checkpoint flush worker")
					return
				}
				if err := r.doFlush(ctx, meta); err != nil {
					errCh <- err
					return
				}
			}
		}
	}

	go flushWorker(ctx, errCh)
	return errCh
}

func (r *LogCheckpointRunner) sendError(err error) {
	select {
	case r.errCh <- err:
	default:
		log.Error("errCh is blocked", logutil.ShortError(err))
	}
}

func (r *LogCheckpointRunner) startCheckpointLoop(ctx context.Context, tickDurationForFlush, tickDurationForLock time.Duration) {
	r.wg.Add(1)
	checkpointLoop := func(ctx context.Context) {
		defer r.wg.Done()
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		var wg sync.WaitGroup
		errCh := r.startCheckpointRunner(cctx, &wg)
		flushTicker := time.NewTicker(tickDurationForFlush)
		defer flushTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-flushTicker.C:
				if err := r.flushMeta(ctx, errCh); err != nil {
					r.sendError(err)
					return
				}
			case msg, ok := <-r.appendCh:
				if !ok {
					log.Info("stop checkpoint runner")
					if err := r.flushMeta(ctx, errCh); err != nil {
						r.sendError(err)
					}
					// close the channel to flush worker
					// and wait it to consumes all the metas
					close(r.metaCh)
					wg.Wait()
					return
				}
				groups, exist := r.meta[msg.GroupKey]
				if !exist {
					groups = &LogRangeGroups{
						GroupKey: msg.GroupKey,
						Groups:   make([]*LogGroupValue, 0),
					}
					r.meta[msg.GroupKey] = groups
				}
				groups.Groups = append(groups.Groups, msg.Group)
			case err := <-errCh:
				// pass flush worker's error back
				r.sendError(err)
				return
			}
		}
	}

	go checkpointLoop(ctx)
}

// flush the meta to the external storage
func (r *LogCheckpointRunner) doFlush(ctx context.Context, meta map[string]*LogRangeGroups) error {
	if len(meta) == 0 {
		return nil
	}

	checkpointData := &CheckpointData{
		DureTime:        summary.NowDureTime(),
		RangeGroupMetas: make([]*RangeGroupData, 0, len(meta)),
	}

	var fname []byte = nil

	for _, group := range meta {
		if len(group.Groups) == 0 {
			continue
		}

		// use the first item's group-key and sub-range-key as the filename
		if len(fname) == 0 {
			fname = append(append([]byte(group.GroupKey), '.', '.'), []byte(fmt.Sprint(group.Groups[0].Goff))...)
		}

		// Flush the metaFile to storage
		content, err := json.Marshal(group)
		if err != nil {
			return errors.Trace(err)
		}

		encryptBuff, iv, err := metautil.Encrypt(content, r.cipher)
		if err != nil {
			return errors.Trace(err)
		}

		checksum := sha256.Sum256(content)

		checkpointData.RangeGroupMetas = append(checkpointData.RangeGroupMetas, &RangeGroupData{
			RangeGroupsEncriptedData: encryptBuff,
			Checksum:                 checksum[:],
			Size:                     len(content),
			CipherIv:                 iv,
		})
	}

	if len(checkpointData.RangeGroupMetas) > 0 {
		data, err := json.Marshal(checkpointData)
		if err != nil {
			return errors.Trace(err)
		}

		checksum := sha256.Sum256(fname)
		checksumEncoded := base64.URLEncoding.EncodeToString(checksum[:])
		path := fmt.Sprintf("%s/%s_%d.cpt", CheckpointDataDir, checksumEncoded, rand.Uint64())
		if err := r.storage.WriteFile(ctx, path, data); err != nil {
			return errors.Trace(err)
		}
	}
	return nil
}

// walk the whole checkpoint range files and retrieve the metadatat of backed up ranges
// and return the total time cost in the past executions
func WalkCheckpointFileForLogRestore(ctx context.Context, s storage.ExternalStorage, cipher *backuppb.CipherInfo, fn func(groupKey string, rg *LogGroupValue)) (time.Duration, error) {
	// records the total time cost in the past executions
	var pastDureTime time.Duration = 0
	err := s.WalkDir(ctx, &storage.WalkOption{SubDir: CheckpointDataDir}, func(path string, size int64) error {
		if strings.HasSuffix(path, ".cpt") {
			content, err := s.ReadFile(ctx, path)
			if err != nil {
				return errors.Trace(err)
			}

			checkpointData := &CheckpointData{}
			if err = json.Unmarshal(content, checkpointData); err != nil {
				return errors.Trace(err)
			}

			if checkpointData.DureTime > pastDureTime {
				pastDureTime = checkpointData.DureTime
			}
			for _, meta := range checkpointData.RangeGroupMetas {
				decryptContent, err := metautil.Decrypt(meta.RangeGroupsEncriptedData, cipher, meta.CipherIv)
				if err != nil {
					return errors.Trace(err)
				}

				checksum := sha256.Sum256(decryptContent)
				if !bytes.Equal(meta.Checksum, checksum[:]) {
					log.Error("checkpoint checksum info's checksum mismatch, skip it",
						zap.ByteString("expect", meta.Checksum),
						zap.ByteString("got", checksum[:]),
					)
					continue
				}

				group := &LogRangeGroups{}
				if err = json.Unmarshal(decryptContent, group); err != nil {
					return errors.Trace(err)
				}

				for _, g := range group.Groups {
					fn(group.GroupKey, g)
				}
			}
		}
		return nil
	})

	return pastDureTime, errors.Trace(err)
}

type bitMap []uint8

func newBitMap(size int) bitMap {
	size = (size + 7) / 8
	return make([]uint8, size)
}

func (m bitMap) Set(off uint64) {
	blockIndex := off >> 3
	bitOffset := uint8(1) << (off & 7)
	m[blockIndex] |= bitOffset
}

func (m bitMap) Hit(off uint64) bool {
	blockIndex := off >> 3
	bitOffset := uint8(1) << (off & 7)
	return (m[blockIndex] & bitOffset) > 0
}

type fileOnceMap struct {
	done  int
	total int
	pos   []bitMap
}

type LogFilesSkipMap struct {
	skipMap map[string]*fileOnceMap
}

func (m *LogFilesSkipMap) needSkip(metaKey string, groupOff int, fileOff uint64) bool {
	mp, exists := m.skipMap[metaKey]
	if !exists {
		return false
	}
	if mp.done == mp.total {
		return true
	}
	return mp.pos[groupOff].Hit(fileOff)
}
