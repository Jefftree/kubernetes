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

package cacher

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"math"
	"strconv"
	"testing"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/sharding"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/apis/example"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/storage"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
)

// The benchmarks in this file quantify KEP-5866 (server-side sharded LIST and
// watch) by comparing two ways of running the same fleet of n controllers,
// each of which owns 1/n of the objects:
//
//	server-side: each replica asks for its shard, and the apiserver only
//	             materializes and ships the objects in that shard.
//	client-side: each replica asks for everything and throws away the 1-1/n
//	             it does not own, which is what controllers have to do today.
//
// One benchmark iteration is one full fleet round (all n replicas served
// once), so the two modes are directly comparable: both give the fleet a
// complete view of the data, and the difference is the work the apiserver did
// to get there.
const (
	shardKeyUID = "object.metadata.uid"
	// benchPodPayload is roughly the size of the unique part of a real pod.
	benchPodPayload = 2048
)

// shardRange returns the requirement for shard i of n, tiling the FNV-1a
// 64-bit hash space without gaps or overlap.
func shardRange(key string, i, n int) sharding.ShardRangeRequirement {
	width := math.MaxUint64 / uint64(n)
	req := sharding.ShardRangeRequirement{
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

// fleetPredicates returns one predicate per replica. With sharded=false every
// replica gets the same match-everything predicate, which is the client-side
// sharding baseline.
func fleetPredicates(shards int, sharded bool) []storage.SelectionPredicate {
	preds := make([]storage.SelectionPredicate, shards)
	for i := range preds {
		preds[i] = storage.SelectionPredicate{
			Label:    labels.Everything(),
			Field:    fields.Everything(),
			GetAttrs: storage.DefaultNamespaceScopedAttr,
		}
		if sharded {
			preds[i].ShardSelector = sharding.NewSelector(shardRange(shardKeyUID, i, shards))
		}
	}
	return preds
}

func shardingBenchPods(n int) []example.Pod {
	pods := make([]example.Pod, n)
	for i := range pods {
		pods[i].Namespace = fmt.Sprintf("namespace-%d", i%100)
		pods[i].Name = fmt.Sprintf("pod-%d", i)
		// A realistic UID: sharding on a low-cardinality or structured key
		// would not distribute evenly, and the benchmark should not flatter
		// the feature by using one.
		pods[i].UID = types.UID(fmt.Sprintf("2a9c1b7e-%04x-4f3a-9c1d-%012x", i%0x10000, i))
		pods[i].ResourceVersion = strconv.Itoa(i + 1)
		data := make([]byte, benchPodPayload)
		if _, err := rand.Read(data); err != nil {
			panic(err)
		}
		pods[i].Spec.NodeSelector = map[string]string{"key": string(data)}
	}
	return pods
}

// shardSizes counts how many of pods fall in each shard. The distribution is
// fixed by the UIDs, so callers can use it as an exact expectation rather
// than assuming a perfectly even split.
func shardSizes(b *testing.B, pods []example.Pod, preds []storage.SelectionPredicate) []int {
	sizes := make([]int, len(preds))
	for i := range preds {
		if preds[i].ShardSelector == nil {
			sizes[i] = len(pods)
			continue
		}
		for j := range pods {
			matches, err := preds[i].ShardSelector.Matches(&pods[j])
			if err != nil {
				b.Fatal(err)
			}
			if matches {
				sizes[i]++
			}
		}
	}
	return sizes
}

// BenchmarkShardedList compares the apiserver-side cost of serving one LIST
// round to a fleet of n replicas, with and without server-side sharding.
//
// Server-side sharding should keep the cost of a round roughly flat as the
// fleet grows, because the fleet still only reads the data once. Client-side
// sharding multiplies it by n.
func BenchmarkShardedList(b *testing.B) {
	featuregatetesting.SetFeatureGateDuringTest(b, utilfeature.DefaultFeatureGate, features.ShardedListAndWatch, true)

	const totalPods = 10_000
	pods := shardingBenchPods(totalPods)
	delegator := newDelegatorWithPods(b, pods)

	for _, shards := range []int{1, 2, 4, 8, 16} {
		for _, sharded := range []bool{true, false} {
			b.Run(fmt.Sprintf("shards=%d/%s", shards, fleetMode(sharded)), func(b *testing.B) {
				preds := fleetPredicates(shards, sharded)
				want := shardSizes(b, pods, preds)

				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					for i := range preds {
						result := &example.PodList{}
						err := delegator.GetList(context.TODO(), "/pods/", storage.ListOptions{
							Predicate:       preds[i],
							Recursive:       true,
							ResourceVersion: "12345",
						}, result)
						if err != nil {
							b.Fatalf("GetList: %v", err)
						}
						if len(result.Items) != want[i] {
							b.Fatalf("shard %d: expected %d items, got %d", i, want[i], len(result.Items))
						}
					}
				}
				b.StopTimer()

				var served int
				for _, n := range want {
					served += n
				}
				// Objects the apiserver copied out of the watch cache to
				// serve one round. This is the quantity the feature is
				// meant to reduce; ns/op should track it.
				b.ReportMetric(float64(served), "objects-served/round")
			})
		}
	}
}

// BenchmarkShardedListEncode measures the serialization half of a LIST round:
// each replica's response has to be encoded onto its connection.
//
// BenchmarkShardedList covers only what the cacher does to materialize the
// response, which is where sharding's cost lives. This covers where its
// benefit lives, and the two have to be read together.
func BenchmarkShardedListEncode(b *testing.B) {
	featuregatetesting.SetFeatureGateDuringTest(b, utilfeature.DefaultFeatureGate, features.ShardedListAndWatch, true)

	const totalPods = 10_000
	pods := shardingBenchPods(totalPods)

	for _, shards := range []int{1, 2, 4, 8, 16} {
		for _, sharded := range []bool{true, false} {
			b.Run(fmt.Sprintf("shards=%d/%s", shards, fleetMode(sharded)), func(b *testing.B) {
				preds := fleetPredicates(shards, sharded)

				// One response per replica, built up front so the timed loop
				// is pure serialization.
				lists := make([][]*example.Pod, shards)
				var served int
				for i := range preds {
					for j := range pods {
						if preds[i].ShardSelector != nil {
							matches, err := preds[i].ShardSelector.Matches(&pods[j])
							if err != nil {
								b.Fatal(err)
							}
							if !matches {
								continue
							}
						}
						lists[i] = append(lists[i], &pods[j])
					}
					served += len(lists[i])
				}

				w := &countingWriter{}
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					for _, list := range lists {
						// Item by item, the way the streaming protobuf list
						// writer serializes a response. Only the list
						// envelope is excluded, which is tens of bytes
						// against megabytes of items.
						for _, pod := range list {
							if err := examplev1ProtoCodec.Encode(pod, w); err != nil {
								b.Fatalf("encode: %v", err)
							}
						}
					}
				}
				b.StopTimer()

				b.ReportMetric(float64(served), "objects-served/round")
				b.ReportMetric(float64(w.n)/float64(max(b.N, 1))/(1<<20), "MiB-on-wire/round")
			})
		}
	}
}

