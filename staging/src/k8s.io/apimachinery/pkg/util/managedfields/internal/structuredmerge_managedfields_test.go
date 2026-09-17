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

package internal_test

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/structured-merge-diff/v7/fieldpath"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/managedfields/internal"
	internaltesting "k8s.io/apimachinery/pkg/util/managedfields/internal/testing"
)

// Typed on purpose: an unstructured live object arrives at ObjectToTyped with
// its fieldsV1 already decoded, so it never pays the reparse this change
// removes. apimachinery cannot import k8s.io/api, and metadata is where
// managedFields, the schema and the cost all live.
type mfPod struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
}

func (p *mfPod) DeepCopyObject() runtime.Object {
	out := &mfPod{TypeMeta: p.TypeMeta}
	p.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	return out
}

var mfPodGV = schema.GroupVersion{Group: "", Version: "v1"}

func newMFPod() *mfPod {
	return &mfPod{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}}
}

// The encoded metav1.ManagedFieldsEntry form is unusable for this: its
// timestamps differ between two runs of the same scenario.
func dumpManaged(t *testing.T, managed fieldpath.ManagedFields) string {
	t.Helper()
	names := make([]string, 0, len(managed))
	for name := range managed {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		versioned := managed[name]
		encoded, err := versioned.Set().ToJSON()
		if err != nil {
			t.Fatalf("encode set for %q: %v", name, err)
		}
		fmt.Fprintf(&b, "%s|%s|%t|%s\n", name, versioned.APIVersion(), versioned.Applied(), encoded)
	}
	return b.String()
}

