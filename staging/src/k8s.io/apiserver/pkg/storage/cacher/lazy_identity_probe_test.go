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
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	examplev1 "k8s.io/apiserver/pkg/apis/example/v1"
)

// This file probes the load-bearing assumption behind splicing raw etcd bytes
// straight to the wire: that the encoder a client request ends up with is
// byte-for-byte and identity-for-identity the same as the encoder the storage
// layer used to write those bytes.
//
// Nothing here is imported from k8s.io/apiserver/pkg/server/storage or
// k8s.io/apiserver/pkg/endpoints/handlers - both transitively import this
// package, so an in-package test cannot import them. The two constructions
// below are transcribed from those packages and cite the source they mirror.

// coreInternalGV is the memory (internal) version the apiserver uses for the
// core group; see k8s.io/kubernetes/pkg/api/legacyscheme.
var coreInternalGV = schema.GroupVersion{Group: "", Version: runtime.APIVersionInternal}

func serializerInfoOrDie(t *testing.T, mediaType string) runtime.SerializerInfo {
	t.Helper()
	info, ok := runtime.SerializerInfoForMediaType(codecs.SupportedMediaTypes(), mediaType)
	if !ok {
		t.Fatalf("no serializer for media type %q", mediaType)
	}
	return info
}

// newStorageEncoder mirrors the encode half of
// staging/src/k8s.io/apiserver/pkg/server/storage/storage_codec.go NewStorageCodec
// (the EncoderDecoratorFn is nil in every in-tree caller).
func newStorageEncoder(t *testing.T, mediaType string, storageGV, memoryGV schema.GroupVersion) runtime.Encoder {
	t.Helper()
	info := serializerInfoOrDie(t, mediaType)
	encodeVersioner := runtime.NewMultiGroupVersioner(
		storageGV,
		schema.GroupKind{Group: storageGV.Group},
		schema.GroupKind{Group: memoryGV.Group},
	)
	return codecs.EncoderForVersion(info.Serializer, encodeVersioner)
}

// newResponseEncoder mirrors
// staging/src/k8s.io/apiserver/pkg/endpoints/handlers/responsewriters/writers.go
// WriteObjectNegotiated: s.EncoderForVersion(serializer.Serializer, gv).
func newResponseEncoder(t *testing.T, mediaType string, gv schema.GroupVersion) runtime.Encoder {
	t.Helper()
	info := serializerInfoOrDie(t, mediaType)
	return codecs.EncoderForVersion(info.Serializer, gv)
}

// newWatchNegotiatedEncoder mirrors the transform==false branch of
// staging/src/k8s.io/apiserver/pkg/endpoints/handlers/watch.go serveWatchHandler
// (lines 127-133): scope.Serializer.EncoderForVersion(serializer.Serializer,
// contentKind.GroupVersion()), where contentKind is scope.Kind.
func newWatchNegotiatedEncoder(t *testing.T, mediaType string, gv schema.GroupVersion) runtime.Encoder {
	t.Helper()
	info := serializerInfoOrDie(t, mediaType)
	enc := codecs.EncoderForVersion(info.Serializer, gv)
	// watch.go wraps in an allocator when the encoder supports one.
	if withAlloc, ok := enc.(runtime.EncoderWithAllocator); ok {
		enc = runtime.NewEncoderWithAllocator(withAlloc, &runtime.Allocator{})
	}
	return enc
}

// probeWatchEmbeddedEncoder transcribes watchEmbeddedEncoder from
// staging/src/k8s.io/apiserver/pkg/endpoints/handlers/response.go for the
// target==nil case, which is what a plain (non-Table, non-PartialObjectMetadata)
// watch gets: Identifier() passes the inner encoder's identifier straight
// through, and doEncode is a no-op transform followed by the inner encode.
type probeWatchEmbeddedEncoder struct {
	encoder runtime.Encoder
}

func (e *probeWatchEmbeddedEncoder) Encode(obj runtime.Object, w io.Writer) error {
	if co, ok := obj.(runtime.CacheableObject); ok {
		return co.CacheEncode(e.Identifier(), e.doEncode, w)
	}
	return e.doEncode(obj, w)
}

