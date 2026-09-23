/*
Copyright 2021 The Kubernetes Authors.

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

package fieldmanager_test

import (
	"context"
	"reflect"
	"testing"

	"sigs.k8s.io/structured-merge-diff/v7/fieldpath"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/endpoints/handlers/fieldmanager"
	"k8s.io/apiserver/pkg/warning"
)

func TestAdmission(t *testing.T) {
	wrap := &mockAdmissionController{}
	ac := fieldmanager.NewManagedFieldsValidatingAdmissionController(wrap)
	now := metav1.Now()

	validFieldsV1 := metav1.FieldsV1{}
	raw, err := fieldpath.NewSet(fieldpath.MakePathOrDie("metadata", "labels", "test-label")).ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	validFieldsV1.SetRawBytes(raw)
	validManagedFieldsEntry := metav1.ManagedFieldsEntry{
		APIVersion: "v1",
		Operation:  metav1.ManagedFieldsOperationApply,
		Time:       &now,
		Manager:    "test",
		FieldsType: "FieldsV1",
		FieldsV1:   &validFieldsV1,
	}

	managedFieldsMutators := map[string]func(in metav1.ManagedFieldsEntry) (out metav1.ManagedFieldsEntry, shouldReset bool){
		"invalid APIVersion": func(managedFields metav1.ManagedFieldsEntry) (metav1.ManagedFieldsEntry, bool) {
			managedFields.APIVersion = ""
			return managedFields, true
		},
		"invalid Operation": func(managedFields metav1.ManagedFieldsEntry) (metav1.ManagedFieldsEntry, bool) {
			managedFields.Operation = "invalid operation"
			return managedFields, true
		},
		"invalid fieldsType": func(managedFields metav1.ManagedFieldsEntry) (metav1.ManagedFieldsEntry, bool) {
			managedFields.FieldsType = "invalid fieldsType"
			return managedFields, true
		},
		"invalid fieldsV1": func(managedFields metav1.ManagedFieldsEntry) (metav1.ManagedFieldsEntry, bool) {
			managedFields.FieldsV1 = metav1.NewFieldsV1("{invalid}")
			return managedFields, true
		},
		"invalid manager": func(managedFields metav1.ManagedFieldsEntry) (metav1.ManagedFieldsEntry, bool) {
			managedFields.Manager = ""
			return managedFields, false
		},
	}

	for name, mutate := range managedFieldsMutators {
		t.Run(name, func(t *testing.T) {
			mutated, shouldReset := mutate(validManagedFieldsEntry)
			validEntries := []metav1.ManagedFieldsEntry{validManagedFieldsEntry}
			mutatedEntries := []metav1.ManagedFieldsEntry{mutated}

			obj := &v1.ConfigMap{}
			obj.SetManagedFields(validEntries)

			wrap.admit = replaceManagedFields(mutatedEntries)

			attrs := admission.NewAttributesRecord(obj, obj, schema.GroupVersionKind{}, "default", "", schema.GroupVersionResource{}, "", admission.Update, nil, false, nil)
			if err := ac.(admission.MutationInterface).Admit(context.TODO(), attrs, nil); err != nil {
				t.Fatal(err)
			}

			if shouldReset && !reflect.DeepEqual(obj.GetManagedFields(), validEntries) {
				t.Fatalf("expected: \n%v\ngot:\n%v", validEntries, obj.GetManagedFields())
			}
			if !shouldReset && reflect.DeepEqual(obj.GetManagedFields(), validEntries) {
				t.Fatalf("expected: \n%v\ngot:\n%v", mutatedEntries, obj.GetManagedFields())
			}
		})
	}
}

type recordingWarnings struct{ warnings []string }

func (r *recordingWarnings) AddWarning(_, text string) { r.warnings = append(r.warnings, text) }

// Validation is skipped when admission leaves managedFields alone, so a change
// made in place, where the slice itself is untouched, must still be caught.
func TestAdmissionValidatesInPlaceMutations(t *testing.T) {
	raw, err := fieldpath.NewSet(fieldpath.MakePathOrDie("metadata", "labels", "test-label")).ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	newEntries := func() []metav1.ManagedFieldsEntry {
		return []metav1.ManagedFieldsEntry{{
			APIVersion: "v1",
			Operation:  metav1.ManagedFieldsOperationApply,
			Time:       &now,
			Manager:    "test",
			FieldsType: "FieldsV1",
			FieldsV1:   &metav1.FieldsV1{Raw: append([]byte(nil), raw...)},
		}}
	}

	cases := map[string]struct {
		mutate      func(entries []metav1.ManagedFieldsEntry)
		wantWarning bool
	}{
		"unchanged":            {mutate: func([]metav1.ManagedFieldsEntry) {}},
		"valid change":         {mutate: func(e []metav1.ManagedFieldsEntry) { e[0].Manager = "other" }},
		"operation in place":   {mutate: func(e []metav1.ManagedFieldsEntry) { e[0].Operation = "invalid" }, wantWarning: true},
		"fieldsType in place":  {mutate: func(e []metav1.ManagedFieldsEntry) { e[0].FieldsType = "invalid" }, wantWarning: true},
		"fieldsV1 bytes":       {mutate: func(e []metav1.ManagedFieldsEntry) { copy(e[0].FieldsV1.Raw, "{invalid") }, wantWarning: true},
		"fieldsV1 raw pointer": {mutate: func(e []metav1.ManagedFieldsEntry) { *e[0].FieldsV1 = metav1.FieldsV1{Raw: []byte("{invalid}")} }, wantWarning: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			obj := &v1.ConfigMap{}
			obj.SetManagedFields(newEntries())
			wrap := &mockAdmissionController{admit: func(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces) error {
				tc.mutate(a.GetObject().(*v1.ConfigMap).ManagedFields)
				return nil
			}}
			ac := fieldmanager.NewManagedFieldsValidatingAdmissionController(wrap)
			recorder := &recordingWarnings{}
			ctx := warning.WithWarningRecorder(context.Background(), recorder)
			attrs := admission.NewAttributesRecord(obj, obj, schema.GroupVersionKind{}, "default", "", schema.GroupVersionResource{}, "", admission.Update, nil, false, nil)
			if err := ac.(admission.MutationInterface).Admit(ctx, attrs, nil); err != nil {
				t.Fatal(err)
			}
			if gotWarning := len(recorder.warnings) > 0; gotWarning != tc.wantWarning {
				t.Errorf("warnings = %v, want a warning: %v", recorder.warnings, tc.wantWarning)
			}
		})
	}
}

func replaceManagedFields(with []metav1.ManagedFieldsEntry) func(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces) error {
	return func(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces) error {
		objectMeta, err := meta.Accessor(a.GetObject())
		if err != nil {
			return err
		}
		objectMeta.SetManagedFields(with)
		return nil
	}
}

type mockAdmissionController struct {
	admit func(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces) error
}

func (c *mockAdmissionController) Handles(operation admission.Operation) bool {
	return true
}

func (c *mockAdmissionController) Admit(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces) error {
	return c.admit(ctx, a, o)
}
