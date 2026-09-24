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

package versioning

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sync"
	"time"
	"unsafe"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog/v2"
)

type defaultingCodecKey struct {
	scheme     *runtime.Scheme
	encWords   [2]uintptr
	decWords   [2]uintptr
	encodeKind uint8
	decodeKind uint8
	encodeGV   schema.GroupVersion
	decodeGV   schema.GroupVersion
}

type defaultingCodecEntry struct {
	key   defaultingCodecKey
	codec runtime.Codec
}

type defaultingCodecShard struct {
	mu      sync.Mutex
	entries [8]defaultingCodecEntry
	next    uint32
}

var defaultingCodecShards [32]defaultingCodecShard

func asCacheableGV(gv runtime.GroupVersioner) (schema.GroupVersion, uint8, bool) {
	if gv == nil {
		return schema.GroupVersion{}, 0, true
	}
	if gv == runtime.InternalGroupVersioner {
		return schema.GroupVersion{}, 1, true
	}
	if gv == runtime.DisabledGroupVersioner {
		return schema.GroupVersion{}, 2, true
	}
	if v, ok := gv.(schema.GroupVersion); ok {
		return v, 3, true
	}
	return schema.GroupVersion{}, 0, false
}

// NewDefaultingCodecForScheme is a convenience method for callers that are using a scheme.
func NewDefaultingCodecForScheme(
	// TODO: I should be a scheme interface?
	scheme *runtime.Scheme,
	encoder runtime.Encoder,
	decoder runtime.Decoder,
	encodeVersion runtime.GroupVersioner,
	decodeVersion runtime.GroupVersioner,
) runtime.Codec {
	if encGV, encKind, ok1 := asCacheableGV(encodeVersion); ok1 {
		if decGV, decKind, ok2 := asCacheableGV(decodeVersion); ok2 {
			encWords := *(*[2]uintptr)(unsafe.Pointer(&encoder))
			decWords := *(*[2]uintptr)(unsafe.Pointer(&decoder))
			key := defaultingCodecKey{
				scheme:     scheme,
				encWords:   encWords,
				decWords:   decWords,
				encodeKind: encKind,
				decodeKind: decKind,
				encodeGV:   encGV,
				decodeGV:   decGV,
			}
			h := (uint64(uintptr(unsafe.Pointer(scheme))) ^ uint64(encWords[1]) ^ uint64(decWords[1])) * 0x9e3779b97f4a7c15
			for i := 0; i < len(encGV.Version); i++ {
				h = (h ^ uint64(encGV.Version[i])) * 1099511628211
			}
			for i := 0; i < len(decGV.Version); i++ {
				h = (h ^ uint64(decGV.Version[i])) * 1099511628211
			}
			shard := &defaultingCodecShards[(h^(h>>16))&31]
			shard.mu.Lock()
			for i := range shard.entries {
				e := &shard.entries[i]
				if e.codec != nil && e.key == key {
					res := e.codec
					shard.mu.Unlock()
					return res
				}
			}
			shard.mu.Unlock()
			c := NewCodec(encoder, decoder, runtime.UnsafeObjectConvertor(scheme), scheme, scheme, scheme, encodeVersion, decodeVersion, scheme.Name())
			if (encoder == nil || reflect.TypeOf(encoder).Kind() == reflect.Ptr) &&
				(decoder == nil || reflect.TypeOf(decoder).Kind() == reflect.Ptr) {
				shard.mu.Lock()
				idx := shard.next & 7
				shard.entries[idx] = defaultingCodecEntry{key: key, codec: c}
				shard.next++
				shard.mu.Unlock()
			}
			return c
		}
	}
	return NewCodec(encoder, decoder, runtime.UnsafeObjectConvertor(scheme), scheme, scheme, scheme, encodeVersion, decodeVersion, scheme.Name())
}

