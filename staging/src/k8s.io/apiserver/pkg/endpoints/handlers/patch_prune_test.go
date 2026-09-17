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
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	kjson "sigs.k8s.io/json"
)

// stockStrategicPatchObject is the unpruned implementation, kept verbatim so the
// pruned path is compared against a fixed reference rather than a moving target.
func stockStrategicPatchObject(
	requestContext context.Context,
	defaulter runtime.ObjectDefaulter,
	originalObject runtime.Object,
	patchBytes []byte,
	objToUpdate runtime.Object,
	schemaReferenceObj runtime.Object,
	validationDirective string,
) error {
	originalObjMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(originalObject)
	if err != nil {
		return err
	}

	patchMap := make(map[string]interface{})
	var strictErrs []error
	if validationDirective == metav1.FieldValidationWarn || validationDirective == metav1.FieldValidationStrict {
		strictErrs, err = kjson.UnmarshalStrict(patchBytes, &patchMap)
		if err != nil {
			return err
		}
	} else {
		if err = kjson.UnmarshalCaseSensitivePreserveInts(patchBytes, &patchMap); err != nil {
			return err
		}
	}

	return applyPatchToObject(requestContext, defaulter, originalObjMap, patchMap, objToUpdate, schemaReferenceObj, strictErrs, validationDirective)
}

// countingDefaulter records how many times each path defaulted, without changing the
// object. Both paths call Default last, so objects equal before that call stay equal for
// any deterministic defaulter. Staging cannot import the real core/v1 defaulters.
type countingDefaulter struct{ n int }

func (d *countingDefaulter) Default(_ runtime.Object) { d.n++ }

func qty(s string) resource.Quantity { return resource.MustParse(s) }

// cl2Pod is the write-throughput benchmark's pod template, rendered.
// perf-tests clusterloader2/testing/write-throughput/modules/initial-pods/pod.yaml
func cl2Pod() *corev1.Pod {
	env := func(container string) []corev1.EnvVar {
		return []corev1.EnvVar{
			{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
			{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
			{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}},
			{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
			{Name: "GOMAXPROCS", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{ContainerName: container, Resource: "limits.cpu"}}},
			{Name: "GOMEMLIMIT", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{ContainerName: container, Resource: "limits.memory"}}},
			{Name: "JAVA_TOOL_OPTIONS", Value: "-XX:MaxRAMPercentage=75.0"},
			{Name: "REDIS_URL", Value: "redis://main:6379/0"},
			{Name: "PYTHONUNBUFFERED", Value: "1"},
			{Name: "NODE_ENV", Value: "prod"},
		}
	}
	res := corev1.ResourceRequirements{Requests: corev1.ResourceList{
		corev1.ResourceCPU:    qty("5m"),
		corev1.ResourceMemory: qty("20M"),
	}}
	optional := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bench-pod-1",
			Namespace: "bench",
			UID:       "8f3a1c2e-0000-4000-8000-000000000001",
			Labels: map[string]string{
				"group":                       "bench-pod",
				"app.kubernetes.io/name":      "payment-service",
				"app.kubernetes.io/instance":  "payment-service-primary",
				"app.kubernetes.io/version":   "v2.1.4",
				"app.kubernetes.io/component": "backend",
				"app.kubernetes.io/part-of":   "ecommerce-platform",
				"env":                         "production",
				"track":                       "canary",
			},
			Annotations: map[string]string{
				"prometheus.io/scrape":                           "false",
				"prometheus.io/port":                             "8080",
				"prometheus.io/path":                             "/metrics",
				"sidecar.istio.io/inject":                        "false",
				"cluster-autoscaler.kubernetes.io/safe-to-evict": "false",
				"vault.hashicorp.com/agent-inject":               "false",
				"vault.hashicorp.com/role":                       "my-app-db-role",
				"fluentbit.io/parser":                            "json",
				"argocd.argoproj.io/hook":                        "PreSync",
				"argocd.argoproj.io/hook-delete-policy":          "HookSucceeded",
			},
			ManagedFields: []metav1.ManagedFieldsEntry{{
				Manager:    "clusterloader2",
				Operation:  metav1.ManagedFieldsOperationUpdate,
				APIVersion: "v1",
				FieldsType: "FieldsV1",
				FieldsV1:   fieldsV1(`{"f:spec":{"f:schedulerName":{}}}`),
			}},
		},
		Spec: corev1.PodSpec{
			SchedulerName: "non-existent-scheduler",
			NodeSelector:  map[string]string{"kubernetes.io/hostname": "non-existent-node"},
			InitContainers: []corev1.Container{
				{Name: "init-0", Image: "registry/e2e-test-images/agnhost:2.53", Command: []string{"/bin/sh", "-c", "sleep 1"}},
				{Name: "init-1", Image: "registry/e2e-test-images/agnhost:2.53", Command: []string{"/bin/sh", "-c", "sleep 1"}},
			},
			Containers: []corev1.Container{
				{
					Name:      "main",
					Image:     "registry/pause:3.9",
					Env:       env("main"),
					Resources: res,
					VolumeMounts: []corev1.VolumeMount{
						{MountPath: "/var/configmap", Name: "configmap"},
						{MountPath: "/var/secret", Name: "secret"},
					},
				},
				{Name: "sidecar", Image: "registry/pause:3.9", Env: env("sidecar"), Resources: res},
			},
			Volumes: []corev1.Volume{
				{Name: "configmap", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "bench-pod-1"}, Optional: &optional}}},
				{Name: "secret", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
					SecretName: "bench-pod-1", Optional: &optional}}},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable"},
			},
		},
	}
}

