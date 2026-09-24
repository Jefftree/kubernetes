/*
Copyright 2018 The Kubernetes Authors.

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

package internal

import (
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/structured-merge-diff/v7/fieldpath"
)

const (
	fieldsCacheShards    = 64
	maxEntriesPerShard   = 64
)

type fieldsCacheShard struct {
	mu      sync.RWMutex
	entries map[string]*fieldpath.Set
}

var fieldsV1Cache [fieldsCacheShards]fieldsCacheShard

func init() {
	for i := range fieldsV1Cache {
		fieldsV1Cache[i].entries = make(map[string]*fieldpath.Set, 16)
	}
}

func fieldsShardFor(s string) *fieldsCacheShard {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return &fieldsV1Cache[h&(fieldsCacheShards-1)]
}

// EmptyFields represents a set with no paths
// It looks like metav1.Fields{Raw: []byte("{}")}
var EmptyFields = func() metav1.FieldsV1 {
	f, err := SetToFields(*fieldpath.NewSet())
	if err != nil {
		panic("should never happen")
	}
	return f
}()

var emptyFieldPathSet = &fieldpath.Set{}

// FieldsToSetPtr returns an immutable cached *fieldpath.Set pointer for the given FieldsV1.
func FieldsToSetPtr(f metav1.FieldsV1) (*fieldpath.Set, error) {
	raw := f.GetRawString()
	if len(raw) == 0 || raw == "{}" {
		return emptyFieldPathSet, nil
	}
	shard := fieldsShardFor(raw)
	shard.mu.RLock()
	cached, ok := shard.entries[raw]
	shard.mu.RUnlock()
	if ok {
		return cached, nil
	}

	var s fieldpath.Set
	if err := s.FromJSON(f.GetRawReader()); err != nil {
		return nil, err
	}
	shard.mu.Lock()
	if existing, ok := shard.entries[raw]; ok {
		shard.mu.Unlock()
		return existing, nil
	}
	if len(shard.entries) >= maxEntriesPerShard {
		clear(shard.entries)
	}
	shard.entries[raw] = &s
	shard.mu.Unlock()
	return &s, nil
}

// FieldsToSet creates a set paths from an input trie of fields
func FieldsToSet(f metav1.FieldsV1) (s fieldpath.Set, err error) {
	ptr, err := FieldsToSetPtr(f)
	if err != nil {
		return fieldpath.Set{}, err
	}
	return *ptr, nil
}

// SetToFields creates a trie of fields from an input set of paths
func SetToFields(s fieldpath.Set) (f metav1.FieldsV1, err error) {
	if s.Empty() {
		f.SetRawString("{}")
		return f, nil
	}
	raw, err := s.ToJSON()
	if err != nil {
		return f, err
	}
	f.SetRawBytes(raw)
	rawStr := f.GetRawString()
	setCopy := s
	shard := fieldsShardFor(rawStr)
	shard.mu.Lock()
	if len(shard.entries) >= maxEntriesPerShard {
		clear(shard.entries)
	}
	shard.entries[rawStr] = &setCopy
	shard.mu.Unlock()
	return f, nil
}
