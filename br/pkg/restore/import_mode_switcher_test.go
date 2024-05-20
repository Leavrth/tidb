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

package restore_test

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pingcap/kvproto/pkg/import_sstpb"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/tidb/br/pkg/restore"
	"github.com/pingcap/tidb/br/pkg/utiltest"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type mockImportServer struct {
	import_sstpb.ImportSSTServer

	count int
	ch    chan struct{}
}

func (s *mockImportServer) SwitchMode(_ context.Context, req *import_sstpb.SwitchModeRequest) (*import_sstpb.SwitchModeResponse, error) {
	s.count -= 1
	if s.count == 0 {
		s.ch <- struct{}{}
	}
	return &import_sstpb.SwitchModeResponse{}, nil
}

func TestRestorePreWork(t *testing.T) {
	ctx := context.Background()
	var port int
	var lis net.Listener
	var err error
	for port = 0; port < 1000; port += 1 {
		addr := fmt.Sprintf(":%d", 51111+port)
		lis, err = net.Listen("tcp", addr)
		if err == nil {
			break
		}
		t.Log(err)
	}

	s := grpc.NewServer()
	ch := make(chan struct{})
	import_sstpb.RegisterImportSSTServer(s, &mockImportServer{count: 3, ch: ch})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := s.Serve(lis)
		require.NoError(t, err)
	}()

	pdClient := utiltest.NewFakePDClient([]*metapb.Store{
		{
			Id:      1,
			Address: fmt.Sprintf(":%d", 51111+port),
		},
	}, false, nil)
	switcher := restore.NewImportModeSwitcher(pdClient, time.Millisecond*200, nil)
	restore.RestorePreWork(ctx, nil, switcher, false, true)
	<-ch
	//err = switcher.switchToNormalMode(ctx)
	require.NoError(t, err)

	s.Stop()
	lis.Close()
}

func TestRestorePreWorkOnline(t *testing.T) {
	ctx := context.Background()
	undo, _, err := restore.RestorePreWork(ctx, nil, nil, true, false)
	require.NoError(t, err)
	restore.RestorePostWork(ctx, nil, undo, true)
}