func TestPrunedStrategicPatchObjectMatchesStock(t *testing.T) {
	cases := []struct {
		name string
		// wantPruned is whether the fast path must fire. A case that silently falls
		// back proves nothing, so this is asserted rather than observed.
		wantPruned bool
		directive  string
		patch      string
	}{
		// The clusterloader2 write-throughput patch, the workload this targets.
		{name: "cl2 status condition", wantPruned: true, patch: `{"status":{"conditions":[{"type":"Ready","status":"False","reason":"Benchmark","message":"1756312345678901234"}]}}`},

		{name: "status phase scalar", wantPruned: true, patch: `{"status":{"phase":"Running"}}`},
		{name: "status replace existing condition", wantPruned: true, patch: `{"status":{"conditions":[{"type":"PodScheduled","status":"True","reason":"Scheduled"}]}}`},
		{name: "labels add", wantPruned: true, patch: `{"metadata":{"labels":{"bench-updated":"1756312345678901234"}}}`},
		{name: "labels delete via null", wantPruned: true, patch: `{"metadata":{"labels":{"track":null}}}`},
		{name: "annotations delete via null", wantPruned: true, patch: `{"metadata":{"annotations":{"fluentbit.io/parser":null}}}`},
		{name: "metadata and status together", wantPruned: true, patch: `{"metadata":{"labels":{"x":"y"}},"status":{"phase":"Running"}}`},
		{name: "spec container merge by mergeKey", wantPruned: true, patch: `{"spec":{"containers":[{"name":"sidecar","image":"registry/pause:3.10"}]}}`},
		{name: "spec new container", wantPruned: true, patch: `{"spec":{"containers":[{"name":"extra","image":"registry/pause:3.9"}]}}`},
		{name: "spec scalar", wantPruned: true, patch: `{"spec":{"schedulerName":"other-scheduler"}}`},
		{name: "spec nodeSelector map", wantPruned: true, patch: `{"spec":{"nodeSelector":{"kubernetes.io/os":"linux"}}}`},
		{name: "spec resources quantity", wantPruned: true, patch: `{"spec":{"containers":[{"name":"main","resources":{"requests":{"cpu":"10m"}}}]}}`},
		{name: "empty patch", wantPruned: true, patch: `{}`},
		{name: "key absent from original", wantPruned: true, patch: `{"status":{"podIP":"10.0.0.1"}}`},
		{name: "delete whole top-level key", wantPruned: true, patch: `{"status":null}`},
		{name: "delete metadata", wantPruned: true, patch: `{"metadata":null}`},
		{name: "delete spec", wantPruned: true, patch: `{"spec":null}`},
		{name: "all three top-level keys", wantPruned: true, patch: `{"metadata":{"labels":{"a":"b"}},"spec":{"schedulerName":"s"},"status":{"phase":"Running"}}`},

		// managedFields is the largest thing under metadata, so a patch naming metadata
		// drags it back onto the converted set.
		{name: "managedFields replaced", wantPruned: true, patch: `{"metadata":{"managedFields":[{"manager":"kubelet","operation":"Update","apiVersion":"v1","fieldsType":"FieldsV1","fieldsV1":{"f:status":{"f:phase":{}}}}]}}`},
		{name: "managedFields cleared", wantPruned: true, patch: `{"metadata":{"managedFields":null}}`},
		{name: "managedFields alongside status", wantPruned: true, patch: `{"metadata":{"managedFields":[{"manager":"kubelet","operation":"Update","apiVersion":"v1","fieldsType":"FieldsV1","fieldsV1":{"f:status":{"f:phase":{}}}}]},"status":{"phase":"Running"}}`},

		// The kubelet's own shape: names metadata and status, so only spec is pruned.
		{name: "kubelet shaped", wantPruned: true, patch: `{"metadata":{"annotations":{"kubernetes.io/config.seen":"2026-01-01T00:00:00Z"}},"status":{"phase":"Running","podIP":"10.0.0.1"}}`},

		// Directives below the top level stay on the fast path: they are handled inside
		// the implicated subtree, which the stock converter builds.
		{name: "nested $patch replace in status", wantPruned: true, patch: `{"status":{"conditions":[{"type":"Ready","status":"True"},{"$patch":"replace"}]}}`},
		{name: "nested $patch replace in spec", wantPruned: true, patch: `{"spec":{"containers":[{"name":"only","image":"i"},{"$patch":"replace"}]}}`},
		{name: "nested $retainKeys in spec", wantPruned: true, patch: `{"spec":{"$retainKeys":["containers"],"containers":[{"name":"main"}]}}`},
		{name: "nested $deleteFromPrimitiveList", wantPruned: true, patch: `{"spec":{"containers":[{"name":"main","$deleteFromPrimitiveList/command":["sleep"]}]}}`},
		{name: "nested $setElementOrder", wantPruned: true, patch: `{"spec":{"$setElementOrder/containers":[{"name":"sidecar"},{"name":"main"}],"containers":[{"name":"sidecar","image":"x"}]}}`},
		{name: "nested $patch delete in list", wantPruned: true, patch: `{"spec":{"containers":[{"name":"sidecar","$patch":"delete"}]}}`},

		// Top-level directives act on the whole object, so pruning must decline.
		{name: "top-level $patch replace", wantPruned: false, patch: `{"$patch":"replace","status":{"phase":"Running"}}`},
		{name: "top-level $patch delete", wantPruned: false, patch: `{"$patch":"delete"}`},
		{name: "top-level $retainKeys", wantPruned: false, patch: `{"$retainKeys":["metadata","spec"],"spec":{"schedulerName":"s"}}`},

		// apiVersion and kind live on the inlined TypeMeta, so they are not distinct
		// struct fields. Declining is required: writing only the fields we can name
		// would drop them.
		{name: "patch names apiVersion", wantPruned: false, patch: `{"apiVersion":"v1","status":{"phase":"Running"}}`},
		{name: "patch names kind", wantPruned: false, patch: `{"kind":"Pod","status":{"phase":"Running"}}`},
		{name: "patch names apiVersion and kind only", wantPruned: false, patch: `{"apiVersion":"v1","kind":"Pod"}`},

		// A prefixed directive names the field it acts on, so it keeps that field.
		{name: "top-level prefixed directive names a real field", wantPruned: true, patch: `{"$setElementOrder/status":{},"status":{"phase":"Running"}}`},

		// TestPrunedStrictMatchesStock covers the reporting these exist for.
		{name: "strict fires", wantPruned: true, directive: metav1.FieldValidationStrict, patch: `{"status":{"phase":"Running"}}`},
		{name: "warn fires", wantPruned: true, directive: metav1.FieldValidationWarn, patch: `{"status":{"phase":"Running"}}`},
	}

	// `stored` is the one that matters: the server only ever patches a storage-decoded
	// object. Against `pristine` the two paths agree semantically but not bitwise, see
	// TestPrunedStrategicPatchObjectQuantityArtifact.
	fixtures := map[string]struct {
		make   func() *corev1.Pod
		strict bool // require reflect.DeepEqual, not just Semantic
	}{
		"pristine": {make: cl2Pod, strict: false},
		"stored":   {make: storedCL2Pod, strict: true},
	}

	for _, tc := range cases {
		for fixName, fix := range fixtures {
			t.Run(tc.name+"/"+fixName, func(t *testing.T) {
				ctx := context.Background()
				directive := tc.directive
				if directive == "" {
					directive = metav1.FieldValidationIgnore
				}

				// Each path gets its own original, so a mutation by one cannot flatter
				// the other.
				stockOrig, prunedOrig := fix.make(), fix.make()
				stockOut, prunedOut := &corev1.Pod{}, &corev1.Pod{}
				stockDef, prunedDef := &countingDefaulter{}, &countingDefaulter{}

				stockErr := stockStrategicPatchObject(ctx, stockDef, stockOrig, []byte(tc.patch), stockOut, &corev1.Pod{}, directive)
				prunedErr := strategicPatchObject(ctx, prunedDef, prunedOrig, []byte(tc.patch), prunedOut, &corev1.Pod{}, directive)

				if (stockErr == nil) != (prunedErr == nil) {
					t.Fatalf("error mismatch: stock=%v pruned=%v", stockErr, prunedErr)
				}
				if stockErr != nil {
					return
				}

				// So no case passes by falling back to what it is compared against.
				var patchMap map[string]interface{}
				if err := kjson.UnmarshalCaseSensitivePreserveInts([]byte(tc.patch), &patchMap); err != nil {
					t.Fatalf("unmarshal patch: %v", err)
				}
				gotPruned, _ := prunedStrategicPatchObject(ctx, &countingDefaulter{}, fix.make(), patchMap, &corev1.Pod{}, &corev1.Pod{}, nil, directive)
				if gotPruned != tc.wantPruned {
					t.Errorf("fast path fired = %v, want %v", gotPruned, tc.wantPruned)
				}

				if stockDef.n != prunedDef.n {
					t.Errorf("defaulter call count: stock=%d pruned=%d", stockDef.n, prunedDef.n)
				}

				if !apiequality.Semantic.DeepEqual(stockOut, prunedOut) {
					t.Errorf("semantic mismatch\n stock: %#v\npruned: %#v", stockOut, prunedOut)
				}
				if fix.strict && !reflect.DeepEqual(stockOut, prunedOut) {
					t.Errorf("bitwise mismatch on a storage-decoded object\n stock: %#v\npruned: %#v", stockOut, prunedOut)
				}

				// Neither path may mutate the original.
				if !apiequality.Semantic.DeepEqual(prunedOrig, fix.make()) {
					t.Errorf("pruned path mutated the original object")
				}
				if !apiequality.Semantic.DeepEqual(stockOrig, fix.make()) {
					t.Errorf("stock path mutated the original object")
				}
			})
		}
	}
}

