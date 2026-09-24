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

package apimachinery

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/sharding"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/kubernetes/test/e2e/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

// calculateShardRange divides the 64-bit hash space [0x0000000000000000, 0x10000000000000000)
// evenly across total shards and returns the hex-encoded start (inclusive) and end (exclusive)
// for the given shard index.
func calculateShardRange(index, total int) (start, end string) {
	maxVal := new(big.Int).Lsh(big.NewInt(1), 64) // 2^64
	span := new(big.Int).Div(maxVal, big.NewInt(int64(total)))

	startVal := new(big.Int).Mul(span, big.NewInt(int64(index)))
	endVal := new(big.Int).Mul(span, big.NewInt(int64(index+1)))
	if index == total-1 {
		endVal = maxVal
	}

	start = fmt.Sprintf("0x%016x", startVal)
	end = fmt.Sprintf("0x%016x", endVal)
	return start, end
}

func shardSelectorString(field string, index, total int) string {
	start, end := calculateShardRange(index, total)
	return fmt.Sprintf("shardRange(%s, '%s', '%s')", field, start, end)
}

func valueInShard(value string, index, total int) bool {
	start, end := calculateShardRange(index, total)
	hash := "0x" + sharding.HashField(value)
	return !sharding.HexLess(hash, start) && sharding.HexLess(hash, end)
}