func (e *probeWatchEmbeddedEncoder) doEncode(obj runtime.Object, w io.Writer) error {
	// doTransformObject with target==nil returns obj unchanged.
	return e.encoder.Encode(obj, w)
}

func (e *probeWatchEmbeddedEncoder) Identifier() runtime.Identifier {
	// embeddedIdentifier() with target==nil returns e.encoder.Identifier().
	return e.encoder.Identifier()
}

// spliceProbe is a stand-in for the lazy watch-cache entry: it holds the raw
// bytes the storage encoder produced and splices them to the wire when the
// requesting encoder's identity matches, exactly as cachingObject does.
type spliceProbe struct {
	obj        runtime.Object
	storageID  runtime.Identifier
	raw        []byte
	askedWith  runtime.Identifier
	spliceHits int
	fallbacks  int
}

var _ runtime.CacheableObject = &spliceProbe{}

func (p *spliceProbe) CacheEncode(id runtime.Identifier, encode func(runtime.Object, io.Writer) error, w io.Writer) error {
	p.askedWith = id
	if id == p.storageID {
		p.spliceHits++
		_, err := w.Write(p.raw)
		return err
	}
	p.fallbacks++
	return encode(p.obj, w)
}

func (p *spliceProbe) GetObject() runtime.Object        { return p.obj }
func (p *spliceProbe) GetObjectKind() schema.ObjectKind { return p.obj.GetObjectKind() }
func (p *spliceProbe) DeepCopyObject() runtime.Object {
	return &spliceProbe{obj: p.obj.DeepCopyObject(), storageID: p.storageID, raw: p.raw}
}

func encodeTo(t *testing.T, enc runtime.Encoder, obj runtime.Object) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := enc.Encode(obj, &buf); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

// firstDivergence reports a human-readable description of where two byte
// strings differ, with a short hexdump window around the divergence.
func firstDivergence(a, b []byte) string {
	if bytes.Equal(a, b) {
		return "<identical>"
	}
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for ; i < n; i++ {
		if a[i] != b[i] {
			break
		}
	}
	if i == n {
		return fmt.Sprintf("common prefix of %d bytes, then lengths differ: len(a)=%d len(b)=%d\n  a tail: % x\n  b tail: % x",
			n, len(a), len(b), tail(a, n), tail(b, n))
	}
	lo := i - 16
	if lo < 0 {
		lo = 0
	}
	return fmt.Sprintf("first divergence at byte %d (len(a)=%d len(b)=%d)\n  a[%d:%d]: % x\n  b[%d:%d]: % x",
		i, len(a), len(b), lo, min(lo+48, len(a)), a[lo:min(lo+48, len(a))], lo, min(lo+48, len(b)), b[lo:min(lo+48, len(b))])
}

func tail(b []byte, from int) []byte {
	if from >= len(b) {
		return nil
	}
	return b[from:min(from+32, len(b))]
}

func prettyIdentifier(id runtime.Identifier) string {
	var v interface{}
	if err := json.Unmarshal([]byte(id), &v); err != nil {
		return string(id)
	}
	out, err := json.MarshalIndent(v, "  ", "  ")
	if err != nil {
		return string(id)
	}
	return string(out)
}