// BenchmarkShardedWatchFilter measures the cacher's per-(watcher, event)
// filter cost for one fleet round: every event is tested against every
// watcher's predicate before it can be dispatched.
//
// Sharding does NOT reduce the number of filter evaluations, only how many
// events survive them. This is therefore the cost the feature adds to the
// watch path, to be weighed against the serialization it removes (see
// BenchmarkShardedWatchEncode).
//
// This drives filterWithAttrsAndPrefixFunction directly rather than running
// events through a live cacher. A live fleet cannot be driven at benchmark
// speed: injecting a round as fast as the loop can go trips the cacher's
// slow-watcher force-close, so the measurement becomes the safety valve
// rather than the filter.
func BenchmarkShardedWatchFilter(b *testing.B) {
	featuregatetesting.SetFeatureGateDuringTest(b, utilfeature.DefaultFeatureGate, features.ShardedListAndWatch, true)

	const eventsPerRound = 500
	pods := shardingBenchPods(eventsPerRound)
	gr := schema.GroupResource{Resource: "pods"}

	// The attributes the cacher has already computed by the time the filter
	// runs, so the benchmark prices the filter and not the attribute lookup.
	keys := make([]string, len(pods))
	fieldSets := make([]fields.Set, len(pods))
	for i := range pods {
		keys[i] = "/pods/" + pods[i].Namespace + "/" + pods[i].Name
		fieldSets[i] = fields.Set{
			"metadata.name":      pods[i].Name,
			"metadata.namespace": pods[i].Namespace,
			"spec.nodeName":      pods[i].Spec.NodeName,
		}
	}

	for _, shards := range []int{1, 2, 4, 8, 16} {
		for _, sharded := range []bool{true, false} {
			b.Run(fmt.Sprintf("shards=%d/%s", shards, fleetMode(sharded)), func(b *testing.B) {
				preds := fleetPredicates(shards, sharded)
				filters := make([]filterWithAttrsFunc, shards)
				for i := range preds {
					filters[i] = filterWithAttrsAndPrefixFunction("/pods/", preds[i], gr)
				}

				var passed int
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					passed = 0
					for _, filter := range filters {
						for j := range pods {
							if filter(keys[j], nil, fieldSets[j], &pods[j]) {
								passed++
							}
						}
					}
				}
				b.StopTimer()

				// Constant across modes: a sharded watch still evaluates
				// every event against every watcher.
				b.ReportMetric(float64(shards*len(pods)), "filter-evals/round")
				// Events that survive to be dispatched and serialized.
				b.ReportMetric(float64(passed), "events-delivered/round")
			})
		}
	}
}

