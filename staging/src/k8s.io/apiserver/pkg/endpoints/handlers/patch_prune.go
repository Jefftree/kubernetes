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

// A strategic merge patch only writes the keys it names, so every field the patch
// does not name comes out of the merge exactly as it went in. A pruned patch
// converts only the named fields to and from unstructured and deep copies the
// rest across, instead of round tripping the whole object. It descends into a
// named struct field when the patch for it is a plain object, so a patch of
// metadata.labels converts only the labels. Within metadata the same holds for
// managedFields, whose fieldsV1 are the most expensive part of the round trip.
type prunedPatch struct {
	root     *pruneNode
	original reflect.Value
	target   reflect.Value
}

// pruneNode is the plan for one struct.
type pruneNode struct {
	fields *structFields
	// whole marks the fields that take the round trip.
	whole []bool
	// nested holds the plans for the fields pruned further, by field index.
	nested []*pruneNode
	// keepManagedFields is set on the root when metadata takes the round trip
	// but managedFields need not.
	keepManagedFields bool
}

type structFields struct {
	// byName maps a JSON key onto a struct field index. The keys of an
	// inlined TypeMeta map onto the TypeMeta field itself.
	byName     map[string]int
	numFields  int
	typeMeta   int
	objectMeta int
	// mayBeOpaque marks the fields whose values have to be checked with
	// containsOpaque before they can skip the round trip.
	mayBeOpaque []bool
	// omitZero marks the fields tagged omitzero. The partial struct built for a
	// nested plan is mostly zero, so it could be dropped from the conversion.
	omitZero []bool
}

var (
	structFieldsCache sync.Map // reflect.Type -> *structFields, nil if unsupported
	typeMetaType      = reflect.TypeFor[metav1.TypeMeta]()
	objectMetaType    = reflect.TypeFor[metav1.ObjectMeta]()
)

// fieldsOf reports the layout of struct type t, or nil unless every field is
// exported and has a JSON name, apart from an inlined TypeMeta.
func fieldsOf(t reflect.Type) *structFields {
	if cached, ok := structFieldsCache.Load(t); ok {
		return cached.(*structFields)
	}
	fields := computeStructFields(t)
	structFieldsCache.Store(t, fields)
	return fields
}

func computeStructFields(t reflect.Type) *structFields {
	if t.Kind() != reflect.Struct {
		return nil
	}
	fields := &structFields{
		byName:      map[string]int{},
		numFields:   t.NumField(),
		typeMeta:    -1,
		objectMeta:  -1,
		mayBeOpaque: make([]bool, t.NumField()),
		omitZero:    make([]bool, t.NumField()),
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			return nil
		}
		fields.mayBeOpaque[i] = roundTripInfoOf(f.Type).mayBeOpaque
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		for _, opt := range strings.Split(opts, ",") {
			if opt == "omitzero" {
				fields.omitZero[i] = true
			}
		}
		switch {
		case f.Anonymous && f.Type == typeMetaType && name == "":
			fields.typeMeta = i
			fields.byName["apiVersion"] = i
			fields.byName["kind"] = i
			continue
		case name == "" || name == "-":
			return nil
		case f.Type == objectMetaType && name == "metadata":
			fields.objectMeta = i
		}
		if _, dup := fields.byName[name]; dup {
			return nil
		}
		fields.byName[name] = i
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
	if fields == nil || fields.typeMeta < 0 || fields.objectMeta < 0 {
		return nil, false
	}
	root, ok := planNode(fields, original.Elem(), patchMap, true, -1)
	if !ok {
		return nil, false
	}
	root.whole[fields.typeMeta] = true
	if root.whole[fields.objectMeta] {
		// managedFields can skip the round trip as long as the metadata patch
		// neither names them nor carries a directive.
		if metadataPatch, ok := patchMap["metadata"].(map[string]interface{}); ok {
			root.keepManagedFields = true
			for k := range metadataPatch {
				if k == "managedFields" || strings.HasPrefix(k, "$") {
					root.keepManagedFields = false
					break
				}
			}
			for k := range patchMap {
				if strings.HasPrefix(k, "$") && strings.HasSuffix(k, "/metadata") {
					root.keepManagedFields = false
				}
			}
		}
	}
	return &prunedPatch{root: root, original: original.Elem(), target: target.Elem()}, true
}

