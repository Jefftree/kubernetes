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

import (
	"reflect"
)

type mapReflect struct {
	valueReflect
}

func (r mapReflect) Length() int {
	val := r.Value
	return val.Len()
}

func (r mapReflect) Empty() bool {
	val := r.Value
	return val.Len() == 0
}

func (r mapReflect) Get(key string) (Value, bool) {
	return r.GetUsing(HeapAllocator, key)
}

func (r mapReflect) GetUsing(a Allocator, key string) (Value, bool) {
	k, v, ok := r.get(key)
	if !ok {
		return nil, false
	}
	return a.allocValueReflect().mustReuse(v, nil, &r.Value, &k), true
}

func (r mapReflect) get(k string) (key, value reflect.Value, ok bool) {
	mapKey := r.toMapKey(k)
	val := r.Value.MapIndex(mapKey)
	return mapKey, val, val.IsValid() && val != reflect.Value{}
}

func (r mapReflect) Has(key string) bool {
	var val reflect.Value
	val = r.Value.MapIndex(r.toMapKey(key))
	if !val.IsValid() {
		return false
	}
	return val != reflect.Value{}
}

func (r mapReflect) Set(key string, val Value) {
	r.Value.SetMapIndex(r.toMapKey(key), reflect.ValueOf(val.Unstructured()))
}

func (r mapReflect) Delete(key string) {
	val := r.Value
	val.SetMapIndex(r.toMapKey(key), reflect.Value{})
}

// TODO: Do we need to support types that implement json.Marshaler and are used as string keys?
func (r mapReflect) toMapKey(key string) reflect.Value {
	val := r.Value
	return reflect.ValueOf(key).Convert(val.Type().Key())
}

func (r mapReflect) Iterate(fn func(string, Value) bool) bool {
	return r.IterateUsing(HeapAllocator, fn)
}

func (r mapReflect) IterateUsing(a Allocator, fn func(string, Value) bool) bool {
	if r.Value.Len() == 0 {
		return true
	}
	v := a.allocValueReflect()
	defer a.Free(v)
	entry := TypeReflectEntryOf(r.Value.Type().Elem())
	// Reading keys into one reused value, rather than with iter.Key, avoids an
	// allocation per entry. Like v, it is only valid during the callback.
	key := reflect.New(r.Value.Type().Key()).Elem()
	val, reuseVal := reusableElem(r.Value.Type().Elem(), entry)
	var parent elemParent
	iter := r.Value.MapRange()
	for iter.Next() {
		key.SetIterKey(iter)
		var elem Value
		if reuseVal {
			val.SetIterValue(iter)
			elem = v.mustReuse(val, entry, nil, nil)
		} else if next := iter.Value(); !next.IsValid() {
			continue
		} else {
			m, k := parent.of(r.Value, key)
			elem = v.mustReuse(next, entry, m, k)
		}
		if !fn(key.String(), elem) {
			return false
		}
	}
	return true
}

// elemParent provides the map and key that a struct read out of a map needs,
// to replace itself in the map when it is modified, since map elements cannot
// be modified in place. Rather than pointers to locals, which would move them
// to the heap in every call, it hands out pointers it allocates once, and only
// when asked. They hold the key most recently asked for.
//
// Elements read into the value reusableElem returns never need them, as they
// are never structs.
type elemParent struct {
	m, key *reflect.Value
}

func (p *elemParent) of(m, key reflect.Value) (*reflect.Value, *reflect.Value) {
	if p.m == nil {
		p.m, p.key = new(reflect.Value), new(reflect.Value)
		*p.m = m
	}
	*p.key = key
	return p.m, p.key
}

// reusableElem returns a value to read map elements of type t into, and true,
// when t is a plain scalar type, or converts to unstructured only with value
// methods. Reading into one reused value avoids an allocation per element. It
// is safe for these types because nothing writes back through them, and their
// conversion does not depend on addressability. Value methods also get a copy
// of their receiver, so nothing they return can refer to the reused value.
func reusableElem(t reflect.Type, entry *TypeReflectCacheEntry) (reflect.Value, bool) {
	if entry.CanConvertToUnstructured() {
		if entry.convertsWithValueMethodsOnly() {
			return reflect.New(t).Elem(), true
		}
		return reflect.Value{}, false
	}
	switch t.Kind() {
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return reflect.New(t).Elem(), true
	}
	return reflect.Value{}, false
}

