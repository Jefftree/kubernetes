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
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"sigs.k8s.io/structured-merge-diff/v7/fieldpath"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type managerKey struct {
	manager     string
	operation   metav1.ManagedFieldsOperationType
	apiVersion  string
	subresource string
}

var (
	managerIDCacheMu sync.RWMutex
	managerIDCache   = make(map[managerKey]string, 64)
	managerEntryMu   sync.RWMutex
	managerEntryMap  = make(map[string]metav1.ManagedFieldsEntry, 64)
)

type origManagedEntry struct {
	entry metav1.ManagedFieldsEntry
	set   *fieldpath.Set
}

// ManagedInterface groups a fieldpath.ManagedFields together with the timestamps associated with each operation.
type ManagedInterface interface {
	// Fields gets the fieldpath.ManagedFields.
	Fields() fieldpath.ManagedFields

	// Times gets the timestamps associated with each operation.
	Times() map[string]*metav1.Time
}

type managedStruct struct {
	fields      fieldpath.ManagedFields
	times       map[string]*metav1.Time
	origEntries map[string]origManagedEntry
}

var _ ManagedInterface = &managedStruct{}

// Fields implements ManagedInterface.
func (m *managedStruct) Fields() fieldpath.ManagedFields {
	return m.fields
}

// Times implements ManagedInterface.
func (m *managedStruct) Times() map[string]*metav1.Time {
	return m.times
}

// NewEmptyManaged creates an empty ManagedInterface.
func NewEmptyManaged() ManagedInterface {
	return NewManaged(fieldpath.ManagedFields{}, map[string]*metav1.Time{})
}

// NewManaged creates a ManagedInterface from a fieldpath.ManagedFields and the timestamps associated with each operation.
func NewManaged(f fieldpath.ManagedFields, t map[string]*metav1.Time) ManagedInterface {
	return &managedStruct{
		fields: f,
		times:  t,
	}
}

func NewManagedWithOrig(f fieldpath.ManagedFields, t map[string]*metav1.Time, orig map[string]origManagedEntry) ManagedInterface {
	return &managedStruct{
		fields:      f,
		times:       t,
		origEntries: orig,
	}
}

// RemoveObjectManagedFields removes the ManagedFields from the object
// before we merge so that it doesn't appear in the ManagedFields
// recursively.
func RemoveObjectManagedFields(obj runtime.Object) {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		panic(fmt.Sprintf("couldn't get accessor: %v", err))
	}
	accessor.SetManagedFields(nil)
}

// EncodeObjectManagedFields converts and stores the fieldpathManagedFields into the objects ManagedFields
func EncodeObjectManagedFields(obj runtime.Object, managed ManagedInterface) error {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		panic(fmt.Sprintf("couldn't get accessor: %v", err))
	}

	encodedManagedFields, err := encodeManagedFields(managed)
	if err != nil {
		return fmt.Errorf("failed to convert back managed fields to API: %v", err)
	}
	accessor.SetManagedFields(encodedManagedFields)

	return nil
}

// DecodeManagedFields converts ManagedFields from the wire format (api format)
// to the format used by sigs.k8s.io/structured-merge-diff
func DecodeManagedFields(encodedManagedFields []metav1.ManagedFieldsEntry) (ManagedInterface, error) {
	managed := managedStruct{
		fields:      make(fieldpath.ManagedFields, len(encodedManagedFields)),
		times:       make(map[string]*metav1.Time, len(encodedManagedFields)),
		origEntries: make(map[string]origManagedEntry, len(encodedManagedFields)),
	}

	for i, encodedVersionedSet := range encodedManagedFields {
		switch encodedVersionedSet.Operation {
		case metav1.ManagedFieldsOperationApply, metav1.ManagedFieldsOperationUpdate:
		default:
			return nil, fmt.Errorf("operation must be `Apply` or `Update`")
		}
		if len(encodedVersionedSet.APIVersion) < 1 {
			return nil, fmt.Errorf("apiVersion must not be empty")
		}
		switch encodedVersionedSet.FieldsType {
		case "FieldsV1":
			// Valid case.
		case "":
			return nil, fmt.Errorf("missing fieldsType in managed fields entry %d", i)
		default:
			return nil, fmt.Errorf("invalid fieldsType %q in managed fields entry %d", encodedVersionedSet.FieldsType, i)
		}
		manager, err := BuildManagerIdentifier(&encodedVersionedSet)
		if err != nil {
			return nil, fmt.Errorf("error decoding manager from %v: %v", encodedVersionedSet, err)
		}
		vs, err := decodeVersionedSet(&encodedVersionedSet)
		if err != nil {
			return nil, fmt.Errorf("error decoding versioned set from %v: %v", encodedVersionedSet, err)
		}
		managed.fields[manager] = vs
		managed.times[manager] = encodedVersionedSet.Time
		managed.origEntries[manager] = origManagedEntry{
			entry: encodedVersionedSet,
			set:   vs.Set(),
		}
	}
	return &managed, nil
}

