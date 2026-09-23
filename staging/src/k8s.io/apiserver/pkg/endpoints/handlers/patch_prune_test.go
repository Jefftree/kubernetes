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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"
	"sigs.k8s.io/randfill"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/apitesting/fuzzer"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metafuzzer "k8s.io/apimachinery/pkg/apis/meta/fuzzer"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/warning"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/structured-merge-diff/v7/fieldpath"
)

// unprunedStrategicPatchObject is strategicPatchObject without pruning, the
// reference the pruned path must match.
func unprunedStrategicPatchObject(ctx context.Context, defaulter runtime.ObjectDefaulter, originalObject runtime.Object, patchBytes []byte, objToUpdate, schemaReferenceObj runtime.Object, validationDirective string) error {
	originalObjMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(originalObject)
	if err != nil {
		return err
	}
	patchMap, strictErrs, err := decodePatchMap(patchBytes, validationDirective)
	if err != nil {
		return err
	}
	return applyPatchToObject(ctx, defaulter, originalObjMap, patchMap, objToUpdate, schemaReferenceObj, strictErrs, validationDirective)
}

type recordingWarnings struct{ warnings []string }

func (r *recordingWarnings) AddWarning(_, text string) { r.warnings = append(r.warnings, text) }

type patchOutcome struct {
	obj      runtime.Object
	err      string
	warnings []string
}

func runPatch(t *testing.T, pruned bool, original runtime.Object, patch []byte, directive string) patchOutcome {
	t.Helper()
	recorder := &recordingWarnings{}
	ctx := warning.WithWarningRecorder(request.WithNamespace(context.Background(), "ns"), recorder)
	originalCopy := original.DeepCopyObject()
	obj := reflect.New(reflect.TypeOf(original).Elem()).Interface().(runtime.Object)
	schemaReferenceObj := reflect.New(reflect.TypeOf(original).Elem()).Interface().(runtime.Object)
	var err error
	if pruned {
		err = strategicPatchObject(ctx, clientgoscheme.Scheme, original, patch, obj, schemaReferenceObj, directive)
	} else {
		err = unprunedStrategicPatchObject(ctx, clientgoscheme.Scheme, original, patch, obj, schemaReferenceObj, directive)
	}
	if !reflect.DeepEqual(originalCopy, original) {
		t.Fatalf("patch mutated the original: %s", cmp.Diff(originalCopy, original))
	}
	out := patchOutcome{obj: obj, warnings: recorder.warnings}
	if err != nil {
		out.err = err.Error()
		out.obj = nil
	}
	sort.Strings(out.warnings)
	return out
}

func comparePatchOutcomes(t *testing.T, original runtime.Object, patch []byte, directive string) (pruned bool) {
	t.Helper()
	patchMap, _, err := decodePatchMap(patch, directive)
	if err == nil {
		_, pruned = planPrunedPatch(original, reflect.New(reflect.TypeOf(original).Elem()).Interface().(runtime.Object), patchMap)
	}
	want := runPatch(t, false, original, patch, directive)
	got := runPatch(t, true, original, patch, directive)
	if want.err != got.err {
		t.Fatalf("patch %s: error mismatch\nunpruned: %v\npruned:   %v", patch, want.err, got.err)
	}
	if !reflect.DeepEqual(want.warnings, got.warnings) {
		t.Fatalf("patch %s: warnings mismatch\nunpruned: %v\npruned:   %v", patch, want.warnings, got.warnings)
	}
	if want.obj == nil {
		return pruned
	}
	// The pruned path skips the round trip for managedFields, which sorts the
	// keys of their fieldsV1. The field manager re-encodes them from the decoded
	// sets after every patch, so only the sets they decode to have to match.
	canonicalizeFieldsV1(t, want.obj)
	canonicalizeFieldsV1(t, got.obj)
	if !apiequality.Semantic.DeepEqual(want.obj, got.obj) {
		t.Fatalf("patch %s: result mismatch (-unpruned +pruned):\n%s", patch, cmp.Diff(want.obj, got.obj))
	}
	wantJSON, err := json.Marshal(want.obj)
	if err != nil {
		t.Fatal(err)
	}
	gotJSON, err := json.Marshal(got.obj)
	if err != nil {
		t.Fatal(err)
	}
	if string(wantJSON) != string(gotJSON) {
		t.Fatalf("patch %s: serialized result mismatch\nunpruned: %s\npruned:   %s", patch, wantJSON, gotJSON)
	}
	// The result must not alias the original: mutating it through every
	// reachable pointer must leave the original untouched.
	originalCopy := original.DeepCopyObject()
	scribble(reflect.ValueOf(got.obj))
	if !reflect.DeepEqual(originalCopy, original) {
		t.Fatalf("patch %s: result aliases the original: %s", patch, cmp.Diff(originalCopy, original))
	}
	return pruned
}

