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

type storeWorkerGroup struct {
	storeID    int64
	workerCh   chan *taskItem
	responseCh chan *taskResponse
}

type storeWorkerGroupControl struct {
	sync.WaitGroup
}

func newStoreWorkerGroup(cctx context.Context, responseCh chan *taskResponse, storeID int64, concurrency int) *storeWorkerGroupControl {
	workerCh := make(chan *taskItem, concurrency)
	// fill worker channel with fake tasks for initialization
	for i := 0; i < concurrency; i += 1 {
		workerCh <- nil
	}

	workerGroup := &storeWorkerGroup{
		storeID:    storeID,
		workerCh:   workerCh,
		responseCh: responseCh,
	}
	workerGroupControl := &storeWorkerGroupControl{}
	// fill worker channel with fake tasks for initialization
	for i := 0; i < concurrency; i += 1 {
		workerGroupControl.Add(1)
		go workerGroup.workerLoop(cctx, workerGroupControl)
	}
	return workerGroupControl
}

func (swg *storeWorkerGroup) workerLoop(cctx context.Context, wg *storeWorkerGroupControl) {
	defer wg.Done()
	for {
		select {
		case <-cctx.Done():
			return
		case task, ok := <-swg.workerCh:
			if !ok {
				return
			}
			if task != nil {

			}

			swg.responseCh <- &taskResponse{}
		}
	}
}
