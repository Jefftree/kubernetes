/*
Copyright 2014 The Kubernetes Authors.

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
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/net/websocket"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/audit"
	"k8s.io/apiserver/pkg/endpoints/handlers/negotiation"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	"k8s.io/apiserver/pkg/endpoints/metrics"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/server/httplog"
	"k8s.io/apiserver/pkg/storage"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	compbasemetrics "k8s.io/component-base/metrics"
	"k8s.io/component-base/tracing"
	"k8s.io/klog/v2"
	"k8s.io/streaming/pkg/httpstream/wsstream"
)

// timeoutFactory abstracts watch timeout logic for testing
type TimeoutFactory interface {
	TimeoutCh() (<-chan time.Time, func() bool)
}

// realTimeoutFactory implements timeoutFactory
type realTimeoutFactory struct {
	timeout time.Duration
}

// TimeoutCh returns a channel which will receive something when the watch times out,
// and a cleanup function to call when this happens.
func (w *realTimeoutFactory) TimeoutCh() (<-chan time.Time, func() bool) {
	if w.timeout == 0 {
		// nothing will ever be sent down this channel
		return nil, func() bool { return false }
	}
	t := time.NewTimer(w.timeout)
	return t.C, t.Stop
}

// serveWatchHandler returns a handle to serve a watch response.
// TODO: the functionality in this method and in WatchServer.Serve is not cleanly decoupled.
func serveWatchHandler(watcher watch.Interface, scope *RequestScope, mediaTypeOptions negotiation.MediaTypeOptions, req *http.Request, w http.ResponseWriter, timeout time.Duration, metricsScope string, isWatchListRequest bool, completeHook WatchListCompleteHook) (http.Handler, error) {
	options, err := optionsForTransform(mediaTypeOptions, req)
	if err != nil {
		return nil, err
	}

	// negotiate for the stream serializer from the scope's serializer
	serializer, err := negotiation.NegotiateOutputMediaTypeStream(req, scope.Serializer, scope)
	if err != nil {
		return nil, err
	}
	framer := serializer.StreamSerializer.Framer
	var encoder runtime.Encoder
	if utilfeature.DefaultFeatureGate.Enabled(features.CBORServingAndStorage) {
		encoder = scope.Serializer.EncoderForVersion(runtime.UseNondeterministicEncoding(serializer.StreamSerializer.Serializer), scope.Kind.GroupVersion())
	} else {
		encoder = scope.Serializer.EncoderForVersion(serializer.StreamSerializer.Serializer, scope.Kind.GroupVersion())
	}
	useTextFraming := serializer.EncodesAsText
	if framer == nil {
		return nil, fmt.Errorf("no framer defined for %q available for embedded encoding", serializer.MediaType)
	}
	// TODO: next step, get back mediaTypeOptions from negotiate and return the exact value here
	mediaType := serializer.MediaType
	switch mediaType {
	case runtime.ContentTypeJSON:
		// as-is
	case runtime.ContentTypeCBOR:
		// If a client indicated it accepts application/cbor (exactly one data item) on a
		// watch request, set the conformant application/cbor-seq media type the watch
		// response. RFC 9110 allows an origin server to deviate from the indicated
		// preference rather than send a 406 (Not Acceptable) response (see
		// https://www.rfc-editor.org/rfc/rfc9110.html#section-12.1-5).
		mediaType = runtime.ContentTypeCBORSequence
	default:
		mediaType += ";stream=watch"
	}

	ctx := req.Context()

	// locate the appropriate embedded encoder based on the transform
	var negotiatedEncoder runtime.Encoder
	contentKind, contentSerializer, transform := targetEncodingForTransform(scope, mediaTypeOptions, req)
	if transform {
		info, ok := runtime.SerializerInfoForMediaType(contentSerializer.SupportedMediaTypes(), serializer.MediaType)
		if !ok {
			return nil, fmt.Errorf("no encoder for %q exists in the requested target %#v", serializer.MediaType, contentSerializer)
		}
		if utilfeature.DefaultFeatureGate.Enabled(features.CBORServingAndStorage) {
			negotiatedEncoder = contentSerializer.EncoderForVersion(runtime.UseNondeterministicEncoding(info.Serializer), contentKind.GroupVersion())
		} else {
			negotiatedEncoder = contentSerializer.EncoderForVersion(info.Serializer, contentKind.GroupVersion())
		}
	} else {
		if utilfeature.DefaultFeatureGate.Enabled(features.CBORServingAndStorage) {
			negotiatedEncoder = scope.Serializer.EncoderForVersion(runtime.UseNondeterministicEncoding(serializer.Serializer), contentKind.GroupVersion())
		} else {
			negotiatedEncoder = scope.Serializer.EncoderForVersion(serializer.Serializer, contentKind.GroupVersion())
		}
	}

	var memoryAllocator runtime.MemoryAllocator

	if encoderWithAllocator, supportsAllocator := negotiatedEncoder.(runtime.EncoderWithAllocator); supportsAllocator {
		// don't put the allocator inside the embeddedEncodeFn as that would allocate memory on every call.
		// instead, we allocate the buffer for the entire watch session and release it when we close the connection.
		memoryAllocator = runtime.AllocatorPool.Get().(*runtime.Allocator)
		negotiatedEncoder = runtime.NewEncoderWithAllocator(encoderWithAllocator, memoryAllocator)
	}
	var tableOptions *metav1.TableOptions
	if options != nil {
		if passedOptions, ok := options.(*metav1.TableOptions); ok {
			tableOptions = passedOptions
		} else {
			return nil, fmt.Errorf("unexpected options type: %T", options)
		}
	}
	embeddedEncoder := newWatchEmbeddedEncoder(ctx, negotiatedEncoder, mediaTypeOptions.Convert, tableOptions, mediaTypeOptions.Drop, scope)

	if encoderWithAllocator, supportsAllocator := encoder.(runtime.EncoderWithAllocator); supportsAllocator {
		if memoryAllocator == nil {
			// don't put the allocator inside the embeddedEncodeFn as that would allocate memory on every call.
			// instead, we allocate the buffer for the entire watch session and release it when we close the connection.
			memoryAllocator = runtime.AllocatorPool.Get().(*runtime.Allocator)
		}
		encoder = runtime.NewEncoderWithAllocator(encoderWithAllocator, memoryAllocator)
	}

	var serverShuttingDownCh <-chan struct{}
	if signals := apirequest.ServerShutdownSignalFrom(req.Context()); signals != nil {
		serverShuttingDownCh = signals.ShuttingDown()
	}

	server := &WatchServer{
		Watching: watcher,
		Scope:    scope,

		UseTextFraming:  useTextFraming,
		MediaType:       mediaType,
		Framer:          framer,
		Encoder:         encoder,
		EmbeddedEncoder: embeddedEncoder,

		MemoryAllocator:      memoryAllocator,
		TimeoutFactory:       &realTimeoutFactory{timeout},
		ServerShuttingDownCh: serverShuttingDownCh,

		metricsScope:          metricsScope,
		isWatchListRequest:    isWatchListRequest,
		watchListCompleteHook: completeHook,
	}

	if wsstream.IsWebSocketRequest(req) {
		w.Header().Set("Content-Type", server.MediaType)
		return websocket.Handler(server.HandleWS), nil
	}
	return http.HandlerFunc(server.HandleHTTP), nil
}

// WatchServer serves a watch.Interface over a websocket or vanilla HTTP.
type WatchServer struct {
	Watching watch.Interface
	Scope    *RequestScope

	// true if websocket messages should use text framing (as opposed to binary framing)
	UseTextFraming bool
	// the media type this watch is being served with
	MediaType string
	// used to frame the watch stream
	Framer runtime.Framer
	// used to encode the watch stream event itself
	Encoder runtime.Encoder
	// used to encode the nested object in the watch stream
	EmbeddedEncoder runtime.Encoder

	MemoryAllocator      runtime.MemoryAllocator
	TimeoutFactory       TimeoutFactory
	ServerShuttingDownCh <-chan struct{}

	metricsScope          string
	isWatchListRequest    bool
	watchListCompleteHook WatchListCompleteHook
}

type WatchListCompleteHook func()

// watchEventMetricsRecorder allows the caller to count bytes written and report the size of the event.
// It is thread-safe, as long as underlying io.Writer is thread-safe.
// Once all Write calls for a given watch event have finished, RecordEvent must be called.
type watchEventMetricsRecorder struct {
	writer      io.Writer
	countMetric compbasemetrics.CounterMetric
	sizeMetric  compbasemetrics.ObserverMetric
	byteCount   atomic.Int64
}

// Write implements io.Writer.
func (c *watchEventMetricsRecorder) Write(p []byte) (n int, err error) {
	n, err = c.writer.Write(p)
	c.byteCount.Add(int64(n))
	return
}

// Record reports the metrics and resets the byte count.
func (c *watchEventMetricsRecorder) RecordEvent() {
	c.countMetric.Inc()
	c.sizeMetric.Observe(float64(c.byteCount.Swap(0)))
}

// watchGzipPool is a no-op until the first Get call (https://pkg.go.dev/sync#Pool.Get).
var watchGzipPool = responsewriters.NewGzipWriterPoolOrDie()

type watchStreamWriter interface {
	io.WriteCloser
	Flush() error
}

var _ watchStreamWriter = &plainResponseWriter{}

type plainResponseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func (p *plainResponseWriter) Write(b []byte) (int, error) {
	return p.w.Write(b)
}

func (p *plainResponseWriter) Flush() error {
	p.flusher.Flush()
	return nil
}

// Close is a no-op because http.ResponseWriter (p.w) doesn't implement io.Closer.
func (p *plainResponseWriter) Close() error {
	return nil
}

var _ watchStreamWriter = &perFlushGzipWriter{}

type perFlushGzipWriter struct {
	delegateRW http.ResponseWriter
	flusher    http.Flusher
	gw         *gzip.Writer
}

func (p *perFlushGzipWriter) Write(b []byte) (int, error) {
	if p.gw == nil {
		p.gw = watchGzipPool.Get().(*gzip.Writer)
		p.gw.Reset(p.delegateRW)
	}
	return p.gw.Write(b)
}

// Flush writes compressed data to the client and releases the gzip.Writer back to the pool.
func (p *perFlushGzipWriter) Flush() error {
	// no writer means Write was not called, nothing to flush
	if p.gw == nil {
		return nil
	}
	if err := p.gw.Flush(); err != nil {
		return err
	}
	err := p.Close()
	p.flusher.Flush()
	return err
}

func (p *perFlushGzipWriter) Close() error {
	if p.gw == nil {
		return nil
	}
	err := p.gw.Close()
	p.gw.Reset(nil)
	watchGzipPool.Put(p.gw)
	// prevent double-close returning the writer to the pool twice
	p.gw = nil
	return err
}

var _ io.Writer = &watchResponseWriter{}

type watchResponseWriter struct {
	delegateRW         http.ResponseWriter
	flusher            http.Flusher
	contentEncoding    string
	isWatchListRequest bool
	writer             watchStreamWriter
}

func newWatchResponseWriter(delegateRW http.ResponseWriter, flusher http.Flusher, contentEncoding string, isWatchListRequest bool) *watchResponseWriter {
	return &watchResponseWriter{
		delegateRW:         delegateRW,
		flusher:            flusher,
		contentEncoding:    contentEncoding,
		isWatchListRequest: isWatchListRequest,
		writer:             &plainResponseWriter{w: delegateRW, flusher: flusher},
	}
}

func (w *watchResponseWriter) BeginStream(mediaType string) {
	w.delegateRW.Header().Set("Content-Type", mediaType)
	w.delegateRW.Header().Set("Transfer-Encoding", "chunked")
	if w.contentEncoding == "gzip" && w.isWatchListRequest {
		w.delegateRW.Header().Set("Content-Encoding", "gzip")
		w.delegateRW.Header().Add("Vary", "Accept-Encoding")
		w.writer = &perFlushGzipWriter{delegateRW: w.delegateRW, flusher: w.flusher}
	}
	w.delegateRW.WriteHeader(http.StatusOK)
	// Flush HTTP headers only
	// gzip applies to the body, not headers.
	w.flusher.Flush()
}

func (w *watchResponseWriter) Write(p []byte) (int, error) {
	return w.writer.Write(p)
}

func (w *watchResponseWriter) Flush() error {
	return w.writer.Flush()
}

func (w *watchResponseWriter) Close() error {
	return w.writer.Close()
}

// HandleHTTP serves a series of encoded events via HTTP with Transfer-Encoding: chunked.
// or over a websocket connection.
func (s *WatchServer) HandleHTTP(w http.ResponseWriter, req *http.Request) {
	ctx, rw, recorder, watchEncoder, span, ok := s.prepareHTTP(w, req)
	if !ok {
		s.releaseAllocator()
		return
	}

	var timeoutCh <-chan time.Time
	var cleanup func() bool
	if rtf, isReal := s.TimeoutFactory.(*realTimeoutFactory); isReal {
		if rtf.timeout > 0 {
			if deadline, hasDeadline := ctx.Deadline(); !hasDeadline || time.Until(deadline) > rtf.timeout+time.Second {
				timer := time.AfterFunc(rtf.timeout, s.Watching.Stop)
				cleanup = timer.Stop
			}
		}
	} else if s.TimeoutFactory != nil {
		timeoutCh, cleanup = s.TimeoutFactory.TimeoutCh()
	}

	s.serveHTTP(ctx, req.URL.Path, rw, recorder, watchEncoder, span, timeoutCh)

	if cleanup != nil {
		cleanup()
	}
	if err := rw.Close(); err != nil {
		utilruntime.HandleErrorWithContext(ctx, err, "Failed to close watch response writer")
	}
	s.releaseAllocator()
}

func (s *WatchServer) releaseAllocator() {
	if s.MemoryAllocator != nil {
		runtime.AllocatorPool.Put(s.MemoryAllocator)
	}
}

func (s *WatchServer) prepareHTTP(w http.ResponseWriter, req *http.Request) (context.Context, *watchResponseWriter, *watchEventMetricsRecorder, *watchEncoder, *tracing.Span, bool) {
	ctx := req.Context()
	ctx, span := tracing.Start(ctx, "WatchServer.HandleHTTP",
		attribute.String("audit-id", audit.GetAuditIDTruncated(ctx)),
		attribute.String("method", req.Method),
		attribute.String("url", req.URL.Path),
		attribute.String("protocol", req.Proto),
		attribute.String("mediaType", s.MediaType),
		attribute.String("encoder", string(s.Encoder.Identifier())))

	flusher, ok := w.(http.Flusher)
	if !ok {
		err := fmt.Errorf("unable to start watch - can't get http.Flusher: %#v", w)
		utilruntime.HandleErrorWithContext(ctx, err, "Unable to start watch")
		s.Scope.err(errors.NewInternalError(err), w, req.WithContext(ctx))
		return ctx, nil, nil, nil, nil, false
	}

	contentEncoding := responsewriters.ContentEncodingSupported(req, features.WatchListCompression)
	rw := newWatchResponseWriter(w, flusher, contentEncoding, s.isWatchListRequest)

	framer := s.Framer.NewFrameWriter(rw)
	if framer == nil {
		// programmer error
		if err := rw.Close(); err != nil {
			utilruntime.HandleErrorWithContext(ctx, err, "Failed to close watch response writer")
		}
		err := fmt.Errorf("no stream framing support is available for media type %q", s.MediaType)
		utilruntime.HandleErrorWithContext(ctx, err, "No stream framing support available")
		s.Scope.err(errors.NewBadRequest(err.Error()), w, req.WithContext(ctx))
		return ctx, nil, nil, nil, nil, false
	}

	// begin the stream
	rw.BeginStream(s.MediaType)

	gvr := s.Scope.Resource

	recorder := &watchEventMetricsRecorder{
		writer:      framer,
		countMetric: metrics.WatchEvents.WithContext(ctx).WithLabelValues(gvr.Group, gvr.Version, gvr.Resource),
		sizeMetric:  metrics.WatchEventsSizes.WithContext(ctx).WithLabelValues(gvr.Group, gvr.Version, gvr.Resource),
	}

	watchEncoder := newWatchEncoder(ctx, gvr, s.EmbeddedEncoder, s.Encoder, recorder)
	span.AddEvent("About to start writing response")
	return ctx, rw, recorder, watchEncoder, span, true
}

var shutdownNotifiers sync.Map // map[<-chan struct{}]*shutdownNotifier

type shutdownShard struct {
	mu       sync.Mutex
	closed   bool
	watchers map[uint64]watch.Interface
}

type shutdownNotifier struct {
	ch     <-chan struct{}
	nextID atomic.Uint64
	shards [64]shutdownShard
}

func registerShutdownWatcher(ch <-chan struct{}, w watch.Interface) (*shutdownNotifier, uint64) {
	select {
	case <-ch:
		w.Stop()
		return nil, 0
	default:
	}
	val, ok := shutdownNotifiers.Load(ch)
	if !ok {
		n := &shutdownNotifier{ch: ch}
		var loaded bool
		val, loaded = shutdownNotifiers.LoadOrStore(ch, n)
		if !loaded {
			go n.wait()
		}
	}
	n := val.(*shutdownNotifier)
	id := n.nextID.Add(1)
	shard := &n.shards[id%64]
	shard.mu.Lock()
	if shard.closed {
		shard.mu.Unlock()
		w.Stop()
		return nil, 0
	}
	if shard.watchers == nil {
		shard.watchers = make(map[uint64]watch.Interface)
	}
	shard.watchers[id] = w
	shard.mu.Unlock()
	return n, id
}

func (n *shutdownNotifier) wait() {
	<-n.ch
	shutdownNotifiers.Delete(n.ch)
	for i := range n.shards {
		shard := &n.shards[i]
		shard.mu.Lock()
		shard.closed = true
		watchers := shard.watchers
		shard.watchers = nil
		shard.mu.Unlock()
		for _, w := range watchers {
			w.Stop()
		}
	}
}

func (n *shutdownNotifier) unregister(id uint64) {
	shard := &n.shards[id%64]
	shard.mu.Lock()
	delete(shard.watchers, id)
	shard.mu.Unlock()
}

func (s *WatchServer) serveHTTP(ctx context.Context, urlPath string, rw *watchResponseWriter, recorder *watchEventMetricsRecorder, watchEncoder *watchEncoder, span *tracing.Span, timeoutCh <-chan time.Time) {
	if timeoutCh == nil {
		stopDone := context.AfterFunc(ctx, s.Watching.Stop)
		var sdNotifier *shutdownNotifier
		var sdID uint64
		if s.ServerShuttingDownCh != nil {
			sdNotifier, sdID = registerShutdownWatcher(s.ServerShuttingDownCh, s.Watching)
		}
		s.serveHTTPChannel(ctx, urlPath, rw, recorder, watchEncoder, span)
		if sdNotifier != nil {
			sdNotifier.unregister(sdID)
		}
		stopDone()
		return
	}
	s.serveHTTPSelect(ctx, urlPath, rw, recorder, watchEncoder, span, timeoutCh)
}

func (s *WatchServer) serveHTTPChannel(ctx context.Context, urlPath string, rw *watchResponseWriter, recorder *watchEventMetricsRecorder, watchEncoder *watchEncoder, span *tracing.Span) {
	if s.ServerShuttingDownCh != nil {
		select {
		case <-s.ServerShuttingDownCh:
			return
		default:
		}
	}
	ch := s.Watching.ResultChan()
	for event := range ch {
		if s.ServerShuttingDownCh != nil {
			select {
			case <-s.ServerShuttingDownCh:
				return
			default:
			}
		}
		if !s.processHTTPEvent(ctx, urlPath, rw, recorder, watchEncoder, span, ch, event) {
			return
		}
	}
}

func (s *WatchServer) serveHTTPSelect(ctx context.Context, urlPath string, rw *watchResponseWriter, recorder *watchEventMetricsRecorder, watchEncoder *watchEncoder, span *tracing.Span, timeoutCh <-chan time.Time) {
	ch := s.Watching.ResultChan()
	done := ctx.Done()

	for {
		select {
		case <-s.ServerShuttingDownCh:
			// the server has signaled that it is shutting down (not accepting
			// any new request), all active watch request(s) should return
			// immediately here. The WithWatchTerminationDuringShutdown server
			// filter will ensure that the response to the client is rate
			// limited in order to avoid any thundering herd issue when the
			// client(s) try to reestablish the WATCH on the other
			// available apiserver instance(s).
			return
		case <-done:
			return
		case <-timeoutCh:
			return
		case event, ok := <-ch:
			if !ok {
				// End of results.
				return
			}
			if !s.processHTTPEvent(ctx, urlPath, rw, recorder, watchEncoder, span, ch, event) {
				return
			}
		}
	}
}

func (s *WatchServer) processHTTPEvent(ctx context.Context, urlPath string, rw *watchResponseWriter, recorder *watchEventMetricsRecorder, watchEncoder *watchEncoder, span *tracing.Span, ch <-chan watch.Event, event watch.Event) bool {
	if ctx.Err() != nil {
		return false
	}
	isWatchListLatencyRecordingRequired := shouldRecordWatchListLatency(ctx, event)

	if err := watchEncoder.Encode(event); err != nil {
		utilruntime.HandleErrorWithContext(ctx, err, "Failed to encode watch event")
		// client disconnect.
		return false
	}
	recorder.RecordEvent()

	if len(ch) == 0 {
		if err := rw.Flush(); err != nil {
			utilruntime.HandleErrorWithContext(ctx, err, "Failed to flush watch response")
			return false
		}
	}
	if isWatchListLatencyRecordingRequired {
		s.recordWatchListLatency(ctx, urlPath, span)
		// release the gzip writer back to the pool so idle watches don't hold gzip state.
		if err := rw.Flush(); err != nil {
			utilruntime.HandleErrorWithContext(ctx, err, "Failed to flush watch response after initial events")
			return false
		}
	}
	return true
}

func (s *WatchServer) recordWatchListLatency(ctx context.Context, urlPath string, span *tracing.Span) {
	// Record completion of initial listing phase for WatchList
	receivedTimestamp, ok := apirequest.ReceivedTimestampFrom(ctx)
	if !ok {
		utilruntime.HandleErrorWithContext(ctx, nil, "Unable to measure watchlist latency, no received timestamp found in the context", "gvr", s.Scope.Resource)
		return
	}
	initLatency := time.Since(receivedTimestamp)
	metrics.RecordWatchListLatency(ctx, s.Scope.Resource, s.metricsScope, initLatency)
	auditID := audit.GetAuditIDTruncated(ctx)
	klog.V(3).InfoS("WatchList initial events sent", "path", urlPath, "auditID", auditID, "initLatency", initLatency)
	httplog.AddKeyValue(ctx, "watchlist_init_latency", initLatency)
	span.AddEvent("Writing initial events done")
	span.End(5 * time.Second)
	s.watchListCompleteHook()
}

// HandleWS serves a series of encoded events over a websocket connection.
func (s *WatchServer) HandleWS(ws *websocket.Conn) {
	ctx := ws.Request().Context()
	logger := klog.FromContext(ctx)

	defer func() {
		if s.MemoryAllocator != nil {
			runtime.AllocatorPool.Put(s.MemoryAllocator)
		}
	}()

	defer ws.Close()
	done := make(chan struct{})
	// ensure the connection times out
	timeoutCh, cleanup := s.TimeoutFactory.TimeoutCh()
	defer cleanup()

	go func() {
		defer utilruntime.HandleCrashWithLogger(logger)
		// This blocks until the connection is closed.
		// Client should not send anything.
		wsstream.IgnoreReceivesWithLogger(logger, ws, 0)
		// Once the client closes, we should also close
		close(done)
	}()

	framer := newWebsocketFramer(ws, s.UseTextFraming)

	gvr := s.Scope.Resource
	watchEncoder := newWatchEncoder(ctx, gvr, s.EmbeddedEncoder, s.Encoder, framer)
	ch := s.Watching.ResultChan()

	for {
		select {
		case <-done:
			return
		case <-timeoutCh:
			return
		case event, ok := <-ch:
			if !ok {
				// End of results.
				return
			}

			if err := watchEncoder.Encode(event); err != nil {
				utilruntime.HandleErrorWithLogger(logger, err, "Failed to encode watch event")
				// client disconnect.
				return
			}
		}
	}
}

type websocketFramer struct {
	ws             *websocket.Conn
	useTextFraming bool
}

func newWebsocketFramer(ws *websocket.Conn, useTextFraming bool) io.Writer {
	return &websocketFramer{
		ws:             ws,
		useTextFraming: useTextFraming,
	}
}

func (w *websocketFramer) Write(p []byte) (int, error) {
	if w.useTextFraming {
		// bytes.Buffer::String() has a special handling of nil value, but given
		// we're writing serialized watch events, this will never happen here.
		if err := websocket.Message.Send(w.ws, string(p)); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	if err := websocket.Message.Send(w.ws, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

var _ io.Writer = &websocketFramer{}

func shouldRecordWatchListLatency(ctx context.Context, event watch.Event) bool {
	if event.Type != watch.Bookmark || !utilfeature.DefaultFeatureGate.Enabled(features.WatchList) {
		return false
	}
	// as of today the initial-events-end annotation is added only to a single event
	// by the watch cache and only when certain conditions are met
	//
	// for more please read https://github.com/kubernetes/enhancements/tree/master/keps/sig-api-machinery/3157-watch-list
	hasAnnotation, err := storage.HasInitialEventsEndBookmarkAnnotation(event.Object)
	if err != nil {
		utilruntime.HandleErrorWithContext(ctx, err, "Unable to determine if the object has the required annotation for measuring watchlist latency", "object", fmt.Sprintf("%T", event.Object))
		return false
	}
	return hasAnnotation
}
