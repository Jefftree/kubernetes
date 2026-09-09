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

package podproto

import (
	"fmt"
	"os"
	"reflect"
	goruntime "runtime"
	"runtime/debug"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"sigs.k8s.io/yaml"
)

func loadExemplar(tb testing.TB) *corev1.Pod {
	tb.Helper()
	raw, err := os.ReadFile("../../testing/testdata/exemplar_pod.yaml")
	if err != nil {
		tb.Fatalf("read exemplar: %v", err)
	}
	var pod corev1.Pod
	if err := yaml.Unmarshal(raw, &pod); err != nil {
		tb.Fatalf("decode exemplar: %v", err)
	}
	return &pod
}

// wantFields mirrors pkg/registry/core/pod/strategy.go ToSelectableFields,
// which is the thing the byte extractor has to agree with.
func wantFields(pod *corev1.Pod) fields.Set {
	s := make(fields.Set, 10)
	s["spec.nodeName"] = pod.Spec.NodeName
	s["spec.restartPolicy"] = string(pod.Spec.RestartPolicy)
	s["spec.schedulerName"] = string(pod.Spec.SchedulerName)
	s["spec.serviceAccountName"] = string(pod.Spec.ServiceAccountName)
	s["spec.hostNetwork"] = strconv.FormatBool(pod.Spec.HostNetwork)
	s["status.phase"] = string(pod.Status.Phase)
	podIP := ""
	if len(pod.Status.PodIPs) > 0 {
		podIP = pod.Status.PodIPs[0].IP
	}
	s["status.podIP"] = podIP
	s["status.nominatedNodeName"] = pod.Status.NominatedNodeName
	s["metadata.name"] = pod.Name
	s["metadata.namespace"] = pod.Namespace
	return s
}

func cases(tb testing.TB) map[string]*corev1.Pod {
	base := loadExemplar(tb)
	out := map[string]*corev1.Pod{}

	full := base.DeepCopy()
	full.Namespace = "ns"
	full.Name = "full"
	full.Spec.NodeName = "node-7"
	full.Spec.RestartPolicy = corev1.RestartPolicyOnFailure
	full.Spec.SchedulerName = "custom-scheduler"
	full.Spec.ServiceAccountName = "sa-1"
	full.Spec.HostNetwork = true
	full.Status.Phase = corev1.PodRunning
	full.Status.NominatedNodeName = "node-9"
	full.Status.PodIPs = []corev1.PodIP{{IP: "10.1.2.3"}, {IP: "fd00::1"}}
	out["fully populated"] = full

	// Everything a selectable field reads left at its zero value. This is the
	// case a naive extractor gets wrong, because absent proto fields are not
	// encoded at all and the extractor has to default them.
	empty := base.DeepCopy()
	empty.Namespace = "ns"
	empty.Name = "empty"
	empty.Spec.NodeName = ""
	empty.Spec.RestartPolicy = ""
	empty.Spec.SchedulerName = ""
	empty.Spec.ServiceAccountName = ""
	empty.Spec.HostNetwork = false
	empty.Status.Phase = ""
	empty.Status.NominatedNodeName = ""
	empty.Status.PodIPs = nil
	out["all selectable fields empty"] = empty

	bare := &corev1.Pod{}
	bare.Name = "bare"
	bare.Namespace = "ns"
	out["bare pod, no spec or status at all"] = bare

	multiIP := full.DeepCopy()
	multiIP.Name = "multi-ip"
	multiIP.Status.PodIPs = []corev1.PodIP{{IP: "192.168.0.1"}, {IP: "192.168.0.2"}, {IP: "192.168.0.3"}}
	out["several pod IPs, only the first counts"] = multiIP

	hostNetFalse := full.DeepCopy()
	hostNetFalse.Name = "hostnet-false"
	hostNetFalse.Spec.HostNetwork = false
	out["hostNetwork false, which proto omits"] = hostNetFalse

	return out
}

func TestSelectableFieldsMatchesFullDecode(t *testing.T) {
	for name, pod := range cases(t) {
		t.Run(name, func(t *testing.T) {
			raw, err := pod.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			elem, err := Build(raw, "999")
			if err != nil {
				t.Fatal(err)
			}
			want := wantFields(pod)
			if !reflect.DeepEqual(fields.Set(elem.Fields), want) {
				t.Errorf("extracted fields differ from ToSelectableFields\n got: %v\nwant: %v", elem.Fields, want)
			}
			if elem.Meta.ResourceVersion != "999" {
				t.Errorf("resourceVersion = %q, want 999", elem.Meta.ResourceVersion)
			}
			if !reflect.DeepEqual(map[string]string(elem.Labels), pod.Labels) {
				t.Errorf("labels differ")
			}
		})
	}
}

func TestAssembleRoundTrip(t *testing.T) {
	for name, pod := range cases(t) {
		t.Run(name, func(t *testing.T) {
			raw, err := pod.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			elem, err := Build(raw, "4242")
			if err != nil {
				t.Fatal(err)
			}
			assembled, err := Assemble(elem.Meta, elem.Tail)
			if err != nil {
				t.Fatal(err)
			}
			var got corev1.Pod
			if err := got.Unmarshal(assembled); err != nil {
				t.Fatalf("assembled bytes do not decode: %v", err)
			}
			want := pod.DeepCopy()
			want.ResourceVersion = "4242"
			wantBytes, _ := want.Marshal()
			if string(assembled) != string(wantBytes) {
				t.Errorf("assembled bytes differ from a full re-encode\n got %d B\nwant %d B", len(assembled), len(wantBytes))
			}
		})
	}
}

