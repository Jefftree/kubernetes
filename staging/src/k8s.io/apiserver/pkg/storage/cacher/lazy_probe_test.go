/*
Copyright 2025 The Kubernetes Authors.

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

package cacher

import (
	"fmt"
	"os"
	goruntime "runtime"
	"runtime/debug"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/yaml"
)

// exemplarCorePodYAML is the same ~18.7 KB realistic pod fixture the storage
// benchmarks use, but decoded into a full v1.Pod rather than the stripped-down
// apis/example Pod (whose PodSpec has no containers, so it drops ~95% of it).
func loadExemplarCorePod(tb testing.TB) *corev1.Pod {
	tb.Helper()
	raw, err := os.ReadFile("../testing/testdata/exemplar_pod.yaml")
	if err != nil {
		tb.Fatalf("read exemplar pod: %v", err)
	}
	var pod corev1.Pod
	if err := yaml.Unmarshal(raw, &pod); err != nil {
		tb.Fatalf("decode exemplar pod: %v", err)
	}
	return &pod
}

// buildCorePods returns n distinct copies of the exemplar pod.
func buildCorePods(tb testing.TB, n int) []*corev1.Pod {
	tb.Helper()
	exemplar := loadExemplarCorePod(tb)
	pods := make([]*corev1.Pod, 0, n)
	for i := 0; i < n; i++ {
		p := exemplar.DeepCopy()
		p.Namespace = fmt.Sprintf("ns-%d", i%50)
		p.Name = fmt.Sprintf("%s%s", p.GenerateName, rand.String(10))
		p.UID = types.UID(rand.String(36))
		p.ResourceVersion = ""
		p.Spec.NodeName = fmt.Sprintf("node-%d", i%5000)
		pods = append(pods, p)
	}
	return pods
}

func gcSettle() {
	for i := 0; i < 3; i++ {
		goruntime.GC()
	}
	debug.FreeOSMemory()
}

// liveHeap returns the live heap after forced GCs. It is only meaningful as a
// difference against another liveHeap reading in the same process.
func liveHeap() (bytes uint64, objects uint64) {
	gcSettle()
	var ms goruntime.MemStats
	goruntime.ReadMemStats(&ms)
	return ms.HeapAlloc, ms.HeapObjects
}

// TestLazyDecodeRetentionProbe measures the live-heap cost of holding N pods in
// the two shapes a watch cache could use: decoded typed objects (today) and
// storage-encoded protobuf blobs (what lazy decode would hold). This is a
// mechanism probe on the isolated data structures, not a claim about the
// prototype. Run with:
//
//	go test ./pkg/storage/cacher/ -run TestLazyDecodeRetentionProbe -v -count=1
func TestLazyDecodeRetentionProbe(t *testing.T) {
	const podCount = 10000

	// Encode outside both measurements. Encoding is charged to neither arm;
	// `encoded` stays live throughout so it cancels out of every delta.
	pods := buildCorePods(t, podCount)
	encoded := make([][]byte, 0, podCount)
	var wireBytes int64
	for _, pod := range pods {
		b, err := runtime.Encode(corev1ProtoCodec, pod)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, len(b)) // right-size: Encode may over-allocate capacity
		copy(buf, b)
		encoded = append(encoded, buf)
		wireBytes += int64(len(buf))
	}
	pods = nil

	t.Logf("pods=%d  mean protobuf wire size=%d bytes  total wire=%.1f MB",
		podCount, wireBytes/podCount, float64(wireBytes)/(1<<20))

	base, baseObjs := liveHeap()

	// Arm 1: decoded typed objects, the shape store.Element holds today.
	decoded := make([]runtime.Object, 0, podCount)
	for _, b := range encoded {
		obj, err := runtime.Decode(corev1ProtoCodec, b)
		if err != nil {
			t.Fatal(err)
		}
		decoded = append(decoded, obj)
	}
	if _, ok := decoded[0].(*corev1.Pod); !ok {
		t.Fatalf("expected *corev1.Pod, got %T", decoded[0])
	}
	withDecoded, withDecodedObjs := liveHeap()
	decodedBytes := int64(withDecoded) - int64(base)
	decodedObjects := int64(withDecodedObjs) - int64(baseObjs)
	goruntime.KeepAlive(decoded)
	decoded = nil

	// Arm 2: storage-encoded bytes, the shape a lazy element would hold.
	afterDrop, afterDropObjs := liveHeap()
	raw := make([][]byte, 0, podCount)
	for _, b := range encoded {
		buf := make([]byte, len(b))
		copy(buf, b)
		raw = append(raw, buf)
	}
	withRaw, withRawObjs := liveHeap()
	rawBytes := int64(withRaw) - int64(afterDrop)
	rawObjects := int64(withRawObjs) - int64(afterDropObjs)
	goruntime.KeepAlive(raw)

	t.Logf("baseline live heap: %.1f MB / %d objects", float64(base)/(1<<20), baseObjs)
	t.Logf("after dropping arm 1: %.1f MB / %d objects (should be ~baseline)",
		float64(afterDrop)/(1<<20), afterDropObjs)
	t.Logf("")
	t.Logf("  decoded typed objects: %8.1f MB  %9d objects   (%6.0f B, %5.1f objs per pod)",
		float64(decodedBytes)/(1<<20), decodedObjects,
		float64(decodedBytes)/podCount, float64(decodedObjects)/podCount)
	t.Logf("  storage-encoded bytes: %8.1f MB  %9d objects   (%6.0f B, %5.1f objs per pod)",
		float64(rawBytes)/(1<<20), rawObjects,
		float64(rawBytes)/podCount, float64(rawObjects)/podCount)
	t.Logf("  reduction: bytes %.2fx   objects %.2fx",
		float64(decodedBytes)/float64(rawBytes),
		float64(decodedObjects)/float64(rawObjects))

	// `encoded` must outlive every measurement above, otherwise it is collected
	// mid-probe and its ~96 MB silently cancels out an arm's delta.
	goruntime.KeepAlive(encoded)
	goruntime.KeepAlive(raw)
}
