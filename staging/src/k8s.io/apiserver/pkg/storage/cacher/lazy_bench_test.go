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
	"context"
	"fmt"
	goruntime "runtime"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/cacher/store"
	"k8s.io/client-go/tools/cache"
)

// benchPods builds n distinct realistic pods, sized like the ones the
// ClusterLoader2 write-throughput test uses. One pod in a hundred carries a
// distinguishing label so a selective LIST has something to match.
//
// Each pod is produced by an independent decode rather than by DeepCopy of a
// shared exemplar. That matters for retention measurements: Go's DeepCopy
// copies string headers, so DeepCopied pods share one allocation per distinct
// string with the exemplar, while pods decoded from the wire each own theirs.
// The storage layer decodes, so decoding is the faithful shape.
func benchPods(tb testing.TB, n int) []*corev1.Pod {
	tb.Helper()
	codec := storageLikeCorev1ProtoCodec()
	exemplar := loadExemplarCorePod(tb)
	exemplar.Namespace = "ns"
	pods := make([]*corev1.Pod, 0, n)
	for i := 0; i < n; i++ {
		exemplar.Name = fmt.Sprintf("pod-%06d", i)
		exemplar.ResourceVersion = strconv.Itoa(i + 1)
		exemplar.Spec.NodeName = fmt.Sprintf("node-%d", i%5000)
		if exemplar.Labels == nil {
			exemplar.Labels = map[string]string{}
		}
		exemplar.Labels["bench-group"] = "common"
		if i%100 == 0 {
			exemplar.Labels["bench-group"] = "rare"
		}
		raw, err := runtime.Encode(codec, exemplar)
		if err != nil {
			tb.Fatal(err)
		}
		obj, err := runtime.Decode(codec, raw)
		if err != nil {
			tb.Fatal(err)
		}
		pods = append(pods, obj.(*corev1.Pod))
	}
	return pods
}

func newBenchWatchCache(tb testing.TB, pods []*corev1.Pod) *testWatchCache {
	tb.Helper()
	wc := newTestWatchCache(len(pods)*2+100, DefaultEventFreshDuration, &cache.Indexers{})
	tb.Cleanup(wc.Stop)
	for _, pod := range pods {
		if err := wc.Add(pod); err != nil {
			tb.Fatal(err)
		}
	}
	return wc
}

// listAllFromCache mirrors what Cacher.GetList does for an unfiltered LIST:
// walk every element and materialize it.
func listAllFromCache(tb testing.TB, wc *testWatchCache) int {
	n := 0
	for _, item := range wc.storage.List() {
		obj, err := item.(*store.Element).TypedObject()
		if err != nil {
			tb.Fatal(err)
		}
		if obj != nil {
			n++
		}
	}
	return n
}

// listMatchingFromCache mirrors the filtered LIST path: match on the
// precomputed attributes first, materialize only what is selected.
func listMatchingFromCache(tb testing.TB, wc *testWatchCache, pred storage.SelectionPredicate) int {
	n := 0
	for _, item := range wc.storage.List() {
		elem := item.(*store.Element)
		if !pred.MatchesObjectAttributes(elem.Labels, elem.Fields) {
			continue
		}
		obj, err := elem.TypedObject()
		if err != nil {
			tb.Fatal(err)
		}
		if obj != nil {
			n++
		}
	}
	return n
}

func groupPredicate(value string) storage.SelectionPredicate {
	return storage.SelectionPredicate{
		Label: labels.SelectorFromSet(labels.Set{"bench-group": value}),
		Field: fields.Everything(),
	}
}

// BenchmarkLazyIngest measures the write path: one Add per iteration into a
// warm cache. With lazy decoding this pays an encode it did not pay before.
func BenchmarkLazyIngest(b *testing.B) {
	pods := benchPods(b, 1000)
	wc := newBenchWatchCache(b, pods)
	updates := make([]*corev1.Pod, len(pods))
	for i, p := range pods {
		u := p.DeepCopy()
		u.ResourceVersion = strconv.Itoa(len(pods) + i + 1)
		updates[i] = u
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		u := updates[i%len(updates)]
		u.ResourceVersion = strconv.Itoa(len(pods) + i + 1)
		if err := wc.Update(u); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLazyListAll measures an unfiltered LIST of 1000 realistic pods.
// This is where lazy decoding costs the most: every returned item is decoded.
func BenchmarkLazyListAll(b *testing.B) {
	wc := newBenchWatchCache(b, benchPods(b, 1000))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := listAllFromCache(b, wc); got != 1000 {
			b.Fatalf("listed %d, want 1000", got)
		}
	}
}

// BenchmarkLazyListSelector1Pct measures a LIST whose selector matches 1% of
// the cache. The other 99% are rejected on precomputed attributes and never
// decoded, which is the property the design depends on.
func BenchmarkLazyListSelector1Pct(b *testing.B) {
	wc := newBenchWatchCache(b, benchPods(b, 1000))
	pred := groupPredicate("rare")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := listMatchingFromCache(b, wc, pred); got != 10 {
			b.Fatalf("matched %d, want 10", got)
		}
	}
}

