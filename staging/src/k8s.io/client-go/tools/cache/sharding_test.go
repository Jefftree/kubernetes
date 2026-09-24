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

package cache

import (
	"context"
	"fmt"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/sharding"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
)

func TestNewShardedListWatch(t *testing.T) {
	pods := make([]v1.Pod, 16)
	for i := range pods {
		pods[i] = v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:            fmt.Sprintf("pod-%d", i),
				Namespace:       "default",
				UID:             types.UID(fmt.Sprintf("uid-%d", i)),
				ResourceVersion: "10",
			},
		}
	}

	for _, serverSharded := range []bool{true, false} {
		t.Run(fmt.Sprintf("serverSharded=%v", serverSharded), func(t *testing.T) {
			const totalShards = 4
			seen := make(map[string]int)

			for shard := range totalShards {
				sel, err := sharding.NewShardRangeSelector("object.metadata.uid", shard, totalShards)
				if err != nil {
					t.Fatalf("NewShardRangeSelector: %v", err)
				}

				var gotListSelector, gotWatchSelector string
				fakeWatch := watch.NewFake()
				baseLW := &ListWatch{
					ListWithContextFunc: func(_ context.Context, opts metav1.ListOptions) (runtime.Object, error) {
						gotListSelector = opts.ShardSelector
						out := &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "10"}}
						if serverSharded {
							out.ShardInfo = &metav1.ShardInfo{Selector: opts.ShardSelector}
							for i := range pods {
								if ok, _ := sel.Matches(&pods[i]); ok {
									out.Items = append(out.Items, pods[i])
								}
							}
						} else {
							out.Items = append(out.Items, pods...)
						}
						return out, nil
					},
					WatchFuncWithContext: func(_ context.Context, opts metav1.ListOptions) (watch.Interface, error) {
						gotWatchSelector = opts.ShardSelector
						return fakeWatch, nil
					},
				}

				shardedLW := NewShardedListWatch(baseLW, sel)
				listObj, err := shardedLW.ListWithContext(context.Background(), metav1.ListOptions{})
				if err != nil {
					t.Fatalf("ListWithContext: %v", err)
				}
				if gotListSelector != sel.String() {
					t.Errorf("expected List ShardSelector %q, got %q", sel.String(), gotListSelector)
				}

				podList := listObj.(*v1.PodList)
				for _, p := range podList.Items {
					if prev, dup := seen[p.Name]; dup {
						t.Errorf("pod %s seen in both shard %d and %d", p.Name, prev, shard)
					}
					seen[p.Name] = shard
				}

				w, err := shardedLW.WatchWithContext(context.Background(), metav1.ListOptions{})
				if err != nil {
					t.Fatalf("WatchWithContext: %v", err)
				}
				if gotWatchSelector != sel.String() {
					t.Errorf("expected Watch ShardSelector %q, got %q", sel.String(), gotWatchSelector)
				}
				go func() {
					for i := range pods {
						if !serverSharded {
							fakeWatch.Add(&pods[i])
						} else if ok, _ := sel.Matches(&pods[i]); ok {
							fakeWatch.Add(&pods[i])
						}
					}
					fakeWatch.Stop()
				}()
				watchCount := 0
				for ev := range w.ResultChan() {
					p := ev.Object.(*v1.Pod)
					if ok, _ := sel.Matches(p); !ok {
						t.Errorf("shard %d received out-of-shard watch event for %s", shard, p.Name)
					}
					watchCount++
				}
				if watchCount != len(podList.Items) {
					t.Errorf("shard %d watch count %d != list count %d", shard, watchCount, len(podList.Items))
				}
			}

			if len(seen) != len(pods) {
				t.Errorf("expected %d total pods across shards, got %d", len(pods), len(seen))
			}
		})
	}
}