// TestLazyIdentityProtobufGet answers questions 1 and 2: does the GET response
// encoder produce the same bytes as, and carry the same Identifier as, the
// storage encoder, for protobuf storage and a protobuf Accept header.
func TestLazyIdentityProtobufGet(t *testing.T) {
	pod := loadExemplarCorePod(t)

	storageEncoder := newStorageEncoder(t, runtime.ContentTypeProtobuf, corev1.SchemeGroupVersion, coreInternalGV)
	responseEncoder := newResponseEncoder(t, runtime.ContentTypeProtobuf, corev1.SchemeGroupVersion)

	storageBytes := encodeTo(t, storageEncoder, pod.DeepCopy())
	responseBytes := encodeTo(t, responseEncoder, pod.DeepCopy())

	t.Logf("storage bytes:  %d", len(storageBytes))
	t.Logf("response bytes: %d", len(responseBytes))

	// Q1: byte equality. This holds, and it is what makes splicing sound.
	if !bytes.Equal(storageBytes, responseBytes) {
		t.Errorf("Q1: storage and response protobuf bytes differ, splicing is UNSOUND:\n%s", firstDivergence(storageBytes, responseBytes))
	} else {
		t.Logf("Q1: storage and response protobuf bytes are IDENTICAL (%d bytes)", len(storageBytes))
	}

	// Q2: identifier equality. This does NOT hold. The storage encoder is built
	// over a multiGroupVersioner and the response encoder over a bare
	// schema.GroupVersion, so versioning.identifier() sees different encodeGV
	// inputs even though both resolve every in-tree object to the same target
	// GVK. A splice gated on Identifier() equality can therefore never fire.
	sid, rid := storageEncoder.Identifier(), responseEncoder.Identifier()
	t.Logf("Q2 storage  Identifier(): %s", sid)
	t.Logf("Q2 response Identifier(): %s", rid)
	if sid == rid {
		t.Errorf("Q2: identifiers are now equal - the encodeGV mismatch documented here has been fixed upstream, revisit this probe")
	} else {
		t.Logf("Q2: identifiers DIFFER (only the encodeGV field):\n  storage:\n  %s\n  response:\n  %s", prettyIdentifier(sid), prettyIdentifier(rid))
	}

	// Confirm the mismatch is entirely the group versioner: swapping the storage
	// encoder's versioner for the bare GV makes the identifiers coincide.
	info := serializerInfoOrDie(t, runtime.ContentTypeProtobuf)
	collapsed := codecs.EncoderForVersion(info.Serializer, corev1.SchemeGroupVersion)
	if collapsed.Identifier() != rid {
		t.Errorf("expected the bare-GV storage encoder to match the response encoder, got %s vs %s", collapsed.Identifier(), rid)
	}
	collapsedBytes := encodeTo(t, collapsed, pod.DeepCopy())
	if !bytes.Equal(collapsedBytes, storageBytes) {
		t.Errorf("bare-GV encoder produced different bytes than the multi-GV storage encoder:\n%s", firstDivergence(collapsedBytes, storageBytes))
	} else {
		t.Logf("bare-GV and multi-GV encoders agree byte-for-byte; only their identifiers disagree")
	}
}

// TestLazyIdentityStorageVersionerShape isolates *which* component of the
// identifier differs: the encodeGV, the encoder, or the name.
func TestLazyIdentityStorageVersionerShape(t *testing.T) {
	info := serializerInfoOrDie(t, runtime.ContentTypeProtobuf)
	t.Logf("protobuf serializer Identifier(): %q", info.Serializer.Identifier())

	multi := runtime.NewMultiGroupVersioner(
		corev1.SchemeGroupVersion,
		schema.GroupKind{Group: corev1.SchemeGroupVersion.Group},
		schema.GroupKind{Group: coreInternalGV.Group},
	)
	t.Logf("storage  GroupVersioner: %T Identifier()=%s", multi, multi.Identifier())
	t.Logf("response GroupVersioner: %T Identifier()=%s", corev1.SchemeGroupVersion, corev1.SchemeGroupVersion.Identifier())

	if multi.Identifier() == corev1.SchemeGroupVersion.Identifier() {
		t.Logf("group versioner identifiers MATCH")
	} else {
		t.Logf("group versioner identifiers DIFFER - this is the only differing input to versioning.identifier()")
	}

	// A non-core group where storage group == memory group: NewMultiGroupVersioner
	// collapses to the bare GroupVersion when all accepted group kinds share the
	// target's group, so the two identifiers should coincide there.
	appsGV := schema.GroupVersion{Group: "apps", Version: "v1"}
	appsInternal := schema.GroupVersion{Group: "apps", Version: runtime.APIVersionInternal}
	appsVersioner := runtime.NewMultiGroupVersioner(appsGV,
		schema.GroupKind{Group: appsGV.Group},
		schema.GroupKind{Group: appsInternal.Group},
	)
	t.Logf("apps storage GroupVersioner: %T Identifier()=%s", appsVersioner, appsVersioner.Identifier())
	if appsVersioner.Identifier() == appsGV.Identifier() {
		t.Logf("apps: storage and response group versioner identifiers MATCH")
	} else {
		t.Logf("apps: storage and response group versioner identifiers DIFFER")
	}
}