var _ = SIGDescribe("Server-Side Sharded List and Watch", framework.WithFeatureGate(features.ShardedListAndWatch), func() {
	f := framework.NewDefaultFramework("sharding")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	ginkgo.It("should partition LIST results across 4 UID shards with disjoint completeness, pagination, and ShardInfo echo", func(ctx context.Context) {
		ns := f.Namespace.Name
		cmClient := f.ClientSet.CoreV1().ConfigMaps(ns)

		const numObjects = 24
		createdUIDs := make(map[string]string, numObjects)
		ginkgo.By(fmt.Sprintf("Creating %d ConfigMaps in namespace %s", numObjects, ns))
		for i := range numObjects {
			cm, err := cmClient.Create(ctx, &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:   fmt.Sprintf("shard-uid-cm-%02d", i),
					Labels: map[string]string{"e2e-sharding": "uid-list"},
				},
				Data: map[string]string{"index": fmt.Sprintf("%d", i)},
			}, metav1.CreateOptions{})
			framework.ExpectNoError(err, "failed to create ConfigMap %d", i)
			createdUIDs[string(cm.UID)] = cm.Name
		}

		const numShards = 4
		seenUnpaginated := make(map[string]int, numObjects)

		ginkgo.By("Listing across 4 UID shards and verifying ShardInfo echo and disjoint completeness")
		for shard := range numShards {
			selector := shardSelectorString("object.metadata.uid", shard, numShards)
			list, err := cmClient.List(ctx, metav1.ListOptions{
				LabelSelector: "e2e-sharding=uid-list",
				ShardSelector: selector,
			})
			framework.ExpectNoError(err, "failed to list shard %d", shard)

			gomega.Expect(list.ShardInfo).ToNot(gomega.BeNil(), "expected ListMeta.ShardInfo to be set for shard %d", shard)
			gomega.Expect(list.ShardInfo.Selector).To(gomega.Equal(selector), "expected ListMeta.ShardInfo.Selector to echo selector for shard %d", shard)

			for _, cm := range list.Items {
				uid := string(cm.UID)
				gomega.Expect(valueInShard(uid, shard, numShards)).To(gomega.BeTrue(),
					"ConfigMap %s (UID %s, hash 0x%s) should fall within shard %d (%s)",
					cm.Name, uid, sharding.HashField(uid), shard, selector)
				prevShard, alreadySeen := seenUnpaginated[uid]
				gomega.Expect(alreadySeen).To(gomega.BeFalse(),
					"ConfigMap %s (UID %s) appeared in both shard %d and shard %d", cm.Name, uid, prevShard, shard)
				seenUnpaginated[uid] = shard
			}
		}

		gomega.Expect(seenUnpaginated).To(gomega.HaveLen(numObjects), "union of 4 shards must contain all created ConfigMaps")
		for uid, name := range createdUIDs {
			gomega.Expect(seenUnpaginated).To(gomega.HaveKey(uid), "ConfigMap %s (UID %s) missing from sharded LIST responses", name, uid)
		}

		ginkgo.By("Paginating (Limit + Continue) within each UID shard and verifying disjoint completeness and ShardInfo on every page")
		seenPaginated := make(map[string]int, numObjects)
		for shard := range numShards {
			selector := shardSelectorString("object.metadata.uid", shard, numShards)
			opts := metav1.ListOptions{
				LabelSelector: "e2e-sharding=uid-list",
				ShardSelector: selector,
				Limit:         2,
			}
			for {
				page, err := cmClient.List(ctx, opts)
				framework.ExpectNoError(err, "failed paginated list for shard %d", shard)

				gomega.Expect(page.ShardInfo).ToNot(gomega.BeNil(), "expected ListMeta.ShardInfo to be set on paginated response for shard %d", shard)
				gomega.Expect(page.ShardInfo.Selector).To(gomega.Equal(selector), "expected ListMeta.ShardInfo.Selector to match on paginated response for shard %d", shard)
				gomega.Expect(len(page.Items)).To(gomega.BeNumerically("<=", opts.Limit))

				for _, cm := range page.Items {
					uid := string(cm.UID)
					gomega.Expect(valueInShard(uid, shard, numShards)).To(gomega.BeTrue(),
						"paginated ConfigMap %s (UID %s) should fall within shard %d", cm.Name, uid, shard)
					prevShard, alreadySeen := seenPaginated[uid]
					gomega.Expect(alreadySeen).To(gomega.BeFalse(),
						"paginated ConfigMap %s (UID %s) appeared more than once (shards %d and %d)", cm.Name, uid, prevShard, shard)
					seenPaginated[uid] = shard
				}

				if page.Continue == "" {
					break
				}
				opts.Continue = page.Continue
			}
		}

		gomega.Expect(seenPaginated).To(gomega.Equal(seenUnpaginated), "paginated sharded LIST results must exactly match unpaginated sharded LIST results")
	})

	ginkgo.It("should filter LIST results by object.metadata.namespace shard range", func(ctx context.Context) {
		ns := f.Namespace.Name
		cmClient := f.ClientSet.CoreV1().ConfigMaps(ns)

		const numObjects = 6
		createdUIDs := make(map[string]bool, numObjects)
		ginkgo.By(fmt.Sprintf("Creating %d ConfigMaps in namespace %s", numObjects, ns))
		for i := range numObjects {
			cm, err := cmClient.Create(ctx, &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:   fmt.Sprintf("shard-ns-cm-%02d", i),
					Labels: map[string]string{"e2e-sharding": "ns-list"},
				},
			}, metav1.CreateOptions{})
			framework.ExpectNoError(err, "failed to create ConfigMap %d", i)
			createdUIDs[string(cm.UID)] = true
		}

		const numShards = 2
		matchingShard := 0
		if valueInShard(ns, 1, numShards) {
			matchingShard = 1
		}
		complementaryShard := 1 - matchingShard

		matchingSelector := shardSelectorString("object.metadata.namespace", matchingShard, numShards)
		complementarySelector := shardSelectorString("object.metadata.namespace", complementaryShard, numShards)

		ginkgo.By(fmt.Sprintf("Listing with matching namespace shard selector %s", matchingSelector))
		matchingList, err := cmClient.List(ctx, metav1.ListOptions{
			LabelSelector: "e2e-sharding=ns-list",
			ShardSelector: matchingSelector,
		})
		framework.ExpectNoError(err, "failed to list with matching namespace shard selector")
		gomega.Expect(matchingList.ShardInfo).ToNot(gomega.BeNil())
		gomega.Expect(matchingList.ShardInfo.Selector).To(gomega.Equal(matchingSelector))
		gomega.Expect(matchingList.Items).To(gomega.HaveLen(numObjects))
		for _, cm := range matchingList.Items {
			gomega.Expect(createdUIDs[string(cm.UID)]).To(gomega.BeTrue())
		}

		ginkgo.By(fmt.Sprintf("Listing with complementary namespace shard selector %s", complementarySelector))
		complementaryList, err := cmClient.List(ctx, metav1.ListOptions{
			LabelSelector: "e2e-sharding=ns-list",
			ShardSelector: complementarySelector,
		})
		framework.ExpectNoError(err, "failed to list with complementary namespace shard selector")
		gomega.Expect(complementaryList.ShardInfo).ToNot(gomega.BeNil())
		gomega.Expect(complementaryList.ShardInfo.Selector).To(gomega.Equal(complementarySelector))
		gomega.Expect(complementaryList.Items).To(gomega.BeEmpty(), "complementary namespace shard range must return 0 items from namespace %s", ns)
	})

	ginkgo.It("should filter WATCH events (ADDED, MODIFIED, DELETED) across complementary shards by object.metadata.uid and object.metadata.namespace", func(ctx context.Context) {
		ns := f.Namespace.Name
		cmClient := f.ClientSet.CoreV1().ConfigMaps(ns)

		initialList, err := cmClient.List(ctx, metav1.ListOptions{})
		framework.ExpectNoError(err, "failed initial ConfigMap list")
		rv := initialList.ResourceVersion

		const numShards = 2

		ginkgo.By("Starting 2 complementary sharded watches by object.metadata.uid and 2 by object.metadata.namespace")
		uidWatchers := make([]watch.Interface, numShards)
		nsWatchers := make([]watch.Interface, numShards)
		for shard := range numShards {
			uidWatcher, err := cmClient.Watch(ctx, metav1.ListOptions{
				ResourceVersion: rv,
				LabelSelector:   "e2e-sharding=watch-test",
				ShardSelector:   shardSelectorString("object.metadata.uid", shard, numShards),
			})
			framework.ExpectNoError(err, "failed to start UID watcher for shard %d", shard)
			defer uidWatcher.Stop()
			uidWatchers[shard] = uidWatcher

			nsWatcher, err := cmClient.Watch(ctx, metav1.ListOptions{
				ResourceVersion: rv,
				LabelSelector:   "e2e-sharding=watch-test",
				ShardSelector:   shardSelectorString("object.metadata.namespace", shard, numShards),
			})
			framework.ExpectNoError(err, "failed to start namespace watcher for shard %d", shard)
			defer nsWatcher.Stop()
			nsWatchers[shard] = nsWatcher
		}

		type recordedEvent struct {
			shard     int
			eventType watch.EventType
			uid       string
		}

		startCollector := func(watchers []watch.Interface) chan recordedEvent {
			ch := make(chan recordedEvent, 256)
			for shard, w := range watchers {
				go func(shard int, w watch.Interface) {
					for evt := range w.ResultChan() {
						cm, ok := evt.Object.(*v1.ConfigMap)
						if !ok {
							continue
						}
						ch <- recordedEvent{
							shard:     shard,
							eventType: evt.Type,
							uid:       string(cm.UID),
						}
					}
				}(shard, w)
			}
			return ch
		}

		uidEventCh := startCollector(uidWatchers)
		nsEventCh := startCollector(nsWatchers)

		collectEvents := func(ch chan recordedEvent, expectedType watch.EventType, expectedCount int) []map[string]bool {
			perShard := make([]map[string]bool, numShards)
			for i := range perShard {
				perShard[i] = make(map[string]bool)
			}
			timeout := time.After(30 * time.Second)
			collected := 0
			for collected < expectedCount {
				select {
				case evt := <-ch:
					if evt.eventType != expectedType {
						continue
					}
					perShard[evt.shard][evt.uid] = true
					collected++
				case <-timeout:
					framework.Failf("timed out waiting for %s watch events: got %d/%d", expectedType, collected, expectedCount)
				}
			}
			return perShard
		}

		const numObjects = 12
		created := make([]*v1.ConfigMap, 0, numObjects)
		expectedUIDShard := make(map[string]int, numObjects)
		expectedNSShard := 0
		if valueInShard(ns, 1, numShards) {
			expectedNSShard = 1
		}

		ginkgo.By(fmt.Sprintf("Creating %d ConfigMaps and verifying ADDED watch events", numObjects))
		for i := range numObjects {
			cm, err := cmClient.Create(ctx, &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:   fmt.Sprintf("shard-watch-cm-%02d", i),
					Labels: map[string]string{"e2e-sharding": "watch-test"},
				},
				Data: map[string]string{"step": "created"},
			}, metav1.CreateOptions{})
			framework.ExpectNoError(err, "failed to create ConfigMap %d", i)
			created = append(created, cm)

			uid := string(cm.UID)
			if valueInShard(uid, 0, numShards) {
				expectedUIDShard[uid] = 0
			} else {
				expectedUIDShard[uid] = 1
			}
		}

		assertPartition := func(phase watch.EventType, uidPerShard, nsPerShard []map[string]bool) {
			for uid, wantShard := range expectedUIDShard {
				otherShard := 1 - wantShard
				gomega.Expect(uidPerShard[wantShard][uid]).To(gomega.BeTrue(),
					"%s: UID watcher %d should receive event for UID %s", phase, wantShard, uid)
				gomega.Expect(uidPerShard[otherShard][uid]).To(gomega.BeFalse(),
					"%s: complementary UID watcher %d must not receive event for UID %s", phase, otherShard, uid)

				otherNSShard := 1 - expectedNSShard
				gomega.Expect(nsPerShard[expectedNSShard][uid]).To(gomega.BeTrue(),
					"%s: matching namespace watcher %d should receive event for UID %s", phase, expectedNSShard, uid)
				gomega.Expect(nsPerShard[otherNSShard][uid]).To(gomega.BeFalse(),
					"%s: complementary namespace watcher %d must not receive event for UID %s", phase, otherNSShard, uid)
			}
		}

		uidAdded := collectEvents(uidEventCh, watch.Added, numObjects)
		nsAdded := collectEvents(nsEventCh, watch.Added, numObjects)
		assertPartition(watch.Added, uidAdded, nsAdded)

		ginkgo.By("Updating ConfigMaps and verifying MODIFIED watch events")
		for i, cm := range created {
			cm.Labels["updated"] = "true"
			cm.Data["step"] = "modified"
			updated, err := cmClient.Update(ctx, cm, metav1.UpdateOptions{})
			framework.ExpectNoError(err, "failed to update ConfigMap %s", cm.Name)
			created[i] = updated
		}

		uidModified := collectEvents(uidEventCh, watch.Modified, numObjects)
		nsModified := collectEvents(nsEventCh, watch.Modified, numObjects)
		assertPartition(watch.Modified, uidModified, nsModified)

		ginkgo.By("Deleting ConfigMaps and verifying DELETED watch events")
		for _, cm := range created {
			err := cmClient.Delete(ctx, cm.Name, metav1.DeleteOptions{})
			framework.ExpectNoError(err, "failed to delete ConfigMap %s", cm.Name)
		}

		uidDeleted := collectEvents(uidEventCh, watch.Deleted, numObjects)
		nsDeleted := collectEvents(nsEventCh, watch.Deleted, numObjects)
		assertPartition(watch.Deleted, uidDeleted, nsDeleted)
	})
})