// BenchmarkLazyListSelectorNoMatch is the CPU negative control. Nothing
// matches, so nothing is decoded in either arm and the two must be equal. If
// this moves, the comparison in the other LIST benchmarks is contaminated.
func BenchmarkLazyListSelectorNoMatch(b *testing.B) {
	wc := newBenchWatchCache(b, benchPods(b, 1000))
	pred := groupPredicate("absent")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := listMatchingFromCache(b, wc, pred); got != 0 {
			b.Fatalf("matched %d, want 0", got)
		}
	}
}

// responseEncoder is the encoder a client asking for protobuf v1 gets. It is
// built the way the endpoint layer builds one, which is NOT the way the
// storage layer builds the storage encoder: same bytes, different
// runtime.Identifier.
func responseEncoder() runtime.Encoder {
	return codecs.EncoderForVersion(protobuf.NewSerializer(scheme, scheme), corev1.SchemeGroupVersion)
}

// serveToWire produces the bytes a response would carry for one cached object.
// This is the fair comparison: a LIST or GET does not stop at the cache, it
// has to put bytes on a socket, and both arms pay for that.
func serveToWire(tb testing.TB, enc runtime.Encoder, obj runtime.Object, buf runtime.Splice) {
	buf.Reset()
	if err := enc.Encode(obj, buf); err != nil {
		tb.Fatal(err)
	}
	if len(buf.Bytes()) == 0 {
		tb.Fatal("encoded to zero bytes")
	}
}

// BenchmarkLazyListAllToWire measures an unfiltered LIST of 1000 realistic
// pods all the way to wire bytes. The non-lazy arm re-encodes every item on
// every request; the lazy arm decodes and encodes once per object and splices
// afterwards, which is what makes the steady state cheaper despite the decode.
func BenchmarkLazyListAllToWire(b *testing.B) {
	wc := newBenchWatchCache(b, benchPods(b, 1000))
	enc := responseEncoder()
	buf := runtime.NewSpliceBuffer()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, item := range wc.storage.List() {
			serveToWire(b, enc, item.(*store.Element).Object, buf)
		}
	}
}

// BenchmarkLazyListAllToWireFirstTouch is the same measurement with nothing
// memoized, which is the lazy arm's worst case: decode plus encode per item.
func BenchmarkLazyListAllToWireFirstTouch(b *testing.B) {
	pods := benchPods(b, 1000)
	enc := responseEncoder()
	buf := runtime.NewSpliceBuffer()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		wc := newBenchWatchCache(b, pods)
		b.StartTimer()
		for _, item := range wc.storage.List() {
			serveToWire(b, enc, item.(*store.Element).Object, buf)
		}
	}
}