// TestLazyIdentityNonCoreGroupGet runs the same GET comparison for a non-core
// group, where the storage group and the memory group are the same non-empty
// string. That is the shape every API group other than core has, and it is the
// case where NewMultiGroupVersioner collapses to a bare GroupVersion.
func TestLazyIdentityNonCoreGroupGet(t *testing.T) {
	exampleV1 := examplev1.SchemeGroupVersion
	exampleInternal := schema.GroupVersion{Group: examplev1.GroupName, Version: runtime.APIVersionInternal}

	storageEncoder := newStorageEncoder(t, runtime.ContentTypeProtobuf, exampleV1, exampleInternal)
	responseEncoder := newResponseEncoder(t, runtime.ContentTypeProtobuf, exampleV1)

	sid, rid := storageEncoder.Identifier(), responseEncoder.Identifier()
	t.Logf("%s storage  Identifier(): %s", exampleV1, sid)
	t.Logf("%s response Identifier(): %s", exampleV1, rid)
	// NewStorageCodec always passes two accepted group kinds, so the
	// len(groupKinds)==1 collapse in NewMultiGroupVersioner never triggers and
	// non-core groups mismatch exactly like core does.
	if sid == rid {
		t.Errorf("non-core group: identifiers unexpectedly equal, the multi-versioner collapse now triggers - revisit this probe")
	} else {
		t.Logf("non-core group: identifiers DIFFER, same encodeGV mismatch as core")
	}

	pod := &examplev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: "default", ResourceVersion: "42"},
		Spec:       examplev1.PodSpec{NodeName: "node-1"},
	}
	storageBytes := encodeTo(t, storageEncoder, pod.DeepCopyObject())
	responseBytes := encodeTo(t, responseEncoder, pod.DeepCopyObject())
	if !bytes.Equal(storageBytes, responseBytes) {
		t.Errorf("non-core group FAIL: bytes differ:\n%s", firstDivergence(storageBytes, responseBytes))
	} else {
		t.Logf("non-core group PASS: bytes identical (%d bytes)", len(storageBytes))
	}
}

