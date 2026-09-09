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
	"io"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/cacher/metrics"
	"k8s.io/apiserver/pkg/storage/cacher/store"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/tools/cache"
	testingclock "k8s.io/utils/clock/testing"
)

// testWatchCacheCodec is the codec newTestWatchCache hands to the watch cache.
// A test can substitute an instrumented codec to observe what the read paths
// actually decode. It is nil by default because corev1ProtoCodec is assigned
// in an init function, which runs after package variable initialization;
// newTestWatchCache substitutes the real codec when this is nil.
var testWatchCacheCodec runtime.Codec

// storageLikeCorev1ProtoCodec is corev1ProtoCodec made to behave like a real
// storage codec.
//
// A production storage codec decodes into a *memory* version, so conversion
// runs and the decoded object carries empty TypeMeta. corev1ProtoCodec encodes
// and decodes the same version, so it skips conversion and leaves
// apiVersion/kind populated, which no storage codec does. Without this shim
// the watch cache's round-trip guard would correctly refuse to enable lazy
// decoding for the test cache.
type storageLikeCodec struct {
	runtime.Codec
}

func (c storageLikeCodec) Decode(data []byte, defaults *schema.GroupVersionKind, into runtime.Object) (runtime.Object, *schema.GroupVersionKind, error) {
	obj, gvk, err := c.Codec.Decode(data, defaults, into)
	if obj != nil {
		obj.GetObjectKind().SetGroupVersionKind(schema.GroupVersionKind{})
	}
	return obj, gvk, err
}

// storageLikeCorev1ProtoCodec is a function rather than a variable because
// corev1ProtoCodec is assigned in an init function.
func storageLikeCorev1ProtoCodec() runtime.Codec { return storageLikeCodec{Codec: corev1ProtoCodec} }

// canonical returns pod as the storage layer would hand it to the watch cache:
// decoded, and therefore defaulted. Comparing against the pre-encode object
// would fail on defaults the decoder legitimately fills in.
func canonical(t *testing.T, pod *corev1.Pod) *corev1.Pod {
	t.Helper()
	codec := storageLikeCorev1ProtoCodec()
	raw, err := runtime.Encode(codec, pod)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := runtime.Decode(codec, raw)
	if err != nil {
		t.Fatal(err)
	}
	return obj.(*corev1.Pod)
}

// Setting LAZY_DECODE_WATCH_CACHE=true turns the gate on for the whole test
// binary, so the existing suite and the benchmarks can be run A/B against the
// same code without editing anything.
func init() {
	if os.Getenv("LAZY_DECODE_WATCH_CACHE") == "true" {
		utilruntime.Must(utilfeature.DefaultMutableFeatureGate.SetFromMap(
			map[string]bool{string(features.LazyDecodeWatchCache): true}))
	}
}

func lazyEnabled() bool {
	return utilfeature.DefaultFeatureGate.Enabled(features.LazyDecodeWatchCache)
}

// countingCodec counts decodes and encodes so a test can assert that a code
// path did not decode.
type countingCodec struct {
	runtime.Codec
	decodes atomic.Int64
	encodes atomic.Int64
}

func (c *countingCodec) Decode(data []byte, defaults *schema.GroupVersionKind, into runtime.Object) (runtime.Object, *schema.GroupVersionKind, error) {
	c.decodes.Add(1)
	return c.Codec.Decode(data, defaults, into)
}

func (c *countingCodec) Encode(obj runtime.Object, w io.Writer) error {
	c.encodes.Add(1)
	return c.Codec.Encode(obj, w)
}

// withTestWatchCacheCodec swaps the codec used by newTestWatchCache for the
// duration of a test.
func withTestWatchCacheCodec(t *testing.T, codec runtime.Codec) {
	t.Helper()
	previous := testWatchCacheCodec
	testWatchCacheCodec = codec
	t.Cleanup(func() { testWatchCacheCodec = previous })
}

func newLazyTestPod(name, namespace, nodeName, rv, labelValue string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			ResourceVersion: rv,
			Labels:          map[string]string{"app": labelValue},
			Annotations:     map[string]string{"note": "original"},
		},
		Spec: corev1.PodSpec{
			NodeName:   nodeName,
			Containers: []corev1.Container{{Name: "c1", Image: "img:1"}},
		},
	}
}