// BenchmarkLazyGet measures a single-object read from the cache.
func BenchmarkLazyGet(b *testing.B) {
	pods := benchPods(b, 1000)
	wc := newBenchWatchCache(b, pods)
	ctx := context.Background()
	keys := make([]string, len(pods))
	for i, p := range pods {
		keys[i] = "/prefix/ns/" + p.Name
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		item, exists, _, err := wc.WaitUntilFreshAndGet(ctx, 0, keys[i%len(keys)])
		if err != nil || !exists {
			b.Fatalf("get: exists=%v err=%v", exists, err)
		}
		if _, err := item.(*store.Element).TypedObject(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLazyWatchInitialEvents measures the watch-list bootstrap path:
// build the interval over the whole store and materialize every event, the
// way a watcher receiving initial events does.
func BenchmarkLazyWatchInitialEvents(b *testing.B) {
	wc := newBenchWatchCache(b, benchPods(b, 1000))
	opts := storage.ListOptions{Predicate: storage.Everything, Recursive: true}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		interval, err := wc.getCacheIntervalForEvents(0, opts)
		if err != nil {
			b.Fatal(err)
		}
		count := 0
		for {
			event, err := interval.Next()
			if err != nil {
				b.Fatal(err)
			}
			if event == nil {
				break
			}
			// getMutableObject is what the watcher applies before delivery.
			if getMutableObject(event.Object) == nil {
				b.Fatal("nil object")
			}
			count++
		}
		if count != 1000 {
			b.Fatalf("got %d initial events, want 1000", count)
		}
	}
}

// TestLazyWatchCacheRetention reports the live heap held by a watch cache of
// 10,000 realistic pods. Run it under both settings of
// LAZY_DECODE_WATCH_CACHE and compare:
//
//	go test ./pkg/storage/cacher/ -run TestLazyWatchCacheRetention -v -count=1
//	LAZY_DECODE_WATCH_CACHE=true go test ./pkg/storage/cacher/ -run TestLazyWatchCacheRetention -v -count=1
func TestLazyWatchCacheRetention(t *testing.T) {
	const podCount = 10000

	// Warm the codec, scheme and metric state so none of it is charged to the
	// measured cache. Everything here dies before the first reading.
	func() {
		warm := newTestWatchCache(16, DefaultEventFreshDuration, &cache.Indexers{})
		defer warm.Stop()
		if err := warm.Add(benchPods(t, 1)[0]); err != nil {
			t.Fatal(err)
		}
	}()

	// Negative control, measured and released before the cache so the two
	// readings cannot contaminate each other. Keys and label/field sets are
	// built by identical code in both arms, so this number must not move; if
	// it does, the cache comparison below is not measuring what it claims.
	controlBytes, controlObjs := measureBareElements(t, podCount)

	baseBytes, baseObjs := liveHeap()

	wc := newTestWatchCache(podCount*2, DefaultEventFreshDuration, &cache.Indexers{})
	defer wc.Stop()
	// The pods must not outlive this scope: the cache has to be the only thing
	// retaining them, otherwise the non-lazy arm looks free because it shares
	// pointers with the slice that built it.
	func() {
		for _, pod := range benchPods(t, podCount) {
			if err := wc.Add(pod); err != nil {
				t.Fatal(err)
			}
		}
	}()

	withAllBytes, withAllObjs := liveHeap()

	// The history ring buffer holds every event with its object decoded, in
	// both arms, because this change deliberately leaves the dispatch path
	// alone. Drop it to isolate the store, then report both numbers.
	wc.Lock()
	wc.history.ResetLocked()
	wc.Unlock()
	withStoreBytes, withStoreObjs := liveHeap()

	storeBytes := int64(withStoreBytes) - int64(baseBytes)
	storeObjs := int64(withStoreObjs) - int64(baseObjs)
	allBytes := int64(withAllBytes) - int64(baseBytes)
	allObjs := int64(withAllObjs) - int64(baseObjs)

	lazy := 0
	for _, item := range wc.storage.List() {
		if _, ok := item.(*store.Element).Object.(*store.LazyObject); ok {
			lazy++
		}
	}

	t.Logf("LazyDecodeWatchCache=%v  elements stored lazily=%d/%d", lazyEnabled(), lazy, podCount)
	t.Logf("store only:      %8.1f MB  %9d heap objects  (%6.0f B, %6.1f objects per pod)",
		float64(storeBytes)/(1<<20), storeObjs,
		float64(storeBytes)/podCount, float64(storeObjs)/podCount)
	t.Logf("store + history: %8.1f MB  %9d heap objects  (%6.0f B, %6.1f objects per pod)",
		float64(allBytes)/(1<<20), allObjs,
		float64(allBytes)/podCount, float64(allObjs)/podCount)
	t.Logf("negative control (keys + label/field sets, untouched by this change): %8.1f MB  %9d heap objects",
		float64(controlBytes)/(1<<20), controlObjs)

	goruntime.KeepAlive(wc)
}

// TestLazyWatchCacheChurnRetention measures whole-watch-cache retention for a
// steady pod population under sustained updates, swept over the size of the
// history ring buffer.
//
// The sweep is the point. Storing the store encoded only helps to the extent
// that the store is what retains the objects. Today the history buffer and the
// store SHARE one decoded object per live key, so with a large history the
// store's copy is free and encoding it adds a second representation instead of
// replacing one. The crossover is where the win appears.
func TestLazyWatchCacheChurnRetention(t *testing.T) {
	const podCount = 2000
	const updatesPerPod = 5

	for _, historyCapacity := range []int{100, 1000, 4000, 12000} {
		t.Run(fmt.Sprintf("history=%d", historyCapacity), func(t *testing.T) {
			func() {
				warm := newTestWatchCache(16, DefaultEventFreshDuration, &cache.Indexers{})
				defer warm.Stop()
				if err := warm.Add(benchPods(t, 1)[0]); err != nil {
					t.Fatal(err)
				}
			}()

			baseBytes, baseObjs := liveHeap()

			wc := newTestWatchCache(historyCapacity, DefaultEventFreshDuration, &cache.Indexers{})
			defer wc.Stop()
			// Pin the ring buffer: newTestWatchCache leaves the upper bound at
			// the production default, so it would just grow back and the sweep
			// would measure one point four times.
			wc.history.lowerBoundCapacity = historyCapacity
			wc.history.upperBoundCapacity = historyCapacity
			rv := 0
			func() {
				pods := benchPods(t, podCount)
				for _, pod := range pods {
					rv++
					pod.ResourceVersion = strconv.Itoa(rv)
					if err := wc.Add(pod); err != nil {
						t.Fatal(err)
					}
				}
				for round := 0; round < updatesPerPod; round++ {
					for _, pod := range pods {
						rv++
						updated := pod.DeepCopy()
						updated.ResourceVersion = strconv.Itoa(rv)
						updated.Annotations["churn"] = strconv.Itoa(rv)
						if err := wc.Update(updated); err != nil {
							t.Fatal(err)
						}
					}
				}
			}()

			totalBytes, totalObjs := liveHeap()
			storedEvents := 0
			for _, e := range wc.history.cache {
				if e != nil {
					storedEvents++
				}
			}
			t.Logf("lazy=%-5v history cap=%-6d events held=%-6d  whole cache: %7.1f MB  %9d heap objects",
				lazyEnabled(), historyCapacity, storedEvents,
				float64(int64(totalBytes)-int64(baseBytes))/(1<<20), int64(totalObjs)-int64(baseObjs))

			goruntime.KeepAlive(wc)
		})
	}
}

// TestLazyGCCost measures the thing the whole change is for: how much CPU the
// garbage collector spends because of what the watch cache retains.
//
// Method: hold a watch cache of 10,000 realistic pods live, then run an
// allocation workload that is byte-for-byte identical in both arms, and read
// off how much of the process's CPU went to GC. Because the workload is the
// same, any difference is attributable to the cost of marking the live heap,
// which is what storing objects encoded changes.
//
// Under GOGC the collector runs when the heap doubles, so a smaller live heap
// means more frequent but individually cheaper cycles. The quantity that
// actually moves is mark work per collected byte, which scales with pointer
// count, not with bytes. That is why heap objects is the number to watch.
//
//	go test ./pkg/storage/cacher/ -run TestLazyGCCost -v -count=1
//	LAZY_DECODE_WATCH_CACHE=true go test ./pkg/storage/cacher/ -run TestLazyGCCost -v -count=1
func TestLazyGCCost(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates several GB of garbage")
	}
	const podCount = 10000
	const garbageBytes = 4 << 30
	const chunk = 4096

	wc := newTestWatchCache(podCount*2, DefaultEventFreshDuration, &cache.Indexers{})
	defer wc.Stop()
	func() {
		for _, pod := range benchPods(t, podCount) {
			if err := wc.Add(pod); err != nil {
				t.Fatal(err)
			}
		}
	}()

	liveBytes, liveObjs := liveHeap()

	var before, after goruntime.MemStats
	goruntime.ReadMemStats(&before)
	start := time.Now()

	// The identical workload: allocate and drop the same number of bytes in
	// the same shape in both arms.
	var sink []byte
	for allocated := 0; allocated < garbageBytes; allocated += chunk {
		sink = make([]byte, chunk)
		sink[0] = byte(allocated)
	}
	goruntime.KeepAlive(sink)

	elapsed := time.Since(start)
	goruntime.ReadMemStats(&after)
	goruntime.KeepAlive(wc)

	cycles := after.NumGC - before.NumGC
	pause := time.Duration(after.PauseTotalNs - before.PauseTotalNs)
	t.Logf("LazyDecodeWatchCache=%v", lazyEnabled())
	t.Logf("live heap while running: %.1f MB / %d heap objects", float64(liveBytes)/(1<<20), liveObjs)
	t.Logf("allocated %d MB of identical garbage in %v", garbageBytes>>20, elapsed.Round(time.Millisecond))
	t.Logf("GC cycles=%d  stop-the-world pause total=%v  GCCPUFraction=%.4f",
		cycles, pause.Round(time.Microsecond), after.GCCPUFraction)
	t.Logf("estimated GC CPU over the run: %v", time.Duration(after.GCCPUFraction*float64(elapsed)).Round(time.Millisecond))
}

// measureBareElements reports the live heap held by n Elements carrying only
// the parts of a cache entry that lazy decoding does not change.
func measureBareElements(t *testing.T, n int) (bytes, objects int64) {
	t.Helper()
	base, baseObjs := liveHeap()
	control := make([]*store.Element, 0, n)
	func() {
		for _, pod := range benchPods(t, n) {
			control = append(control, &store.Element{
				Key:    "/prefix/ns/" + pod.Name,
				Labels: labels.Set(pod.Labels),
				Fields: fields.Set{"spec.nodeName": pod.Spec.NodeName},
			})
		}
	}()
	with, withObjs := liveHeap()
	goruntime.KeepAlive(control)
	return int64(with) - int64(base), int64(withObjs) - int64(baseObjs)
}

var _ = runtime.Object(nil)
