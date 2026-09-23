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

package typed

import (
	"sync"

	"sigs.k8s.io/structured-merge-diff/v7/fieldpath"
	"sigs.k8s.io/structured-merge-diff/v7/schema"
	"sigs.k8s.io/structured-merge-diff/v7/value"
)

var vPool = sync.Pool{
	New: func() interface{} { return &validatingObjectWalker{} },
}

func (tv TypedValue) walker() *validatingObjectWalker {
	v := vPool.Get().(*validatingObjectWalker)
	v.value = tv.value
	v.schema = tv.schema
	v.typeRef = tv.typeRef
	v.allowDuplicates = false
	if v.allocator == nil {
		v.allocator = value.NewFreelistAllocator()
	}
	return v
}

func (v *validatingObjectWalker) finished() {
	v.schema = nil
	v.typeRef = schema.TypeRef{}
	vPool.Put(v)
}

type validatingObjectWalker struct {
	value   value.Value
	schema  *schema.Schema
	typeRef schema.TypeRef
	// If set to true, duplicates will be allowed in
	// associativeLists/sets.
	allowDuplicates bool

	// Allocate only as many walkers as needed for the depth by storing them here.
	spareWalkers *[]*validatingObjectWalker
	allocator    value.Allocator

	// visitMapItems state. The callback is bound to this walker once, rather
	// than a closure being allocated for every map.
	mapItemFn func(key string, val value.Value) bool
	boundTo   *validatingObjectWalker
	curMap    *schema.Map
	curErrs   ValidationErrors
}

func (v *validatingObjectWalker) prepareDescent(tr schema.TypeRef) *validatingObjectWalker {
	if v.spareWalkers == nil {
		// first descent.
		v.spareWalkers = &[]*validatingObjectWalker{}
	}
	var v2 *validatingObjectWalker
	if n := len(*v.spareWalkers); n > 0 {
		v2, *v.spareWalkers = (*v.spareWalkers)[n-1], (*v.spareWalkers)[:n-1]
	} else {
		v2 = &validatingObjectWalker{}
	}
	fn, boundTo := v2.mapItemFn, v2.boundTo
	*v2 = *v
	v2.mapItemFn, v2.boundTo = fn, boundTo
	v2.typeRef = tr
	return v2
}

func (v *validatingObjectWalker) finishDescent(v2 *validatingObjectWalker) {
	// if the descent caused a realloc, ensure that we reuse the buffer
	// for the next sibling.
	*v.spareWalkers = append(*v.spareWalkers, v2)
}

func (v *validatingObjectWalker) validate(prefixFn func() string) ValidationErrors {
	return resolveSchema(v.schema, v.typeRef, v.value, v).WithLazyPrefix(prefixFn)
}

func validateScalar(t *schema.Scalar, v value.Value, prefix string) (errs ValidationErrors) {
	if v == nil {
		return nil
	}
	if v.IsNull() {
		return nil
	}
	switch *t {
	case schema.Numeric:
		if !v.IsFloat() && !v.IsInt() {
			// TODO: should the schema separate int and float?
			return errorf("%vexpected numeric (int or float), got %T", prefix, v.Unstructured())
		}
	case schema.String:
		if !v.IsString() {
			return errorf("%vexpected string, got %#v", prefix, v)
		}
	case schema.Boolean:
		if !v.IsBool() {
			return errorf("%vexpected boolean, got %v", prefix, v)
		}
	case schema.Untyped:
		if !v.IsFloat() && !v.IsInt() && !v.IsString() && !v.IsBool() {
			return errorf("%vexpected any scalar, got %v", prefix, v)
		}
	default:
		return errorf("%vunexpected scalar type in schema: %v", prefix, *t)
	}
	return nil
}

func (v *validatingObjectWalker) doScalar(t *schema.Scalar) ValidationErrors {
	if errs := validateScalar(t, v.value, ""); len(errs) > 0 {
		return errs
	}
	return nil
}

