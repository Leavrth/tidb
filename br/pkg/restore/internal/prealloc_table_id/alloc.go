// Copyright 2022 PingCAP, Inc. Licensed under Apache-2.0.

package prealloctableid

import (
	"fmt"
	"math"

	"github.com/pingcap/tidb/br/pkg/metautil"
	"github.com/pingcap/tidb/br/pkg/utils"
	"github.com/pingcap/tidb/pkg/meta/model"
)

const (
	// insaneTableIDThreshold is the threshold for "normal" table ID.
	// Sometimes there might be some tables with huge table ID.
	// For example, DDL metadata relative tables may have table ID up to 1 << 48.
	// When calculating the max table ID, we would ignore tables with table ID greater than this.
	// NOTE: In fact this could be just `1 << 48 - 1000` (the max available global ID),
	// however we are going to keep some gap here for some not-yet-known scenario, which means
	// at least, BR won't exhaust all global IDs.
	insaneTableIDThreshold = math.MaxUint32
)

func MaxSysTableID(databases map[string]*metautil.Database) int64 {
	maxSysTableID := int64(0)
	for _, database := range databases {
		if utils.IsTemplateSysDB(database.Info.Name) {
			for _, table := range database.Tables {
				if table.Info.ID > maxSysTableID && table.Info.ID < insaneTableIDThreshold {
					maxSysTableID = table.Info.ID
				}

				if table.Info.Partition != nil && table.Info.Partition.Definitions != nil {
					for _, part := range table.Info.Partition.Definitions {
						if part.ID > maxSysTableID && part.ID < insaneTableIDThreshold {
							maxSysTableID = part.ID
						}
					}
				}
			}
		}
	}
	return maxSysTableID
}

// Allocator is the interface needed to allocate table IDs.
type Allocator interface {
	GetGlobalID() (int64, error)
	AdvanceGlobalIDs(n int) (int64, error)
}

// PreallocIDs mantains the state of preallocated table IDs.
type PreallocIDs struct {
	end int64

	allocedFrom int64

	minUserTableID int64
}

// New collects the requirement of prealloc IDs and return a
// not-yet-allocated PreallocIDs.
func New(tables []*metautil.Table, maxSysTableID int64) *PreallocIDs {
	if len(tables) == 0 {
		return &PreallocIDs{
			allocedFrom: math.MaxInt64,
		}
	}

	maxv := int64(0)
	minv := int64(math.MaxInt64)

	for _, t := range tables {
		if t.Info.ID < insaneTableIDThreshold {
			if t.Info.ID > maxv {
				maxv = t.Info.ID
			}
			if t.Info.ID < minv && t.Info.ID > maxSysTableID {
				minv = t.Info.ID
			}
		}

		if t.Info.Partition != nil && t.Info.Partition.Definitions != nil {
			for _, part := range t.Info.Partition.Definitions {
				if part.ID < insaneTableIDThreshold {
					if part.ID > maxv {
						maxv = part.ID
					}
					if part.ID < minv && part.ID > maxSysTableID {
						minv = part.ID
					}
				}

			}
		}
	}
	return &PreallocIDs{
		end: maxv + 1,

		allocedFrom: math.MaxInt64,

		minUserTableID: minv,
	}
}

// String implements fmt.Stringer.
func (p *PreallocIDs) String() string {
	if p.allocedFrom >= p.end {
		return fmt.Sprintf("ID:empty(end=%d)", p.end)
	}
	return fmt.Sprintf("ID:[%d,%d)", p.allocedFrom, p.end)
}

func (p *PreallocIDs) AllUserTableIDReused() bool {
	return p.allocedFrom <= p.minUserTableID
}

// preallocTableIDs peralloc the id for [start, end)
func (p *PreallocIDs) Alloc(m Allocator) error {
	currentId, err := m.GetGlobalID()
	if err != nil {
		return err
	}
	if currentId > p.end {
		return nil
	}

	alloced, err := m.AdvanceGlobalIDs(int(p.end - currentId))
	if err != nil {
		return err
	}
	p.allocedFrom = alloced
	return nil
}

// Prealloced checks whether a table ID has been successfully allocated.
func (p *PreallocIDs) Prealloced(tid int64) bool {
	return p.allocedFrom <= tid && tid < p.end
}

func (p *PreallocIDs) PreallocedFor(ti *model.TableInfo) bool {
	if !p.Prealloced(ti.ID) {
		return false
	}
	if ti.Partition != nil && ti.Partition.Definitions != nil {
		for _, part := range ti.Partition.Definitions {
			if !p.Prealloced(part.ID) {
				return false
			}
		}
	}
	return true
}
