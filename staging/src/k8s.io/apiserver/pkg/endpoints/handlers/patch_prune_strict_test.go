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
	"fmt"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/warning"

	kjson "sigs.k8s.io/json"
)

// recordingWarnings captures everything addStrictDecodingWarnings emits, in order.
type recordingWarnings struct{ got []string }

func (r *recordingWarnings) AddWarning(agent, text string) {
	r.got = append(r.got, agent+"|"+text)
}

func withWarnings(t *testing.T) (context.Context, *recordingWarnings) {
	t.Helper()
	rec := &recordingWarnings{}
	return warning.WithWarningRecorder(context.Background(), rec), rec
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// TestPrunedStrictMatchesStock asserts the pruned path emits the same warning set and
// returns the same error as stock under Warn and Strict, not merely the same object.
func TestPrunedStrictMatchesStock(t *testing.T) {
	cases := []struct {
		name string
		// wantPruned: a case that silently falls back to the implementation it is
		// compared against proves nothing, so it is asserted rather than observed.
		wantPruned bool
		patch      string
		// wantUnknown: a case reporting no unknown field makes the comparison vacuous.
		wantUnknown bool
	}{
		{name: "clean status patch", wantPruned: true, patch: `{"status":{"phase":"Running"}}`},
		{name: "clean cl2 condition patch", wantPruned: true, patch: `{"status":{"conditions":[{"type":"Ready","status":"False","reason":"Benchmark","message":"1756312345678901234"}]}}`},

		// Unknown field one level under a top-level key the patch names.
		{name: "unknown under named status", wantPruned: true, wantUnknown: true, patch: `{"status":{"bogusStatusField":"x"}}`},
		{name: "unknown under named spec", wantPruned: true, wantUnknown: true, patch: `{"spec":{"bogusSpecField":1}}`},
		{name: "unknown under named metadata", wantPruned: true, wantUnknown: true, patch: `{"metadata":{"bogusMetaField":true}}`},

		// Unknown field deep inside a merged list element.
		{name: "unknown in container", wantPruned: true, wantUnknown: true, patch: `{"spec":{"containers":[{"name":"main","bogusContainerField":"x"}]}}`},
		{name: "unknown in new container", wantPruned: true, wantUnknown: true, patch: `{"spec":{"containers":[{"name":"extra","image":"i","bogusContainerField":"x"}]}}`},

		// Several at once, to catch an ordering or dedup difference in the error set.
		{name: "unknown in two named keys", wantPruned: true, wantUnknown: true, patch: `{"spec":{"bogusA":1},"status":{"bogusB":2}}`},

		// managedFields is the subtree the fast path most often skips, so when a patch
		// does name it the reporting inside it must still match.
		{name: "managedFields clean", wantPruned: true, patch: `{"metadata":{"managedFields":[{"manager":"kubelet","operation":"Update","apiVersion":"v1","fieldsType":"FieldsV1","fieldsV1":{"f:status":{"f:phase":{}}}}]}}`},
		{name: "unknown in managedFields entry", wantPruned: true, wantUnknown: true, patch: `{"metadata":{"managedFields":[{"manager":"kubelet","bogusEntryField":"x"}]}}`},
		// fieldsV1 has its own UnmarshalJSON, so the converter hands it the raw value
		// and never walks it. Neither path may report anything from inside it.
		{name: "arbitrary keys inside fieldsV1", wantPruned: true, patch: `{"metadata":{"managedFields":[{"manager":"kubelet","fieldsV1":{"f:anythingGoesHere":{"k:{\"x\":1}":{}}}}]}}`},

		// A top-level key that is not a Pod field at all: the fast path must decline,
		// since it is the one unknown field the pruned map could not have seen.
		{name: "unknown top-level key", wantPruned: false, wantUnknown: true, patch: `{"bogusTopLevel":{"a":1},"status":{"phase":"Running"}}`},

		// A duplicate key never reaches the merged map, so it exercises the strictErrs
		// plumbing rather than the converter's unknown-field walk.
		{name: "duplicate key in patch", wantPruned: true, patch: `{"status":{"phase":"Running","phase":"Pending"}}`},
		{name: "duplicate key plus unknown", wantPruned: true, wantUnknown: true, patch: `{"status":{"bogusC":1,"bogusC":2}}`},

		// A merge error must surface identically.
		{name: "malformed list merge", wantPruned: true, patch: `{"spec":{"containers":{"not":"a list"}}}`},
	}

	directives := []string{metav1.FieldValidationWarn, metav1.FieldValidationStrict}

	// The fixture is the storage-decoded CL2 pod, so every subtree the pruned path omits
	// under a status-only patch is non-trivial. A typed Pod cannot carry an unknown field
	// under an unnamed key at all; see TestPrunedStrictAsymmetricMarshalerDiverges.
	ran := 0
	for _, tc := range cases {
		for _, directive := range directives {
			t.Run(tc.name+"/"+directive, func(t *testing.T) {
				ran++
				t.Logf("ran %s under %s", tc.name, directive)

				stockCtx, stockWarn := withWarnings(t)
				prunedCtx, prunedWarn := withWarnings(t)

				stockOrig, prunedOrig := storedCL2Pod(), storedCL2Pod()
				stockOut, prunedOut := &corev1.Pod{}, &corev1.Pod{}
				stockDef, prunedDef := &countingDefaulter{}, &countingDefaulter{}

				stockErr := stockStrategicPatchObject(stockCtx, stockDef, stockOrig, []byte(tc.patch), stockOut, &corev1.Pod{}, directive)
				prunedErr := strategicPatchObject(prunedCtx, prunedDef, prunedOrig, []byte(tc.patch), prunedOut, &corev1.Pod{}, directive)

				// Assert the fast path fired (or declined) before comparing anything,
				// so no case can pass by falling back.
				patchMap := map[string]interface{}{}
				if _, err := kjson.UnmarshalStrict([]byte(tc.patch), &patchMap); err != nil {
					t.Fatalf("unmarshal patch: %v", err)
				}
				gotPruned, _ := prunedStrategicPatchObject(context.Background(), &countingDefaulter{}, storedCL2Pod(), patchMap, &corev1.Pod{}, &corev1.Pod{}, nil, directive)
				if gotPruned != tc.wantPruned {
					t.Fatalf("fast path fired = %v, want %v", gotPruned, tc.wantPruned)
				}

				if errString(stockErr) != errString(prunedErr) {
					t.Errorf("error mismatch\n stock: %s\npruned: %s", errString(stockErr), errString(prunedErr))
				}
				if !reflect.DeepEqual(stockWarn.got, prunedWarn.got) {
					t.Errorf("warning mismatch\n stock: %#v\npruned: %#v", stockWarn.got, prunedWarn.got)
				}

				// Or the comparison above is between two empty sets.
				sawUnknown := len(stockWarn.got) > 0 || (stockErr != nil && directive == metav1.FieldValidationStrict)
				if tc.wantUnknown && !sawUnknown {
					t.Errorf("case reported nothing under %s: warnings=%v err=%v", directive, stockWarn.got, stockErr)
				}

				if stockErr != nil {
					return
				}
				if stockDef.n != prunedDef.n {
					t.Errorf("defaulter call count: stock=%d pruned=%d", stockDef.n, prunedDef.n)
				}
				if !reflect.DeepEqual(stockOut, prunedOut) {
					t.Errorf("object mismatch\n stock: %#v\npruned: %#v", stockOut, prunedOut)
				}
				if !apiequality.Semantic.DeepEqual(prunedOrig, storedCL2Pod()) {
					t.Errorf("pruned path mutated the original object")
				}
			})
		}
	}

	if want := len(cases) * len(directives); ran != want {
		t.Fatalf("ran %d subtests, want %d -- the table did not execute", ran, want)
	}
	t.Logf("executed %d subtests across %d patches x %d directives", ran, len(cases), len(directives))
}

// ghostMarshaler serialises a key that has no field to decode back into. It is the only
// shape that can put an unknown field under a top-level key the patch does not name.
// Nothing in core/v1 does this.
type ghostMarshaler struct {
	Known string `json:"known,omitempty"`
}

func (g ghostMarshaler) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf(`{"known":%q,"ghost":"seen"}`, g.Known)), nil
}