// fieldsV1 builds a FieldsV1 without naming any of its fields. The two build tag
// variants are not source compatible (Raw []byte by default, an unexported
// unique.Handle under -tags=fieldsv1string), so JSON is the only construction that
// compiles in both.
func fieldsV1(raw string) *metav1.FieldsV1 {
	out := &metav1.FieldsV1{}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		panic(err)
	}
	return out
}

// storedCL2Pod is the fixture as the server would actually see it: decoded from
// storage, so cache fields that a literal leaves zero are populated.
func storedCL2Pod() *corev1.Pod {
	b, err := json.Marshal(cl2Pod())
	if err != nil {
		panic(err)
	}
	out := &corev1.Pod{}
	if err := json.Unmarshal(b, out); err != nil {
		panic(err)
	}
	return out
}

// TestPrunedStrategicPatchObjectQuantityArtifact pins the only difference the two paths
// showed, and why it cannot reach the wire. ResourceFieldSelector.Divisor has no
// omitempty, so the stock path's round trip rewrites a zero Quantity's cached string and
// Format while the pruned path preserves them. Both encode identically, and a live
// object has already been through storage.
func TestPrunedStrategicPatchObjectQuantityArtifact(t *testing.T) {
	pristine := resource.Quantity{}
	decoded := resource.MustParse("0")

	pj, err := json.Marshal(pristine)
	if err != nil {
		t.Fatal(err)
	}
	dj, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(pj) != string(dj) {
		t.Errorf("JSON differs: pristine=%s decoded=%s", pj, dj)
	}

	pp, err := pristine.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	dp, err := decoded.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(pp) != string(dp) {
		t.Errorf("protobuf differs: pristine=%q decoded=%q", pp, dp)
	}

	// And the shape the server actually holds is the decoded one.
	live := storedCL2Pod().Spec.Containers[0].Env[4].ValueFrom.ResourceFieldRef.Divisor
	if live.Format != resource.DecimalSI {
		t.Errorf("a storage-decoded Divisor should carry a Format, got %q", live.Format)
	}
}

