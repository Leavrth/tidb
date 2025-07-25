// Copyright 2024 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package resourcegroup

import (
	rmpb "github.com/pingcap/kvproto/pkg/resource_manager"
	"github.com/tikv/client-go/v2/tikvrpc"
	"github.com/tikv/client-go/v2/util"
)

// DefaultResourceGroupName is the default resource group name.
const DefaultResourceGroupName = "default"

// RunawayChecker is used to check runaway queries.
type RunawayChecker interface {
	// BeforeExecutor checks whether query is in watch list before executing and after compiling.
	BeforeExecutor() (string, error)
	// BeforeCopRequest checks runaway and modifies the request if necessary before sending coprocessor request.
	BeforeCopRequest(req *tikvrpc.Request) error
	// CheckThresholds checks TiKV error whether it exceeds the threshold.
	CheckThresholds(ruDetail *util.RUDetails, processKeys int64, err error) error
	// ResetTotalProcessedKeys resets the current total processed keys.
	ResetTotalProcessedKeys()
	// CheckAction is used to check current action of the query.
	// It's safe to call this method concurrently.
	CheckAction() rmpb.RunawayAction
	// CheckRuleKillAction checks whether the query should be killed according to the group settings.
	CheckRuleKillAction() (string, bool)
}

// ConsumptionReporter is used to report raw resource consumptions.
type ConsumptionReporter interface {
	// ReportConsumption report the consumption directly, it's used at
	// scenarios that the `ResourceGroupKVInterceptor` is not available.
	ReportConsumption(resourceGroupName string, consumption *rmpb.Consumption)
}