// BuildManagerIdentifier creates a manager identifier string from a ManagedFieldsEntry
func BuildManagerIdentifier(encodedManager *metav1.ManagedFieldsEntry) (manager string, err error) {
	apiVersion := encodedManager.APIVersion
	if encodedManager.Operation == metav1.ManagedFieldsOperationApply {
		apiVersion = ""
	}
	key := managerKey{
		manager:     encodedManager.Manager,
		operation:   encodedManager.Operation,
		apiVersion:  apiVersion,
		subresource: encodedManager.Subresource,
	}
	managerIDCacheMu.RLock()
	cached, ok := managerIDCache[key]
	managerIDCacheMu.RUnlock()
	if ok {
		return cached, nil
	}

	encodedManagerCopy := *encodedManager
	encodedManagerCopy.FieldsType = ""
	encodedManagerCopy.FieldsV1 = nil
	encodedManagerCopy.Time = nil
	encodedManagerCopy.APIVersion = apiVersion

	b, err := json.Marshal(&encodedManagerCopy)
	if err != nil {
		return "", fmt.Errorf("error marshalling manager identifier: %v", err)
	}
	id := string(b)

	managerIDCacheMu.Lock()
	if len(managerIDCache) >= 1024 {
		clear(managerIDCache)
	}
	managerIDCache[key] = id
	managerIDCacheMu.Unlock()

	managerEntryMu.Lock()
	if len(managerEntryMap) >= 1024 {
		clear(managerEntryMap)
	}
	managerEntryMap[id] = encodedManagerCopy
	managerEntryMu.Unlock()

	return id, nil
}

func decodeVersionedSet(encodedVersionedSet *metav1.ManagedFieldsEntry) (versionedSet fieldpath.VersionedSet, err error) {
	fields := EmptyFields
	if encodedVersionedSet.FieldsV1 != nil {
		fields = *encodedVersionedSet.FieldsV1
	}
	setPtr, err := FieldsToSetPtr(fields)
	if err != nil {
		return nil, fmt.Errorf("error decoding set: %v", err)
	}
	return fieldpath.NewVersionedSet(setPtr, fieldpath.APIVersion(encodedVersionedSet.APIVersion), encodedVersionedSet.Operation == metav1.ManagedFieldsOperationApply), nil
}

// encodeManagedFields converts ManagedFields from the format used by
// sigs.k8s.io/structured-merge-diff to the wire format (api format)
func encodeManagedFields(managed ManagedInterface) (encodedManagedFields []metav1.ManagedFieldsEntry, err error) {
	fields := managed.Fields()
	if len(fields) == 0 {
		return nil, nil
	}
	var origEntries map[string]origManagedEntry
	if ms, ok := managed.(*managedStruct); ok {
		origEntries = ms.origEntries
	}
	times := managed.Times()
	encodedManagedFields = make([]metav1.ManagedFieldsEntry, 0, len(fields))
	for manager, versionedSet := range fields {
		if orig, ok := origEntries[manager]; ok && orig.entry.FieldsV1 != nil &&
			orig.entry.APIVersion == string(versionedSet.APIVersion()) &&
			(orig.entry.Operation == metav1.ManagedFieldsOperationApply) == versionedSet.Applied() &&
			(versionedSet.Set() == orig.set || versionedSet.Set().Equals(orig.set)) {
			entry := orig.entry
			if t, ok := times[manager]; ok {
				entry.Time = t
			}
			encodedManagedFields = append(encodedManagedFields, entry)
			continue
		}
		v, err := encodeManagerVersionedSet(manager, versionedSet)
		if err != nil {
			return nil, fmt.Errorf("error encoding versioned set for %v: %v", manager, err)
		}
		if t, ok := times[manager]; ok {
			v.Time = t
		}
		encodedManagedFields = append(encodedManagedFields, *v)
	}
	return sortEncodedManagedFields(encodedManagedFields)
}

func sortEncodedManagedFields(encodedManagedFields []metav1.ManagedFieldsEntry) (sortedManagedFields []metav1.ManagedFieldsEntry, err error) {
	sort.Slice(encodedManagedFields, func(i, j int) bool {
		p, q := encodedManagedFields[i], encodedManagedFields[j]

		if p.Operation != q.Operation {
			return p.Operation < q.Operation
		}

		pSeconds, qSeconds := int64(0), int64(0)
		if p.Time != nil {
			pSeconds = p.Time.Unix()
		}
		if q.Time != nil {
			qSeconds = q.Time.Unix()
		}
		if pSeconds != qSeconds {
			return pSeconds < qSeconds
		}

		if p.Manager != q.Manager {
			return p.Manager < q.Manager
		}

		if p.APIVersion != q.APIVersion {
			return p.APIVersion < q.APIVersion
		}
		return p.Subresource < q.Subresource
	})

	return encodedManagedFields, nil
}

func encodeManagerVersionedSet(manager string, versionedSet fieldpath.VersionedSet) (encodedVersionedSet *metav1.ManagedFieldsEntry, err error) {
	managerEntryMu.RLock()
	cachedEntry, ok := managerEntryMap[manager]
	managerEntryMu.RUnlock()
	if ok {
		entry := cachedEntry
		encodedVersionedSet = &entry
	} else {
		encodedVersionedSet = &metav1.ManagedFieldsEntry{}
		// Get as many fields as we can from the manager identifier
		err = json.Unmarshal([]byte(manager), encodedVersionedSet)
		if err != nil {
			return nil, fmt.Errorf("error unmarshalling manager identifier %v: %v", manager, err)
		}
	}

	// Get the APIVersion, Operation, and Fields from the VersionedSet
	encodedVersionedSet.APIVersion = string(versionedSet.APIVersion())
	if versionedSet.Applied() {
		encodedVersionedSet.Operation = metav1.ManagedFieldsOperationApply
	}
	encodedVersionedSet.FieldsType = "FieldsV1"
	fields, err := SetToFields(*versionedSet.Set())
	if err != nil {
		return nil, fmt.Errorf("error encoding set: %v", err)
	}
	encodedVersionedSet.FieldsV1 = &fields

	return encodedVersionedSet, nil
}