// TestPrunedStrategicPatchObjectSparseOriginal covers a patch naming a top-level key
// absent from the live object, so the merge has to create it. Pod's spec and status have
// omitempty, so a zero-valued one really is missing from the scratch conversion.
func TestPrunedStrategicPatchObjectSparseOriginal(t *testing.T) {
	cases := []struct {
		name  string
		patch string
	}{
		{name: "create status", patch: `{"status":{"phase":"Running"}}`},
		{name: "create spec", patch: `{"spec":{"schedulerName":"s"}}`},
		{name: "create metadata", patch: `{"metadata":{"labels":{"a":"b"}}}`},
		{name: "create all", patch: `{"metadata":{"labels":{"a":"b"}},"spec":{"schedulerName":"s"},"status":{"phase":"Running"}}`},
		{name: "delete an absent key", patch: `{"status":null}`},
	}
	// An empty Pod: nothing but the zero value in any top-level field.
	mk := func() *corev1.Pod { return &corev1.Pod{} }

	for _, tc := range cases {
		for _, directive := range []string{metav1.FieldValidationIgnore, metav1.FieldValidationStrict} {
			t.Run(tc.name+"/"+directive, func(t *testing.T) {
				ctx := context.Background()
				stockOut, prunedOut := &corev1.Pod{}, &corev1.Pod{}

				stockErr := stockStrategicPatchObject(ctx, &countingDefaulter{}, mk(), []byte(tc.patch), stockOut, &corev1.Pod{}, directive)
				prunedErr := strategicPatchObject(ctx, &countingDefaulter{}, mk(), []byte(tc.patch), prunedOut, &corev1.Pod{}, directive)

				patchMap := map[string]interface{}{}
				if _, err := kjson.UnmarshalStrict([]byte(tc.patch), &patchMap); err != nil {
					t.Fatalf("unmarshal patch: %v", err)
				}
				gotPruned, _ := prunedStrategicPatchObject(ctx, &countingDefaulter{}, mk(), patchMap, &corev1.Pod{}, &corev1.Pod{}, nil, directive)
				if !gotPruned {
					t.Fatalf("fast path declined; the case proves nothing")
				}

				if errString(stockErr) != errString(prunedErr) {
					t.Fatalf("error mismatch\n stock: %s\npruned: %s", errString(stockErr), errString(prunedErr))
				}
				if stockErr != nil {
					return
				}
				if !reflect.DeepEqual(stockOut, prunedOut) {
					t.Errorf("object mismatch\n stock: %#v\npruned: %#v", stockOut, prunedOut)
				}
			})
		}
	}
}

