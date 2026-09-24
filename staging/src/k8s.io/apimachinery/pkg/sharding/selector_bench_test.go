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

package sharding

import (
	"fmt"
	"math"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// ShardRange returns the requirement for shard i of n over the full FNV-1a
// 64-bit hash space, so that the n requirements tile [0, 2^64) without gaps
// or overlap.
func benchShardRange(key string, i, n int) ShardRangeRequirement {
	width := math.MaxUint64 / uint64(n)
	req := ShardRangeRequirement{
		Key:   key,
		Start: fmt.Sprintf("0x%016x", uint64(i)*width),
		End:   fmt.Sprintf("0x%016x", uint64(i+1)*width),
	}
	if i == n-1 {
		// The top of the space is 2^64, which does not fit in a uint64.
		req.End = "0x10000000000000000"
	}
	return req
}

// benchPod is a minimal runtime.Object with the metadata the shard accessor
// reads. Using it instead of a full Pod keeps meta.Accessor out of the
// measurement of unrelated deep object structure.
type benchPod struct {
	metav1.TypeMeta
	metav1.ObjectMeta
}

func (p *benchPod) DeepCopyObject() runtime.Object {
	out := *p
	return &out
}

func benchPods(n int) []runtime.Object {
	pods := make([]runtime.Object, n)
	for i := range pods {
		pods[i] = &benchPod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("pod-%d", i),
				Namespace: fmt.Sprintf("namespace-%d", i%100),
				UID:       types.UID(fmt.Sprintf("2a9c1b7e-%04d-4f3a-9c1d-%012d", i%10000, i)),
			},
		}
	}
	return pods
}

// BenchmarkSelectorMatches measures the per-object cost the apiserver pays to
// decide whether an object belongs to a shard. This is the tax the sharding
// feature adds to every LIST item and every dispatched watch event, so it
// bounds how much of the saving from shipping 1/n of the data gets eaten by
// the filter itself.
func BenchmarkSelectorMatches(b *testing.B) {
	const objects = 1024
	pods := benchPods(objects)

	for _, key := range []string{"object.metadata.uid", "object.metadata.namespace"} {
		for _, shards := range []int{2, 8, 64} {
			b.Run(fmt.Sprintf("key=%s/shards=%d", shortKey(key), shards), func(b *testing.B) {
				sel := NewSelector(benchShardRange(key, 0, shards))
				b.ReportAllocs()
				b.ResetTimer()
				var matched int
				for i := 0; b.Loop(); i++ {
					ok, err := sel.Matches(pods[i%objects])
					if err != nil {
						b.Fatal(err)
					}
					if ok {
						matched++
					}
				}
				b.StopTimer()
				// Sanity: a uniform hash should place roughly 1/shards of the
				// objects in shard 0. Reported so a skewed key is visible
				// rather than silently making the benchmark look fast.
				b.ReportMetric(float64(matched)/float64(b.N), "match-rate")
			})
		}
	}

	b.Run("baseline=Everything", func(b *testing.B) {
		sel := Everything()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; b.Loop(); i++ {
			if _, err := sel.Matches(pods[i%objects]); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkSelectorMatchesMultiRange measures a selector that stitches several
// disjoint ranges together, which is what a client does when it owns
// non-contiguous parts of the ring after a rebalance. Cost should stay flat:
// the hash is computed once and only the range comparison repeats.
func BenchmarkSelectorMatchesMultiRange(b *testing.B) {
	const objects = 1024
	pods := benchPods(objects)

	for _, ranges := range []int{1, 2, 4, 8} {
		b.Run(fmt.Sprintf("ranges=%d", ranges), func(b *testing.B) {
			// Take every other range out of a 2*ranges tiling, so the owned
			// ranges are disjoint and never adjacent.
			reqs := make([]ShardRangeRequirement, 0, ranges)
			for i := range ranges {
				reqs = append(reqs, benchShardRange("object.metadata.uid", 2*i, 2*ranges))
			}
			sel := NewSelector(reqs...)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; b.Loop(); i++ {
				if _, err := sel.Matches(pods[i%objects]); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkHashField isolates the hash itself from object access and range
// comparison.
func BenchmarkHashField(b *testing.B) {
	const uid = "2a9c1b7e-0042-4f3a-9c1d-000000000042"
	b.ReportAllocs()
	for b.Loop() {
		HashField(uid)
	}
}

func shortKey(key string) string {
	switch key {
	case "object.metadata.uid":
		return "uid"
	case "object.metadata.namespace":
		return "namespace"
	default:
		return key
	}
}