// TestLazyIdentityWatchEmbedded answers question 3: is the encoder the watch
// handler uses for the embedded object identity-equal and byte-equal to the
// storage encoder.
func TestLazyIdentityWatchEmbedded(t *testing.T) {
	pod := loadExemplarCorePod(t)

	storageEncoder := newStorageEncoder(t, runtime.ContentTypeProtobuf, corev1.SchemeGroupVersion, coreInternalGV)
	negotiated := newWatchNegotiatedEncoder(t, runtime.ContentTypeProtobuf, corev1.SchemeGroupVersion)
	embedded := &probeWatchEmbeddedEncoder{encoder: negotiated}

	sid := storageEncoder.Identifier()
	eid := embedded.Identifier()
	t.Logf("Q3 storage        Identifier(): %s", sid)
	t.Logf("Q3 watch embedded Identifier(): %s", eid)
	if sid == eid {
		t.Errorf("Q3: storage and watch embedded identifiers are now equal - revisit this probe")
	} else {
		t.Logf("Q3: storage and watch embedded identifiers DIFFER, same encodeGV mismatch as the GET path")
	}

	// The watch embedded identifier is exactly the GET response identifier: the
	// embedded encoder for a plain watch is the same construction, and
	// watchEmbeddedEncoder.embeddedIdentifier() passes through when target==nil.
	getID := newResponseEncoder(t, runtime.ContentTypeProtobuf, corev1.SchemeGroupVersion).Identifier()
	if eid != getID {
		t.Errorf("Q3: watch embedded identifier differs from the GET identifier: %s vs %s", eid, getID)
	} else {
		t.Logf("Q3: watch embedded identifier == GET response identifier")
	}

	storageBytes := encodeTo(t, storageEncoder, pod.DeepCopy())
	embeddedBytes := encodeTo(t, embedded, pod.DeepCopy())
	if !bytes.Equal(storageBytes, embeddedBytes) {
		t.Errorf("Q3: storage and watch embedded bytes differ, splicing into a watch stream is UNSOUND:\n%s", firstDivergence(storageBytes, embeddedBytes))
	} else {
		t.Logf("Q3: storage and watch embedded bytes are IDENTICAL (%d bytes)", len(storageBytes))
	}

	// The allocator wrapper watch.go applies must not perturb the identifier.
	plain := newResponseEncoder(t, runtime.ContentTypeProtobuf, corev1.SchemeGroupVersion)
	if plain.Identifier() != negotiated.Identifier() {
		t.Errorf("allocator wrapper changed the identifier: %s vs %s", plain.Identifier(), negotiated.Identifier())
	} else {
		t.Logf("allocator wrapper preserves the identifier")
	}
}

// TestLazyIdentityJSONNegativeControl answers question 4: a JSON client must
// NOT be handed the protobuf storage bytes.
func TestLazyIdentityJSONNegativeControl(t *testing.T) {
	pod := loadExemplarCorePod(t)

	storageEncoder := newStorageEncoder(t, runtime.ContentTypeProtobuf, corev1.SchemeGroupVersion, coreInternalGV)
	jsonEncoder := newResponseEncoder(t, runtime.ContentTypeJSON, corev1.SchemeGroupVersion)

	sid, jid := storageEncoder.Identifier(), jsonEncoder.Identifier()
	t.Logf("Q4 storage (protobuf) Identifier(): %s", sid)
	t.Logf("Q4 response (json)    Identifier(): %s", jid)
	if sid == jid {
		t.Fatalf("Q4 FAIL: protobuf storage and json response identifiers are equal - the splice would fire and corrupt the response")
	}
	t.Logf("Q4 PASS: identifiers differ, splice cannot fire")

	storageBytes := encodeTo(t, storageEncoder, pod.DeepCopy())
	jsonBytes := encodeTo(t, jsonEncoder, pod.DeepCopy())
	if bytes.Equal(storageBytes, jsonBytes) {
		t.Fatalf("Q4 FAIL: protobuf and json bytes are equal, which is impossible")
	}
	t.Logf("Q4 bytes differ as expected: protobuf=%d json=%d", len(storageBytes), len(jsonBytes))
}

// TestLazyIdentityServedVersionNegativeControl is the second negative control.
// A resource can be stored at one version and served at another (the
// SetResourceEncoding storage-version overrides). The stored bytes are at the
// storage version, so a request for a different served version must not splice.
// Keying the splice on an encoder built from the *storage* GV gives that for
// free, because encodeGV is part of the identifier.
func TestLazyIdentityServedVersionNegativeControl(t *testing.T) {
	storedAtV1 := newResponseEncoder(t, runtime.ContentTypeProtobuf, examplev1.SchemeGroupVersion)
	otherVersion := schema.GroupVersion{Group: examplev1.GroupName, Version: "v1beta1"}
	servedAtV1Beta1 := newResponseEncoder(t, runtime.ContentTypeProtobuf, otherVersion)

	t.Logf("splice key for bytes stored at %s: %s", examplev1.SchemeGroupVersion, storedAtV1.Identifier())
	t.Logf("encoder for a request served at %s: %s", otherVersion, servedAtV1Beta1.Identifier())
	if storedAtV1.Identifier() == servedAtV1Beta1.Identifier() {
		t.Fatalf("identifiers for different target versions are equal - a cross-version splice would fire")
	}
	t.Logf("identifiers differ across target versions, cross-version splice cannot fire")
}

