/*
Copyright 2026 The Kubernetes Authors.

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
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/structured-merge-diff/v7/fieldpath"
)

type embedsObjectMeta struct {
	metav1.TypeMeta
	metav1.ObjectMeta
	Spec map[string]string
}

func (o *embedsObjectMeta) DeepCopyObject() runtime.Object {
	panic("not used")
}

type pointsToObjectMeta struct {
	metav1.TypeMeta
	*metav1.ObjectMeta
}

func (o *pointsToObjectMeta) DeepCopyObject() runtime.Object {
	panic("not used")
}

func TestWithoutManagedFields(t *testing.T) {
	managedFields := []metav1.ManagedFieldsEntry{{Manager: "m", Operation: metav1.ManagedFieldsOperationUpdate}}
	obj := &embedsObjectMeta{
		ObjectMeta: metav1.ObjectMeta{Name: "n", Labels: map[string]string{"a": "b"}, ManagedFields: managedFields},
		Spec:       map[string]string{"c": "d"},
	}
	got, ok := withoutManagedFields(obj).(*embedsObjectMeta)
	if !ok || got == obj {
		t.Fatalf("expected a copy, got %#v", got)
	}
	if got.ManagedFields != nil {
		t.Errorf("expected managedFields to be cleared, got %v", got.ManagedFields)
	}
	if len(obj.ManagedFields) != 1 {
		t.Errorf("the original was modified: %v", obj.ManagedFields)
	}
	want := *obj
	want.ManagedFields = nil
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("got %#v, want %#v", *got, want)
	}

	withoutAny := &embedsObjectMeta{ObjectMeta: metav1.ObjectMeta{Name: "n"}}
	u := &unstructured.Unstructured{Object: map[string]interface{}{"metadata": map[string]interface{}{"managedFields": []interface{}{}}}}
	pointer := &pointsToObjectMeta{ObjectMeta: &metav1.ObjectMeta{ManagedFields: managedFields}}
	for _, unchanged := range []runtime.Object{withoutAny, u, pointer} {
		if got := withoutManagedFields(unchanged); got != unchanged {
			t.Errorf("expected %T to be returned as is, got %#v", unchanged, got)
		}
	}
}

func TestOwnsManagedFields(t *testing.T) {
	set := func(paths ...fieldpath.Path) *fieldpath.Set { return fieldpath.NewSet(paths...) }
	tests := []struct {
		name string
		sets []*fieldpath.Set
		want bool
	}{
		{name: "none"},
		{name: "other metadata", sets: []*fieldpath.Set{set(fieldpath.MakePathOrDie("metadata", "labels", "a"), fieldpath.MakePathOrDie("spec"))}},
		{name: "managedFields outside metadata", sets: []*fieldpath.Set{set(fieldpath.MakePathOrDie("spec", "managedFields"))}},
		{name: "managedFields", sets: []*fieldpath.Set{set(fieldpath.MakePathOrDie("spec")), set(fieldpath.MakePathOrDie("metadata", "managedFields"))}, want: true},
		{name: "within managedFields", sets: []*fieldpath.Set{set(fieldpath.MakePathOrDie("metadata", "managedFields", 0, "manager"))}, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			managers := fieldpath.ManagedFields{}
			for i, s := range tc.sets {
				managers[string(rune('a'+i))] = fieldpath.NewVersionedSet(s, "v1", false)
			}
			if got := ownsManagedFields(managers); got != tc.want {
				t.Errorf("ownsManagedFields() = %v, want %v", got, tc.want)
			}
		})
	}
}
