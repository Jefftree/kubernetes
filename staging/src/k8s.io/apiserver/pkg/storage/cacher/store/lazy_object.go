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
	// encodedBy. It stays nil until something asks for one, because an eager
	// empty map would be an extra allocation on every cached object and the
	// whole point of this type is to hold as few as possible.
	serializations atomic.Pointer[serializationsCache]
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
	return &LazyObject{raw: raw, decoder: decoder, encodedBy: encodedBy}
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
	if cache := o.serializations.Load(); cache != nil {
		if result, exists := (*cache)[id]; exists {
			return result
		}
	}

	o.lock.Lock()
	defer o.lock.Unlock()

	// Copy on write: readers hold the old map without a lock.
	next := serializationsCache{}
	if cache := o.serializations.Load(); cache != nil {
		if result, exists := (*cache)[id]; exists {
			return result
		}
		for k, v := range *cache {
			next[k] = v
		}
	}
	result := &serializationResult{}
	next[id] = result
	o.serializations.Store(&next)
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

// EncodeToLazyObject encodes obj and wraps the result.
func EncodeToLazyObject(encoder runtime.Encoder, decoder runtime.Decoder, obj runtime.Object) (*LazyObject, error) {
	w := &captureWriter{}
	if alloc, ok := encoder.(runtime.EncoderWithAllocator); ok {
		// The protobuf serializer sizes the object, allocates once and writes
		// that buffer in a single call. Owning the allocation lets us keep
		// exactly that buffer instead of copying it into a second one, which
		// would double the allocation this change adds to the write path.
		w.owned = true
		if err := alloc.EncodeWithAllocator(obj, w, w); err != nil {
			return nil, err
		}
	} else if err := encoder.Encode(obj, w); err != nil {
		return nil, err
	}
	return NewLazyObject(w.bytes(), decoder, encoder.Identifier()), nil
}

// captureWriter is both the encoder's memory allocator and its writer, so a
// single-write encoder hands back a slice of a buffer we already own and no
// copy is needed. Anything that writes more than once, or that writes a buffer
// we did not allocate, falls back to accumulating into a right-sized slice:
// slack capacity would be retained for the lifetime of the cache entry, which
// is what this whole change exists to shrink.
type captureWriter struct {
	owned    bool
	arena    []byte
	captured []byte
	spill    []byte
	writes   int
}

func (w *captureWriter) Allocate(n uint64) []byte {
	w.arena = make([]byte, n)
	return w.arena
}

func (w *captureWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == 1 && w.owned && len(p) > 0 && sameArray(p, w.arena) {
		w.captured = p
		return len(p), nil
	}
	if w.writes == 1 {
		w.spill = make([]byte, 0, len(p))
	} else if w.captured != nil {
		// A second write means the first was not the whole encoding.
		w.spill = append(make([]byte, 0, len(w.captured)+len(p)), w.captured...)
		w.captured = nil
	}
	w.spill = append(w.spill, p...)
	return len(p), nil
}

func (w *captureWriter) bytes() []byte {
	if w.captured == nil {
		return w.spill
	}
	// The allocator sizes from an upper-bound estimate, so the captured slice
	// can carry a little slack. A little is fine; a lot would be retained for
	// the life of the cache entry, so right-size it instead.
	if slack := cap(w.captured) - len(w.captured); slack > 64+len(w.captured)/16 {
		exact := make([]byte, len(w.captured))
		copy(exact, w.captured)
		return exact
	}
	return w.captured
}

func sameArray(p, arena []byte) bool {
	return len(arena) > 0 && len(p) <= len(arena) && &p[0] == &arena[0]
}
