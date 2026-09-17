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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kjson "sigs.k8s.io/json"
)

// cl2StatusPatch is verbatim what the write-throughput writer sends, with the
// timestamp rendered.
// perf-tests util-images/request-benchmark/mode_patch.go:321
var cl2StatusPatch = []byte(`{"status":{"conditions":[{"type":"Ready","status":"False","reason":"Benchmark","message":"1756312345678901234"}]}}`)

// steadyStateCL2Pod is what the server holds after the first patch of a run: every
// later patch merges into this, not into the pristine initial pod.
func steadyStateCL2Pod() *corev1.Pod {
	p := storedCL2Pod()
	p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{
		Type:    corev1.PodReady,
		Status:  corev1.ConditionFalse,
		Reason:  "Benchmark",
		Message: "1756312345678901233",
	})
	return p
}

type patchFn func(context.Context, runtime.ObjectDefaulter, runtime.Object, []byte, runtime.Object, runtime.Object, string) error

// assertFastPathFires refuses to benchmark a configuration where the pruned entry point
// falls back to stock, which would report a null result.
func assertFastPathFires(b *testing.B, orig *corev1.Pod, patch []byte) {
	b.Helper()
	var patchMap map[string]interface{}
	if err := kjson.UnmarshalCaseSensitivePreserveInts(patch, &patchMap); err != nil {
		b.Fatalf("unmarshal patch: %v", err)
	}
	done, err := prunedStrategicPatchObject(context.Background(), &countingDefaulter{}, orig, patchMap, &corev1.Pod{}, &corev1.Pod{}, nil, metav1.FieldValidationIgnore)
	if err != nil {
		b.Fatalf("fast path errored: %v", err)
	}
	if !done {
		b.Fatal("fast path declined this input; the benchmark would compare stock against stock")
	}
}

func runPatchBench(b *testing.B, orig *corev1.Pod, patch []byte, fn patchFn) {
	ctx := context.Background()
	def := &countingDefaulter{}
	ref := &corev1.Pod{}

	// Correctness is asserted elsewhere; this only guards against timing an error path.
	if err := fn(ctx, def, orig, patch, &corev1.Pod{}, ref, metav1.FieldValidationIgnore); err != nil {
		b.Fatalf("warmup: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := &corev1.Pod{}
		if err := fn(ctx, def, orig, patch, out, ref, metav1.FieldValidationIgnore); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStrategicPatchStock(b *testing.B) {
	for name, mk := range map[string]func() *corev1.Pod{"initial": storedCL2Pod, "steady": steadyStateCL2Pod} {
		b.Run(name, func(b *testing.B) {
			runPatchBench(b, mk(), cl2StatusPatch, stockStrategicPatchObject)
		})
	}
}

func BenchmarkStrategicPatchPruned(b *testing.B) {
	for name, mk := range map[string]func() *corev1.Pod{"initial": storedCL2Pod, "steady": steadyStateCL2Pod} {
		b.Run(name, func(b *testing.B) {
			orig := mk()
			assertFastPathFires(b, orig, cl2StatusPatch)
			runPatchBench(b, orig, cl2StatusPatch, strategicPatchObject)
		})
	}
}

// The component benchmarks below give the whole-function delta a second, independent
// derivation: saving = whole-pod ToUnstructured + FromUnstructured - the status-only
// conversions - DeepCopy.

func BenchmarkComponentToUnstructuredWholePod(b *testing.B) {
	pod := steadyStateCL2Pod()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pod); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkComponentToUnstructuredStatusOnly(b *testing.B) {
	full := steadyStateCL2Pod()
	pod := &corev1.Pod{Status: full.Status}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pod); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkComponentFromUnstructuredWholePod(b *testing.B) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(steadyStateCL2Pod())
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := &corev1.Pod{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(m, out, false); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkComponentFromUnstructuredStatusOnly(b *testing.B) {
	full := steadyStateCL2Pod()
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&corev1.Pod{Status: full.Status})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := &corev1.Pod{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(m, out, false); err != nil {
			b.Fatal(err)
		}
	}
}

// The server's default fieldValidation directive is Warn, so the real call passes
// returnUnknownFields=true. These two pin how that changes the whole-vs-pruned ratio.

func BenchmarkComponentFromUnstructuredWholePodStrict(b *testing.B) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(steadyStateCL2Pod())
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := &corev1.Pod{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(m, out, true); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkComponentFromUnstructuredStatusOnlyStrict(b *testing.B) {
	full := steadyStateCL2Pod()
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&corev1.Pod{Status: full.Status})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := &corev1.Pod{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(m, out, true); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkComponentDeepCopyPod(b *testing.B) {
	pod := steadyStateCL2Pod()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = pod.DeepCopyObject()
	}
}