// NewCodec takes objects in their internal versions and converts them to external versions before
// serializing them. It assumes the serializer provided to it only deals with external versions.
// This class is also a serializer, but is generally used with a specific version.
func NewCodec(
	encoder runtime.Encoder,
	decoder runtime.Decoder,
	convertor runtime.ObjectConvertor,
	creater runtime.ObjectCreater,
	typer runtime.ObjectTyper,
	defaulter runtime.ObjectDefaulter,
	encodeVersion runtime.GroupVersioner,
	decodeVersion runtime.GroupVersioner,
	originalSchemeName string,
) runtime.Codec {
	internal := &codec{
		encoder:   encoder,
		decoder:   decoder,
		convertor: convertor,
		creater:   creater,
		typer:     typer,
		defaulter: defaulter,

		encodeVersion: encodeVersion,
		decodeVersion: decodeVersion,

		identifier: identifier(encodeVersion, encoder),

		originalSchemeName: originalSchemeName,
	}
	return internal
}

type codec struct {
	encoder   runtime.Encoder
	decoder   runtime.Decoder
	convertor runtime.ObjectConvertor
	creater   runtime.ObjectCreater
	typer     runtime.ObjectTyper
	defaulter runtime.ObjectDefaulter

	encodeVersion runtime.GroupVersioner
	decodeVersion runtime.GroupVersioner

	identifier runtime.Identifier

	// originalSchemeName is optional, but when filled in it holds the name of the scheme from which this codec originates
	originalSchemeName string
}

var _ runtime.EncoderWithAllocator = &codec{}

var identifiersMap sync.Map

type codecIdentifier struct {
	EncodeGV string `json:"encodeGV,omitempty"`
	Encoder  string `json:"encoder,omitempty"`
	Name     string `json:"name,omitempty"`
}

// identifier computes Identifier of Encoder based on codec parameters.
func identifier(encodeGV runtime.GroupVersioner, encoder runtime.Encoder) runtime.Identifier {
	result := codecIdentifier{
		Name: "versioning",
	}

	if encodeGV != nil {
		result.EncodeGV = encodeGV.Identifier()
	}
	if encoder != nil {
		result.Encoder = string(encoder.Identifier())
	}
	if id, ok := identifiersMap.Load(result); ok {
		return id.(runtime.Identifier)
	}
	identifier, err := json.Marshal(result)
	if err != nil {
		//nolint:logcheck // Should not be reached.
		klog.Fatalf("Failed marshaling identifier for codec: %v", err)
	}
	identifiersMap.Store(result, runtime.Identifier(identifier))
	return runtime.Identifier(identifier)
}

// Decode attempts a decode of the object, then tries to convert it to the internal version. If into is provided and the decoding is
// successful, the returned runtime.Object will be the value passed as into. Note that this may bypass conversion if you pass an
// into that matches the serialized version.
func (c *codec) Decode(data []byte, defaultGVK *schema.GroupVersionKind, into runtime.Object) (runtime.Object, *schema.GroupVersionKind, error) {
	// If the into object is unstructured and expresses an opinion about its group/version,
	// create a new instance of the type so we always exercise the conversion path (skips short-circuiting on `into == obj`)
	decodeInto := into
	if into != nil {
		if _, ok := into.(runtime.Unstructured); ok && !into.GetObjectKind().GroupVersionKind().GroupVersion().Empty() {
			decodeInto = reflect.New(reflect.TypeOf(into).Elem()).Interface().(runtime.Object)
		}
	}

	var strictDecodingErrs []error
	obj, gvk, err := c.decoder.Decode(data, defaultGVK, decodeInto)
	if err != nil {
		if strictErr, ok := runtime.AsStrictDecodingError(err); obj != nil && ok {
			// save the strictDecodingError and let the caller decide what to do with it
			strictDecodingErrs = append(strictDecodingErrs, strictErr.Errors()...)
		} else {
			return nil, gvk, err
		}
	}

	if d, ok := obj.(runtime.NestedObjectDecoder); ok {
		if err := d.DecodeNestedObjects(runtime.WithoutVersionDecoder{Decoder: c.decoder}); err != nil {
			if strictErr, ok := runtime.AsStrictDecodingError(err); ok {
				// save the strictDecodingError let and the caller decide what to do with it
				strictDecodingErrs = append(strictDecodingErrs, strictErr.Errors()...)
			} else {
				return nil, gvk, err

			}
		}
	}

	// aggregate the strict decoding errors into one
	var strictDecodingErr error
	if len(strictDecodingErrs) > 0 {
		strictDecodingErr = runtime.NewStrictDecodingError(strictDecodingErrs)
	}
	// if we specify a target, use generic conversion.
	if into != nil {
		// perform defaulting if requested
		if c.defaulter != nil {
			c.defaulter.Default(obj)
		}

		// Short-circuit conversion if the into object is same object
		if into == obj {
			return into, gvk, strictDecodingErr
		}

		if err := c.convertor.Convert(obj, into, c.decodeVersion); err != nil {
			return nil, gvk, err
		}

		return into, gvk, strictDecodingErr
	}

	// perform defaulting if requested
	if c.defaulter != nil {
		c.defaulter.Default(obj)
	}

	out, err := c.convertor.ConvertToVersion(obj, c.decodeVersion)
	if err != nil {
		return nil, gvk, err
	}
	return out, gvk, strictDecodingErr
}