func canonicalizeFieldsV1(t *testing.T, obj runtime.Object) {
	t.Helper()
	accessor, err := meta.Accessor(obj)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range accessor.GetManagedFields() {
		if entry.FieldsV1 == nil {
			continue
		}
		set := &fieldpath.Set{}
		if err := set.FromJSON(bytes.NewReader(entry.FieldsV1.Raw)); err != nil {
			// Both sides carry the same bytes when they fail to decode.
			continue
		}
		raw, err := set.ToJSON()
		if err != nil {
			t.Fatal(err)
		}
		entry.FieldsV1.Raw = raw
	}
}

// scribble overwrites every string and integer reachable from v.
func scribble(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			scribble(v.Elem())
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				scribble(v.Field(i))
			}
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			scribble(v.Index(i))
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			e := reflect.New(v.Type().Elem()).Elem()
			e.Set(v.MapIndex(k))
			scribble(e)
			v.SetMapIndex(k, e)
		}
	case reflect.String:
		if v.CanSet() {
			v.SetString("scribbled")
		}
	case reflect.Int, reflect.Int32, reflect.Int64:
		if v.CanSet() {
			v.SetInt(-7)
		}
	}
}

func testPod() *corev1.Pod {
	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        "p",
			Namespace:   "ns",
			Labels:      map[string]string{"a": "b"},
			Annotations: map[string]string{"x": "y"},
			Finalizers:  []string{"f1", "f2"},
			ManagedFields: []metav1.ManagedFieldsEntry{{
				Manager:    "m",
				Operation:  metav1.ManagedFieldsOperationUpdate,
				APIVersion: "v1",
				FieldsType: "FieldsV1",
				FieldsV1:   &metav1.FieldsV1{Raw: []byte(`{"f:metadata":{"f:labels":{".":{},"f:a":{}}},"f:spec":{"f:containers":{"k:{\"name\":\"c\"}":{".":{},"f:image":{}}}}}`)},
			}},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Image: "i", Env: []corev1.EnvVar{{Name: "E", Value: "V"}}}},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
}