// Untouched must be a pointer with omitempty, or the zeroed scratch object still
// serialises it and the pruned map is not actually pruned.
type ghostRoot struct {
	metav1.TypeMeta `json:",inline"`
	Touched         string          `json:"touched,omitempty"`
	Untouched       *ghostMarshaler `json:"untouched,omitempty"`
}

func (g *ghostRoot) GetObjectKind() schema.ObjectKind { return &g.TypeMeta }
func (g *ghostRoot) DeepCopyObject() runtime.Object   { out := *g; return &out }

// TestPrunedStrictAsymmetricMarshalerDiverges pins the one shape that breaks the pruned
// path's unknown-field equivalence. It asserts the divergence, not equality: the guard
// against it is k8s.io/api's TestMarshalerUnmarshalerSymmetry, which fails if a built-in
// kind ever acquires this shape.
func TestPrunedStrictAsymmetricMarshalerDiverges(t *testing.T) {
	patch := `{"touched":"x"}`
	ran := 0
	for _, directive := range []string{metav1.FieldValidationWarn, metav1.FieldValidationStrict} {
		t.Run(directive, func(t *testing.T) {
			ran++
			stockCtx, stockWarn := withWarnings(t)
			prunedCtx, prunedWarn := withWarnings(t)

			mk := func() *ghostRoot { return &ghostRoot{Untouched: &ghostMarshaler{Known: "k"}} }

			// Guard against the vacuous form of this test: the pruned map must
			// really omit the untouched key, or nothing below is exercised.
			scratch := &ghostRoot{}
			partial, err := runtime.DefaultUnstructuredConverter.ToUnstructured(scratch)
			if err != nil {
				t.Fatalf("scratch conversion: %v", err)
			}
			if _, present := partial["untouched"]; present {
				t.Fatalf("pruned map still carries the untouched key; the case proves nothing")
			}

			stockOut, prunedOut := &ghostRoot{}, &ghostRoot{}
			stockErr := stockStrategicPatchObject(stockCtx, &countingDefaulter{}, mk(), []byte(patch), stockOut, &ghostRoot{}, directive)
			prunedErr := strategicPatchObject(prunedCtx, &countingDefaulter{}, mk(), []byte(patch), prunedOut, &ghostRoot{}, directive)

			patchMap := map[string]interface{}{}
			if _, err := kjson.UnmarshalStrict([]byte(patch), &patchMap); err != nil {
				t.Fatalf("unmarshal patch: %v", err)
			}
			gotPruned, _ := prunedStrategicPatchObject(context.Background(), &countingDefaulter{}, mk(), patchMap, &ghostRoot{}, &ghostRoot{}, nil, directive)
			if !gotPruned {
				t.Fatalf("fast path declined; this test says nothing unless it fires")
			}

			t.Logf("%s: stock err=%s warnings=%v", directive, errString(stockErr), stockWarn.got)
			t.Logf("%s: pruned err=%s warnings=%v", directive, errString(prunedErr), prunedWarn.got)

			// Stock sees the ghost key because it walks the whole original; the
			// pruned path never converts the untouched field, so it cannot.
			const ghost = `unknown field "untouched.ghost"`
			switch directive {
			case metav1.FieldValidationWarn:
				if want := []string{"|" + ghost}; !reflect.DeepEqual(stockWarn.got, want) {
					t.Fatalf("stock warnings = %#v, want %#v", stockWarn.got, want)
				}
				if len(prunedWarn.got) != 0 {
					t.Fatalf("pruned warnings = %#v, want none", prunedWarn.got)
				}
				if stockErr != nil || prunedErr != nil {
					t.Fatalf("unexpected errors: stock=%v pruned=%v", stockErr, prunedErr)
				}
			case metav1.FieldValidationStrict:
				if stockErr == nil || !strings.Contains(stockErr.Error(), ghost) {
					t.Fatalf("stock err = %s, want it to name the ghost field", errString(stockErr))
				}
				if prunedErr != nil {
					t.Fatalf("pruned err = %s, want nil", errString(prunedErr))
				}
			}
		})
	}
	if ran != 2 {
		t.Fatalf("ran %d subtests, want 2", ran)
	}
}