// EncodeWithAllocator ensures the provided object is output in the appropriate group and version, invoking
// conversion if necessary. Unversioned objects (according to the ObjectTyper) are output as is.
// In addition, it allows for providing a memory allocator for efficient memory usage during object serialization.
func (c *codec) EncodeWithAllocator(obj runtime.Object, w io.Writer, memAlloc runtime.MemoryAllocator) error {
	return c.encode(obj, w, memAlloc)
}

// Encode ensures the provided object is output in the appropriate group and version, invoking
// conversion if necessary. Unversioned objects (according to the ObjectTyper) are output as is.
func (c *codec) Encode(obj runtime.Object, w io.Writer) error {
	return c.encode(obj, w, nil)
}

func (c *codec) encode(obj runtime.Object, w io.Writer, memAlloc runtime.MemoryAllocator) error {
	if co, ok := obj.(runtime.CacheableObject); ok {
		return co.CacheEncode(c.Identifier(), func(obj runtime.Object, w io.Writer) error { return c.doEncode(obj, w, memAlloc) }, w)
	}
	return c.doEncode(obj, w, memAlloc)
}

func (c *codec) encodeToWriter(obj runtime.Object, w io.Writer, memAlloc runtime.MemoryAllocator) error {
	if memAlloc != nil {
		if encoder, supportsAllocator := c.encoder.(runtime.EncoderWithAllocator); supportsAllocator {
			return encoder.EncodeWithAllocator(obj, w, memAlloc)
		}
		//nolint:logcheck // Extending the API is not worth it for contextual, structured logging of this.
		klog.V(6).Infof("a memory allocator was provided but the encoder %s doesn't implement the runtime.EncoderWithAllocator, using regular encoder.Encode method", c.encoder.Identifier())
	}
	return c.encoder.Encode(obj, w)
}

