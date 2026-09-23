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

package value

import "reflect"

// ReflectDeepEqual reports whether lhs and rhs are both backed by Go values
// through reflection, and those are deeply equal, in which case lhs and rhs are
// equal in every respect: as values, and in the fields and items they hold. It
// is a cheap, conservative check. A false result means nothing.
func ReflectDeepEqual(lhs, rhs Value) bool {
	l, ok := lhs.(*valueReflect)
	if !ok {
		return false
	}
	r, ok := rhs.(*valueReflect)
	if !ok || !l.Value.IsValid() || !r.Value.IsValid() {
		return false
	}
	// Addressability decides whether pointer receiver marshalers apply.
	if l.Value.Type() != r.Value.Type() || l.Value.CanAddr() != r.Value.CanAddr() {
		return false
	}
	return deepEqualReflect(l.Value, r.Value, 0)
}

// maxDeepEqualDepth bounds deepEqualReflect, which does not detect cycles.
const maxDeepEqualDepth = 64

var mapStringStringType = reflect.TypeFor[map[string]string]()

func deepEqualReflect(a, b reflect.Value, depth int) bool {
	if depth > maxDeepEqualDepth {
		return false
	}
	switch a.Kind() {
	case reflect.Bool:
		return a.Bool() == b.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return a.Int() == b.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return a.Uint() == b.Uint()
	case reflect.Float32, reflect.Float64:
		return a.Float() == b.Float()
	case reflect.String:
		return a.String() == b.String()
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			return a.IsNil() == b.IsNil()
		}
		return a.Pointer() == b.Pointer() || deepEqualReflect(a.Elem(), b.Elem(), depth+1)
	case reflect.Interface:
		if a.IsNil() || b.IsNil() {
			return a.IsNil() == b.IsNil()
		}
		ae, be := a.Elem(), b.Elem()
		return ae.Type() == be.Type() && deepEqualReflect(ae, be, depth+1)
	case reflect.Struct:
		for i, n := 0, a.NumField(); i < n; i++ {
			if !deepEqualReflect(a.Field(i), b.Field(i), depth+1) {
				return false
			}
		}
		return true
	case reflect.Slice:
		// nil and empty are told apart, since they can convert differently.
		if a.IsNil() != b.IsNil() || a.Len() != b.Len() {
			return false
		}
		if a.Pointer() == b.Pointer() {
			return true
		}
		fallthrough
	case reflect.Array:
		for i, n := 0, a.Len(); i < n; i++ {
			if !deepEqualReflect(a.Index(i), b.Index(i), depth+1) {
				return false
			}
		}
		return true
	case reflect.Map:
		if a.IsNil() != b.IsNil() || a.Len() != b.Len() {
			return false
		}
		if a.Pointer() == b.Pointer() {
			return true
		}
		if a.Type() == mapStringStringType && a.CanInterface() && b.CanInterface() {
			bm := b.Interface().(map[string]string)
			for k, av := range a.Interface().(map[string]string) {
				if bv, ok := bm[k]; !ok || av != bv {
					return false
				}
			}
			return true
		}
		// Reading entries into reused values saves copying each one.
		key := reflect.New(a.Type().Key()).Elem()
		val := reflect.New(a.Type().Elem()).Elem()
		iter := a.MapRange()
		for iter.Next() {
			key.SetIterKey(iter)
			val.SetIterValue(iter)
			bv := b.MapIndex(key)
			if !bv.IsValid() || !deepEqualReflect(val, bv, depth+1) {
				return false
			}
		}
		return true
	default:
		// Funcs, channels and the like are never treated as equal.
		return false
	}
}
