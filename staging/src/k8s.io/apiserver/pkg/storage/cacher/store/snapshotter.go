/*
Copyright 2022 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package store

import (
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/cacher/consistency"
	"k8s.io/klog/v2"
)

const initialSnapshotterCapacity = 64

// newSnapshotter returns a snapshotter that stores snapshots in a sorted cyclic
// buffer for serving read requests with exact resource versions (RV) and pagination.
func newSnapshotter(enabled bool) snapshotter {
	return snapshotter{
		enabled: enabled,
	}
}

type snapshotter struct {
	buf             []*btreeStore
	head            int
	count           int
	resourceVersion uint64
	compactRevision uint64
	enabled         bool
}

func (s *snapshotter) at(i int) *btreeStore {
	return s.buf[(s.head+i)%len(s.buf)]
}

func (s *snapshotter) pushBack(snap *btreeStore) {
	if s.count == len(s.buf) {
		newCap := initialSnapshotterCapacity
		if len(s.buf) > 0 {
			newCap = len(s.buf) * 2
		}
		newBuf := make([]*btreeStore, newCap)
		if s.count > 0 {
			first := min(s.count, len(s.buf)-s.head)
			copy(newBuf, s.buf[s.head:s.head+first])
			if first < s.count {
				copy(newBuf[first:], s.buf[:s.count-first])
			}
		}
		s.buf = newBuf
		s.head = 0
	}
	s.buf[(s.head+s.count)%len(s.buf)] = snap
	s.count++
}

func (s *snapshotter) popFront() {
	s.buf[s.head] = nil
	s.head = (s.head + 1) % len(s.buf)
	s.count--
}

func (s *snapshotter) Enabled() bool {
	return s.enabled
}

func (s *snapshotter) SetEnabled(enabled bool) {
	if s.enabled == enabled {
		return
	}
	s.enabled = enabled
	s.reset()
}

func (s *snapshotter) reset() {
	if len(s.buf) > initialSnapshotterCapacity {
		s.buf = make([]*btreeStore, initialSnapshotterCapacity)
	} else {
		clear(s.buf)
	}
	s.head = 0
	s.count = 0
	s.resourceVersion = 0
	s.compactRevision = 0
}

func (s *snapshotter) HasSnapshot(rv uint64) bool {
	oldest, ok := s.OldestResourceVersion()
	return ok && rv >= oldest
}

func (s *snapshotter) OldestResourceVersion() (uint64, bool) {
	if !s.enabled || s.count == 0 {
		return 0, false
	}
	return max(s.at(0).resourceVersion, s.compactRevision), true
}

func (s *snapshotter) GetSnapshot(rv uint64) (*btreeStore, error) {
	if !s.enabled || s.resourceVersion == 0 || rv < s.compactRevision {
		return nil, errors.NewResourceExpired(fmt.Sprintf("too old resource version: %d", rv))
	}
	if rv > s.resourceVersion {
		if consistency.PanicOnCacheInconsistency {
			panic(fmt.Sprintf("snapshotter (on %d) got future resourceVersion (%d) that it doesn't properly handle as it depends on caller ensuring consistency", s.resourceVersion, rv))
		}
		klog.ErrorS(nil, "Snapshotter got future resourceVersion that it doesn't properly handle as it depends on caller ensuring consistency", "requestResourceVersion", rv, "currentResourceVersion", s.resourceVersion)
		return nil, storage.NewTooLargeResourceVersionError(rv, s.resourceVersion, 0)
	}
	snapshot := s.getSnapshot(rv)
	if snapshot == nil {
		return nil, errors.NewResourceExpired(fmt.Sprintf("too old resource version: %d", rv))
	}
	return &btreeStore{
		tree:            snapshot.tree,
		resourceVersion: rv,
	}, nil
}

func (s *snapshotter) Replace(snapshot *btreeStore) {
	if !s.enabled {
		return
	}
	s.reset()
	s.Add(snapshot)
}

func (s *snapshotter) Add(snapshot *btreeStore) {
	if !s.enabled {
		return
	}
	if snapshot.resourceVersion <= s.resourceVersion {
		if consistency.PanicOnCacheInconsistency {
			panic(fmt.Sprintf("snapshot resourceVersion (%d) must be greater than current resourceVersion (%d)", snapshot.resourceVersion, s.resourceVersion))
		}
		klog.ErrorS(nil, "Snapshot resourceVersion must be greater than current resourceVersion", "snapshotResourceVersion", snapshot.resourceVersion, "currentResourceVersion", s.resourceVersion)
	}
	if s.count == 0 || snapshot.resourceVersion > s.at(s.count-1).resourceVersion {
		s.pushBack(snapshot)
	} else {
		idx := sort.Search(s.count, func(i int) bool {
			return s.at(i).resourceVersion >= snapshot.resourceVersion
		})
		if idx < s.count && s.at(idx).resourceVersion == snapshot.resourceVersion {
			s.buf[(s.head+idx)%len(s.buf)] = snapshot
		} else {
			s.pushBack(snapshot)
			for i := s.count - 1; i > idx; i-- {
				a := (s.head + i - 1) % len(s.buf)
				b := (s.head + i) % len(s.buf)
				s.buf[a], s.buf[b] = s.buf[b], s.buf[a]
			}
		}
	}
	s.resourceVersion = max(s.resourceVersion, snapshot.resourceVersion)
	if snapshot.resourceVersion <= s.compactRevision {
		s.compact()
	}
}

func (s *snapshotter) UpdateResourceVersion(rv uint64) {
	if !s.enabled {
		return
	}
	if rv < s.resourceVersion {
		if consistency.PanicOnCacheInconsistency {
			panic(fmt.Sprintf("updated resourceVersion (%d) must be greater than or equal to current resourceVersion (%d)", rv, s.resourceVersion))
		}
		klog.ErrorS(nil, "Updated resourceVersion must be greater than or equal to current resourceVersion", "updatedResourceVersion", rv, "currentResourceVersion", s.resourceVersion)
	}
	s.resourceVersion = max(s.resourceVersion, rv)
}

func (s *snapshotter) Compact(rv uint64) {
	if !s.enabled {
		return
	}
	s.compactRevision = max(s.compactRevision, rv)
	s.compact()
}

func (s *snapshotter) compact() {
	// Keep the last snapshot at or before compactRevision so requests for
	// RVs in [compactRevision, nextSnapshotRV) can still be served.
	for s.count > 1 && s.at(1).resourceVersion <= s.compactRevision {
		s.popFront()
	}
	if len(s.buf) > initialSnapshotterCapacity && s.count <= len(s.buf)/4 {
		newCap := max(initialSnapshotterCapacity, len(s.buf)/2)
		newBuf := make([]*btreeStore, newCap)
		if s.count > 0 {
			first := min(s.count, len(s.buf)-s.head)
			copy(newBuf, s.buf[s.head:s.head+first])
			if first < s.count {
				copy(newBuf[first:], s.buf[:s.count-first])
			}
		}
		s.buf = newBuf
		s.head = 0
	}
}

func (s *snapshotter) getSnapshot(rv uint64) *btreeStore {
	if s.count == 0 || s.at(0).resourceVersion > rv {
		return nil
	}
	if s.at(s.count-1).resourceVersion <= rv {
		return s.at(s.count - 1)
	}
	// Find the first index with resourceVersion > rv; the predecessor is <= rv.
	idx := sort.Search(s.count, func(i int) bool {
		return s.at(i).resourceVersion > rv
	})
	return s.at(idx - 1)
}

func (s *snapshotter) Len() int {
	return s.count
}