func (c *codec) doEncode(obj runtime.Object, w io.Writer, memAlloc runtime.MemoryAllocator) error {
	switch obj := obj.(type) {
	case *runtime.Unknown:
		return c.encodeToWriter(obj, w, memAlloc)
	case runtime.Unstructured:
		// An unstructured list can contain objects of multiple group version kinds. don't short-circuit just
		// because the top-level type matches our desired destination type. actually send the object to the converter
		// to give it a chance to convert the list items if needed.
		if _, ok := obj.(*unstructured.UnstructuredList); !ok {
			// avoid conversion roundtrip if GVK is the right one already or is empty (yes, this is a hack, but the old behaviour we rely on in kubectl)
			objGVK := obj.GetObjectKind().GroupVersionKind()
			if len(objGVK.Version) == 0 {
				return c.encodeToWriter(obj, w, memAlloc)
			}
			targetGVK, ok := c.encodeVersion.KindForGroupVersionKinds([]schema.GroupVersionKind{objGVK})
			if !ok {
				return runtime.NewNotRegisteredGVKErrForTarget(c.originalSchemeName, objGVK, c.encodeVersion)
			}
			if targetGVK == objGVK {
				return c.encodeToWriter(obj, w, memAlloc)
			}
		}
	}

	gvks, isUnversioned, err := c.typer.ObjectKinds(obj)
	if err != nil {
		return err
	}

	objectKind := obj.GetObjectKind()
	old := objectKind.GroupVersionKind()
	// restore the old GVK after encoding
	defer objectKind.SetGroupVersionKind(old)

	if c.encodeVersion == nil || isUnversioned {
		if e, ok := obj.(runtime.NestedObjectEncoder); ok {
			if err := e.EncodeNestedObjects(runtime.WithVersionEncoder{Encoder: c.encoder, ObjectTyper: c.typer}); err != nil {
				return err
			}
		}
		objectKind.SetGroupVersionKind(gvks[0])
		return c.encodeToWriter(obj, w, memAlloc)
	}

	// Perform a conversion if necessary
	out, err := c.convertor.ConvertToVersion(obj, c.encodeVersion)
	if err != nil {
		return err
	}

	if e, ok := out.(runtime.NestedObjectEncoder); ok {
		if err := e.EncodeNestedObjects(runtime.WithVersionEncoder{Version: c.encodeVersion, Encoder: c.encoder, ObjectTyper: c.typer}); err != nil {
			return err
		}
	}

	// Conversion is responsible for setting the proper group, version, and kind onto the outgoing object
	return c.encodeToWriter(out, w, memAlloc)
}

type captureAllocatorWriter struct {
	lastAlloc []byte
	out       []byte
}

func (c *captureAllocatorWriter) Allocate(n uint64) []byte {
	b := make([]byte, n)
	c.lastAlloc = b
	return b
}

func (c *captureAllocatorWriter) Write(p []byte) (int, error) {
	if c.out == nil && len(p) > 0 && len(c.lastAlloc) >= len(p) && &p[0] == &c.lastAlloc[0] {
		c.out = p
		return len(p), nil
	}
	c.out = append(c.out, p...)
	return len(p), nil
}

// EncodeReturningVersioned encodes obj to a byte slice and also returns the intermediate
// versioned runtime.Object when obj was converted to a distinct external struct.
func (c *codec) EncodeReturningVersioned(obj runtime.Object) ([]byte, runtime.Object, error) {
	if _, ok := obj.(runtime.CacheableObject); ok {
		b, err := runtime.Encode(c, obj)
		return b, nil, err
	}
	switch obj.(type) {
	case *runtime.Unknown, runtime.Unstructured, runtime.NestedObjectEncoder, runtime.NestedObjectDecoder:
		var caw captureAllocatorWriter
		if err := c.doEncode(obj, &caw, &caw); err != nil {
			return nil, nil, err
		}
		return caw.out, nil, nil
	}
	if c.encodeVersion == nil {
		var caw captureAllocatorWriter
		if err := c.doEncode(obj, &caw, &caw); err != nil {
			return nil, nil, err
		}
		return caw.out, nil, nil
	}
	gvks, isUnversioned, err := c.typer.ObjectKinds(obj)
	if err != nil {
		return nil, nil, err
	}
	if isUnversioned {
		var caw captureAllocatorWriter
		objectKind := obj.GetObjectKind()
		old := objectKind.GroupVersionKind()
		objectKind.SetGroupVersionKind(gvks[0])
		err := c.encodeToWriter(obj, &caw, &caw)
		objectKind.SetGroupVersionKind(old)
		if err != nil {
			return nil, nil, err
		}
		return caw.out, nil, nil
	}

	objectKind := obj.GetObjectKind()
	old := objectKind.GroupVersionKind()
	out, err := c.convertor.ConvertToVersion(obj, c.encodeVersion)
	objectKind.SetGroupVersionKind(old)
	if err != nil {
		return nil, nil, err
	}
	if e, ok := out.(runtime.NestedObjectEncoder); ok {
		if err := e.EncodeNestedObjects(runtime.WithVersionEncoder{Version: c.encodeVersion, Encoder: c.encoder, ObjectTyper: c.typer}); err != nil {
			return nil, nil, err
		}
		var caw captureAllocatorWriter
		if err := c.encodeToWriter(out, &caw, &caw); err != nil {
			return nil, nil, err
		}
		return caw.out, nil, nil
	}
	var caw captureAllocatorWriter
	if err := c.encodeToWriter(out, &caw, &caw); err != nil {
		return nil, nil, err
	}
	if out == obj {
		return caw.out, nil, nil
	}
	if _, ok := out.(runtime.NestedObjectDecoder); ok {
		return caw.out, nil, nil
	}
	return caw.out, out, nil
}