// TestLazyIdentitySpliceEndToEnd drives the real runtime.CacheableObject
// mechanism. It runs the same three request paths against two candidate splice
// keys: the storage encoder's Identifier (the naive gate, which never fires)
// and the protobuf response encoder's Identifier (which fires for exactly the
// protobuf GET and watch paths and never for JSON).
func TestLazyIdentitySpliceEndToEnd(t *testing.T) {
	pod := loadExemplarCorePod(t)

	storageEncoder := newStorageEncoder(t, runtime.ContentTypeProtobuf, corev1.SchemeGroupVersion, coreInternalGV)
	storageBytes := encodeTo(t, storageEncoder, pod.DeepCopy())
	protobufResponseID := newResponseEncoder(t, runtime.ContentTypeProtobuf, corev1.SchemeGroupVersion).Identifier()

	keys := []struct {
		name string
		id   runtime.Identifier
		// wantSplice, indexed by request path below.
		wantSplice map[string]bool
	}{
		{
			name:       "keyed on storage encoder Identifier",
			id:         storageEncoder.Identifier(),
			wantSplice: map[string]bool{"protobuf GET": false, "protobuf WATCH embedded": false, "json GET": false},
		},
		{
			name:       "keyed on protobuf response encoder Identifier",
			id:         protobufResponseID,
			wantSplice: map[string]bool{"protobuf GET": true, "protobuf WATCH embedded": true, "json GET": false},
		},
	}

	paths := []struct {
		name    string
		encoder runtime.Encoder
	}{
		{"protobuf GET", newResponseEncoder(t, runtime.ContentTypeProtobuf, corev1.SchemeGroupVersion)},
		{"protobuf WATCH embedded", &probeWatchEmbeddedEncoder{encoder: newWatchNegotiatedEncoder(t, runtime.ContentTypeProtobuf, corev1.SchemeGroupVersion)}},
		{"json GET", newResponseEncoder(t, runtime.ContentTypeJSON, corev1.SchemeGroupVersion)},
	}

	for _, key := range keys {
		t.Run(key.name, func(t *testing.T) {
			t.Logf("splice key: %s", key.id)
			for _, path := range paths {
				t.Run(path.name, func(t *testing.T) {
					probe := &spliceProbe{
						obj:       pod.DeepCopy(),
						storageID: key.id,
						raw:       storageBytes,
					}
					var buf bytes.Buffer
					if err := path.encoder.Encode(probe, &buf); err != nil {
						t.Fatalf("encode: %v", err)
					}
					t.Logf("asked with: %s", probe.askedWith)
					t.Logf("hits=%d fallbacks=%d out=%d bytes", probe.spliceHits, probe.fallbacks, buf.Len())

					want := key.wantSplice[path.name]
					if want && probe.spliceHits != 1 {
						t.Errorf("expected the splice to fire, got hits=%d fallbacks=%d", probe.spliceHits, probe.fallbacks)
					}
					if !want && probe.spliceHits != 0 {
						t.Errorf("splice fired but must not have (hits=%d)", probe.spliceHits)
					}
					if want && !bytes.Equal(buf.Bytes(), storageBytes) {
						t.Errorf("spliced output is not the storage bytes:\n%s", firstDivergence(buf.Bytes(), storageBytes))
					}
					// Whether or not the splice fired, the bytes on the wire must be
					// correct for the requested media type.
					switch path.name {
					case "json GET":
						if !bytes.HasPrefix(bytes.TrimSpace(buf.Bytes()), []byte("{")) {
							t.Errorf("json client did not receive json, got prefix % x", buf.Bytes()[:min(16, buf.Len())])
						}
					default:
						if !bytes.Equal(buf.Bytes(), storageBytes) {
							t.Errorf("protobuf client received bytes that differ from the storage bytes:\n%s", firstDivergence(buf.Bytes(), storageBytes))
						}
					}
				})
			}
		})
	}
}