// TestLazyObjectMaterializationIsPrivate is the mutation-safety test that
// matters: two readers of the same cached object must not be able to see each
// other's writes, and neither must be able to corrupt the cache.
//
// This test can fail. Make Materialize hand back a memoized object instead of
// a fresh decode and it goes red immediately.
func TestLazyObjectMaterializationIsPrivate(t *testing.T) {
	pod := canonical(t, newLazyTestPod("p1", "ns", "node-a", "10", "before"))
	lazy, err := store.EncodeToLazyObject(storageLikeCorev1ProtoCodec(), storageLikeCorev1ProtoCodec(), pod)
	if err != nil {
		t.Fatal(err)
	}

	first, err := lazy.Materialize()
	if err != nil {
		t.Fatal(err)
	}
	second, err := lazy.Materialize()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("Materialize returned the same pointer twice; readers would share state")
	}

	// Mutate everything a caller plausibly might: a map value, a slice
	// element, and a scalar.
	firstPod := first.(*corev1.Pod)
	firstPod.Labels["app"] = "mutated"
	firstPod.Annotations["note"] = "mutated"
	firstPod.Spec.Containers[0].Image = "img:mutated"
	firstPod.Spec.NodeName = "node-mutated"

	secondPod := second.(*corev1.Pod)
	if got := secondPod.Labels["app"]; got != "before" {
		t.Errorf("label leaked between readers: got %q, want %q", got, "before")
	}
	if got := secondPod.Annotations["note"]; got != "original" {
		t.Errorf("annotation leaked between readers: got %q, want %q", got, "original")
	}
	if got := secondPod.Spec.Containers[0].Image; got != "img:1" {
		t.Errorf("container image leaked between readers: got %q, want %q", got, "img:1")
	}
	if got := secondPod.Spec.NodeName; got != "node-a" {
		t.Errorf("node name leaked between readers: got %q, want %q", got, "node-a")
	}

	// And a read taken after the mutations still sees the original.
	third, err := lazy.Materialize()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(third, pod) {
		t.Error("cached object changed after a reader mutated its copy")
	}
}

// TestLazyObjectRoundTrip asserts the cached form decodes back to the object
// that went in.
func TestLazyObjectRoundTrip(t *testing.T) {
	realistic := loadExemplarCorePod(t)
	realistic.ResourceVersion = "42"
	realistic = canonical(t, realistic)
	for _, tc := range []struct {
		name string
		pod  *corev1.Pod
	}{
		{"simple", canonical(t, newLazyTestPod("p1", "ns", "node-a", "10", "v"))},
		{"realistic", realistic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lazy, err := store.EncodeToLazyObject(storageLikeCorev1ProtoCodec(), storageLikeCorev1ProtoCodec(), tc.pod)
			if err != nil {
				t.Fatal(err)
			}
			got, err := lazy.Materialize()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.pod) {
				t.Error("round trip changed the object")
			}
			if lazy.EncodedSize() == 0 {
				t.Error("encoded size is zero")
			}
		})
	}
}