// TestImplicatedFieldsDeclines pins the shapes the fast path must refuse, directly on
// implicatedFields so the reason is visible rather than inferred from an end-to-end
// result. Every decline here is a correctness requirement, not a missed optimisation:
// firing on one of these would write an object the stock path would not.
func TestImplicatedFieldsDeclines(t *testing.T) {
	podType := reflect.TypeOf(corev1.Pod{})
	cases := []struct {
		name    string
		patch   map[string]interface{}
		wantOK  bool
		reason  string
		wantLen int
	}{
		{name: "bare $patch", patch: map[string]interface{}{"$patch": "replace"}, reason: "acts on the whole object"},
		{name: "bare $retainKeys", patch: map[string]interface{}{"$retainKeys": []interface{}{"spec"}}, reason: "acts on the whole object"},
		{name: "inlined TypeMeta key", patch: map[string]interface{}{"kind": "Pod"}, reason: "kind has no distinct struct field"},
		{name: "key that is not a field", patch: map[string]interface{}{"nonsense": 1}, reason: "cannot be mapped to a field"},
		{name: "prefixed directive on a non-field", patch: map[string]interface{}{"$setElementOrder/nonsense": nil}, reason: "names a key that is not a field"},

		{name: "empty patch", patch: map[string]interface{}{}, wantOK: true, wantLen: 0},
		{name: "status only", patch: map[string]interface{}{"status": nil}, wantOK: true, wantLen: 1},
		{name: "status named twice", patch: map[string]interface{}{"status": nil, "$setElementOrder/status": nil}, wantOK: true, wantLen: 1},
		{name: "metadata and status", patch: map[string]interface{}{"metadata": nil, "status": nil}, wantOK: true, wantLen: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fields, ok := implicatedFields(tc.patch, podType)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (%s)", ok, tc.wantOK, tc.reason)
			}
			if !ok {
				return
			}
			if len(fields) != tc.wantLen {
				t.Fatalf("got %d fields %v, want %d", len(fields), fields, tc.wantLen)
			}
			// Duplicates would set the same field twice, harmless, but they would also
			// mean the key set was not deduplicated, which is how a directive-plus-field
			// pair could grow the converted set without bound.
			seen := map[int]bool{}
			for _, idx := range fields {
				if seen[idx] {
					t.Errorf("field index %d appears twice", idx)
				}
				seen[idx] = true
			}
		})
	}
}