func fleetMode(sharded bool) string {
	if sharded {
		return "server-side"
	}
	return "client-side"
}

// BenchmarkShardedWatchEncode measures the serialization the apiserver does to
// serve one fleet round: one encode per event that crosses a watcher boundary.
//
// This is deterministic and goroutine-free, so unlike the dispatch benchmark
// it is not perturbed by the cacher's back-pressure machinery. Serialization
// dominates the per-event cost of a watch, so this is where server-side
// sharding pays: unsharded the fleet forces n encodes of every event, sharded
// it forces one.
func BenchmarkShardedWatchEncode(b *testing.B) {
	featuregatetesting.SetFeatureGateDuringTest(b, utilfeature.DefaultFeatureGate, features.ShardedListAndWatch, true)

	const eventsPerRound = 500
	pods := shardingBenchPods(eventsPerRound)

	for _, shards := range []int{1, 2, 4, 8, 16} {
		for _, sharded := range []bool{true, false} {
			b.Run(fmt.Sprintf("shards=%d/%s", shards, fleetMode(sharded)), func(b *testing.B) {
				preds := fleetPredicates(shards, sharded)

				// The delivery plan: which pods each replica's connection has
				// to be handed. Computed up front so the timed loop is pure
				// serialization.
				plan := make([][]*example.Pod, shards)
				for i := range preds {
					for j := range pods {
						if preds[i].ShardSelector != nil {
							matches, err := preds[i].ShardSelector.Matches(&pods[j])
							if err != nil {
								b.Fatal(err)
							}
							if !matches {
								continue
							}
						}
						plan[i] = append(plan[i], &pods[j])
					}
				}

				var delivered int
				for i := range plan {
					delivered += len(plan[i])
				}

				w := &countingWriter{}
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					for i := range plan {
						for _, pod := range plan[i] {
							if err := examplev1ProtoCodec.Encode(pod, w); err != nil {
								b.Fatalf("encode: %v", err)
							}
						}
					}
				}
				b.StopTimer()

				b.ReportMetric(float64(delivered), "events-encoded/round")
				b.ReportMetric(float64(w.n)/float64(max(b.N, 1))/(1<<20), "MiB-on-wire/round")
			})
		}
	}
}

type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

var _ io.Writer = &countingWriter{}