// TestLazyObjectCacheEncodeSplices asserts that an encoder matching the one
// that produced the bytes gets them verbatim with no decode and no re-encode,
// and that a different encoder still produces correct bytes exactly once.
func TestLazyObjectCacheEncodeSplices(t *testing.T) {
	pod := newLazyTestPod("p1", "ns", "node-a", "10", "v")
	counting := &countingCodec{Codec: corev1ProtoCodec}
	lazy, err := store.EncodeToLazyObject(counting, counting, pod)
	if err != nil {
		t.Fatal(err)
	}
	encodesAfterBuild := counting.encodes.Load()
	decodesAfterBuild := counting.decodes.Load()

	buf := runtime.NewSpliceBuffer()
	if err := lazy.CacheEncode(counting.Identifier(), func(runtime.Object, io.Writer) error {
		t.Error("fallback encode called despite matching identifier")
		return nil
	}, buf); err != nil {
		t.Fatal(err)
	}
	if got := counting.decodes.Load() - decodesAfterBuild; got != 0 {
		t.Errorf("splice path decoded %d times, want 0", got)
	}
	if got := counting.encodes.Load() - encodesAfterBuild; got != 0 {
		t.Errorf("splice path encoded %d times, want 0", got)
	}
	want, err := runtime.Encode(corev1ProtoCodec, pod)
	if err != nil {
		t.Fatal(err)
	}
	if got := buf.Bytes(); !reflect.DeepEqual(got, want) {
		t.Errorf("spliced %d bytes, want %d equal bytes", len(got), len(want))
	}

	// A non-matching identity must produce correct bytes every time, and must
	// NOT be memoized: this object lives as long as its cache entry, and the
	// watch path keys CacheEncode on the whole framed event, so caching here
	// would retain a full extra copy per event type forever. See
	// TestLazyObjectCacheEncodeRetainsNothing.
	var fallbackCalls int
	fallback := func(obj runtime.Object, w io.Writer) error {
		fallbackCalls++
		return corev1ProtoCodec.Encode(obj, w)
	}
	other := runtime.Identifier(`{"name":"not-the-storage-encoder"}`)
	for i := 0; i < 3; i++ {
		buf := runtime.NewSpliceBuffer()
		if err := lazy.CacheEncode(other, fallback, buf); err != nil {
			t.Fatal(err)
		}
		if got := buf.Bytes(); !reflect.DeepEqual(got, want) {
			t.Errorf("iteration %d: fallback produced different bytes", i)
		}
	}
	if fallbackCalls != 3 {
		t.Errorf("fallback encode ran %d times, want 3: results must not be retained on a cache-resident object", fallbackCalls)
	}
}

// TestLazyWatchCacheGetDoesNotAliasCache is the mutation-safety test through
// the real watch cache: what a reader gets back must not be wired into the
// cache.
//
// It only runs with the gate on. With the gate off the cacher deliberately
// hands out an object that shares its pointer graph with the cache, and this
// test would correctly fail.
func TestLazyWatchCacheGetDoesNotAliasCache(t *testing.T) {
	if !lazyEnabled() {
		t.Skip("requires LAZY_DECODE_WATCH_CACHE=true; without lazy decode the cacher intentionally aliases")
	}
	wc := newTestWatchCache(10, DefaultEventFreshDuration, &cache.Indexers{})
	defer wc.Stop()

	if err := wc.Add(newLazyTestPod("p1", "ns", "node-a", "5", "before")); err != nil {
		t.Fatal(err)
	}

	first := getStoredPod(t, wc, "/prefix/ns/p1")
	first.Labels["app"] = "mutated"
	first.Spec.Containers[0].Image = "img:mutated"

	second := getStoredPod(t, wc, "/prefix/ns/p1")
	if got := second.Labels["app"]; got != "before" {
		t.Errorf("a reader's mutation leaked into the cache: label %q", got)
	}
	if got := second.Spec.Containers[0].Image; got != "img:1" {
		t.Errorf("a reader's mutation leaked into the cache: image %q", got)
	}
}

func getStoredPod(t *testing.T, wc *testWatchCache, key string) *corev1.Pod {
	t.Helper()
	item, exists, _, err := wc.WaitUntilFreshAndGet(t.Context(), 0, key)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatalf("key %q not in cache", key)
	}
	obj, err := item.(*store.Element).TypedObject()
	if err != nil {
		t.Fatal(err)
	}
	return obj.(*corev1.Pod)
}

// TestLazySelectorListDoesNotDecodeNonMatches is the crux of the design: a
// LIST with a selector filters on the precomputed attributes and decodes only
// the items it actually returns. If filtering ever starts needing the typed
// object, this goes red.
func TestLazySelectorListDoesNotDecodeNonMatches(t *testing.T) {
	if !lazyEnabled() {
		t.Skip("requires LAZY_DECODE_WATCH_CACHE=true")
	}
	const podCount = 200

	counting := &countingCodec{Codec: storageLikeCorev1ProtoCodec()}
	withTestWatchCacheCodec(t, counting)

	wc := newTestWatchCache(podCount*2, DefaultEventFreshDuration, &cache.Indexers{})
	defer wc.Stop()
	for i := 0; i < podCount; i++ {
		label := "other"
		if i == 7 {
			label = "wanted"
		}
		pod := newLazyTestPod(fmt.Sprintf("p%03d", i), "ns", "node-a", fmt.Sprint(i+1), label)
		if err := wc.Add(pod); err != nil {
			t.Fatal(err)
		}
	}
	// Ingest must not have decoded anything beyond the one-off round-trip
	// probe the watch cache runs when it is constructed.
	if got := counting.decodes.Load(); got > 1 {
		t.Errorf("ingesting %d objects decoded %d times, want at most 1 (the construction probe)", podCount, got)
	}
	counting.decodes.Store(0)

	pred := storage.SelectionPredicate{
		Label: labels.SelectorFromSet(labels.Set{"app": "wanted"}),
		Field: fields.Everything(),
	}
	selected := 0
	for _, item := range wc.storage.List() {
		elem := item.(*store.Element)
		if !pred.MatchesObjectAttributes(elem.Labels, elem.Fields) {
			continue
		}
		if _, err := elem.TypedObject(); err != nil {
			t.Fatal(err)
		}
		selected++
	}
	if selected != 1 {
		t.Fatalf("selector matched %d items, want 1", selected)
	}
	if got := counting.decodes.Load(); got != 1 {
		t.Errorf("serving a selector LIST matching 1 of %d items decoded %d objects, want 1", podCount, got)
	}
}

