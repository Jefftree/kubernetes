/*
Copyright 2019 The Kubernetes Authors.

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
	"bytes"
	"fmt"
	"reflect"
	"sync"
	"unsafe"

	"sigs.k8s.io/structured-merge-diff/v7/fieldpath"
	"sigs.k8s.io/structured-merge-diff/v7/merge"
	"sigs.k8s.io/structured-merge-diff/v7/typed"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type structSubFieldInfo struct {
	parentIdx int
	fieldIdx  int
	offset    uintptr
	size      uintptr
	pool      sync.Pool
}

type structTopFieldsInfo struct {
	specIdx         int
	specOffset      uintptr
	specSize        uintptr
	specPool        sync.Pool
	statusIdx       int
	statusOffset    uintptr
	statusSize      uintptr
	statusPool      sync.Pool
	statusSubFields []structSubFieldInfo
	metaSubFields   []structSubFieldInfo
}

var structTopFieldsCache sync.Map // map[reflect.Type]*structTopFieldsInfo

func getStructTopFieldsInfo(t reflect.Type) *structTopFieldsInfo {
	if v, ok := structTopFieldsCache.Load(t); ok {
		return v.(*structTopFieldsInfo)
	}
	info := &structTopFieldsInfo{
		specIdx:   -1,
		statusIdx: -1,
	}
	if sf, ok := t.FieldByName("Spec"); ok && !sf.Anonymous && len(sf.Index) == 1 && sf.Type.Kind() == reflect.Struct {
		info.specIdx = sf.Index[0]
		info.specOffset = sf.Offset
		info.specSize = sf.Type.Size()
		specType := sf.Type
		info.specPool.New = func() any {
			v := reflect.New(specType).Elem()
			return &v
		}
	}
	if sf, ok := t.FieldByName("Status"); ok && !sf.Anonymous && len(sf.Index) == 1 && sf.Type.Kind() == reflect.Struct {
		info.statusIdx = sf.Index[0]
		info.statusOffset = sf.Offset
		info.statusSize = sf.Type.Size()
		statusType := sf.Type
		info.statusPool.New = func() any {
			v := reflect.New(statusType).Elem()
			return &v
		}
		for i := 0; i < statusType.NumField(); i++ {
			f := statusType.Field(i)
			if !f.IsExported() || f.Anonymous {
				continue
			}
			k := f.Type.Kind()
			if k == reflect.Slice || k == reflect.Map || k == reflect.Ptr || k == reflect.String || k == reflect.Struct {
				ft := f.Type
				sub := structSubFieldInfo{
					parentIdx: info.statusIdx,
					fieldIdx:  i,
					offset:    f.Offset,
					size:      ft.Size(),
				}
				sub.pool.New = func() any {
					v := reflect.New(ft).Elem()
					return &v
				}
				info.statusSubFields = append(info.statusSubFields, sub)
			}
		}
	}
	if sf, ok := t.FieldByName("ObjectMeta"); ok && len(sf.Index) == 1 && sf.Type.Kind() == reflect.Struct {
		metaIdx := sf.Index[0]
		metaType := sf.Type
		for _, name := range []string{"Annotations", "Labels", "OwnerReferences", "Finalizers"} {
			if f, ok := metaType.FieldByName(name); ok && len(f.Index) == 1 {
				ft := f.Type
				sub := structSubFieldInfo{
					parentIdx: metaIdx,
					fieldIdx:  f.Index[0],
					offset:    f.Offset,
					size:      ft.Size(),
				}
				sub.pool.New = func() any {
					v := reflect.New(ft).Elem()
					return &v
				}
				info.metaSubFields = append(info.metaSubFields, sub)
			}
		}
	}
	actual, _ := structTopFieldsCache.LoadOrStore(t, info)
	return actual.(*structTopFieldsInfo)
}

func shallowStructFieldEqual(a, b reflect.Value, offset, size uintptr) bool {
	if size == 0 {
		return true
	}
	ptrA := unsafe.Add(a.Addr().UnsafePointer(), offset)
	ptrB := unsafe.Add(b.Addr().UnsafePointer(), offset)
	return bytes.Equal(unsafe.Slice((*byte)(ptrA), size), unsafe.Slice((*byte)(ptrB), size))
}

type structuredMergeManager struct {
	typeConverter   TypeConverter
	objectConverter runtime.ObjectConvertor
	objectDefaulter runtime.ObjectDefaulter
	groupVersion    schema.GroupVersion
	hubVersion      schema.GroupVersion
	updater         merge.Updater
	ignoreSpec      bool
	ignoreStatus    bool
}

var _ Manager = &structuredMergeManager{}

func checkIgnoredTopFields(gv schema.GroupVersion, resetFields map[fieldpath.APIVersion]fieldpath.Filter) (ignoreSpec, ignoreStatus bool) {
	if resetFields == nil {
		return false, false
	}
	filter := resetFields[fieldpath.APIVersion(gv.String())]
	if filter == nil {
		return false, false
	}
	ignoreSpec = filter.Filter(fieldpath.NewSet(fieldpath.MakePathOrDie("spec"))).Empty()
	ignoreStatus = filter.Filter(fieldpath.NewSet(fieldpath.MakePathOrDie("status"))).Empty()
	return ignoreSpec, ignoreStatus
}

// NewStructuredMergeManager creates a new Manager that merges apply requests
// and update managed fields for other types of requests.
func NewStructuredMergeManager(typeConverter TypeConverter, objectConverter runtime.ObjectConvertor, objectDefaulter runtime.ObjectDefaulter, gv schema.GroupVersion, hub schema.GroupVersion, resetFields map[fieldpath.APIVersion]fieldpath.Filter) (Manager, error) {
	if typeConverter == nil {
		return nil, fmt.Errorf("typeconverter must not be nil")
	}
	ignoreSpec, ignoreStatus := checkIgnoredTopFields(gv, resetFields)
	return &structuredMergeManager{
		typeConverter:   typeConverter,
		objectConverter: objectConverter,
		objectDefaulter: objectDefaulter,
		groupVersion:    gv,
		hubVersion:      hub,
		updater: merge.Updater{
			Converter:    newVersionConverter(typeConverter, objectConverter, hub), // This is the converter provided to SMD from k8s
			IgnoreFilter: resetFields,
		},
		ignoreSpec:   ignoreSpec,
		ignoreStatus: ignoreStatus,
	}, nil
}

// NewCRDStructuredMergeManager creates a new Manager specifically for
// CRDs. This allows for the possibility of fields which are not defined
// in models, as well as having no models defined at all.
func NewCRDStructuredMergeManager(typeConverter TypeConverter, objectConverter runtime.ObjectConvertor, objectDefaulter runtime.ObjectDefaulter, gv schema.GroupVersion, hub schema.GroupVersion, resetFields map[fieldpath.APIVersion]fieldpath.Filter) (_ Manager, err error) {
	ignoreSpec, ignoreStatus := checkIgnoredTopFields(gv, resetFields)
	return &structuredMergeManager{
		typeConverter:   typeConverter,
		objectConverter: objectConverter,
		objectDefaulter: objectDefaulter,
		groupVersion:    gv,
		hubVersion:      hub,
		updater: merge.Updater{
			Converter:    newCRDVersionConverter(typeConverter, objectConverter, hub),
			IgnoreFilter: resetFields,
		},
		ignoreSpec:   ignoreSpec,
		ignoreStatus: ignoreStatus,
	}, nil
}

func objectGVKNN(obj runtime.Object) string {
	name := "<unknown>"
	namespace := "<unknown>"
	if accessor, err := meta.Accessor(obj); err == nil {
		name = accessor.GetName()
		namespace = accessor.GetNamespace()
	}

	return fmt.Sprintf("%v/%v; %v", namespace, name, obj.GetObjectKind().GroupVersionKind())
}

type savedPairSubField struct {
	sub       *structSubFieldInfo
	liveField reflect.Value
	newField  reflect.Value
	savedLive *reflect.Value
	savedNew  *reflect.Value
}

type updateZeroState struct {
	info            *structTopFieldsInfo
	specZeroed      bool
	statusZeroed    bool
	liveSpecField   reflect.Value
	newSpecField    reflect.Value
	savedLiveSpec   *reflect.Value
	savedNewSpec    *reflect.Value
	liveStatusField reflect.Value
	newStatusField  reflect.Value
	savedLiveStatus *reflect.Value
	savedNewStatus  *reflect.Value
	subSaved        [24]savedPairSubField
	numSubSaved     int
}

func (z *updateZeroState) restore() {
	for i := z.numSubSaved - 1; i >= 0; i-- {
		entry := &z.subSaved[i]
		entry.newField.Set(*entry.savedNew)
		entry.liveField.Set(*entry.savedLive)
		entry.savedNew.SetZero()
		entry.savedLive.SetZero()
		entry.sub.pool.Put(entry.savedNew)
		entry.sub.pool.Put(entry.savedLive)
		z.subSaved[i] = savedPairSubField{}
	}
	z.numSubSaved = 0
	if z.statusZeroed {
		z.newStatusField.Set(*z.savedNewStatus)
		z.liveStatusField.Set(*z.savedLiveStatus)
		z.savedNewStatus.SetZero()
		z.savedLiveStatus.SetZero()
		z.info.statusPool.Put(z.savedNewStatus)
		z.info.statusPool.Put(z.savedLiveStatus)
		z.statusZeroed = false
	}
	if z.specZeroed {
		z.newSpecField.Set(*z.savedNewSpec)
		z.liveSpecField.Set(*z.savedLiveSpec)
		z.savedNewSpec.SetZero()
		z.savedLiveSpec.SetZero()
		z.info.specPool.Put(z.savedNewSpec)
		z.info.specPool.Put(z.savedLiveSpec)
		z.specZeroed = false
	}
}

// Update implements Manager.
func (f *structuredMergeManager) Update(liveObj, newObj runtime.Object, managed Managed, manager string) (runtime.Object, Managed, error) {
	apiVersion := fieldpath.APIVersion(f.groupVersion.String())

	var z updateZeroState
	sameVersion := true
	for _, vs := range managed.Fields() {
		if vs.APIVersion() != apiVersion {
			sameVersion = false
			break
		}
	}
	if sameVersion {
		liveRV := reflect.ValueOf(liveObj)
		newRV := reflect.ValueOf(newObj)
		if liveRV.Kind() == reflect.Ptr && newRV.Kind() == reflect.Ptr && liveRV.Type() == newRV.Type() && !liveRV.IsNil() && !newRV.IsNil() {
			liveElem := liveRV.Elem()
			newElem := newRV.Elem()
			if liveElem.Kind() == reflect.Struct {
				z.info = getStructTopFieldsInfo(liveElem.Type())
				if z.info.specIdx >= 0 && (f.ignoreSpec || shallowStructFieldEqual(liveElem, newElem, z.info.specOffset, z.info.specSize)) {
					z.liveSpecField = liveElem.Field(z.info.specIdx)
					z.newSpecField = newElem.Field(z.info.specIdx)
					z.savedLiveSpec = z.info.specPool.Get().(*reflect.Value)
					z.savedNewSpec = z.info.specPool.Get().(*reflect.Value)
					z.savedLiveSpec.Set(z.liveSpecField)
					z.savedNewSpec.Set(z.newSpecField)
					z.liveSpecField.SetZero()
					z.newSpecField.SetZero()
					z.specZeroed = true
				}
				if z.info.statusIdx >= 0 && (f.ignoreStatus || shallowStructFieldEqual(liveElem, newElem, z.info.statusOffset, z.info.statusSize)) {
					z.liveStatusField = liveElem.Field(z.info.statusIdx)
					z.newStatusField = newElem.Field(z.info.statusIdx)
					z.savedLiveStatus = z.info.statusPool.Get().(*reflect.Value)
					z.savedNewStatus = z.info.statusPool.Get().(*reflect.Value)
					z.savedLiveStatus.Set(z.liveStatusField)
					z.savedNewStatus.Set(z.newStatusField)
					z.liveStatusField.SetZero()
					z.newStatusField.SetZero()
					z.statusZeroed = true
				} else if z.info.statusIdx >= 0 && len(z.info.statusSubFields) > 0 {
					liveStatus := liveElem.Field(z.info.statusIdx)
					newStatus := newElem.Field(z.info.statusIdx)
					for i := range z.info.statusSubFields {
						if z.numSubSaved >= len(z.subSaved) {
							break
						}
						sf := &z.info.statusSubFields[i]
						lf := liveStatus.Field(sf.fieldIdx)
						if !lf.IsZero() && shallowStructFieldEqual(liveStatus, newStatus, sf.offset, sf.size) {
							nf := newStatus.Field(sf.fieldIdx)
							sl := sf.pool.Get().(*reflect.Value)
							sn := sf.pool.Get().(*reflect.Value)
							sl.Set(lf)
							sn.Set(nf)
							lf.SetZero()
							nf.SetZero()
							z.subSaved[z.numSubSaved] = savedPairSubField{sub: sf, liveField: lf, newField: nf, savedLive: sl, savedNew: sn}
							z.numSubSaved++
						}
					}
				}
				for i := range z.info.metaSubFields {
					if z.numSubSaved >= len(z.subSaved) {
						break
					}
					sf := &z.info.metaSubFields[i]
					liveMeta := liveElem.Field(sf.parentIdx)
					newMeta := newElem.Field(sf.parentIdx)
					lf := liveMeta.Field(sf.fieldIdx)
					if !lf.IsZero() && shallowStructFieldEqual(liveMeta, newMeta, sf.offset, sf.size) {
						nf := newMeta.Field(sf.fieldIdx)
						sl := sf.pool.Get().(*reflect.Value)
						sn := sf.pool.Get().(*reflect.Value)
						sl.Set(lf)
						sn.Set(nf)
						lf.SetZero()
						nf.SetZero()
						z.subSaved[z.numSubSaved] = savedPairSubField{sub: sf, liveField: lf, newField: nf, savedLive: sl, savedNew: sn}
						z.numSubSaved++
					}
				}
				if z.specZeroed || z.statusZeroed || z.numSubSaved > 0 {
					defer z.restore()
				}
			}
		}
	}

	newObjVersioned, err := f.toVersioned(newObj)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to convert new object (%v) to proper version (%v): %v", objectGVKNN(newObj), f.groupVersion, err)
	}
	liveObjVersioned, err := f.toVersioned(liveObj)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to convert live object (%v) to proper version: %v", objectGVKNN(liveObj), err)
	}
	if liveObjVersioned != liveObj && newObjVersioned != newObj {
		z.restore()
	}

	newObjTyped, err := f.typeConverter.ObjectToTyped(newObjVersioned, typed.AllowDuplicates)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to convert new object (%v) to smd typed: %v", objectGVKNN(newObjVersioned), err)
	}
	liveObjTyped, err := f.typeConverter.ObjectToTyped(liveObjVersioned, typed.AllowDuplicates)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to convert live object (%v) to smd typed: %v", objectGVKNN(liveObjVersioned), err)
	}

	// TODO(apelisse) use the first return value when unions are implemented
	_, managedFields, err := f.updater.Update(liveObjTyped, newObjTyped, apiVersion, managed.Fields(), manager)
	z.restore()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to update ManagedFields (%v): %v", objectGVKNN(newObjVersioned), err)
	}
	if ms, ok := managed.(*managedStruct); ok {
		managed = NewManagedWithOrig(managedFields, managed.Times(), ms.origEntries)
	} else {
		managed = NewManaged(managedFields, managed.Times())
	}

	return newObj, managed, nil
}

var (
	specFieldName     = "spec"
	statusFieldName   = "status"
	specPathElement   = fieldpath.PathElement{FieldName: &specFieldName}
	statusPathElement = fieldpath.PathElement{FieldName: &statusFieldName}
)

type applyZeroState struct {
	info             *structTopFieldsInfo
	liveStructType   reflect.Type
	skipSpec         bool
	skipStatus       bool
	liveSpecZeroed   bool
	liveStatusZeroed bool
	liveSpecField    reflect.Value
	liveStatusField  reflect.Value
	savedLiveSpec    *reflect.Value
	savedLiveStatus  *reflect.Value
}

func (a *applyZeroState) cleanup() {
	if a.liveStatusZeroed {
		a.liveStatusField.Set(*a.savedLiveStatus)
		a.liveStatusZeroed = false
	}
	if a.liveSpecZeroed {
		a.liveSpecField.Set(*a.savedLiveSpec)
		a.liveSpecZeroed = false
	}
	if a.skipStatus {
		a.savedLiveStatus.SetZero()
		a.info.statusPool.Put(a.savedLiveStatus)
		a.skipStatus = false
	}
	if a.skipSpec {
		a.savedLiveSpec.SetZero()
		a.info.specPool.Put(a.savedLiveSpec)
		a.skipSpec = false
	}
}

// Apply implements Manager.
func (f *structuredMergeManager) Apply(liveObj, patchObj runtime.Object, managed Managed, manager string, force bool) (runtime.Object, Managed, error) {
	// Check that the patch object has the same version as the live object
	if patchVersion := patchObj.GetObjectKind().GroupVersionKind().GroupVersion(); patchVersion != f.groupVersion {
		return nil, nil,
			errors.NewBadRequest(
				fmt.Sprintf("Incorrect version specified in apply patch. "+
					"Specified patch version: %s, expected: %s",
					patchVersion, f.groupVersion))
	}

	patchObjMeta, err := meta.Accessor(patchObj)
	if err != nil {
		return nil, nil, fmt.Errorf("couldn't get accessor: %v", err)
	}
	if patchObjMeta.GetManagedFields() != nil {
		return nil, nil, errors.NewBadRequest("metadata.managedFields must be nil")
	}

	apiVersion := fieldpath.APIVersion(f.groupVersion.String())
	var a applyZeroState
	if u, ok := patchObj.(*unstructured.Unstructured); ok && u.Object != nil {
		if liveMeta, err := meta.Accessor(liveObj); err == nil && liveMeta.GetUID() != "" {
			sameVersion := true
			for _, vs := range managed.Fields() {
				if vs.APIVersion() != apiVersion {
					sameVersion = false
					break
				}
			}
			if sameVersion {
				if liveRV := reflect.ValueOf(liveObj); liveRV.Kind() == reflect.Ptr && !liveRV.IsNil() {
					if liveElem := liveRV.Elem(); liveElem.Kind() == reflect.Struct {
						a.liveStructType = liveElem.Type()
						a.info = getStructTopFieldsInfo(a.liveStructType)
						lastSet := managed.Fields()[manager]
						if _, hasSpec := u.Object["spec"]; !hasSpec && a.info.specIdx >= 0 && (lastSet == nil || !lastSet.Set().Members.Has(specPathElement)) {
							a.liveSpecField = liveElem.Field(a.info.specIdx)
							a.savedLiveSpec = a.info.specPool.Get().(*reflect.Value)
							a.savedLiveSpec.Set(a.liveSpecField)
							a.liveSpecField.SetZero()
							a.liveSpecZeroed = true
							a.skipSpec = true
						}
						if _, hasStatus := u.Object["status"]; !hasStatus && a.info.statusIdx >= 0 && (lastSet == nil || !lastSet.Set().Members.Has(statusPathElement)) {
							a.liveStatusField = liveElem.Field(a.info.statusIdx)
							a.savedLiveStatus = a.info.statusPool.Get().(*reflect.Value)
							a.savedLiveStatus.Set(a.liveStatusField)
							a.liveStatusField.SetZero()
							a.liveStatusZeroed = true
							a.skipStatus = true
						}
						if a.skipSpec || a.skipStatus {
							defer a.cleanup()
						}
					}
				}
			}
		}
	}

	liveObjVersioned, err := f.toVersioned(liveObj)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to convert live object (%v) to proper version: %v", objectGVKNN(liveObj), err)
	}
	if liveObjVersioned != liveObj {
		if a.liveStatusZeroed {
			a.liveStatusField.Set(*a.savedLiveStatus)
			a.liveStatusZeroed = false
		}
		if a.liveSpecZeroed {
			a.liveSpecField.Set(*a.savedLiveSpec)
			a.liveSpecZeroed = false
		}
	}

	// Don't allow duplicates in the applied object.
	patchObjTyped, err := f.typeConverter.ObjectToTyped(patchObj)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create typed patch object (%v): %v", objectGVKNN(patchObj), err)
	}

	liveObjTyped, err := f.typeConverter.ObjectToTyped(liveObjVersioned, typed.AllowDuplicates)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create typed live object (%v): %v", objectGVKNN(liveObjVersioned), err)
	}

	newObjTyped, managedFields, err := f.updater.Apply(liveObjTyped, patchObjTyped, apiVersion, managed.Fields(), manager, force)
	if err != nil {
		return nil, nil, err
	}
	if ms, ok := managed.(*managedStruct); ok {
		managed = NewManagedWithOrig(managedFields, managed.Times(), ms.origEntries)
	} else {
		managed = NewManaged(managedFields, managed.Times())
	}

	if newObjTyped == nil {
		return nil, managed, nil
	}

	newObj, err := f.typeConverter.TypedToObject(newObjTyped)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to convert new typed object (%v) to object: %v", objectGVKNN(patchObj), err)
	}
	if a.liveStatusZeroed {
		a.liveStatusField.Set(*a.savedLiveStatus)
		a.liveStatusZeroed = false
	}
	if a.liveSpecZeroed {
		a.liveSpecField.Set(*a.savedLiveSpec)
		a.liveSpecZeroed = false
	}

	newObjVersioned, err := f.toVersioned(newObj)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to convert new object (%v) to proper version: %v", objectGVKNN(patchObj), err)
	}
	f.objectDefaulter.Default(newObjVersioned)

	newObjUnversioned, err := f.toUnversioned(newObjVersioned)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to convert to unversioned (%v): %v", objectGVKNN(patchObj), err)
	}
	if a.skipSpec || a.skipStatus {
		if newRV := reflect.ValueOf(newObjUnversioned); newRV.Kind() == reflect.Ptr && !newRV.IsNil() {
			if newElem := newRV.Elem(); newElem.Kind() == reflect.Struct && newElem.Type() == a.liveStructType {
				if a.skipStatus {
					newElem.Field(a.info.statusIdx).Set(*a.savedLiveStatus)
				}
				if a.skipSpec {
					newElem.Field(a.info.specIdx).Set(*a.savedLiveSpec)
				}
			}
		}
		a.cleanup()
	}
	return newObjUnversioned, managed, nil
}

func (f *structuredMergeManager) toVersioned(obj runtime.Object) (runtime.Object, error) {
	return f.objectConverter.ConvertToVersion(obj, f.groupVersion)
}

func (f *structuredMergeManager) toUnversioned(obj runtime.Object) (runtime.Object, error) {
	return f.objectConverter.ConvertToVersion(obj, f.hubVersion)
}