func TestPrunedStrategicPatchMatchesUnpruned(t *testing.T) {
	cases := []struct {
		patch      string
		wantPruned bool
	}{
		{`{"metadata":{"labels":{"a":"c","d":"e"}}}`, true},
		{`{"metadata":{"labels":null}}`, true},
		{`{"metadata":{"annotations":{"x":null}},"status":{"phase":"Failed"}}`, true},
		{`{"status":{"conditions":[{"type":"Ready","status":"False"}]}}`, true},
		{`{"status":{"$setElementOrder/conditions":[{"type":"Ready"}],"conditions":[{"type":"Ready","status":"False"}]}}`, true},
		{`{"spec":{"containers":[{"name":"c","image":"j"}]}}`, true},
		{`{"spec":{"containers":[{"name":"c","$patch":"delete"}]}}`, true},
		{`{"spec":{"$retainKeys":["containers"],"containers":[{"name":"c","image":"j"}]}}`, true},
		{`{"metadata":{"$deleteFromPrimitiveList/finalizers":["f1"]}}`, true},
		{`{"metadata":{"$setElementOrder/finalizers":["f2","f1"],"finalizers":["f2","f1"]}}`, true},
		{`{"metadata":{"$patch":"replace","name":"p"}}`, true},
		{`{"metadata":{"$retainKeys":["name"],"name":"p"}}`, true},
		{`{"metadata":{"managedFields":[{"manager":"other","operation":"Update","apiVersion":"v1","fieldsType":"FieldsV1","fieldsV1":{"f:spec":{}}}]}}`, true},
		{`{"metadata":{"managedFields":null}}`, true},
		{`{"metadata":null}`, true},
		{`{"status":null}`, true},
		{`{"$setElementOrder/metadata":[]}`, true},
		{`{"apiVersion":"v1","kind":"Pod"}`, true},
		{`{"kind":"Other"}`, true},
		{`{"$patch":"replace","metadata":{"name":"p"}}`, false},
		{`{"$retainKeys":["metadata"],"metadata":{"name":"p"}}`, false},
		{`{"unknown":1,"metadata":{"labels":{"a":"z"}}}`, false},
		{`{"metadata":{"unknown":1,"labels":{"a":"z"}}}`, true},
		{`{"spec":{"containers":[{"name":"c","unknown":1}]}}`, true},
		{`{"metadata":{"labels":{"a":"z"}},"metadata":{"labels":{"a":"y"}}}`, true},
		{`{"metadata":{"labels":"notamap"}}`, true},
		{`{"metadata":"notamap"}`, true},
		{`{"spec":{"containers":[{"name":"c","image":1}]}}`, true},
		{`{"status":{"conditions":[{"status":"False"}]}}`, true},
		{`{}`, true},
	}
	for _, tc := range cases {
		for _, directive := range []string{"", metav1.FieldValidationIgnore, metav1.FieldValidationWarn, metav1.FieldValidationStrict} {
			t.Run(fmt.Sprintf("%s/%s", tc.patch, directive), func(t *testing.T) {
				pruned := comparePatchOutcomes(t, testPod(), []byte(tc.patch), directive)
				if pruned != tc.wantPruned {
					t.Errorf("pruned = %v, want %v", pruned, tc.wantPruned)
				}
			})
		}
	}
}

// A managedFields entry nested in a template is not re-encoded by the field
// manager, so the field holding it has to take the round trip.
func TestPrunedStrategicPatchConvertsNestedFieldsV1(t *testing.T) {
	pod := testPod()
	pod.Spec.Volumes = []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{
		VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{ObjectMeta: metav1.ObjectMeta{ManagedFields: []metav1.ManagedFieldsEntry{{
			Manager:  "m",
			FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:spec":{},"f:metadata":{}}`)},
		}}}},
	}}}}
	patch := []byte(`{"metadata":{"labels":{"a":"c"}}}`)
	patchMap, _, err := decodePatchMap(patch, "")
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := planPrunedPatch(pod, &corev1.Pod{}, patchMap)
	if !ok {
		t.Fatal("expected a pruned plan")
	}
	if !plan.named[2] {
		t.Error("spec carries a nested fieldsV1 and must take the round trip")
	}
	if plan.named[3] {
		t.Error("status should not take the round trip")
	}
	want := runPatch(t, false, pod, patch, "")
	got := runPatch(t, true, pod, patch, "")
	if !reflect.DeepEqual(want.obj.(*corev1.Pod).Spec, got.obj.(*corev1.Pod).Spec) {
		t.Errorf("spec mismatch: %s", cmp.Diff(want.obj.(*corev1.Pod).Spec, got.obj.(*corev1.Pod).Spec))
	}
}