// planNode plans the pruned conversion of struct value v under patchMap. It
// reports ok=false when some key of patchMap is not a field of v, or is a bare
// directive such as $patch or $retainKeys, which acts on the whole struct. Field
// exempt, if not -1, skips the round trip unless named, even if it is opaque.
func planNode(fields *structFields, v reflect.Value, patchMap map[string]interface{}, isRoot bool, exempt int) (*pruneNode, bool) {
	n := &pruneNode{fields: fields, whole: make([]bool, fields.numFields)}
	directed := map[int]bool{}
	for k := range patchMap {
		name := k
		if strings.HasPrefix(k, "$") {
			_, after, found := strings.Cut(k, "/")
			if !found {
				return nil, false
			}
			name = after
		}
		i, ok := fields.byName[name]
		if !ok || (!isRoot && i == fields.typeMeta) {
			return nil, false
		}
		if name != k {
			directed[i] = true
		}
		n.whole[i] = true
	}

	// Descend into a named struct field whose patch is a plain object.
	for k, fieldPatch := range patchMap {
		i, ok := fields.byName[k]
		if !ok || i == fields.typeMeta || directed[i] || fields.omitZero[i] {
			continue
		}
		fieldPatchMap, ok := fieldPatch.(map[string]interface{})
		if !ok {
			continue
		}
		child := fieldsOf(v.Type().Field(i).Type)
		if child == nil || child.typeMeta >= 0 {
			continue
		}
		childExempt := -1
		if isRoot && i == fields.objectMeta {
			childExempt = child.byName["managedFields"]
		}
		childNode, ok := planNode(child, v.Field(i), fieldPatchMap, false, childExempt)
		if !ok {
			continue
		}
		if n.nested == nil {
			n.nested = make([]*pruneNode, fields.numFields)
		}
		n.nested[i] = childNode
		n.whole[i] = false
	}

	// An opaque field has to take the round trip even when not named. The
	// root's metadata is exempt, and so are the managedFields within it: the
	// field manager decodes and re-encodes managedFields after every patch, so
	// how the JSON of their fieldsV1 was laid out never reaches the result.
	for i, mayBeOpaque := range fields.mayBeOpaque {
		if !mayBeOpaque || n.whole[i] || n.nested != nil && n.nested[i] != nil || i == exempt || isRoot && i == fields.objectMeta {
			continue
		}
		if containsOpaque(v.Field(i)) {
			n.whole[i] = true
		}
	}
	return n, true
}

func (p *prunedPatch) apply(
	requestContext context.Context,
	defaulter runtime.ObjectDefaulter,
	patchMap map[string]interface{},
	schemaReferenceObj runtime.Object,
	strictErrs []error,
	validationDirective string,
) error {
	scratch := reflect.New(p.original.Type()).Elem()
	p.root.copyConverted(scratch, p.original)
	if p.root.keepManagedFields {
		objectMeta(scratch, p.root.fields).ManagedFields = nil
	}
	partialMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(scratch.Addr().Interface())
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
	scratch.Set(p.original)
	p.root.zeroConverted(scratch)
	if p.root.keepManagedFields {
		objectMeta(scratch, p.root.fields).ManagedFields = objectMeta(p.original, p.root.fields).ManagedFields
	}
	rest := reflect.ValueOf(scratch.Addr().Interface().(runtime.Object).DeepCopyObject()).Elem()
	var managedFields []metav1.ManagedFieldsEntry
	if p.root.keepManagedFields {
		managedFields = objectMeta(rest, p.root.fields).ManagedFields
	}
	p.root.copyConverted(rest, p.target)
	if p.root.keepManagedFields {
		objectMeta(rest, p.root.fields).ManagedFields = managedFields
	}
	p.target.Set(rest)

	defaulter.Default(objToUpdate)
	return nil
}

// copyConverted copies the fields n converts from src to dst.
func (n *pruneNode) copyConverted(dst, src reflect.Value) {
	for i, whole := range n.whole {
		switch {
		case whole:
			dst.Field(i).Set(src.Field(i))
		case n.nested != nil && n.nested[i] != nil:
			n.nested[i].copyConverted(dst.Field(i), src.Field(i))
		}
	}
}

// zeroConverted zeroes the fields n converts in v.
func (n *pruneNode) zeroConverted(v reflect.Value) {
	for i, whole := range n.whole {
		switch {
		case whole:
			v.Field(i).SetZero()
		case n.nested != nil && n.nested[i] != nil:
			n.nested[i].zeroConverted(v.Field(i))
		}
	}
}

func objectMeta(v reflect.Value, fields *structFields) *metav1.ObjectMeta {
	return v.Field(fields.objectMeta).Addr().Interface().(*metav1.ObjectMeta)
}

type noopDefaulter struct{}

func (noopDefaulter) Default(runtime.Object) {}