func (r mapReflect) Unstructured() interface{} {
	result := make(map[string]interface{}, r.Length())
	r.Iterate(func(s string, value Value) bool {
		result[s] = value.Unstructured()
		return true
	})
	return result
}

func (r mapReflect) Equals(m Map) bool {
	return r.EqualsUsing(HeapAllocator, m)
}

func (r mapReflect) EqualsUsing(a Allocator, m Map) bool {
	lhsLength := r.Length()
	rhsLength := m.Length()
	if lhsLength != rhsLength {
		return false
	}
	if lhsLength == 0 {
		return true
	}
	vr := a.allocValueReflect()
	defer a.Free(vr)
	entry := TypeReflectEntryOf(r.Value.Type().Elem())
	return m.Iterate(func(key string, value Value) bool {
		_, lhsVal, ok := r.get(key)
		if !ok {
			return false
		}
		return EqualsUsing(a, vr.mustReuse(lhsVal, entry, nil, nil), value)
	})
}

func (r mapReflect) Zip(other Map, order MapTraverseOrder, fn func(key string, lhs, rhs Value) bool) bool {
	return r.ZipUsing(HeapAllocator, other, order, fn)
}

func (r mapReflect) ZipUsing(a Allocator, other Map, order MapTraverseOrder, fn func(key string, lhs, rhs Value) bool) bool {
	if otherMapReflect, ok := other.(*mapReflect); ok && order == Unordered {
		return r.unorderedReflectZip(a, otherMapReflect, fn)
	}
	// Taking the address of a copy keeps r itself off the heap on the fast path.
	rr := r
	return defaultMapZip(a, &rr, other, order, fn)
}

// unorderedReflectZip provides an optimized unordered zip for mapReflect types.
func (r mapReflect) unorderedReflectZip(a Allocator, other *mapReflect, fn func(key string, lhs, rhs Value) bool) bool {
	if r.Empty() && (other == nil || other.Empty()) {
		return true
	}

	lhs := r.Value
	lhsEntry := TypeReflectEntryOf(lhs.Type().Elem())

	// map lookup via reflection is expensive enough that it is better to keep track of visited keys
	visited := map[string]struct{}{}

	vlhs, vrhs := a.allocValueReflect(), a.allocValueReflect()
	defer a.Free(vlhs)
	defer a.Free(vrhs)
	var lhsParent elemParent

	if other != nil {
		rhs := other.Value
		rhsEntry := TypeReflectEntryOf(rhs.Type().Elem())
		iter := rhs.MapRange()
		key := reflect.New(rhs.Type().Key()).Elem()
		val, reuseVal := reusableElem(rhs.Type().Elem(), rhsEntry)
		sameType := lhs.Type() == rhs.Type()

		var rhsParent elemParent
		for iter.Next() {
			key.SetIterKey(iter)
			keyString := key.String()
			var rhsVal Value
			if reuseVal {
				val.SetIterValue(iter)
				rhsVal = vrhs.mustReuse(val, rhsEntry, nil, nil)
			} else if next := iter.Value(); !next.IsValid() {
				continue
			} else {
				m, k := rhsParent.of(rhs, key)
				rhsVal = vrhs.mustReuse(next, rhsEntry, m, k)
			}
			visited[keyString] = struct{}{}
			var lhsVal Value
			if sameType {
				// The key read from rhs indexes lhs directly.
				if v := lhs.MapIndex(key); v.IsValid() {
					m, k := lhsParent.of(lhs, key)
					lhsVal = vlhs.mustReuse(v, lhsEntry, m, k)
				}
			} else if mk, v, ok := r.get(keyString); ok {
				m, k := lhsParent.of(lhs, mk)
				lhsVal = vlhs.mustReuse(v, lhsEntry, m, k)
			}
			if !fn(keyString, lhsVal, rhsVal) {
				return false
			}
		}
	}

	iter := lhs.MapRange()
	key := reflect.New(lhs.Type().Key()).Elem()
	for iter.Next() {
		key.SetIterKey(iter)
		if _, ok := visited[key.String()]; ok {
			continue
		}
		next := iter.Value()
		if !next.IsValid() {
			continue
		}
		m, k := lhsParent.of(lhs, key)
		if !fn(key.String(), vlhs.mustReuse(next, lhsEntry, m, k), nil) {
			return false
		}
	}
	return true
}
