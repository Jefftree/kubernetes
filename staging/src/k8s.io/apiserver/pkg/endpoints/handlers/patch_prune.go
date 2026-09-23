/*
Copyright The Kubernetes Authors.

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

package handlers

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// A strategic merge patch only writes the keys it names, so every top-level field
// the patch does not name comes out of the merge exactly as it went in. A pruned
// patch converts only the named fields to and from unstructured and deep copies
// the rest across, instead of round tripping the whole object. Within metadata the
// same holds for managedFields, whose fieldsV1 are the most expensive part of the
// round trip, as long as the metadata patch neither names it nor carries a
// directive.
type prunedPatch struct {
	fields   *topLevelFields
	original reflect.Value
	target   reflect.Value
	// named holds the struct field indexes the patch reaches.
	named []bool
	// keepManagedFields is set when metadata is named but managedFields is not.
	keepManagedFields bool
}

type topLevelFields struct {
	// byName maps a top-level JSON key onto a struct field index. The keys of an
	// inlined TypeMeta map onto the TypeMeta field itself.
	byName     map[string]int
	numFields  int
	typeMeta   int
	objectMeta int
	// mayBeOpaque marks the fields whose values have to be checked with
	// containsOpaque before they can skip the round trip.
	mayBeOpaque []bool
}

var (
	topLevelFieldsCache sync.Map // reflect.Type -> *topLevelFields, nil if unsupported
	typeMetaType        = reflect.TypeFor[metav1.TypeMeta]()
	objectMetaType      = reflect.TypeFor[metav1.ObjectMeta]()
)

// fieldsOf reports the top-level layout of t, or nil when t is anything other
// than a struct of exported, JSON named fields with an inlined TypeMeta and an
// embedded ObjectMeta. That is the shape of every built-in API type.
func fieldsOf(t reflect.Type) *topLevelFields {
	if cached, ok := topLevelFieldsCache.Load(t); ok {
		return cached.(*topLevelFields)
	}
	fields := computeTopLevelFields(t)
	topLevelFieldsCache.Store(t, fields)
	return fields
}

func computeTopLevelFields(t reflect.Type) *topLevelFields {
	fields := &topLevelFields{byName: map[string]int{}, numFields: t.NumField(), typeMeta: -1, objectMeta: -1, mayBeOpaque: make([]bool, t.NumField())}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			return nil
		}
		// ObjectMeta is exempt: its only opaque content is the fieldsV1 of
		// managedFields, which the field manager decodes and re-encodes after
		// every patch, so how their JSON was laid out never reaches the result.
		fields.mayBeOpaque[i] = f.Type != objectMetaType && roundTripInfoOf(f.Type).mayBeOpaque
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch {
		case f.Anonymous && f.Type == typeMetaType && name == "":
			fields.typeMeta = i
			fields.byName["apiVersion"] = i
			fields.byName["kind"] = i
			continue
		case name == "" || name == "-" || f.Anonymous && f.Type != objectMetaType:
			return nil
		case f.Type == objectMetaType:
			if name != "metadata" {
				return nil
			}
			fields.objectMeta = i
		}
		if _, dup := fields.byName[name]; dup {
			return nil
		}
		fields.byName[name] = i
	}
	if fields.typeMeta < 0 || fields.objectMeta < 0 {
		return nil
	}
	return fields
}

var (
	jsonMarshalerType          = reflect.TypeFor[json.Marshaler]()
	jsonUnmarshalerType        = reflect.TypeFor[json.Unmarshaler]()
	unstructuredConverterType  = reflect.TypeFor[interface{ ToUnstructured() interface{} }]()
	roundTripsToSameValueTypes = map[reflect.Type]bool{
		reflect.TypeFor[metav1.Time]():        true,
		reflect.TypeFor[metav1.MicroTime]():   true,
		reflect.TypeFor[metav1.Duration]():    true,
		reflect.TypeFor[resource.Quantity]():  true,
		reflect.TypeFor[intstr.IntOrString](): true,
	}
	roundTripInfoCache sync.Map // reflect.Type -> *roundTripInfo
)

// A value is opaque when converting it to unstructured and back can yield
// something other than a deep copy of it. That is the case for a field the
// conversion drops, and for a custom marshaler that holds raw JSON, such as
// runtime.RawExtension, which comes back with its keys sorted. The known scalar
// marshalers come back as the same value.
type roundTripInfo struct {
	// mayBeOpaque is false when no value of the type can be opaque.
	mayBeOpaque bool
	// opaque is set when any value of the type is.
	opaque bool
	// fields lists the struct fields that may be opaque.
	fields []int
}

func roundTripInfoOf(t reflect.Type) *roundTripInfo {
	if cached, ok := roundTripInfoCache.Load(t); ok {
		return cached.(*roundTripInfo)
	}
	info := &roundTripInfo{opaque: isOpaqueType(t), mayBeOpaque: mayBeOpaque(t, map[reflect.Type]bool{})}
	if t.Kind() == reflect.Struct && info.mayBeOpaque && !info.opaque {
		for i := 0; i < t.NumField(); i++ {
			if mayBeOpaque(t.Field(i).Type, map[reflect.Type]bool{}) {
				info.fields = append(info.fields, i)
			}
		}
	}
	roundTripInfoCache.Store(t, info)
	return info
}

func isOpaqueType(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer || roundTripsToSameValueTypes[t] {
		return false
	}
	pt := reflect.PointerTo(t)
	if t.Implements(jsonMarshalerType) || pt.Implements(jsonMarshalerType) ||
		t.Implements(jsonUnmarshalerType) || pt.Implements(jsonUnmarshalerType) ||
		t.Implements(unstructuredConverterType) || pt.Implements(unstructuredConverterType) {
		return true
	}
	switch t.Kind() {
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if f := t.Field(i); !f.IsExported() || f.Tag.Get("json") == "-" {
				return true
			}
		}
		return false
	case reflect.Interface, reflect.Func, reflect.Chan, reflect.UnsafePointer, reflect.Complex64, reflect.Complex128, reflect.Uintptr:
		return true
	default:
		return false
	}
}

// mayBeOpaque reports whether an opaque type is reachable from t. A type already
// being visited contributes nothing more, since its other paths are explored by
// the call that first reached it.
func mayBeOpaque(t reflect.Type, visiting map[reflect.Type]bool) bool {
	if visiting[t] || roundTripsToSameValueTypes[t] {
		return false
	}
	if isOpaqueType(t) {
		return true
	}
	visiting[t] = true
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return mayBeOpaque(t.Elem(), visiting)
	case reflect.Map:
		return mayBeOpaque(t.Key(), visiting) || mayBeOpaque(t.Elem(), visiting)
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if mayBeOpaque(t.Field(i).Type, visiting) {
				return true
			}
		}
	}
	return false
}

// containsOpaque reports whether v holds an opaque value.
func containsOpaque(v reflect.Value) bool {
	info := roundTripInfoOf(v.Type())
	if !info.mayBeOpaque {
		return false
	}
	if info.opaque {
		return true
	}
	switch v.Kind() {
	case reflect.Pointer:
		return !v.IsNil() && containsOpaque(v.Elem())
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if containsOpaque(v.Index(i)) {
				return true
			}
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			if containsOpaque(iter.Key()) || containsOpaque(iter.Value()) {
				return true
			}
		}
	case reflect.Struct:
		for _, i := range info.fields {
			if containsOpaque(v.Field(i)) {
				return true
			}
		}
	}
	return false
}

// planPrunedPatch reports ok=false when the patch cannot be pruned, in which case
// the caller must round trip the whole object.
func planPrunedPatch(originalObject, objToUpdate runtime.Object, patchMap map[string]interface{}) (*prunedPatch, bool) {
	original, target := reflect.ValueOf(originalObject), reflect.ValueOf(objToUpdate)
	if original.Kind() != reflect.Pointer || original.IsNil() || target.Kind() != reflect.Pointer || target.IsNil() {
		return nil, false
	}
	t := original.Elem().Type()
	if t.Kind() != reflect.Struct || target.Elem().Type() != t {
		return nil, false
	}
	fields := fieldsOf(t)
	if fields == nil {
		return nil, false
	}

	p := &prunedPatch{fields: fields, original: original.Elem(), target: target.Elem(), named: make([]bool, fields.numFields)}
	p.named[fields.typeMeta] = true
	for i, mayBeOpaque := range fields.mayBeOpaque {
		if mayBeOpaque && containsOpaque(p.original.Field(i)) {
			p.named[i] = true
		}
	}
	metadataDirective := false
	for k := range patchMap {
		name := k
		if strings.HasPrefix(k, "$") {
			// A bare directive such as $patch or $retainKeys acts on the whole object.
			_, after, found := strings.Cut(k, "/")
			if !found {
				return nil, false
			}
			name = after
			if name == "metadata" {
				metadataDirective = true
			}
		}
		i, ok := fields.byName[name]
		if !ok {
			return nil, false
		}
		p.named[i] = true
	}
	if p.named[fields.objectMeta] && !metadataDirective {
		if metadataPatch, ok := patchMap["metadata"].(map[string]interface{}); ok {
			p.keepManagedFields = true
			for k := range metadataPatch {
				if k == "managedFields" || strings.HasPrefix(k, "$") {
					p.keepManagedFields = false
					break
				}
			}
		}
	}
	return p, true
}

func (p *prunedPatch) apply(
	requestContext context.Context,
	defaulter runtime.ObjectDefaulter,
	patchMap map[string]interface{},
	schemaReferenceObj runtime.Object,
	strictErrs []error,
	validationDirective string,
) error {
	scratch := reflect.New(p.original.Type())
	for i, named := range p.named {
		if named {
			scratch.Elem().Field(i).Set(p.original.Field(i))
		}
	}
	if p.keepManagedFields {
		objectMeta(scratch.Elem(), p.fields).ManagedFields = nil
	}
	partialMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(scratch.Interface())
	if err != nil {
		return err
	}

	// Defaulting waits until the object is whole again, since a defaulter may read
	// fields that the partial object lacks.
	objToUpdate := p.target.Addr().Interface().(runtime.Object)
	if err := applyPatchToObject(requestContext, noopDefaulter{}, partialMap, patchMap, objToUpdate, schemaReferenceObj, strictErrs, validationDirective); err != nil {
		return err
	}

	// Deep copy the fields carried across, so the result never aliases the
	// original, which can be shared with a cache.
	scratch.Elem().SetZero()
	for i, named := range p.named {
		if !named {
			scratch.Elem().Field(i).Set(p.original.Field(i))
		}
	}
	if p.keepManagedFields {
		objectMeta(scratch.Elem(), p.fields).ManagedFields = objectMeta(p.original, p.fields).ManagedFields
	}
	rest := reflect.ValueOf(scratch.Interface().(runtime.Object).DeepCopyObject()).Elem()
	for i, named := range p.named {
		if !named {
			p.target.Field(i).Set(rest.Field(i))
		}
	}
	if p.keepManagedFields {
		objectMeta(p.target, p.fields).ManagedFields = objectMeta(rest, p.fields).ManagedFields
	}

	defaulter.Default(objToUpdate)
	return nil
}

func objectMeta(v reflect.Value, fields *topLevelFields) *metav1.ObjectMeta {
	return v.Field(fields.objectMeta).Addr().Interface().(*metav1.ObjectMeta)
}

type noopDefaulter struct{}

func (noopDefaulter) Default(runtime.Object) {}