func TestPrunedStrategicPatchMatchesUnprunedFuzzed(t *testing.T) {
	scheme := clientgoscheme.Scheme
	codecs := serializer.NewCodecFactory(scheme)
	proto := protobuf.NewSerializer(scheme, scheme)
	seed := rand.Int63()
	t.Logf("seed %d", seed)
	r := rand.New(rand.NewSource(seed))
	f := fuzzer.FuzzerFor(fuzzer.MergeFuzzerFuncs(metafuzzer.Funcs, func(serializer.CodecFactory) []interface{} {
		return []interface{}{
			func(j *metav1.ManagedFieldsEntry, c randfill.Continue) {
				c.FillNoCustom(j)
				j.FieldsType = "FieldsV1"
				// Keys out of sorted order, as the field manager writes keyed list items.
				j.FieldsV1 = &metav1.FieldsV1{Raw: []byte(fmt.Sprintf(`{"f:spec":{".":{},"f:ports":{"k:{\"port\":80}":{},"k:{\"port\":443}":{}}},"f:metadata":{"f:labels":{"f:%s":{}}}}`, randomLabelValue(c.Rand)))}
			},
		}
	}), rand.NewSource(seed), codecs)

	gvks := []schema.GroupVersionKind{}
	for gvk, typ := range scheme.AllKnownTypes() {
		if gvk.Version == runtime.APIVersionInternal || fieldsOf(typ) == nil {
			continue
		}
		if _, ok := reflect.New(typ).Interface().(interface{ Marshal() ([]byte, error) }); !ok {
			continue
		}
		gvks = append(gvks, gvk)
	}
	sort.Slice(gvks, func(i, j int) bool { return gvks[i].String() < gvks[j].String() })
	if len(gvks) < 100 {
		t.Fatalf("only %d prunable kinds found", len(gvks))
	}

	storedForm := func(gvk schema.GroupVersionKind) runtime.Object {
		obj, err := scheme.New(gvk)
		if err != nil {
			t.Fatal(err)
		}
		f.Fill(obj)
		obj.GetObjectKind().SetGroupVersionKind(gvk)
		data, err := runtime.Encode(proto, obj)
		if err != nil {
			t.Fatalf("%v: %v", gvk, err)
		}
		out, err := scheme.New(gvk)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := proto.Decode(data, &gvk, out); err != nil {
			t.Fatalf("%v: %v", gvk, err)
		}
		out.GetObjectKind().SetGroupVersionKind(gvk)
		return out
	}

	prunedCount := 0
	for _, gvk := range gvks {
		for i := 0; i < 5; i++ {
			original, modified := storedForm(gvk), storedForm(gvk)
			originalJSON, err := json.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			// Carry a random subset of top-level fields over from modified, and
			// always keep the identity so the diff reads like a real patch.
			var originalMap, modifiedMap map[string]interface{}
			modifiedJSON, _ := json.Marshal(modified)
			_ = json.Unmarshal(originalJSON, &originalMap)
			_ = json.Unmarshal(modifiedJSON, &modifiedMap)
			target := map[string]interface{}{}
			for k, v := range originalMap {
				target[k] = v
			}
			for k, v := range modifiedMap {
				if r.Intn(2) == 0 {
					target[k] = v
				}
			}
			if r.Intn(2) == 0 {
				if md, ok := target["metadata"].(map[string]interface{}); ok {
					md = copyMap(md)
					if omd, ok := originalMap["metadata"].(map[string]interface{}); ok {
						md["managedFields"] = omd["managedFields"]
					}
					target["metadata"] = md
				}
			}
			targetJSON, _ := json.Marshal(target)
			patch, err := strategicpatch.CreateTwoWayMergePatch(originalJSON, targetJSON, original)
			if err != nil {
				continue
			}
			for _, directive := range []string{"", metav1.FieldValidationStrict} {
				if comparePatchOutcomes(t, original, patch, directive) {
					prunedCount++
				}
			}
		}
	}
	t.Logf("%d kinds, %d pruned patches compared", len(gvks), prunedCount)
	if prunedCount == 0 {
		t.Fatal("no patch took the pruned path")
	}
}

func copyMap(m map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func randomLabelValue(r *rand.Rand) string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, 1+r.Intn(8))
	for i := range b {
		b[i] = letters[r.Intn(len(letters))]
	}
	return string(b)
}