// An unknown future field in the tail must survive untouched, since the whole
// premise is that the cache does not understand spec and status.
func TestAssemblePreservesUnknownTailFields(t *testing.T) {
	pod := loadExemplar(t)
	pod.Name, pod.Namespace = "unknown", "ns"
	raw, err := pod.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	// Append a field 99, length-delimited, which no v1.Pod knows about.
	raw = append(raw, 0xfa, 0x06, 0x04, 'a', 'b', 'c', 'd')

	elem, err := Build(raw, "7")
	if err != nil {
		t.Fatal(err)
	}
	assembled, err := Assemble(elem.Meta, elem.Tail)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(assembled, []byte{0xfa, 0x06, 0x04, 'a', 'b', 'c', 'd'}) {
		t.Error("unknown tail field was dropped")
	}
}

func contains(h, n []byte) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if string(h[i:i+len(n)]) == string(n) {
			return true
		}
	}
	return false
}

// ---- cost ----

func benchPod(tb testing.TB) []byte {
	pod := loadExemplar(tb)
	pod.Namespace, pod.Name = "ns", "bench"
	raw, err := pod.Marshal()
	if err != nil {
		tb.Fatal(err)
	}
	return raw
}

// BenchmarkIngestSplit is the proposed ingest: split, decode metadata only,
// extract attributes from the encoded spec and status.
func BenchmarkIngestSplit(b *testing.B) {
	raw := benchPod(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Build(raw, "1"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkIngestFullDecode is what upstream does today: decode the whole pod,
// then read the attributes off the typed object.
func BenchmarkIngestFullDecode(b *testing.B) {
	raw := benchPod(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var pod corev1.Pod
		if err := pod.Unmarshal(raw); err != nil {
			b.Fatal(err)
		}
		_ = wantFields(&pod)
	}
}

// BenchmarkServeAssemble is the proposed serve: re-encode metadata, splice tail.
func BenchmarkServeAssemble(b *testing.B) {
	raw := benchPod(b)
	elem, err := Build(raw, "1")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Assemble(elem.Meta, elem.Tail); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkServeFullEncode is what serving costs today from a decoded object.
func BenchmarkServeFullEncode(b *testing.B) {
	raw := benchPod(b)
	var pod corev1.Pod
	if err := pod.Unmarshal(raw); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := pod.Marshal(); err != nil {
			b.Fatal(err)
		}
	}
}

func gcSettle() {
	for i := 0; i < 3; i++ {
		goruntime.GC()
	}
	debug.FreeOSMemory()
}

func liveHeap() (uint64, uint64) {
	gcSettle()
	var ms goruntime.MemStats
	goruntime.ReadMemStats(&ms)
	return ms.HeapAlloc, ms.HeapObjects
}

// TestRetentionThreeWay compares what 10,000 pods cost to retain in each shape.
func TestRetentionThreeWay(t *testing.T) {
	const n = 10000
	base := loadExemplar(t)

	raws := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		p := base.DeepCopy()
		p.Namespace, p.Name = "ns", fmt.Sprintf("pod-%06d", i)
		p.Spec.NodeName = fmt.Sprintf("node-%d", i%5000)
		b, err := p.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		exact := make([]byte, len(b))
		copy(exact, b)
		raws = append(raws, exact)
	}

	measure := func(build func() any) (int64, int64) {
		b0, o0 := liveHeap()
		held := build()
		b1, o1 := liveHeap()
		goruntime.KeepAlive(held)
		return int64(b1) - int64(b0), int64(o1) - int64(o0)
	}

	decB, decO := measure(func() any {
		out := make([]*corev1.Pod, 0, n)
		for _, r := range raws {
			var p corev1.Pod
			if err := p.Unmarshal(r); err != nil {
				t.Fatal(err)
			}
			out = append(out, &p)
		}
		return out
	})

	splitB, splitO := measure(func() any {
		out := make([]*Element, 0, n)
		for i, r := range raws {
			e, err := Build(r, strconv.Itoa(i))
			if err != nil {
				t.Fatal(err)
			}
			// Own the tail rather than aliasing the fixture, as a real cache would.
			tail := make([]byte, len(e.Tail))
			copy(tail, e.Tail)
			e.Tail = tail
			out = append(out, e)
		}
		return out
	})

	wholeB, wholeO := measure(func() any {
		out := make([][]byte, 0, n)
		for _, r := range raws {
			c := make([]byte, len(r))
			copy(c, r)
			out = append(out, c)
		}
		return out
	})

	row := func(name string, b, o int64) {
		t.Logf("%-34s %7.1f MB %9d objects  %6.0f B/pod %6.1f objs/pod",
			name, float64(b)/(1<<20), o, float64(b)/n, float64(o)/n)
	}
	t.Logf("10,000 exemplar pods, %d B each on the wire", len(raws[0]))
	row("decoded v1.Pod (upstream)", decB, decO)
	row("metadata split (proposed)", splitB, splitO)
	row("whole-object bytes (built)", wholeB, wholeO)
	goruntime.KeepAlive(raws)
	var _ = metav1.ObjectMeta{}
}
