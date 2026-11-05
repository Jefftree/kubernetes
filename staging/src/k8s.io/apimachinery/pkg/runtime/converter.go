/*
Copyright 2017 The Kubernetes Authors.

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

package runtime

import (
	encodingjson "encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"sigs.k8s.io/structured-merge-diff/v7/value"

	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/util/json"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"

	"k8s.io/klog/v2"
)

// UnstructuredConverter is an interface for converting between interface{}
// and map[string]interface representation.
type UnstructuredConverter interface {
	ToUnstructured(obj interface{}) (map[string]interface{}, error)
	FromUnstructured(u map[string]interface{}, obj interface{}) error
}

type structField struct {
	structType reflect.Type
	field      int
}

type fieldInfo struct {
	name             string
	nameValue        reflect.Value
	omitempty        bool
	omitzero         func(dv reflect.Value) bool
	hasPatchStrategy bool
}

type fieldsCacheMap map[structField]*fieldInfo

type fieldsCache struct {
	sync.Mutex
	value atomic.Value
}

func newFieldsCache() *fieldsCache {
	cache := &fieldsCache{}
	cache.value.Store(make(fieldsCacheMap))
	return cache
}

type structTypeInfo struct {
	fields            []*fieldInfo
	canPruneSubfields bool
}

type structInfoCacheMap map[reflect.Type]*structTypeInfo

type structInfoCache struct {
	sync.Mutex
	value atomic.Value
}

func newStructInfoCache() *structInfoCache {
	cache := &structInfoCache{}
	cache.value.Store(make(structInfoCacheMap))
	return cache
}

var (
	mapStringInterfaceType = reflect.TypeOf(map[string]interface{}{})
	mapStringStringType    = reflect.TypeOf(map[string]string{})
	stringType             = reflect.TypeOf(string(""))
	fieldCache             = newFieldsCache()
	typeInfoCache          = newStructInfoCache()

	// DefaultUnstructuredConverter performs unstructured to Go typed object conversions.
	DefaultUnstructuredConverter = &unstructuredConverter{
		mismatchDetection: parseBool(os.Getenv("KUBE_PATCH_CONVERSION_DETECTOR")),
		comparison: conversion.EqualitiesOrDie(
			func(a, b time.Time) bool {
				return a.UTC() == b.UTC()
			},
		),
	}
)

func structInfoFromType(st reflect.Type) *structTypeInfo {
	cacheMap := typeInfoCache.value.Load().(structInfoCacheMap)
	if info, ok := cacheMap[st]; ok {
		return info
	}

	numField := st.NumField()
	fields := make([]*fieldInfo, numField)
	canPrune := !value.TypeReflectEntryOf(st).CanConvertToUnstructured() &&
		!value.TypeReflectEntryOf(st).CanConvertFromUnstructured()
	seenNames := make(map[string]struct{}, numField)
	for i := 0; i < numField; i++ {
		fi := computeFieldInfo(st, i)
		fields[i] = fi
		tf := st.Field(i)
		if !tf.IsExported() || fi.name == "-" || len(fi.name) == 0 {
			canPrune = false
		} else {
			if _, dup := seenNames[fi.name]; dup {
				canPrune = false
			}
			seenNames[fi.name] = struct{}{}
		}
	}

	info := &structTypeInfo{
		fields:            fields,
		canPruneSubfields: canPrune,
	}

	typeInfoCache.Lock()
	defer typeInfoCache.Unlock()
	cacheMap = typeInfoCache.value.Load().(structInfoCacheMap)
	if existing, ok := cacheMap[st]; ok {
		return existing
	}
	newCacheMap := make(structInfoCacheMap, len(cacheMap)+1)
	for k, v := range cacheMap {
		newCacheMap[k] = v
	}
	newCacheMap[st] = info
	typeInfoCache.value.Store(newCacheMap)
	return info
}

func parseBool(key string) bool {
	if len(key) == 0 {
		return false
	}
	value, err := strconv.ParseBool(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't parse '%s' as bool for unstructured mismatch detection", key))
	}
	return value
}

// unstructuredConverter knows how to convert between interface{} and
// Unstructured in both ways.
type unstructuredConverter struct {
	// If true, we will be additionally running conversion via json
	// to ensure that the result is true.
	// This is supposed to be set only in tests.
	mismatchDetection bool
	// comparison is the default test logic used to compare
	comparison conversion.Equalities
}

// NewTestUnstructuredConverter creates an UnstructuredConverter that accepts JSON typed maps and translates them
// to Go types via reflection. It performs mismatch detection automatically and is intended for use by external
// test tools. Use DefaultUnstructuredConverter if you do not explicitly need mismatch detection.
func NewTestUnstructuredConverter(comparison conversion.Equalities) UnstructuredConverter {
	return NewTestUnstructuredConverterWithValidation(comparison)
}

// NewTestUnstrucutredConverterWithValidation allows for access to
// FromUnstructuredWithValidation from within tests.
func NewTestUnstructuredConverterWithValidation(comparison conversion.Equalities) *unstructuredConverter {
	return &unstructuredConverter{
		mismatchDetection: true,
		comparison:        comparison,
	}
}

// fromUnstructuredContext provides options for informing the converter
// the state of its recursive walk through the conversion process.
type fromUnstructuredContext struct {
	// isInlined indicates whether the converter is currently in
	// an inlined field or not to determine whether it should
	// validate the matchedKeys yet or only collect them.
	// This should only be set from `structFromUnstructured`
	isInlined bool
	// matchedKeys is a stack of the set of all fields that exist in the
	// concrete go type of the object being converted into. The keys of every
	// level are kept in one slice, with each level starting at the index
	// recorded in matchedKeysStarts.
	// This should only be manipulated via `pushMatchedKeyTracker`,
	// `recordMatchedKey`, or `popAndVerifyMatchedKeys`
	matchedKeys       []string
	matchedKeysStarts []int
	// parentPath collects the path that the conversion
	// takes as it traverses the unstructured json map.
	// It is used to report the full path to any unknown
	// fields that the converter encounters.
	parentPath []string
	// returnUnknownFields indicates whether or not
	// unknown field errors should be collected and
	// returned to the caller
	returnUnknownFields bool
	// unknownFieldErrors are the collection of
	// the full path to each unknown field in the
	// object.
	unknownFieldErrors   []error
	topLevelKeys         map[string]interface{}
	matchedKeysBuf       [64]string
	matchedKeysStartsBuf [8]int
	parentPathBuf        [16]string
}

// pushMatchedKeyTracker adds a placeholder set for tracking
// matched keys for the given level. This should only be
// called from `structFromUnstructured`.
func (c *fromUnstructuredContext) pushMatchedKeyTracker() {
	if !c.returnUnknownFields {
		return
	}

	c.matchedKeysStarts = append(c.matchedKeysStarts, len(c.matchedKeys))
}

// recordMatchedKey initializes the last element of matchedKeys
// (if needed) and sets 'key'. This should only be called from
// `structFromUnstructured`.
func (c *fromUnstructuredContext) recordMatchedKey(key string) {
	if !c.returnUnknownFields {
		return
	}

	c.matchedKeys = append(c.matchedKeys, key)
}

// popAndVerifyMatchedKeys pops the last element of matchedKeys,
// checks the matched keys against the data, and adds unknown
// field errors for any matched keys.
// `mapValue` is the value of sv containing all of the keys that exist at this level
// (ie. sv.MapKeys) in the source data.
// `matchedKeys` are all the keys found for that level in the destination object.
// This should only be called from `structFromUnstructured`.
func (c *fromUnstructuredContext) popAndVerifyMatchedKeys(mapValue reflect.Value) {
	if !c.returnUnknownFields {
		return
	}

	last := len(c.matchedKeysStarts) - 1
	start := c.matchedKeysStarts[last]
	c.matchedKeysStarts = c.matchedKeysStarts[:last]
	curMatchedKeys := c.matchedKeys[start:]
	// A struct has few enough fields that a linear search beats building a set.
	if mapValue.CanInterface() {
		if m, ok := mapValue.Interface().(map[string]interface{}); ok {
			for key := range m {
				if !slices.Contains(curMatchedKeys, key) {
					c.recordUnknownField(key)
				}
			}
			c.matchedKeys = c.matchedKeys[:start]
			return
		}
	}
	iter := mapValue.MapRange()
	for iter.Next() {
		if key := iter.Key().String(); !slices.Contains(curMatchedKeys, key) {
			c.recordUnknownField(key)
		}
	}
	c.matchedKeys = c.matchedKeys[:start]
}

func (c *fromUnstructuredContext) recordUnknownField(field string) {
	if !c.returnUnknownFields {
		return
	}

	pathLen := len(c.parentPath)
	c.pushKey(field)
	errPath := strings.Join(c.parentPath, "")
	c.parentPath = c.parentPath[:pathLen]
	c.unknownFieldErrors = append(c.unknownFieldErrors, fmt.Errorf(`unknown field "%s"`, errPath))
}

func (c *fromUnstructuredContext) pushIndex(index int) {
	if !c.returnUnknownFields {
		return
	}

	c.parentPath = append(c.parentPath, "[", strconv.Itoa(index), "]")
}

func (c *fromUnstructuredContext) pushKey(key string) {
	if !c.returnUnknownFields {
		return
	}

	if len(c.parentPath) > 0 {
		c.parentPath = append(c.parentPath, ".")
	}
	c.parentPath = append(c.parentPath, key)
}

// FromUnstructuredWithValidation converts an object from map[string]interface{} representation into a concrete type.
// It uses encoding/json/Unmarshaler if object implements it or reflection if not.
// It takes a validationDirective that indicates how to behave when it encounters unknown fields.
func (c *unstructuredConverter) FromUnstructuredWithValidation(u map[string]interface{}, obj interface{}, returnUnknownFields bool) error {
	return c.FromUnstructuredWithValidationTopLevel(u, obj, returnUnknownFields, nil)
}

// FromUnstructuredWithValidationTopLevel converts an object from map[string]interface{} representation into obj,
// updating only the struct fields (and nested struct subfields) whose JSON names appear in topLevelKeys when topLevelKeys is non-nil.
func (c *unstructuredConverter) FromUnstructuredWithValidationTopLevel(u map[string]interface{}, obj interface{}, returnUnknownFields bool, topLevelKeys map[string]interface{}) error {
	t := reflect.TypeOf(obj)
	value := reflect.ValueOf(obj)
	if t == nil || t.Kind() != reflect.Pointer || value.IsNil() {
		return fmt.Errorf("FromUnstructured requires a non-nil pointer to an object, got %v", t)
	}

	fromUnstructuredContext := &fromUnstructuredContext{
		returnUnknownFields: returnUnknownFields,
		topLevelKeys:        topLevelKeys,
	}
	fromUnstructuredContext.matchedKeys = fromUnstructuredContext.matchedKeysBuf[:0]
	fromUnstructuredContext.matchedKeysStarts = fromUnstructuredContext.matchedKeysStartsBuf[:0]
	fromUnstructuredContext.parentPath = fromUnstructuredContext.parentPathBuf[:0]
	err := fromUnstructured(reflect.ValueOf(u), value.Elem(), fromUnstructuredContext)
	if c.mismatchDetection && topLevelKeys == nil {
		newObj := reflect.New(t.Elem()).Interface()
		newErr := fromUnstructuredViaJSON(u, newObj)
		if (err != nil) != (newErr != nil) {
			//nolint:logcheck // Should not be reached.
			klog.Fatalf("FromUnstructured unexpected error for %v: error: %v", u, err)
		}
		if err == nil && !c.comparison.DeepEqual(obj, newObj) {
			//nolint:logcheck // Should not be reached.
			klog.Fatalf("FromUnstructured mismatch\nobj1: %#v\nobj2: %#v", obj, newObj)
		}
	}
	if err != nil {
		return err
	}
	if returnUnknownFields && len(fromUnstructuredContext.unknownFieldErrors) > 0 {
		sort.Slice(fromUnstructuredContext.unknownFieldErrors, func(i, j int) bool {
			return fromUnstructuredContext.unknownFieldErrors[i].Error() <
				fromUnstructuredContext.unknownFieldErrors[j].Error()
		})
		return NewStrictDecodingError(fromUnstructuredContext.unknownFieldErrors)
	}
	return nil
}

// FromUnstructured converts an object from map[string]interface{} representation into a concrete type.
// It uses encoding/json/Unmarshaler if object implements it or reflection if not.
func (c *unstructuredConverter) FromUnstructured(u map[string]interface{}, obj interface{}) error {
	return c.FromUnstructuredWithValidation(u, obj, false)
}

func fromUnstructuredViaJSON(u map[string]interface{}, obj interface{}) error {
	data, err := json.Marshal(u)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, obj)
}

func fromUnstructured(sv, dv reflect.Value, ctx *fromUnstructuredContext) error {
	sv = unwrapInterface(sv)
	if !sv.IsValid() {
		if !dv.IsZero() {
			dv.SetZero()
		}
		return nil
	}
	st, dt := sv.Type(), dv.Type()

	switch dt.Kind() {
	case reflect.Map, reflect.Slice, reflect.Pointer, reflect.Struct, reflect.Interface:
		// Those require non-trivial conversion.
	default:
		// This should handle all simple types.
		if st.AssignableTo(dt) {
			dv.Set(sv)
			return nil
		}
		// We cannot simply use "ConvertibleTo" as JSON doesn't support conversions
		// between those four groups: bools, integers, floats and string. We need to
		// do the same.
		if st.ConvertibleTo(dt) {
			switch st.Kind() {
			case reflect.String:
				switch dt.Kind() {
				case reflect.String:
					dv.SetString(sv.String())
					return nil
				}
			case reflect.Bool:
				switch dt.Kind() {
				case reflect.Bool:
					dv.SetBool(sv.Bool())
					return nil
				}
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				switch dt.Kind() {
				case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
					dv.SetInt(sv.Int())
					return nil
				case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
					dv.SetUint(uint64(sv.Int()))
					return nil
				case reflect.Float32, reflect.Float64:
					dv.SetFloat(float64(sv.Int()))
					return nil
				}
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
				switch dt.Kind() {
				case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
					dv.SetInt(int64(sv.Uint()))
					return nil
				case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
					dv.SetUint(sv.Uint())
					return nil
				case reflect.Float32, reflect.Float64:
					dv.SetFloat(float64(sv.Uint()))
					return nil
				}
			case reflect.Float32, reflect.Float64:
				switch dt.Kind() {
				case reflect.Float32, reflect.Float64:
					dv.SetFloat(sv.Float())
					return nil
				}
				if sv.Float() == math.Trunc(sv.Float()) {
					dv.Set(sv.Convert(dt))
					return nil
				}
			}
			return fmt.Errorf("cannot convert %s to %s", st.String(), dt.String())
		}
	}

	// Check if the object has a custom JSON marshaller/unmarshaller.
	entry := value.TypeReflectEntryOf(dv.Type())
	if entry.CanConvertFromUnstructured() {
		return entry.FromUnstructured(sv, dv)
	}

	switch dt.Kind() {
	case reflect.Map:
		return mapFromUnstructured(sv, dv, ctx)
	case reflect.Slice:
		return sliceFromUnstructured(sv, dv, ctx)
	case reflect.Pointer:
		return pointerFromUnstructured(sv, dv, ctx)
	case reflect.Struct:
		return structFromUnstructured(sv, dv, ctx)
	case reflect.Interface:
		return interfaceFromUnstructured(sv, dv)
	default:
		return fmt.Errorf("unrecognized type: %v", dt.Kind())
	}
}

func computeFieldInfo(structType reflect.Type, field int) *fieldInfo {
	info := &fieldInfo{}
	typeField := structType.Field(field)
	info.hasPatchStrategy = typeField.Tag.Get("patchStrategy") != ""
	jsonTag, exists := typeField.Tag.Lookup("json")
	if !exists || len(jsonTag) == 0 {
		if !typeField.Anonymous {
			// match stdlib behavior for naming fields that don't specify a json tag name
			info.name = typeField.Name
		}
	} else {
		items := strings.Split(jsonTag, ",")
		info.name = items[0]
		if isInlinedFromTag(typeField, items[0], items[1:]) {
			// match stdlib behavior when controlled by tag
			info.name = ""
		} else if len(info.name) == 0 && !typeField.Anonymous {
			// match stdlib behavior for naming fields that don't specify a json tag name
			info.name = typeField.Name
		}

		for i := range items {
			if i > 0 && items[i] == "omitempty" {
				info.omitempty = true
			}
			if i > 0 && items[i] == "omitzero" {
				info.omitzero = value.OmitZeroFunc(typeField.Type)
			}
		}
	}
	info.nameValue = reflect.ValueOf(info.name)
	return info
}

func fieldInfoFromField(structType reflect.Type, field int) *fieldInfo {
	return structInfoFromType(structType).fields[field]
}

func unwrapInterface(v reflect.Value) reflect.Value {
	for v.Kind() == reflect.Interface {
		v = v.Elem()
	}
	return v
}

func mapFromUnstructured(sv, dv reflect.Value, ctx *fromUnstructuredContext) error {
	st, dt := sv.Type(), dv.Type()
	if st.Kind() != reflect.Map {
		return fmt.Errorf("cannot restore map from %v", st.Kind())
	}

	if !st.Key().AssignableTo(dt.Key()) && !st.Key().ConvertibleTo(dt.Key()) {
		return fmt.Errorf("cannot copy map with non-assignable keys: %v %v", st.Key(), dt.Key())
	}

	if sv.IsNil() {
		if !dv.IsZero() {
			dv.SetZero()
		}
		return nil
	}
	if st == mapStringInterfaceType && dt == mapStringStringType && sv.CanInterface() {
		// Labels and annotations are common enough to skip reflecting over.
		src := sv.Interface().(map[string]interface{})
		if dst, ok := stringMapFromUnstructured(src); ok {
			dv.Set(reflect.ValueOf(dst))
			return nil
		}
	}
	dv.Set(reflect.MakeMapWithSize(dt, sv.Len()))
	if st == mapStringInterfaceType && dt.Key() == stringType && sv.CanInterface() {
		srcMap := sv.Interface().(map[string]interface{})
		value := reflect.New(dt.Elem()).Elem()
		for k, rawVal := range srcMap {
			if !value.IsZero() {
				value.SetZero()
			}
			if rawVal != nil {
				if err := fromUnstructured(reflect.ValueOf(rawVal), value, ctx); err != nil {
					return err
				}
			}
			dv.SetMapIndex(reflect.ValueOf(k), value)
		}
		return nil
	}
	for _, key := range sv.MapKeys() {
		value := reflect.New(dt.Elem()).Elem()
		if val := unwrapInterface(sv.MapIndex(key)); val.IsValid() {
			if err := fromUnstructured(val, value, ctx); err != nil {
				return err
			}
		} else {
			value.SetZero()
		}
		if st.Key().AssignableTo(dt.Key()) {
			dv.SetMapIndex(key, value)
		} else {
			dv.SetMapIndex(key.Convert(dt.Key()), value)
		}
	}
	return nil
}

// stringMapFromUnstructured copies src, reporting false unless every value in it
// is a string.
func stringMapFromUnstructured(src map[string]interface{}) (map[string]string, bool) {
	for _, v := range src {
		if _, ok := v.(string); !ok {
			return nil, false
		}
	}
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v.(string)
	}
	return dst, true
}

func sliceFromUnstructured(sv, dv reflect.Value, ctx *fromUnstructuredContext) error {
	st, dt := sv.Type(), dv.Type()
	if st.Kind() == reflect.String && dt.Elem().Kind() == reflect.Uint8 {
		// We store original []byte representation as string.
		// This conversion is allowed, but we need to be careful about
		// marshaling data appropriately.
		if len(sv.Interface().(string)) > 0 {
			marshalled, err := json.Marshal(sv.Interface())
			if err != nil {
				return fmt.Errorf("error encoding %s to json: %v", st, err)
			}
			// TODO: Is this Unmarshal needed?
			var data []byte
			err = json.Unmarshal(marshalled, &data)
			if err != nil {
				return fmt.Errorf("error decoding from json: %v", err)
			}
			dv.SetBytes(data)
		} else {
			dv.Set(reflect.MakeSlice(dt, 0, 0))
		}
		return nil
	}
	if st.Kind() != reflect.Slice {
		return fmt.Errorf("cannot restore slice from %v", st.Kind())
	}

	if sv.IsNil() {
		if !dv.IsZero() {
			dv.SetZero()
		}
		return nil
	}
	svLen := sv.Len()
	dv.Set(reflect.MakeSlice(dt, svLen, sv.Cap()))

	pathLen := len(ctx.parentPath)
	defer func() {
		ctx.parentPath = ctx.parentPath[:pathLen]
	}()
	if sv.CanInterface() {
		if srcSlice, ok := sv.Interface().([]interface{}); ok {
			for i, elem := range srcSlice {
				ctx.pushIndex(i)
				if elem == nil {
					if !dv.Index(i).IsZero() {
						dv.Index(i).SetZero()
					}
				} else if err := fromUnstructured(reflect.ValueOf(elem), dv.Index(i), ctx); err != nil {
					return err
				}
				ctx.parentPath = ctx.parentPath[:pathLen]
			}
			return nil
		}
	}
	for i := 0; i < svLen; i++ {
		ctx.pushIndex(i)
		if err := fromUnstructured(sv.Index(i), dv.Index(i), ctx); err != nil {
			return err
		}
		ctx.parentPath = ctx.parentPath[:pathLen]
	}
	return nil
}

func pointerFromUnstructured(sv, dv reflect.Value, ctx *fromUnstructuredContext) error {
	st, dt := sv.Type(), dv.Type()

	if st.Kind() == reflect.Pointer && sv.IsNil() {
		if !dv.IsZero() {
			dv.SetZero()
		}
		return nil
	}
	dv.Set(reflect.New(dt.Elem()))
	switch st.Kind() {
	case reflect.Pointer, reflect.Interface:
		return fromUnstructured(sv.Elem(), dv.Elem(), ctx)
	default:
		return fromUnstructured(sv, dv.Elem(), ctx)
	}
}

func structFromUnstructured(sv, dv reflect.Value, ctx *fromUnstructuredContext) error {
	st, dt := sv.Type(), dv.Type()
	if st.Kind() != reflect.Map {
		return fmt.Errorf("cannot restore struct from: %v", st.Kind())
	}

	sInfo := structInfoFromType(dt)
	pathLen := len(ctx.parentPath)
	svInlined := ctx.isInlined
	defer func() {
		ctx.parentPath = ctx.parentPath[:pathLen]
		ctx.isInlined = svInlined
	}()
	var topLevelKeys map[string]interface{}
	if !svInlined {
		topLevelKeys, ctx.topLevelKeys = ctx.topLevelKeys, nil
		ctx.pushMatchedKeyTracker()
	}

	if sv.CanInterface() {
		if realMap, ok := sv.Interface().(map[string]interface{}); ok {
			for i, fieldInfo := range sInfo.fields {
				fv := dv.Field(i)
				if len(fieldInfo.name) == 0 {
					ctx.isInlined = true
					if err := fromUnstructured(sv, fv, ctx); err != nil {
						return err
					}
					ctx.isInlined = svInlined
				} else {
					ctx.recordMatchedKey(fieldInfo.name)
					var subKeys map[string]interface{}
					if topLevelKeys != nil {
						if shouldSkipTopLevelField(fieldInfo, topLevelKeys) {
							continue
						}
						subKeys = subPatchKeysForField(fv, fieldInfo, topLevelKeys)
						if subKeys == nil && !fv.IsZero() {
							fv.SetZero()
						}
					}
					rawVal, exists := realMap[fieldInfo.name]
					if exists && rawVal != nil {
						ctx.isInlined = false
						ctx.pushKey(fieldInfo.name)
						ctx.topLevelKeys = subKeys
						err := fromUnstructured(reflect.ValueOf(rawVal), fv, ctx)
						ctx.topLevelKeys = nil
						if err != nil {
							return err
						}
						ctx.parentPath = ctx.parentPath[:pathLen]
						ctx.isInlined = svInlined
					} else if subKeys != nil {
						ClearPatchedFields(fv, subKeys)
					} else if !fv.IsZero() {
						fv.SetZero()
					}
				}
			}
			if !svInlined {
				ctx.popAndVerifyMatchedKeys(sv)
			}
			return nil
		}
	}

	for i, fieldInfo := range sInfo.fields {
		fv := dv.Field(i)

		if len(fieldInfo.name) == 0 {
			// This field is inlined, recurse into fromUnstructured again
			// with the same set of matched keys.
			ctx.isInlined = true
			if err := fromUnstructured(sv, fv, ctx); err != nil {
				return err
			}
			ctx.isInlined = svInlined
		} else {
			// This field is not inlined so we recurse into
			// child field of sv corresponding to field i of
			// dv, with a new set of matchedKeys and updating
			// the parentPath to indicate that we are one level
			// deeper.
			ctx.recordMatchedKey(fieldInfo.name)
			var subKeys map[string]interface{}
			if topLevelKeys != nil {
				if shouldSkipTopLevelField(fieldInfo, topLevelKeys) {
					continue
				}
				subKeys = subPatchKeysForField(fv, fieldInfo, topLevelKeys)
				if subKeys == nil && !fv.IsZero() {
					fv.SetZero()
				}
			}
			value := unwrapInterface(sv.MapIndex(fieldInfo.nameValue))
			if value.IsValid() {
				ctx.isInlined = false
				ctx.pushKey(fieldInfo.name)
				ctx.topLevelKeys = subKeys
				err := fromUnstructured(value, fv, ctx)
				ctx.topLevelKeys = nil
				if err != nil {
					return err
				}
				ctx.parentPath = ctx.parentPath[:pathLen]
				ctx.isInlined = svInlined
			} else if subKeys != nil {
				ClearPatchedFields(fv, subKeys)
			} else if !fv.IsZero() {
				fv.SetZero()
			}
		}
	}
	if !svInlined {
		ctx.popAndVerifyMatchedKeys(sv)
	}
	return nil
}

func shouldSkipTopLevelField(fieldInfo *fieldInfo, topLevelKeys map[string]interface{}) bool {
	if topLevelKeys == nil || len(fieldInfo.name) == 0 {
		return false
	}
	_, ok := topLevelKeys[fieldInfo.name]
	return !ok
}

func subPatchKeysForField(fv reflect.Value, fieldInfo *fieldInfo, patchKeys map[string]interface{}) map[string]interface{} {
	if patchKeys == nil || len(fieldInfo.name) == 0 || fieldInfo.omitzero != nil || fieldInfo.hasPatchStrategy {
		return nil
	}
	if fv.Kind() != reflect.Struct || fv.NumField() == 0 {
		return nil
	}
	subMap, ok := patchKeys[fieldInfo.name].(map[string]interface{})
	if !ok || len(subMap) == 0 {
		return nil
	}
	for k := range subMap {
		if strings.HasPrefix(k, "$") {
			return nil
		}
	}
	if !structInfoFromType(fv.Type()).canPruneSubfields {
		return nil
	}
	return subMap
}

// ClearPatchedFields zeroes the struct fields (or nested struct subfields) in v
// that will be overwritten when unmarshaling a pruned strategic merge patch.
func ClearPatchedFields(v reflect.Value, patchKeys map[string]interface{}) {
	if patchKeys == nil || v.Kind() != reflect.Struct {
		return
	}
	sInfo := structInfoFromType(v.Type())
	for i, fieldInfo := range sInfo.fields {
		if fieldInfo.name == "-" || len(fieldInfo.name) == 0 {
			continue
		}
		if _, ok := patchKeys[fieldInfo.name]; !ok {
			continue
		}
		fv := v.Field(i)
		if subKeys := subPatchKeysForField(fv, fieldInfo, patchKeys); subKeys != nil {
			ClearPatchedFields(fv, subKeys)
		} else if !fv.IsZero() {
			fv.SetZero()
		}
	}
}

func interfaceFromUnstructured(sv, dv reflect.Value) error {
	// TODO: Is this conversion safe?
	dv.Set(sv)
	return nil
}

// ToUnstructured converts an object into map[string]interface{} representation.
// It uses encoding/json/Marshaler if object implements it or reflection if not.
func (c *unstructuredConverter) ToUnstructured(obj interface{}) (map[string]interface{}, error) {
	return c.ToUnstructuredTopLevel(obj, nil)
}

// ToUnstructuredTopLevel converts an object into map[string]interface{} representation.
// When topLevelKeys is non-nil and obj is a pointer to a struct without a custom converter,
// only struct fields (and nested struct subfields) whose JSON names appear in topLevelKeys (plus inlined fields) are converted.
func (c *unstructuredConverter) ToUnstructuredTopLevel(obj interface{}, topLevelKeys map[string]interface{}) (map[string]interface{}, error) {
	var u map[string]interface{}
	var err error
	if unstr, ok := obj.(Unstructured); ok {
		u = unstr.UnstructuredContent()
	} else {
		t := reflect.TypeOf(obj)
		v := reflect.ValueOf(obj)
		if t == nil || t.Kind() != reflect.Pointer || v.IsNil() {
			return nil, fmt.Errorf("ToUnstructured requires a non-nil pointer to an object, got %v", t)
		}
		sv := v.Elem()
		if sv.Kind() == reflect.Struct && !value.TypeReflectEntryOf(sv.Type()).CanConvertToUnstructured() {
			sInfo := structInfoFromType(sv.Type())
			capHint := len(sInfo.fields)
			if topLevelKeys != nil && len(topLevelKeys)+2 < capHint {
				capHint = len(topLevelKeys) + 2
			} else if capHint > 8 {
				capHint = 8
			}
			u = make(map[string]interface{}, capHint)
			err = structToUnstructuredIntoMap(sv, sInfo, u, topLevelKeys)
		} else {
			u = map[string]interface{}{}
			err = toUnstructured(sv, reflect.ValueOf(&u).Elem())
		}
	}
	if c.mismatchDetection && topLevelKeys == nil {
		newUnstr := map[string]interface{}{}
		newErr := toUnstructuredViaJSON(obj, &newUnstr)
		if (err != nil) != (newErr != nil) {
			//nolint:logcheck // Should not be reached.
			klog.Fatalf("ToUnstructured unexpected error for %v: error: %v; newErr: %v", obj, err, newErr)
		}
		if err == nil && !c.comparison.DeepEqual(u, newUnstr) {
			//nolint:logcheck // Should not be reached.
			klog.Fatalf("ToUnstructured mismatch\nobj1: %#v\nobj2: %#v", u, newUnstr)
		}
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// DeepCopyJSON deep copies the passed value, assuming it is a valid JSON representation i.e. only contains
// types produced by json.Unmarshal() and also int64.
// bool, int64, float64, string, []interface{}, map[string]interface{}, json.Number and nil
func DeepCopyJSON(x map[string]interface{}) map[string]interface{} {
	return DeepCopyJSONValue(x).(map[string]interface{})
}

// DeepCopyJSONValue deep copies the passed value, assuming it is a valid JSON representation i.e. only contains
// types produced by json.Unmarshal() and also int64.
// bool, int64, float64, string, []interface{}, map[string]interface{}, json.Number and nil
func DeepCopyJSONValue(x interface{}) interface{} {
	switch x := x.(type) {
	case map[string]interface{}:
		if x == nil {
			// Typed nil - an interface{} that contains a type map[string]interface{} with a value of nil
			return x
		}
		clone := make(map[string]interface{}, len(x))
		for k, v := range x {
			clone[k] = DeepCopyJSONValue(v)
		}
		return clone
	case []interface{}:
		if x == nil {
			// Typed nil - an interface{} that contains a type []interface{} with a value of nil
			return x
		}
		clone := make([]interface{}, len(x))
		for i, v := range x {
			clone[i] = DeepCopyJSONValue(v)
		}
		return clone
	case string, int64, bool, float64, nil, encodingjson.Number:
		return x
	default:
		panic(fmt.Errorf("cannot deep copy %T", x))
	}
}

func toUnstructuredViaJSON(obj interface{}, u *map[string]interface{}) error {
	data, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, u)
}

func toUnstructured(sv, dv reflect.Value) error {
	// Check if the object has a custom string converter.
	entry := value.TypeReflectEntryOf(sv.Type())
	if entry.CanConvertToUnstructured() {
		v, err := entry.ToUnstructured(sv)
		if err != nil {
			return err
		}
		if v != nil {
			dv.Set(reflect.ValueOf(v))
		}
		return nil
	}
	st := sv.Type()
	switch st.Kind() {
	case reflect.String:
		dv.Set(reflect.ValueOf(sv.String()))
		return nil
	case reflect.Bool:
		dv.Set(reflect.ValueOf(sv.Bool()))
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		dv.Set(reflect.ValueOf(sv.Int()))
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		val, err := uintToUnstructuredHelper(sv.Uint())
		if err != nil {
			return err
		}
		dv.Set(reflect.ValueOf(val))
		return nil
	case reflect.Float32, reflect.Float64:
		dv.Set(reflect.ValueOf(sv.Float()))
		return nil
	case reflect.Map:
		return mapToUnstructured(sv, dv)
	case reflect.Slice:
		return sliceToUnstructured(sv, dv)
	case reflect.Pointer:
		return pointerToUnstructured(sv, dv)
	case reflect.Struct:
		return structToUnstructured(sv, dv)
	case reflect.Interface:
		return interfaceToUnstructured(sv, dv)
	default:
		return fmt.Errorf("unrecognized type: %v", st.Kind())
	}
}

func toUnstructuredValue(sv reflect.Value) (interface{}, error) {
	entry := value.TypeReflectEntryOf(sv.Type())
	if entry.CanConvertToUnstructured() {
		return entry.ToUnstructured(sv)
	}
	st := sv.Type()
	switch st.Kind() {
	case reflect.String:
		return sv.String(), nil
	case reflect.Bool:
		return sv.Bool(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return sv.Int(), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return uintToUnstructuredHelper(sv.Uint())
	case reflect.Float32, reflect.Float64:
		return sv.Float(), nil
	case reflect.Pointer:
		if sv.IsNil() {
			return nil, nil
		}
		return toUnstructuredValue(sv.Elem())
	case reflect.Interface:
		if !sv.IsValid() || sv.IsNil() {
			return nil, nil
		}
		return toUnstructuredValue(sv.Elem())
	case reflect.Struct:
		sInfo := structInfoFromType(st)
		capHint := len(sInfo.fields)
		if capHint > 8 {
			capHint = 8
		}
		out := make(map[string]interface{}, capHint)
		if err := structToUnstructuredIntoMap(sv, sInfo, out, nil); err != nil {
			return nil, err
		}
		return out, nil
	case reflect.Slice:
		if sv.IsNil() {
			return nil, nil
		}
		if st.Elem().Kind() == reflect.Uint8 {
			data, err := json.Marshal(sv.Bytes())
			if err != nil {
				return nil, err
			}
			var result string
			if err = json.Unmarshal(data, &result); err != nil {
				return nil, err
			}
			return result, nil
		}
		n := sv.Len()
		out := make([]interface{}, n)
		for i := 0; i < n; i++ {
			v, err := toUnstructuredValue(sv.Index(i))
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	case reflect.Map:
		if sv.IsNil() {
			return nil, nil
		}
		if st.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("cannot convert map with non-string key: %v", st.Key())
		}
		if st == mapStringStringType && sv.CanInterface() {
			srcMap := sv.Interface().(map[string]string)
			out := make(map[string]interface{}, len(srcMap))
			for k, v := range srcMap {
				out[k] = v
			}
			return out, nil
		}
		out := make(map[string]interface{}, sv.Len())
		iter := sv.MapRange()
		for iter.Next() {
			v, err := toUnstructuredValue(iter.Value())
			if err != nil {
				return nil, err
			}
			out[iter.Key().String()] = v
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unrecognized type: %v", st.Kind())
	}
}

func mapToUnstructured(sv, dv reflect.Value) error {
	st, dt := sv.Type(), dv.Type()
	if sv.IsNil() {
		dv.Set(reflect.Zero(dt))
		return nil
	}
	if dt.Kind() == reflect.Interface && dv.NumMethod() == 0 {
		if st.Key().Kind() == reflect.String {
			dv.Set(reflect.MakeMapWithSize(mapStringInterfaceType, sv.Len()))
			dv = dv.Elem()
			dt = dv.Type()
		}
	}
	if dt.Kind() != reflect.Map {
		return fmt.Errorf("cannot convert map to: %v", dt.Kind())
	}

	if !st.Key().AssignableTo(dt.Key()) && !st.Key().ConvertibleTo(dt.Key()) {
		return fmt.Errorf("cannot copy map with non-assignable keys: %v %v", st.Key(), dt.Key())
	}

	if st == mapStringStringType && dt == mapStringInterfaceType && sv.CanInterface() {
		// Labels and annotations are common enough to skip reflecting over.
		dst := dv.Interface().(map[string]interface{})
		for k, v := range sv.Interface().(map[string]string) {
			dst[k] = v
		}
		return nil
	}

	for _, key := range sv.MapKeys() {
		value := reflect.New(dt.Elem()).Elem()
		if err := toUnstructured(sv.MapIndex(key), value); err != nil {
			return err
		}
		if st.Key().AssignableTo(dt.Key()) {
			dv.SetMapIndex(key, value)
		} else {
			dv.SetMapIndex(key.Convert(dt.Key()), value)
		}
	}
	return nil
}

func sliceToUnstructured(sv, dv reflect.Value) error {
	st, dt := sv.Type(), dv.Type()
	if sv.IsNil() {
		dv.Set(reflect.Zero(dt))
		return nil
	}
	if st.Elem().Kind() == reflect.Uint8 {
		dv.Set(reflect.New(stringType))
		data, err := json.Marshal(sv.Bytes())
		if err != nil {
			return err
		}
		var result string
		if err = json.Unmarshal(data, &result); err != nil {
			return err
		}
		dv.Set(reflect.ValueOf(result))
		return nil
	}
	if dt.Kind() == reflect.Interface && dv.NumMethod() == 0 {
		dv.Set(reflect.MakeSlice(reflect.SliceOf(dt), sv.Len(), sv.Cap()))
		dv = dv.Elem()
		dt = dv.Type()
	}
	if dt.Kind() != reflect.Slice {
		return fmt.Errorf("cannot convert slice to: %v", dt.Kind())
	}
	for i := 0; i < sv.Len(); i++ {
		if err := toUnstructured(sv.Index(i), dv.Index(i)); err != nil {
			return err
		}
	}
	return nil
}

func pointerToUnstructured(sv, dv reflect.Value) error {
	if sv.IsNil() {
		// We're done - we don't need to store anything.
		return nil
	}
	return toUnstructured(sv.Elem(), dv)
}

func isEmpty(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.String:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Map, reflect.Slice:
		// TODO: It seems that 0-len maps are ignored in it.
		return v.IsNil() || v.Len() == 0
	case reflect.Pointer, reflect.Interface:
		return v.IsNil()
	}
	return false
}

func structToUnstructured(sv, dv reflect.Value) error {
	return structToUnstructuredTopLevel(sv, dv, nil)
}

func structToUnstructuredTopLevel(sv, dv reflect.Value, topLevelKeys map[string]interface{}) error {
	st, dt := sv.Type(), dv.Type()
	sInfo := structInfoFromType(st)
	if dt.Kind() == reflect.Interface && dv.NumMethod() == 0 {
		capHint := len(sInfo.fields)
		if topLevelKeys != nil && len(topLevelKeys)+2 < capHint {
			capHint = len(topLevelKeys) + 2
		} else if capHint > 8 {
			capHint = 8
		}
		dv.Set(reflect.MakeMapWithSize(mapStringInterfaceType, capHint))
		dv = dv.Elem()
		dt = dv.Type()
	}
	if dt.Kind() != reflect.Map {
		return fmt.Errorf("cannot convert struct to: %v", dt.Kind())
	}
	realMap := dv.Interface().(map[string]interface{})
	return structToUnstructuredIntoMap(sv, sInfo, realMap, topLevelKeys)
}

//go:noinline
func toUnstructuredInlinedFallback(fv reflect.Value, realMap map[string]interface{}) error {
	return toUnstructured(fv, reflect.ValueOf(&realMap).Elem())
}

func structToUnstructuredIntoMap(sv reflect.Value, sInfo *structTypeInfo, realMap map[string]interface{}, topLevelKeys map[string]interface{}) error {
	for i, fieldInfo := range sInfo.fields {
		if fieldInfo.name == "-" || shouldSkipTopLevelField(fieldInfo, topLevelKeys) {
			// This field should be skipped.
			continue
		}
		fv := sv.Field(i)
		if subKeys := subPatchKeysForField(fv, fieldInfo, topLevelKeys); subKeys != nil {
			subMap := make(map[string]interface{}, len(subKeys))
			if err := structToUnstructuredIntoMap(fv, structInfoFromType(fv.Type()), subMap, subKeys); err != nil {
				return err
			}
			realMap[fieldInfo.name] = subMap
			continue
		}
		if fieldInfo.omitempty && isEmpty(fv) {
			// omitempty fields should be ignored.
			continue
		}
		if fieldInfo.omitzero != nil && fieldInfo.omitzero(fv) {
			// omitzero fields should be ignored
			continue
		}
		if len(fieldInfo.name) == 0 {
			// This field is inlined.
			inlined := fv
			if inlined.Kind() == reflect.Pointer {
				if inlined.IsNil() {
					continue
				}
				inlined = inlined.Elem()
			}
			if inlined.Kind() == reflect.Struct && !value.TypeReflectEntryOf(inlined.Type()).CanConvertToUnstructured() {
				if err := structToUnstructuredIntoMap(inlined, structInfoFromType(inlined.Type()), realMap, nil); err != nil {
					return err
				}
				continue
			}
			if err := toUnstructuredInlinedFallback(fv, realMap); err != nil {
				return err
			}
			continue
		}
		switch fv.Kind() {
		case reflect.String:
			realMap[fieldInfo.name] = fv.String()
		case reflect.Bool:
			realMap[fieldInfo.name] = fv.Bool()
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			realMap[fieldInfo.name] = fv.Int()
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			val, err := uintToUnstructuredHelper(fv.Uint())
			if err != nil {
				return err
			}
			realMap[fieldInfo.name] = val
		case reflect.Float32, reflect.Float64:
			realMap[fieldInfo.name] = fv.Float()
		default:
			val, err := toUnstructuredValue(fv)
			if err != nil {
				return err
			}
			realMap[fieldInfo.name] = val
		}
	}
	return nil
}

func interfaceToUnstructured(sv, dv reflect.Value) error {
	if !sv.IsValid() || sv.IsNil() {
		dv.Set(reflect.Zero(dv.Type()))
		return nil
	}
	return toUnstructured(sv.Elem(), dv)
}

func uintToUnstructuredHelper(uVal uint64) (int64, error) {
	if uVal > math.MaxInt64 {
		return 0, fmt.Errorf("unsigned value %d does not fit into int64 (overflow)", uVal)
	}
	return int64(uVal), nil
}