// TestLazyObjectNeverEscapesTheCacher asserts the containment property the
// whole design rests on: a *store.LazyObject is an internal representation and
// must never reach a watch consumer, which may legitimately type-assert to the
// concrete API type.
//
// Every delivery path funnels through getMutableObject, which materializes.
// This test pins that. Remove the materialization and it goes red.
func TestLazyObjectNeverEscapesTheCacher(t *testing.T) {
	wc := newTestWatchCache(100, DefaultEventFreshDuration, &cache.Indexers{})
	defer wc.Stop()
	if err := wc.Add(newLazyTestPod("p1", "ns", "node-a", "5", "v")); err != nil {
		t.Fatal(err)
	}
	if err := wc.Update(newLazyTestPod("p1", "ns", "node-a", "6", "v2")); err != nil {
		t.Fatal(err)
	}

	watcher := newCacheWatcher(
		10,
		filterWithAttrsAndPrefixFunction("/prefix/", storage.Everything, schema.GroupResource{Resource: "pods"}),
		func(bool) {},
		storage.APIObjectVersioner{},
		time.Time{},
		false,
		schema.GroupResource{Resource: "pods"},
		metrics.NewWatcherMetricsObservers(schema.GroupResource{Resource: "pods"}),
		testingclock.NewFakeClock(time.Now()),
		"test",
	)
	defer watcher.stopLocked()

	interval, err := wc.getCacheIntervalForEvents(0, storage.ListOptions{Predicate: storage.Everything, Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go watcher.processInterval(ctx, interval, wc.resourceVersion)

	select {
	case event, ok := <-watcher.ResultChan():
		if !ok {
			t.Fatal("watch channel closed without an event")
		}
		if _, bad := event.Object.(*store.LazyObject); bad {
			t.Fatalf("watch delivered an internal *store.LazyObject to a consumer")
		}
		if _, good := event.Object.(*corev1.Pod); !good {
			t.Fatalf("watch delivered %T, want *corev1.Pod", event.Object)
		}
	case <-time.After(wait.ForeverTestTimeout):
		t.Fatal("timed out waiting for an initial event")
	}
}

// TestLazyDeleteEventCarriesCurrentResourceVersion pins the nastiest failure
// mode this design can have.
//
// When a watcher's selector stops matching an object, the cacher synthesizes a
// DELETE carrying the object's previous content stamped with the CURRENT
// resource version. That stamping goes through meta.Accessor, which fails on a
// wrapper that does not implement metav1.Object. The failure is only logged,
// so a broken implementation ships a DELETE with a stale resourceVersion and a
// client resuming from it silently rewinds. Nothing crashes.
//
// The same path is taken by real deletes replayed from the history ring
// buffer, which never go through setCachingObjects.
func TestLazyDeleteEventCarriesCurrentResourceVersion(t *testing.T) {
	wc := newTestWatchCache(100, DefaultEventFreshDuration, &cache.Indexers{})
	defer wc.Stop()

	// The object matches the watcher's selector, then stops matching.
	if err := wc.Add(newLazyTestPod("p1", "ns", "node-a", "5", "wanted")); err != nil {
		t.Fatal(err)
	}
	if err := wc.Update(newLazyTestPod("p1", "ns", "node-a", "9", "other")); err != nil {
		t.Fatal(err)
	}

	elem, exists, err := wc.storage.GetByKey("/prefix/ns/p1")
	if err != nil || !exists {
		t.Fatalf("GetByKey: exists=%v err=%v", exists, err)
	}
	if _, isLazy := elem.(*store.Element).Object.(*store.LazyObject); isLazy != lazyEnabled() {
		t.Fatalf("stored object lazy=%v, want %v", isLazy, lazyEnabled())
	}

	// Build the transition event by hand: the previous version is what the
	// cache held for the matching version, left in whatever form it stores.
	prev, err := store.EncodeToLazyObject(storageLikeCorev1ProtoCodec(), storageLikeCorev1ProtoCodec(),
		canonical(t, newLazyTestPod("p1", "ns", "node-a", "5", "wanted")))
	if err != nil {
		t.Fatal(err)
	}
	var prevObject runtime.Object = prev
	if !lazyEnabled() {
		if prevObject, err = prev.Materialize(); err != nil {
			t.Fatal(err)
		}
	}

	pred := storage.SelectionPredicate{
		Label: labels.SelectorFromSet(labels.Set{"app": "wanted"}),
		Field: fields.Everything(),
	}
	watcher := newCacheWatcher(
		10,
		filterWithAttrsAndPrefixFunction("/prefix/", pred, schema.GroupResource{Resource: "pods"}),
		func(bool) {}, storage.APIObjectVersioner{}, time.Time{}, false,
		schema.GroupResource{Resource: "pods"},
		metrics.NewWatcherMetricsObservers(schema.GroupResource{Resource: "pods"}),
		testingclock.NewFakeClock(time.Now()), "test",
	)
	defer watcher.stopLocked()

	const deleteRV = 9
	event := watcher.convertToWatchEvent(&watchCacheEvent{
		Type:            watch.Modified,
		Object:          nil,
		ObjLabels:       labels.Set{"app": "other"},
		ObjFields:       fields.Set{},
		PrevObject:      prevObject,
		PrevObjLabels:   labels.Set{"app": "wanted"},
		PrevObjFields:   fields.Set{},
		Key:             "/prefix/ns/p1",
		ResourceVersion: deleteRV,
	})
	if event == nil {
		t.Fatal("expected a synthesized DELETE event, got none")
	}
	if event.Type != watch.Deleted {
		t.Fatalf("got event type %v, want DELETED", event.Type)
	}
	if _, bad := event.Object.(*store.LazyObject); bad {
		t.Fatal("DELETE event carries an internal *store.LazyObject")
	}
	got, err := storage.APIObjectVersioner{}.ObjectResourceVersion(event.Object)
	if err != nil {
		t.Fatalf("DELETE event object has unreadable metadata: %v", err)
	}
	if got != deleteRV {
		t.Errorf("DELETE event carries resourceVersion %d, want %d; a client resuming from it would rewind", got, deleteRV)
	}
	// And the cached previous version must not have been mutated by the stamp.
	stillPrev, err := prev.Materialize()
	if err != nil {
		t.Fatal(err)
	}
	if rv := stillPrev.(*corev1.Pod).ResourceVersion; rv != "5" {
		t.Errorf("stamping the delete event mutated the cached object: resourceVersion is now %q, want \"5\"", rv)
	}
}

// TestLazyStoreRetainsNoDecodedObjects asserts the store really is holding
// encoded bytes, not typed objects. Without this the retention benchmark
// could be measuring nothing.
func TestLazyStoreRetainsNoDecodedObjects(t *testing.T) {
	wc := newTestWatchCache(10, DefaultEventFreshDuration, &cache.Indexers{})
	defer wc.Stop()
	if err := wc.Add(newLazyTestPod("p1", "ns", "node-a", "5", "v")); err != nil {
		t.Fatal(err)
	}
	item, exists, err := wc.storage.GetByKey("/prefix/ns/p1")
	if err != nil || !exists {
		t.Fatalf("GetByKey: exists=%v err=%v", exists, err)
	}
	_, isLazy := item.(*store.Element).Object.(*store.LazyObject)
	if isLazy != lazyEnabled() {
		t.Errorf("stored object lazy=%v, want %v (feature gate)", isLazy, lazyEnabled())
	}
}
