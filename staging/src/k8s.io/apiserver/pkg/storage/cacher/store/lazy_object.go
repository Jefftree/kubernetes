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

package store

import (
	"bytes"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
)

// LazyObject holds an API object in its encoded form and produces the typed
// form only when a caller actually needs one.
//
// Retaining the encoded form rather than a decoded object tree is the whole
// point: a realistic pod decodes to a few hundred separately allocated heap
// objects that the garbage collector must walk on every mark cycle, while the
// same pod encoded is a single []byte.
//
// Mutation safety is structural rather than conventional. raw is written once
// at construction and only ever read afterwards, and every typed
// materialization is a fresh decode, so no two callers can ever be handed the
// same decoded object. Nothing a caller does to a materialized object can be
// observed by the cache or by any other reader.
//
// LazyObject is a runtime.CacheableObject, so a request whose encoder produced
// raw is served the bytes verbatim with no decode and no re-encode. Requests
// using a different encoder fall back to decode-and-encode and their result is
// memoized per encoder identity, exactly as cachingObject does.
type LazyObject struct {
	// raw is the encoded object. It is immutable; anything that would write to
	// it is a correctness bug, and there is deliberately no accessor handing
	// out a mutable reference.
	raw []byte

	decoder runtime.Decoder

	// encodedBy identifies the encoder that produced raw.
	encodedBy runtime.Identifier

	// serializations memoizes encodings under identities other than
	// encodedBy. It holds a serializationsCache and is empty in the common
	// case where the requesting encoder is the one that produced raw.
	serializations atomic.Value
	lock           sync.Mutex
}

type serializationResult struct {
	once sync.Once
	raw  []byte
	err  error
}

type serializationsCache map[runtime.Identifier]*serializationResult

var (
	_ runtime.Object          = &LazyObject{}
	_ runtime.CacheableObject = &LazyObject{}
)

// NewLazyObject wraps encoded bytes produced by the encoder identified by
// encodedBy. Ownership of raw passes to the returned LazyObject; the caller
// must not retain or mutate it.
func NewLazyObject(raw []byte, decoder runtime.Decoder, encodedBy runtime.Identifier) *LazyObject {
	o := &LazyObject{raw: raw, decoder: decoder, encodedBy: encodedBy}
	o.serializations.Store(make(serializationsCache))
	return o
}

// Materialize decodes the object. The result is owned solely by the caller.
func (o *LazyObject) Materialize() (runtime.Object, error) {
	obj, err := runtime.Decode(o.decoder, o.raw)
	if err != nil {
		return nil, fmt.Errorf("lazy decode of %d cached bytes: %w", len(o.raw), err)
	}
	return obj, nil
}

// EncodedSize is the number of bytes retained for this object, excluding any
// memoized alternative serializations.
func (o *LazyObject) EncodedSize() int { return len(o.raw) }

// GetObject implements runtime.CacheableObject.
func (o *LazyObject) GetObject() runtime.Object {
	obj, err := o.Materialize()
	if err != nil {
		utilruntime.HandleError(err)
		return nil
	}
	return obj
}

// CacheEncode implements runtime.CacheableObject.
func (o *LazyObject) CacheEncode(id runtime.Identifier, encode func(runtime.Object, io.Writer) error, w io.Writer) error {
	if id == o.encodedBy {
		return write(w, o.raw)
	}
	result := o.getSerializationResult(id)
	result.once.Do(func() {
		obj, err := o.Materialize()
		if err != nil {
			result.err = err
			return
		}
		buf := runtime.NewSpliceBuffer()
		if result.err = encode(obj, buf); result.err != nil {
			return
		}
		encoded := buf.Bytes()
		// The storage encoder and the encoder serving a request for the same
		// group-version and media type produce identical bytes but report
		// different runtime.Identifiers, because the storage side targets a
		// multiGroupVersioner. Detecting that here keeps a second copy of
		// every cached object out of the heap, which is the whole point.
		if bytes.Equal(encoded, o.raw) {
			result.raw = o.raw
			return
		}
		result.raw = encoded
	})
	if result.err != nil {
		return result.err
	}
	return write(w, result.raw)
}

func write(w io.Writer, raw []byte) error {
	if splicer, ok := w.(runtime.Splice); ok {
		splicer.Splice(raw)
		return nil
	}
	_, err := w.Write(raw)
	return err
}

func (o *LazyObject) getSerializationResult(id runtime.Identifier) *serializationResult {
	cache := o.serializations.Load().(serializationsCache)
	if result, exists := cache[id]; exists {
		return result
	}

	o.lock.Lock()
	defer o.lock.Unlock()

	cache = o.serializations.Load().(serializationsCache)
	if result, exists := cache[id]; exists {
		return result
	}
	next := make(serializationsCache, len(cache)+1)
	for k, v := range cache {
		next[k] = v
	}
	result := &serializationResult{}
	next[id] = result
	o.serializations.Store(next)
	return result
}

// GetObjectKind implements runtime.Object. Objects decoded from storage into
// the memory version carry empty TypeMeta, so a lazy object reports the same.
func (o *LazyObject) GetObjectKind() schema.ObjectKind { return schema.EmptyObjectKind }

// DeepCopyObject implements runtime.Object.
//
// A LazyObject is immutable, so the copy shares raw. This is only sound
// because LazyObject exposes no setter: a caller that intends to mutate must
// go through Materialize (or Element.TypedObject), which decodes.
func (o *LazyObject) DeepCopyObject() runtime.Object {
	return NewLazyObject(o.raw, o.decoder, o.encodedBy)
}

// Materialize returns obj itself when it is already typed, and a fresh decode
// when it is lazy.
func Materialize(obj runtime.Object) (runtime.Object, error) {
	if lazy, ok := obj.(*LazyObject); ok {
		return lazy.Materialize()
	}
	return obj, nil
}

// EncodeToLazyObject encodes obj and wraps the result. The encoder must be the
// one whose Identifier is passed, so that a request using the same encoder can
// be served the bytes without re-encoding.
func EncodeToLazyObject(encoder runtime.Encoder, decoder runtime.Decoder, obj runtime.Object) (*LazyObject, error) {
	var w exactWriter
	if err := encoder.Encode(obj, &w); err != nil {
		return nil, err
	}
	return NewLazyObject(w.buf, decoder, encoder.Identifier()), nil
}

// exactWriter captures written bytes into a right-sized buffer. bytes.Buffer
// would leave slack capacity, and slack is retained for the lifetime of the
// cache entry, which is exactly what this change exists to shrink.
type exactWriter struct {
	buf []byte
}

func (e *exactWriter) Write(p []byte) (int, error) {
	if e.buf == nil {
		e.buf = make([]byte, len(p))
		copy(e.buf, p)
		return len(p), nil
	}
	e.buf = append(e.buf, p...)
	return len(p), nil
}