var (
	stdTimeType     = reflect.TypeOf(time.Time{})
	metaTimeType    = reflect.TypeOf(metav1.Time{})
	metaMicroType   = reflect.TypeOf(metav1.MicroTime{})
)

func normalizeVersionedForDecode(v reflect.Value) {
	switch v.Kind() {
	case reflect.Struct:
		t := v.Type()
		if t == metaTimeType {
			if v.CanAddr() {
				tp := (*metav1.Time)(v.Addr().UnsafePointer())
				if !tp.Time.IsZero() {
					tp.Time = time.Unix(tp.Time.Unix(), 0).Local()
				}
			}
			return
		}
		if t == metaMicroType {
			if v.CanAddr() {
				tp := (*metav1.MicroTime)(v.Addr().UnsafePointer())
				if !tp.Time.IsZero() {
					truncated := time.Duration(tp.Time.Nanosecond()).Truncate(time.Microsecond)
					tp.Time = time.Unix(tp.Time.Unix(), int64(truncated)).Local()
				}
			}
			return
		}
		if t == stdTimeType {
			return
		}
		for i, n := 0, v.NumField(); i < n; i++ {
			f := v.Field(i)
			if f.CanSet() {
				normalizeVersionedForDecode(f)
			}
		}
	case reflect.Slice:
		if v.IsNil() {
			return
		}
		if v.Len() == 0 {
			v.SetZero()
			return
		}
		elemKind := v.Type().Elem().Kind()
		if elemKind == reflect.Struct || elemKind == reflect.Ptr || elemKind == reflect.Slice || elemKind == reflect.Map {
			for i, n := 0, v.Len(); i < n; i++ {
				normalizeVersionedForDecode(v.Index(i))
			}
		}
	case reflect.Map:
		if !v.IsNil() && v.Len() == 0 {
			v.SetZero()
		}
	case reflect.Ptr:
		if !v.IsNil() {
			normalizeVersionedForDecode(v.Elem())
		}
	}
}

// DecodeVersionedInto defaults the intermediate versioned object and converts it into into,
// bypassing byte unmarshaling.
func (c *codec) DecodeVersionedInto(versionedObj runtime.Object, into runtime.Object) error {
	if versionedObj == nil || into == nil {
		return fmt.Errorf("nil object")
	}
	if rv := reflect.ValueOf(versionedObj); rv.Kind() == reflect.Ptr && !rv.IsNil() {
		normalizeVersionedForDecode(rv.Elem())
	}
	if c.defaulter != nil {
		c.defaulter.Default(versionedObj)
	}
	if into == versionedObj {
		return nil
	}
	if iv := reflect.ValueOf(into); iv.Kind() == reflect.Ptr && !iv.IsNil() {
		iv.Elem().SetZero()
	}
	return c.convertor.Convert(versionedObj, into, c.decodeVersion)
}

// Identifier implements runtime.Encoder interface.
func (c *codec) Identifier() runtime.Identifier {
	return c.identifier
}