func dumpObject(t *testing.T, obj runtime.Object) string {
	t.Helper()
	b, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

type mfStep struct {
	manager string
	obj     *mfPod
}

// The wrapping mirrors the real chain: stripMetaManager sits directly above
// structuredMergeManager, buildManagerInfoManager builds manager identifiers.
// Each iteration does what FieldManager.Update does around the call.
func runUpdates(t *testing.T, initialLive *mfPod, steps []mfStep) (managedDump string, liveDump string) {
	t.Helper()
	smm, err := internal.NewStructuredMergeManager(
		fakeTypeConverter,
		&internaltesting.FakeObjectConvertor{},
		&internaltesting.FakeObjectDefaulter{},
		mfPodGV, mfPodGV, nil,
	)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	manager := internal.NewBuildManagerInfoManager(internal.NewStripMetaManager(smm), mfPodGV, "")

	live := initialLive.DeepCopyObject().(*mfPod)
	var managed internal.ManagedInterface
	for i, s := range steps {
		managed, err = internal.DecodeManagedFields(live.ManagedFields)
		if err != nil {
			t.Fatalf("step %d: decode managed fields: %v", i, err)
		}
		newObj := s.obj.DeepCopyObject().(*mfPod)
		newObj.ManagedFields = nil

		out, updated, err := manager.Update(live, newObj, managed, s.manager)
		if err != nil {
			t.Fatalf("step %d (manager %q): %v", i, s.manager, err)
		}
		managed = updated
		if err := internal.EncodeObjectManagedFields(out, managed); err != nil {
			t.Fatalf("step %d: encode managed fields: %v", i, err)
		}
		live = out.(*mfPod)
	}
	if managed == nil {
		t.Fatal("no steps were run")
	}
	return dumpManaged(t, managed.Fields()), dumpObject(t, live)
}

func mfPodWith(labels, annotations map[string]string) *mfPod {
	p := newMFPod()
	p.Name = "p"
	p.Labels = labels
	p.Annotations = annotations
	return p
}

// Every step submits a COMPLETE object, the way an update request does, so a
// step that omits a field deletes it and drops its ownership. These fixtures
// are cumulative for that reason.
var (
	mfBase          = func() *mfPod { return mfPodWith(map[string]string{"a": "1"}, nil) }
	mfBaseAnnotated = func() *mfPod {
		return mfPodWith(map[string]string{"a": "1"}, map[string]string{"note": "x"})
	}
	mfGrown = func() *mfPod {
		return mfPodWith(map[string]string{"a": "1", "b": "2"}, map[string]string{"note": "y"})
	}
	mfShrunk = func() *mfPod { return mfPodWith(map[string]string{"a": "1"}, nil) }
)

// A managedFields entry claiming metadata.managedFields, which only an older
// server writes. This is the one case where excluding managedFields from the
// live conversion changes what the merge sees.
func mfLegacyLive() *mfPod {
	p := mfBase()
	p.ManagedFields = []metav1.ManagedFieldsEntry{{
		Manager:    "legacy",
		Operation:  metav1.ManagedFieldsOperationUpdate,
		APIVersion: "v1",
		FieldsType: "FieldsV1",
		FieldsV1: &metav1.FieldsV1{
			Raw: []byte(`{"f:metadata":{"f:managedFields":{},"f:labels":{"f:a":{}}}}`),
		},
	}}
	return p
}

var mfScenarios = []struct {
	name  string
	live  func() *mfPod
	steps []mfStep
}{
	{
		name:  "single update",
		steps: []mfStep{{"a", mfBase()}},
	},
	{
		name:  "two managers, disjoint fields",
		steps: []mfStep{{"a", mfBase()}, {"b", mfBaseAnnotated()}},
	},
	{
		name:  "same manager updates twice, growing the object",
		steps: []mfStep{{"a", mfBase()}, {"a", mfGrown()}},
	},
	{
		name:  "second manager takes over a field the first owned",
		steps: []mfStep{{"a", mfBase()}, {"b", mfGrown()}},
	},
	{
		name:  "a field is removed, ownership must be dropped",
		steps: []mfStep{{"a", mfGrown()}, {"a", mfShrunk()}},
	},
	{
		name:  "three managers interleaved",
		steps: []mfStep{{"a", mfBase()}, {"b", mfBaseAnnotated()}, {"c", mfGrown()}, {"a", mfGrown()}},
	},
}

// The load-bearing test: the managed set and the live object must come out
// identical whether or not managedFields are excluded.
func TestStructuredMergeManagerUpdateManagedFieldsDifferential(t *testing.T) {
	for _, sc := range mfScenarios {
		t.Run(sc.name, func(t *testing.T) {
			live := newMFPod()
			if sc.live != nil {
				live = sc.live()
			}

			restore := internal.SetStripLiveManagedFieldsForUpdate(false)
			stockManaged, stockLive := runUpdates(t, live, sc.steps)
			restore()

			restore = internal.SetStripLiveManagedFieldsForUpdate(true)
			newManaged, newLive := runUpdates(t, live, sc.steps)
			restore()

			if stockManaged != newManaged {
				t.Errorf("managedFields differ\n stock:\n%s new:\n%s", stockManaged, newManaged)
			}
			if stockLive != newLive {
				t.Errorf("live object differs\n stock: %s\n new:   %s", stockLive, newLive)
			}
		})
	}
}

// The one divergence. Stock converts the live managedFields, so the merge sees
// metadata.managedFields as removed and strips the claim from its owner.
// Nothing has written such a claim since stripMetaManager landed in v1.17, so
// only an object untouched in etcd since v1.16 can carry one, and it now
// survives instead.
func TestStructuredMergeManagerUpdateKeepsLegacyManagedFieldsOwnership(t *testing.T) {
	const legacy = `{"manager":"legacy","operation":"Update","apiVersion":"v1"}`
	steps := []mfStep{{"b", mfGrown()}}

	restore := internal.SetStripLiveManagedFieldsForUpdate(false)
	stock, _ := runUpdates(t, mfLegacyLive(), steps)
	restore()

	restore = internal.SetStripLiveManagedFieldsForUpdate(true)
	got, _ := runUpdates(t, mfLegacyLive(), steps)
	restore()

	if strings.Contains(stock, `"f:managedFields"`) {
		t.Errorf("stock was expected to drop the claim, got:\n%s", stock)
	}
	if !strings.Contains(got, `"f:managedFields"`) {
		t.Errorf("the claim was expected to survive, got:\n%s", got)
	}
	if !strings.Contains(got, legacy) {
		t.Errorf("the legacy manager disappeared, got:\n%s", got)
	}
}

// Proves the comparison above discriminates, and that the seam it depends on
// is reached rather than passing vacuously.
func TestStructuredMergeManagerUpdateDifferentialDetectsBreakage(t *testing.T) {
	base := []mfStep{{"a", mfBase()}, {"b", mfBaseAnnotated()}}

	t.Run("a different manager name is caught", func(t *testing.T) {
		got, _ := runUpdates(t, newMFPod(), base)
		other, _ := runUpdates(t, newMFPod(), []mfStep{{"a", mfBase()}, {"different", mfBaseAnnotated()}})
		if got == other {
			t.Fatal("comparison cannot tell two different managed sets apart")
		}
	})

	t.Run("a different field set is caught", func(t *testing.T) {
		got, _ := runUpdates(t, newMFPod(), base)
		other, _ := runUpdates(t, newMFPod(), []mfStep{{"a", mfBase()}, {"b", mfGrown()}})
		if got == other {
			t.Fatal("comparison cannot tell two different field sets apart")
		}
	})

	t.Run("the exclusion applies to the typed live object", func(t *testing.T) {
		live := mfLegacyLive()
		stripped, ok := internal.ShallowCopyWithoutManagedFields(live)
		if !ok {
			t.Fatal("shallowCopyWithoutManagedFields declined a typed pod with managedFields")
		}
		if got := stripped.(*mfPod).ManagedFields; got != nil {
			t.Errorf("copy still carries managedFields: %v", got)
		}
		if len(live.ManagedFields) != 1 {
			t.Errorf("the original lost its managedFields: %v", live.ManagedFields)
		}
		if stripped.(*mfPod).Labels == nil {
			t.Error("the copy did not carry the rest of the metadata over")
		}
	})

	t.Run("the exclusion refuses an unstructured live object", func(t *testing.T) {
		// A shallow copy of an Unstructured shares the metadata map, so
		// clearing managedFields on it would clear them on the live object.
		u := &unstructured.Unstructured{}
		if err := json.Unmarshal([]byte(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p","managedFields":[{"manager":"legacy","operation":"Update","apiVersion":"v1","fieldsType":"FieldsV1","fieldsV1":{"f:metadata":{"f:labels":{}}}}]}}`), &u.Object); err != nil {
			t.Fatal(err)
		}
		if _, ok := internal.ShallowCopyWithoutManagedFields(u); ok {
			t.Fatal("shallowCopyWithoutManagedFields accepted an unstructured object")
		}
		if len(u.GetManagedFields()) != 1 {
			t.Fatalf("the unstructured live object lost its managedFields: %v", u.GetManagedFields())
		}
	})
}

func loadBenchPod(b *testing.B) *mfPod {
	b.Helper()
	raw, err := os.ReadFile("testdata/bench-pod.json")
	if err != nil {
		b.Fatal(err)
	}
	pod := &mfPod{}
	if err := json.Unmarshal(raw, pod); err != nil {
		b.Fatal(err)
	}
	pod.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
	if len(pod.ManagedFields) != 2 {
		b.Fatalf("fixture must carry 2 managedFields entries, got %d", len(pod.ManagedFields))
	}
	sizes := []int{len(pod.ManagedFields[0].FieldsV1.Raw), len(pod.ManagedFields[1].FieldsV1.Raw)}
	if sizes[0] != 3715 || sizes[1] != 174 {
		b.Fatalf("fixture fieldsV1 sizes are %v, the measured pod had [3715 174]", sizes)
	}
	return pod
}

// The live object is taken off the write-throughput rig: 2 managedFields
// entries, fieldsV1 of 3,715 B and 174 B.
func benchmarkUpdate(b *testing.B, strip bool, liveHasManagedFields bool) {
	restore := internal.SetStripLiveManagedFieldsForUpdate(strip)
	defer restore()

	live := loadBenchPod(b)
	if !liveHasManagedFields {
		live.ManagedFields = nil
	}

	newObj := live.DeepCopyObject().(*mfPod)
	newObj.ManagedFields = nil
	newObj.Labels["track"] = "stable"

	manager, err := internal.NewStructuredMergeManager(
		fakeTypeConverter,
		&internaltesting.FakeObjectConvertor{},
		&internaltesting.FakeObjectDefaulter{},
		mfPodGV, mfPodGV, nil,
	)
	if err != nil {
		b.Fatal(err)
	}
	managed, err := internal.DecodeManagedFields(live.ManagedFields)
	if err != nil {
		b.Fatal(err)
	}
	if _, _, err := manager.Update(live, newObj.DeepCopyObject(), managed, "bench"); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		managed, err := internal.DecodeManagedFields(live.ManagedFields)
		if err != nil {
			b.Fatal(err)
		}
		if _, _, err := manager.Update(live, newObj.DeepCopyObject(), managed, "bench"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStructuredMergeUpdateStock(b *testing.B) {
	benchmarkUpdate(b, false, true)
}

func BenchmarkStructuredMergeUpdateStrippedLive(b *testing.B) {
	benchmarkUpdate(b, true, true)
}

// Control: with no managedFields on the live object the two settings do
// identical work, so these rows must not move apart.
func BenchmarkStructuredMergeUpdateControlStock(b *testing.B) {
	benchmarkUpdate(b, false, false)
}

func BenchmarkStructuredMergeUpdateControlStripped(b *testing.B) {
	benchmarkUpdate(b, true, false)
}