// TestPrunedStrategicPatchObjectDeclinesNonStruct covers the entry-point guards. SMP
// never reaches an Unstructured today, but the fast path is pure reflection and must
// not depend on that staying true.
func TestPrunedStrategicPatchObjectDeclinesNonStruct(t *testing.T) {
	ctx := context.Background()
	patchMap := map[string]interface{}{"status": map[string]interface{}{"phase": "Running"}}

	cases := []struct {
		name           string
		orig, toUpdate runtime.Object
	}{
		{name: "unstructured original", orig: &unstructured.Unstructured{Object: map[string]interface{}{}}, toUpdate: &unstructured.Unstructured{}},
		{name: "mismatched types", orig: &corev1.Pod{}, toUpdate: &corev1.Service{}},
		{name: "typed nil original", orig: (*corev1.Pod)(nil), toUpdate: &corev1.Pod{}},
		{name: "typed nil target", orig: &corev1.Pod{}, toUpdate: (*corev1.Pod)(nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			done, err := prunedStrategicPatchObject(ctx, &countingDefaulter{}, tc.orig, patchMap, tc.toUpdate, &corev1.Pod{}, nil, metav1.FieldValidationIgnore)
			if done || err != nil {
				t.Fatalf("fast path fired on %s: done=%v err=%v", tc.name, done, err)
			}
		})
	}
}

// TestPrunedStrategicPatchObjectAliasing guards the one hazard the deep-copy variant
// exists to avoid: the output must not share mutable memory with the original.
func TestPrunedStrategicPatchObjectAliasing(t *testing.T) {
	ctx := context.Background()
	orig := cl2Pod()
	out := &corev1.Pod{}

	var patchMap map[string]interface{}
	if err := kjson.UnmarshalCaseSensitivePreserveInts([]byte(`{"status":{"phase":"Running"}}`), &patchMap); err != nil {
		t.Fatal(err)
	}
	done, err := prunedStrategicPatchObject(ctx, &countingDefaulter{}, orig, patchMap, out, &corev1.Pod{}, nil, metav1.FieldValidationIgnore)
	if err != nil || !done {
		t.Fatalf("fast path did not run: done=%v err=%v", done, err)
	}

	out.Spec.Containers[0].Image = "mutated"
	out.Labels["group"] = "mutated"
	out.Spec.Containers[0].Env[0].Name = "MUTATED"

	if orig.Spec.Containers[0].Image == "mutated" {
		t.Error("output aliases the original's container slice")
	}
	if orig.Labels["group"] == "mutated" {
		t.Error("output aliases the original's label map")
	}
	if orig.Spec.Containers[0].Env[0].Name == "MUTATED" {
		t.Error("output aliases the original's env slice")
	}
}
