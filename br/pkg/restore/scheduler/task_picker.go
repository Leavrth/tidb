// Copyright 2024 PingCAP, Inc.
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

package scheduler

import (
	"context"
	"sync"
)

/* TaskPicker
 *
 *
 *
 */
type TaskPicker struct {
	wg     sync.WaitGroup
	cctx   context.Context
	cancel context.CancelCauseFunc

	taskItems []*taskItem

	// store ID -> channel
	storeDownloadWorkerSenders  map[int64]chan *taskItem
	storeDownloadWorkerReceiver chan taskResponse
}

func NewTaskPicker(ctx context.Context) *TaskPicker {
	taskPicker := &TaskPicker{}
	taskPicker.wg.Add(1)
	go taskPicker.pickLoop()
	return taskPicker
}

// error can be nil
func (picker *TaskPicker) StopByError(err error) {
	if picker.cancel != nil {
		picker.cancel(err)
	}
	picker.wg.Wait()
	for _, sender := range picker.storeDownloadWorkerSenders {
		close(sender)
	}

	// worker.wg.wait
}

func (picker *TaskPicker) pickLoop() {
	defer picker.wg.Done()
	for {
		select {
		case <-picker.cctx.Done():
			return
		case resp, ok := <-picker.storeDownloadWorkerReceiver:
			if !ok {
				return
			}
			_ = resp
		}
	}
}
