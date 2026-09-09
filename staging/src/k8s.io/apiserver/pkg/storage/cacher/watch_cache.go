/*
Copyright 2015 The Kubernetes Authors.

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
	"reflect"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/cacher/delegator"
	"k8s.io/apiserver/pkg/storage/cacher/metrics"
	"k8s.io/apiserver/pkg/storage/cacher/progress"
	"k8s.io/apiserver/pkg/storage/cacher/store"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/tools/cache"
	"k8s.io/component-base/tracing"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
)

const (
	// blockTimeout determines how long we're willing to block the request
	// to wait for a given resource version to be propagated to cache,
	// before terminating request and returning Timeout error with retry
	// after suggestion.
	blockTimeout = 3 * time.Second

	// resourceVersionTooHighRetrySeconds is the seconds before a operation should be retried by the client
	// after receiving a 'too high resource version' error.
	resourceVersionTooHighRetrySeconds = 1
)

// watchCacheEvent is a single "watch event" that is send to users of
// watchCache. Additionally to a typical "watch.Event" it contains
// the previous value of the object to enable proper filtering in the
// upper layers.
type watchCacheEvent struct {
	Type          watch.EventType
	Object        runtime.Object
	ObjLabels     labels.Set
	ObjFields     fields.Set
	PrevObject    runtime.Object
	PrevObjLabels labels.Set
	PrevObjFields fields.Set
	// TriggerValue and PrevTriggerValue are the cacher's indexedTrigger value
	// for Object and PrevObject, computed on ingest. They are precomputed
	// because dispatch reads them for every event and Object may be a
	// store.LazyObject that would otherwise have to be decoded for them.
	// HasTriggerValue distinguishes "no trigger configured" from "empty value".
	TriggerValue     string
	PrevTriggerValue string
	HasTriggerValue  bool
	// dispatchObject carries the decoded object to the dispatch goroutine so it
	// does not have to re-decode what the storage layer decoded moments ago.
	//
	// It is cleared from the event that the history ring buffer retains, right
	// after the handler has taken its copy, because retaining it there would
	// reintroduce exactly the decoded-object retention this change removes.
	// Events replayed from history therefore have it nil and materialize.
	dispatchObject  runtime.Object
	Key             string
	ResourceVersion uint64
	RecordTime      time.Time
	// timeline carries the shared, pre-fan-out dispatch-lifecycle timestamps of
	// this event (currently PointCacheReceived). Per-watcher points are filled in
	// on delivery.
	timeline metrics.DispatchTimeline
}

// watchCache implements a Store interface.
// However, it depends on the elements implementing runtime.Object interface.
//
// watchCache is a "sliding window" (with a limited capacity) of objects
// observed from a watch.
type watchCache struct {
	sync.RWMutex

	// Condition on which lists are waiting for the fresh enough
	// resource version.
	cond *sync.Cond

	// ResourceVersion up to which the watchCache is propagated.
	resourceVersion uint64

	// This handler is run at the end of every successful Replace() method.
	onReplace func()

	history *watchCacheHistory
	storage *store.WatchCacheStorage

	config *ImmutableWatchCacheConfig
}

type ImmutableWatchCacheConfig struct {
	// keyFunc is used to get a key in the underlying storage for a given object.
	keyFunc func(runtime.Object) (string, error)

	// getAttrsFunc is used to get labels and fields of an object.
	getAttrsFunc func(runtime.Object) (labels.Set, fields.Set, error)

	// This handler is run at the end of every Add/Update/Delete method
	// and additionally gets the previous value of the object.
	eventHandler func(*watchCacheEvent)

	// for testing timeouts.
	clock clock.Clock

	// An underlying storage.Versioner.
	versioner storage.Versioner

	// cacher's group resource
	groupResource schema.GroupResource

	// For testing cache interval invalidation.
	indexValidator indexValidator

	// Requests progress notification if there are requests waiting for watch
	// to be fresh
	waitingUntilFresh *progress.ConditionalProgressRequester

	getCurrentRV func(context.Context) (uint64, error)

	// lazyDecode makes the cache retain objects in their encoded form and
	// decode them only when a reader needs the typed representation.
	lazyDecode bool

	// codec is the storage codec. Only used when lazyDecode is set: to produce
	// the encoded form on ingest, and to decode it back on demand.
	codec runtime.Codec

	// objectType is the concrete type this cacher's codec was verified against.
	objectType reflect.Type

	// indexers and triggerFunc are evaluated eagerly on ingest for lazy
	// elements, because both are read on paths that must not decode.
	indexers    *cache.Indexers
	triggerFunc storage.IndexerFunc
}

func newWatchCache(
	keyFunc func(runtime.Object) (string, error),
	eventHandler func(*watchCacheEvent),
	getAttrsFunc func(runtime.Object) (labels.Set, fields.Set, error),
	versioner storage.Versioner,
	indexers *cache.Indexers,
	clock clock.WithTicker,
	eventFreshDuration time.Duration,
	groupResource schema.GroupResource,
	progressRequester *progress.ConditionalProgressRequester,
	getCurrentRV func(context.Context) (uint64, error),
	codec runtime.Codec,
	newFunc func() runtime.Object,
	triggerFunc storage.IndexerFunc,
) *watchCache {
	config := &ImmutableWatchCacheConfig{
		keyFunc:           keyFunc,
		getAttrsFunc:      getAttrsFunc,
		eventHandler:      eventHandler,
		clock:             clock,
		versioner:         versioner,
		groupResource:     groupResource,
		waitingUntilFresh: progressRequester,
		getCurrentRV:      getCurrentRV,
		lazyDecode:        utilfeature.DefaultFeatureGate.Enabled(features.LazyDecodeWatchCache) && codecRoundTripsCleanly(codec, newFunc, groupResource),
		codec:             codec,
		objectType:        objectTypeOf(newFunc),
		indexers:          indexers,
		triggerFunc:       triggerFunc,
	}

	wc := &watchCache{
		resourceVersion: 0,
		config:          config,
		history:         newWatchCacheHistory(config, eventFreshDuration),
		storage:         store.NewWatchCacheStorage(config.keyFunc, indexers),
	}
	wc.cond = sync.NewCond(wc.RLocker())
	wc.config.indexValidator = wc.history.isIndexValidLocked

	return wc
}

// Add takes runtime.Object as an argument.
func (w *watchCache) Add(obj interface{}) error {
	object, resourceVersion, err := w.objectToVersionedRuntimeObject(obj)
	if err != nil {
		return err
	}
	event := watch.Event{Type: watch.Added, Object: object}

	return w.processEvent(event, resourceVersion)
}

// Update takes runtime.Object as an argument.
func (w *watchCache) Update(obj interface{}) error {
	object, resourceVersion, err := w.objectToVersionedRuntimeObject(obj)
	if err != nil {
		return err
	}
	event := watch.Event{Type: watch.Modified, Object: object}

	return w.processEvent(event, resourceVersion)
}

// Delete takes runtime.Object as an argument.
func (w *watchCache) Delete(obj interface{}) error {
	object, resourceVersion, err := w.objectToVersionedRuntimeObject(obj)
	if err != nil {
		return err
	}
	event := watch.Event{Type: watch.Deleted, Object: object}

	return w.processEvent(event, resourceVersion)
}

// objectTypeOf is the concrete type a cacher stores, or nil if unknown.
func objectTypeOf(newFunc func() runtime.Object) reflect.Type {
	if newFunc == nil {
		return nil
	}
	return reflect.TypeOf(newFunc())
}

// codecRoundTripsCleanly reports whether storing objects encoded with this
// codec is observationally equivalent to storing them decoded.
//
// Everything a reader gets from a lazy cache has been through encode followed
// by decode. That is only safe if the pair is the identity, which requires the
// codec's decode target to be the version the cache holds. A codec that
// decodes into the same version it encodes to skips conversion and leaves
// TypeMeta populated, for instance, which would silently change what the cache
// hands out. Probing with an empty object catches that class of asymmetry; it
// cannot catch a field that only some objects populate.
func codecRoundTripsCleanly(codec runtime.Codec, newFunc func() runtime.Object, groupResource schema.GroupResource) bool {
	if codec == nil || newFunc == nil {
		return false
	}
	probe := newFunc()
	raw, err := runtime.Encode(codec, probe)
	if err != nil {
		klog.V(2).InfoS("Disabling lazy watch cache decoding: probe object does not encode", "groupResource", groupResource, "err", err)
		return false
	}
	decoded, err := runtime.Decode(codec, raw)
	if err != nil {
		klog.V(2).InfoS("Disabling lazy watch cache decoding: probe object does not decode", "groupResource", groupResource, "err", err)
		return false
	}
	if !apiequality.Semantic.DeepEqual(probe, decoded) {
		klog.V(2).InfoS("Disabling lazy watch cache decoding: codec does not round trip cleanly", "groupResource", groupResource)
		return false
	}
	return true
}

// newElement builds the unit the cache retains. When lazy decoding is on it
// encodes the object and keeps only the bytes, so everything derived from the
// typed object -- attributes, index values, the trigger value -- has to be
// computed here, while the decoded object is still in hand.
func (w *watchCache) newElement(key string, object runtime.Object) (*store.Element, error) {
	objLabels, objFields, err := w.config.getAttrsFunc(object)
	if err != nil {
		return nil, err
	}
	elem := &store.Element{
		Key:    key,
		Object: object,
		Labels: objLabels,
		Fields: objFields,
	}
	if w.config.triggerFunc != nil {
		elem.TriggerValue = w.config.triggerFunc(object)
	}
	if !w.config.lazyDecode {
		return elem, nil
	}
	// The round-trip guard at construction only proves the codec is faithful
	// for the type this cacher is configured for. An object of any other type
	// may still encode "successfully" under that codec and come back as
	// something else entirely, losing fields with no error anywhere. Retain
	// such an object decoded rather than convert it silently.
	if reflect.TypeOf(object) != w.config.objectType {
		klog.V(2).InfoS("Falling back to storing a decoded object in the watch cache: unexpected type",
			"groupResource", w.config.groupResource, "got", fmt.Sprintf("%T", object), "want", w.config.objectType)
		metrics.RecordLazyEncodeFallback(w.config.groupResource)
		return elem, nil
	}
	lazy, err := store.EncodeToLazyObject(w.config.codec, w.config.codec, object)
	if err != nil {
		// Storing the decoded object is always correct, just more expensive.
		// Failing the write instead would take the whole cache unready over an
		// optimization, so degrade rather than break.
		klog.V(2).ErrorS(err, "Falling back to storing a decoded object in the watch cache", "groupResource", w.config.groupResource)
		metrics.RecordLazyEncodeFallback(w.config.groupResource)
		return elem, nil
	}
	indexValues, err := store.ComputeIndexValues(w.config.indexers, object)
	if err != nil {
		return nil, err
	}
	elem.Object = lazy
	elem.IndexValues = indexValues
	return elem, nil
}

func (w *watchCache) objectToVersionedRuntimeObject(obj interface{}) (runtime.Object, uint64, error) {
	object, ok := obj.(runtime.Object)
	if !ok {
		return nil, 0, fmt.Errorf("obj does not implement runtime.Object interface: %v", obj)
	}
	resourceVersion, err := w.config.versioner.ObjectResourceVersion(object)
	if err != nil {
		return nil, 0, err
	}
	return object, resourceVersion, nil
}

// processEvent is safe as long as there is at most one call to it in flight
// at any point in time.
func (w *watchCache) processEvent(event watch.Event, resourceVersion uint64) error {
	cacheReceived := w.config.clock.Now()
	recordTime := cacheReceived
	if withRecordTime, ok := event.Object.(storage.WatchEventWithRecordTime); ok {
		recordTime = withRecordTime.RecordTime()
		event.Object = withRecordTime.Unwrap()
	}

	metrics.EventsReceivedCounter.WithLabelValues(w.config.groupResource.Group, w.config.groupResource.Resource).Inc()

	key, err := w.config.keyFunc(event.Object)
	if err != nil {
		return fmt.Errorf("couldn't compute key: %v", err)
	}
	elem, err := w.newElement(key, event.Object)
	if err != nil {
		return err
	}

	wcEvent := &watchCacheEvent{
		Type: event.Type,
		// Deliberately the same object the store holds, which is the encoded
		// form when lazy decoding is on. The history ring buffer keeps this
		// event for the whole freshness window, so if it held a separate
		// decoded copy the cache would retain two representations of every
		// object instead of one, and encoding the store would add memory
		// rather than save it.
		//
		// Dispatch materializes it (see setCachingObjects), and does so on a
		// shallow copy of this event, so what the ring buffer retains stays
		// encoded.
		Object:          elem.Object,
		dispatchObject:  event.Object,
		ObjLabels:       elem.Labels,
		ObjFields:       elem.Fields,
		TriggerValue:    elem.TriggerValue,
		HasTriggerValue: w.config.triggerFunc != nil,
		Key:             key,
		ResourceVersion: resourceVersion,
		RecordTime:      recordTime,
	}
	wcEvent.timeline.MarkAt(metrics.PointStorageDecoded, recordTime)
	wcEvent.timeline.MarkAt(metrics.PointCacheReceived, cacheReceived)

	// We can call w.storage.Get() outside of a critical section,
	// because the w.storage itself is thread-safe and the only
	// place where it is modified is below (via UpdateStoreLocked)
	// and these calls are serialized because reflector is processing
	// events one-by-one.
	previous, exists, err := w.storage.Get(event.Object)
	if err != nil {
		return err
	}
	if exists {
		previousElem := previous.(*store.Element)
		// Deliberately not materialized: PrevObject is only needed for the
		// rare filter transition that turns an update into a delete event, and
		// its trigger value and attributes are already precomputed. Decoding
		// it here would put a decode on every write.
		wcEvent.PrevObject = previousElem.Object
		wcEvent.PrevObjLabels = previousElem.Labels
		wcEvent.PrevObjFields = previousElem.Fields
		wcEvent.PrevTriggerValue = previousElem.TriggerValue
	}

	if err := func() error {
		w.Lock()
		defer w.Unlock()

		w.history.updateCache(wcEvent)
		w.resourceVersion = resourceVersion
		defer w.cond.Broadcast()

		if w.history.isCacheFullLocked() {
			oldestRV := w.history.OldestResourceVersionLocked()
			w.storage.CompactSnapshotsLocked(oldestRV)
		}
		if err := w.storage.UpdateStoreLocked(event.Type, elem, resourceVersion); err != nil {
			return err
		}
		return nil
	}(); err != nil {
		return err
	}

	// Avoid calling event handler under lock.
	// This is safe as long as there is at most one call to Add/Update/Delete and
	// UpdateResourceVersion in flight at any point in time, which is true now,
	// because reflector calls them synchronously from its main thread.
	if w.config.eventHandler != nil {
		// The handler takes a copy of the struct, so clearing the field
		// afterwards releases the decoded object from the copy the history
		// buffer holds without affecting the one in flight to dispatch.
		w.config.eventHandler(wcEvent)
	}
	wcEvent.dispatchObject = nil
	metrics.RecordResourceVersion(w.config.groupResource, resourceVersion)
	return nil
}

func (w *watchCache) UpdateResourceVersion(resourceVersion string) {
	rv, err := w.config.versioner.ParseResourceVersion(resourceVersion)
	if err != nil {
		klog.Errorf("Couldn't parse resourceVersion: %v", err)
		return
	}

	func() {
		w.Lock()
		defer w.Unlock()
		w.resourceVersion = rv
		w.cond.Broadcast()
	}()

	// Avoid calling event handler under lock.
	// This is safe as long as there is at most one call to Add/Update/Delete and
	// UpdateResourceVersion in flight at any point in time, which is true now,
	// because reflector calls them synchronously from its main thread.
	if w.config.eventHandler != nil {
		wcEvent := &watchCacheEvent{
			Type:            watch.Bookmark,
			ResourceVersion: rv,
		}
		w.config.eventHandler(wcEvent)
	}
	metrics.RecordResourceVersion(w.config.groupResource, rv)
}

// waitUntilFreshLocked waits until cache is at least as fresh as given resourceVersion.
func (w *watchCache) waitUntilFreshLocked(ctx context.Context, consistentReadSupported bool, resourceVersion uint64) error {
	if resourceVersion == 0 || resourceVersion <= w.resourceVersion {
		return nil
	}
	if consistentReadSupported {
		w.config.waitingUntilFresh.Add()
		defer w.config.waitingUntilFresh.Remove()
	}
	startTime := w.config.clock.Now()
	defer func() {
		if resourceVersion > 0 {
			metrics.WatchCacheReadWait.WithContext(ctx).WithLabelValues(w.config.groupResource.Group, w.config.groupResource.Resource).Observe(w.config.clock.Since(startTime).Seconds())
		}
	}()

	// In case resourceVersion is 0, we accept arbitrarily stale result.
	// As a result, the condition in the below for loop will never be
	// satisfied (w.resourceVersion is never negative), this call will
	// never hit the w.cond.Wait().
	// As a result - we can optimize the code by not firing the wakeup
	// function (and avoid starting a gorotuine), especially given that
	// resourceVersion=0 is the most common case.
	if resourceVersion > 0 {
		go func() {
			// Wake us up when the time limit has expired.  The docs
			// promise that time.After (well, NewTimer, which it calls)
			// will wait *at least* the duration given. Since this go
			// routine starts sometime after we record the start time, and
			// it will wake up the loop below sometime after the broadcast,
			// we don't need to worry about waking it up before the time
			// has expired accidentally.
			<-w.config.clock.After(blockTimeout)
			w.cond.Broadcast()
		}()
	}

	for w.resourceVersion < resourceVersion {
		if w.config.clock.Since(startTime) >= blockTimeout {
			// Request that the client retry after 'resourceVersionTooHighRetrySeconds' seconds.
			return storage.NewTooLargeResourceVersionError(resourceVersion, w.resourceVersion, resourceVersionTooHighRetrySeconds)
		}
		w.cond.Wait()
	}
	return nil
}

func (c *watchCache) WaitUntilFreshAndGetList(ctx context.Context, key string, opts storage.ListOptions) (listResp, string, error) {
	if opts.Recursive {
		return c.waitUntilFreshAndList(ctx, key, opts)
	}
	return c.waitUntilFreshAndGetList(ctx, key, opts)
}

func (c *watchCache) waitUntilFreshAndGetList(ctx context.Context, key string, opts storage.ListOptions) (listResp, string, error) {
	var listRV uint64
	var err error
	if opts.ResourceVersionMatch == "" && opts.ResourceVersion == "" {
		// Consistent read
		listRV, err = c.config.getCurrentRV(ctx)
		if err != nil {
			return listResp{}, "", err
		}
	} else {
		listRV, err = c.config.versioner.ParseResourceVersion(opts.ResourceVersion)
		if err != nil {
			return listResp{}, "", err
		}
	}
	obj, exists, readResourceVersion, err := c.WaitUntilFreshAndGet(ctx, listRV, key)
	if err != nil {
		return listResp{}, "", err
	}
	if exists {
		return listResp{Items: []interface{}{obj}, ResourceVersion: readResourceVersion}, "", nil
	}
	return listResp{ResourceVersion: readResourceVersion}, "", nil
}

// WaitUntilFreshAndList returns list of pointers to `storeElement` objects along
// with their ResourceVersion and the name of the index, if any, that was used.
func (w *watchCache) WaitUntilFreshAndGetKeys(ctx context.Context, resourceVersion uint64) ([]string, error) {
	span := tracing.SpanFromContext(ctx)
	consistentReadSupported := delegator.ConsistentReadSupported()
	w.RLock()
	span.AddEvent("watchCache locked acquired")
	defer w.RUnlock()
	err := w.waitUntilFreshLocked(ctx, consistentReadSupported, resourceVersion)
	if err != nil {
		return nil, err
	}
	span.AddEvent("watchCache fresh enough")
	keys := w.storage.ListKeys()
	span.AddEvent("ListKeys success")
	return keys, nil
}

// NOTICE: Structure follows the shouldDelegateList function in
// staging/src/k8s.io/apiserver/pkg/storage/cacher/delegator.go
func (w *watchCache) waitUntilFreshAndList(ctx context.Context, key string, opts storage.ListOptions) (resp listResp, index string, err error) {
	listRV, err := w.config.versioner.ParseResourceVersion(opts.ResourceVersion)
	if err != nil {
		return listResp{}, "", err
	}
	switch opts.ResourceVersionMatch {
	case metav1.ResourceVersionMatchExact:
		return w.waitAndListExactRV(ctx, key, "", listRV)
	case metav1.ResourceVersionMatchNotOlderThan:
	case "":
		// Continue
		if len(opts.Predicate.Continue) > 0 {
			continueKey, continueRV, err := storage.DecodeContinue(opts.Predicate.Continue, key)
			if err != nil {
				return listResp{}, "", errors.NewBadRequest(fmt.Sprintf("invalid continue token: %v", err))
			}
			if continueRV > 0 {
				return w.waitAndListExactRV(ctx, key, continueKey, uint64(continueRV))
			} else {
				// Don't pass matchValues as they don't support continueKey
				return w.waitAndListConsistent(ctx, key, continueKey, nil)
			}
		}
		// Legacy exact match
		if opts.Predicate.Limit > 0 && len(opts.ResourceVersion) > 0 && opts.ResourceVersion != "0" {
			return w.waitAndListExactRV(ctx, key, "", listRV)
		}
		if opts.ResourceVersion == "" {
			return w.waitAndListConsistent(ctx, key, "", opts.Predicate.MatcherIndex(ctx))
		}
	}
	return w.waitAndListLatestRV(ctx, listRV, key, "", opts.Predicate.MatcherIndex(ctx))
}

func (w *watchCache) waitAndListExactRV(ctx context.Context, key, continueKey string, resourceVersion uint64) (resp listResp, index string, err error) {
	store, err := w.waitAndGetExactSnapshot(ctx, resourceVersion)
	if err != nil {
		return listResp{}, "", err
	}
	items, err := store.OrderedListPrefix(key, continueKey)
	return listResp{
		Items:           items,
		ResourceVersion: resourceVersion,
	}, "", err
}

func (w *watchCache) waitAndGetExactSnapshot(ctx context.Context, resourceVersion uint64) (store.Snapshot, error) {
	span := tracing.SpanFromContext(ctx)
	consistentReadSupported := delegator.ConsistentReadSupported()
	w.RLock()
	span.AddEvent("watchCache locked acquired")
	defer w.RUnlock()
	err := w.waitUntilFreshLocked(ctx, consistentReadSupported, resourceVersion)
	if err != nil {
		return nil, err
	}
	span.AddEvent("watchCache fresh enough")

	store, err := w.storage.GetExactSnapshotLocked(resourceVersion)
	if err != nil {
		span.AddEvent("GetExactSnapshotLocked failed", attribute.String("error", err.Error()))
		return nil, err
	}
	span.AddEvent("GetExactSnapshotLocked success")
	return store, nil
}

func (w *watchCache) waitAndListConsistent(ctx context.Context, key, continueKey string, matchValues []storage.MatchValue) (resp listResp, index string, err error) {
	span := tracing.SpanFromContext(ctx)
	resourceVersion, err := w.config.getCurrentRV(ctx)
	if err != nil {
		return listResp{}, "", err
	}
	span.AddEvent("getCurrentRV success")
	return w.waitAndListLatestRV(ctx, resourceVersion, key, continueKey, matchValues)
}

func (w *watchCache) waitAndListLatestRV(ctx context.Context, minResourceVersion uint64, key, continueKey string, matchValues []storage.MatchValue) (resp listResp, index string, err error) {
	snap, resourceVersion, index, err := w.waitAndGetLatestSnapshot(ctx, minResourceVersion, key, continueKey, matchValues)
	if err != nil {
		return listResp{}, "", err
	}
	items, err := snap.OrderedListPrefix(key, continueKey)
	if err != nil {
		return listResp{}, "", err
	}
	return listResp{
		Items:           items,
		ResourceVersion: resourceVersion,
	}, index, nil
}

func (w *watchCache) waitAndGetLatestSnapshot(ctx context.Context, minResourceVersion uint64, key, continueKey string, matchValues []storage.MatchValue) (snap store.Snapshot, resourceVersion uint64, index string, err error) {
	consistentReadSupported := delegator.ConsistentReadSupported()
	span := tracing.SpanFromContext(ctx)
	w.RLock()
	span.AddEvent("watchCache locked acquired")
	defer w.RUnlock()
	err = w.waitUntilFreshLocked(ctx, consistentReadSupported, minResourceVersion)
	if err != nil {
		return nil, 0, "", err
	}
	span.AddEvent("watchCache fresh enough")
	// This isn't the place where we do "final filtering" - only some "prefiltering" is happening here. So the only
	// requirement here is to NOT miss anything that should be returned. We can return as many non-matching items as we
	// want - they will be filtered out later. The fact that we return less things is only further performance improvement.
	// TODO: if multiple indexes match, return the one with the fewest items, so as to do as much filtering as possible.
	for _, matchValue := range matchValues {
		snap, err := w.storage.GetByIndexSnapshot(matchValue.IndexName, matchValue.Value)
		if err == nil {
			span.AddEvent("GetByIndexSnapshot success", attribute.String("index", matchValue.IndexName))
			return snap, w.resourceVersion, matchValue.IndexName, nil
		}
		span.AddEvent("GetByIndexSnapshot fail", attribute.String("index", matchValue.IndexName), attribute.String("error", err.Error()))
	}
	snap, err = w.storage.GetLatestSnapshotOrBuildLocked(key, continueKey)
	if err != nil {
		span.AddEvent("GetLatestSnapshotOrBuildLocked failed", attribute.String("error", err.Error()))
		return nil, 0, "", err
	}
	span.AddEvent("GetLatestSnapshotOrBuildLocked success")
	return snap, w.resourceVersion, "", nil
}

func (w *watchCache) notFresh(resourceVersion uint64) bool {
	w.RLock()
	defer w.RUnlock()
	return resourceVersion > w.resourceVersion
}

// WaitUntilFreshAndGet returns a pointers to <storeElement> object.
func (w *watchCache) WaitUntilFreshAndGet(ctx context.Context, resourceVersion uint64, key string) (interface{}, bool, uint64, error) {
	span := tracing.SpanFromContext(ctx)
	consistentReadSupported := delegator.ConsistentReadSupported()
	w.RLock()
	span.AddEvent("watchCache locked acquired")
	defer w.RUnlock()
	err := w.waitUntilFreshLocked(ctx, consistentReadSupported, resourceVersion)
	if err != nil {
		return nil, false, 0, err
	}
	span.AddEvent("watchCache fresh enough")
	value, exists, err := w.storage.GetByKey(key)
	if err != nil {
		span.AddEvent("GetByKey failed", attribute.String("error", err.Error()))
		return nil, false, 0, err
	}
	span.AddEvent("GetByKey success")
	return value, exists, w.resourceVersion, err
}

// Replace takes slice of runtime.Object as a parameter.
func (w *watchCache) Replace(objs []interface{}, resourceVersion string) error {
	version, err := w.config.versioner.ParseResourceVersion(resourceVersion)
	if err != nil {
		return err
	}

	toReplace := make([]interface{}, 0, len(objs))
	for _, obj := range objs {
		object, ok := obj.(runtime.Object)
		if !ok {
			return fmt.Errorf("didn't get runtime.Object for replace: %#v", obj)
		}
		key, err := w.config.keyFunc(object)
		if err != nil {
			return fmt.Errorf("couldn't compute key: %v", err)
		}
		elem, err := w.newElement(key, object)
		if err != nil {
			return err
		}
		toReplace = append(toReplace, elem)
	}

	w.Lock()
	defer w.Unlock()

	// Ensure startIndex never decreases, so that existing watchCacheInterval
	// instances get "invalid" errors if the try to download from the buffer
	// using their own start/end indexes calculated from previous buffer
	// content.

	// Empty the cyclic buffer, ensuring startIndex doesn't decrease.
	w.history.ResetLocked()

	if err := w.storage.ReplaceLocked(toReplace, resourceVersion, version); err != nil {
		return err
	}
	w.resourceVersion = version
	if w.onReplace != nil {
		w.onReplace()
	}
	w.cond.Broadcast()

	metrics.RecordResourceVersion(w.config.groupResource, version)
	klog.V(3).Infof("Replaced watchCache (rev: %v) ", resourceVersion)
	return nil
}

func (w *watchCache) SetOnReplace(onReplace func()) {
	w.Lock()
	defer w.Unlock()
	w.onReplace = onReplace
}

func (w *watchCache) Resync() error {
	// Nothing to do
	return nil
}

func (w *watchCache) getListResourceVersion() uint64 {
	w.RLock()
	defer w.RUnlock()
	return w.storage.ListResourceVersion()
}

func (w *watchCache) suggestedWatchChannelSize(indexExists, triggerUsed bool) int {
	w.RLock()
	defer w.RUnlock()
	return w.history.suggestedWatchChannelSize(indexExists, triggerUsed)
}

// getAllEventsSinceLocked returns a watchCacheInterval that can be used to
// retrieve events since a certain resourceVersion. This function assumes to
// be called under the watchCache lock.
func (w *watchCache) getAllEventsSinceLocked(resourceVersion uint64, key string, opts storage.ListOptions) (*watchCacheInterval, error) {
	_, matchesSingle := opts.Predicate.MatchesSingle()
	matchesSingle = matchesSingle && !opts.Recursive
	if opts.SendInitialEvents != nil && *opts.SendInitialEvents {
		return w.getIntervalFromStoreLocked(key, matchesSingle)
	}

	if resourceVersion == 0 {
		if opts.SendInitialEvents == nil {
			// resourceVersion = 0 means that we don't require any specific starting point
			// and we would like to start watching from ~now.
			// However, to keep backward compatibility, we additionally need to return the
			// current state and only then start watching from that point.
			//
			// TODO: In v2 api, we should stop returning the current state - #13969.
			return w.getIntervalFromStoreLocked(key, matchesSingle)
		}
		// SendInitialEvents = false and resourceVersion = 0
		// means that the request would like to start watching
		// from Any resourceVersion
		resourceVersion = w.resourceVersion
	}

	return w.history.GetIntervalLocked(resourceVersion, w.storage.ListResourceVersion(), w.RWMutex.RLocker())
}

// getIntervalFromStoreLocked returns a watchCacheInterval
// that covers the entire storage state.
// This function assumes to be called under the watchCache lock.
func (w *watchCache) getIntervalFromStoreLocked(key string, matchesSingle bool) (*watchCacheInterval, error) {
	// When not matching a single key, an immutable snapshot lets us
	// defer the O(N) interval build off the watchCache lock.
	if !matchesSingle {
		if snapshot, ok := w.storage.LatestSnapshotLocked(); ok {
			return newCacheIntervalFromLazySnapshot(w.resourceVersion, snapshot), nil
		}
	}
	return newCacheIntervalFromStore(w.resourceVersion, w.storage.StoreLocked(), key, matchesSingle)
}