func (v *validatingObjectWalker) visitListItems(t *schema.List, list value.List) (errs ValidationErrors) {
	associative := t.ElementRelationship == schema.Associative
	// Path elements are only needed to detect duplicates, and to prefix errors.
	trackKeys := associative && !v.allowDuplicates
	var observedKeys fieldpath.PathElementSet
	if trackKeys {
		observedKeys = fieldpath.MakePathElementSet(list.Length())
	}
	for i := 0; i < list.Length(); i++ {
		child := list.AtUsing(v.allocator, i)
		var pe fieldpath.PathElement
		if trackKeys {
			// observedKeys can refer into child, so it has to outlive the loop.
			defer v.allocator.Free(child)
			var err error
			pe, err = listItemToPathElement(v.allocator, v.schema, t, child)
			if err != nil {
				errs = append(errs, errorf("element %v: %v", i, err.Error())...)
				// If we can't construct the path element, we can't
				// even report errors deeper in the schema, so bail on
				// this element.
				return
			}
			if observedKeys.Has(pe) {
				errs = append(errs, errorf("duplicate entries for key %v", pe.String())...)
			}
			observedKeys.Insert(pe)
		} else if associative {
			if err := validateListItemKey(v.allocator, v.schema, t, child); err != nil {
				v.allocator.Free(child)
				errs = append(errs, errorf("element %v: %v", i, err.Error())...)
				return
			}
		}
		v2 := v.prepareDescent(t.ElementType)
		v2.value = child
		if childErrs := v2.validate(nil); len(childErrs) > 0 {
			switch {
			case trackKeys:
			case associative:
				// The key was validated above, so this cannot fail.
				pe, _ = listItemToPathElement(v.allocator, v.schema, t, child)
			default:
				idx := i
				pe.Index = &idx
			}
			errs = append(errs, childErrs.WithLazyPrefix(pe.String)...)
		}
		v.finishDescent(v2)
		if !trackKeys {
			v.allocator.Free(child)
		}
	}
	return errs
}

func (v *validatingObjectWalker) doList(t *schema.List) (errs ValidationErrors) {
	list, err := listValue(v.allocator, v.value)
	if err != nil {
		return errorf("%v", err)
	}

	if list == nil {
		return nil
	}

	defer v.allocator.Free(list)
	errs = v.visitListItems(t, list)

	return errs
}

func (v *validatingObjectWalker) visitMapItems(t *schema.Map, m value.Map) (errs ValidationErrors) {
	if v.boundTo != v {
		v.mapItemFn = v.visitCurrentMapItem
		v.boundTo = v
	}
	prevMap, prevErrs := v.curMap, v.curErrs
	v.curMap, v.curErrs = t, nil
	m.IterateUsing(v.allocator, v.mapItemFn)
	errs = v.curErrs
	v.curMap, v.curErrs = prevMap, prevErrs
	return errs
}

func (v *validatingObjectWalker) visitCurrentMapItem(key string, val value.Value) bool {
	t := v.curMap
	tr := t.ElementType
	if sf, ok := t.FindFieldRef(key); ok {
		tr = sf.Type
	} else if (t.ElementType == schema.TypeRef{}) {
		// Taking the address of a copy keeps key itself off the heap.
		k := key
		pe := fieldpath.PathElement{FieldName: &k}
		v.curErrs = append(v.curErrs, errorf("field not declared in schema").WithPrefix(pe.String())...)
		return false
	}
	v2 := v.prepareDescent(tr)
	v2.value = val
	if childErrs := v2.validate(nil); len(childErrs) > 0 {
		k := key
		pe := fieldpath.PathElement{FieldName: &k}
		v.curErrs = append(v.curErrs, childErrs.WithLazyPrefix(pe.String)...)
	}
	v.finishDescent(v2)
	return true
}

func (v *validatingObjectWalker) doMap(t *schema.Map) (errs ValidationErrors) {
	m, err := mapValue(v.allocator, v.value)
	if err != nil {
		return errorf("%v", err)
	}
	if m == nil {
		return nil
	}
	defer v.allocator.Free(m)
	errs = v.visitMapItems(t, m)

	return errs
}
