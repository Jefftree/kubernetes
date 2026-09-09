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

// Package podproto is a spike, not production code.
//
// It answers one question: can the watch cache keep a pod's spec and status as
// opaque bytes, decoding only ObjectMeta, and still do everything the cache
// needs? The two things it has to prove are that the selectable fields can be
// read straight out of the encoded spec and status, and that a servable object
// can be reassembled from a re-encoded ObjectMeta plus the untouched tail.
//
// Everything here is specific to v1.Pod and to protobuf storage. A real
// implementation would need a fallback for every other combination.
package podproto

import (
	"encoding/binary"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
)

// Top-level v1.Pod field numbers, from k8s.io/api/core/v1/generated.proto.
const (
	podMetadata = 1
	podSpec     = 2
	podStatus   = 3
)

// The PodSpec and PodStatus fields that pod.ToSelectableFields reads. If that
// function changes, this list has to change with it, which is the standing cost
// of this approach.
const (
	specRestartPolicy      = 3
	specServiceAccountName = 8
	specNodeName           = 10
	specHostNetwork        = 11
	specSchedulerName      = 19

	statusPhase             = 1
	statusPodIP             = 6
	statusNominatedNodeName = 11
	statusPodIPs            = 12

	podIPIP = 1
)

const wireVarint = 0
const wireBytes = 2

// SplitPod locates the three top-level fields of an encoded v1.Pod without
// decoding any of them. The returned slices alias raw.
//
// tail is everything from the spec field onwards, kept verbatim so it can be
// spliced back out again. Keeping it as one span rather than two means unknown
// future fields survive a round trip untouched.
func SplitPod(raw []byte) (meta, spec, status, tail []byte, err error) {
	i := 0
	metaEnd := -1
	for i < len(raw) {
		key, n := binary.Uvarint(raw[i:])
		if n <= 0 {
			return nil, nil, nil, nil, fmt.Errorf("bad tag at %d", i)
		}
		i += n
		num, wire := int(key>>3), key&7
		if wire != wireBytes {
			return nil, nil, nil, nil, fmt.Errorf("field %d: unexpected wire type %d", num, wire)
		}
		l, n := binary.Uvarint(raw[i:])
		if n <= 0 {
			return nil, nil, nil, nil, fmt.Errorf("bad length at %d", i)
		}
		i += n
		if i+int(l) > len(raw) {
			return nil, nil, nil, nil, fmt.Errorf("field %d overruns buffer", num)
		}
		val := raw[i : i+int(l)]
		switch num {
		case podMetadata:
			meta = val
			metaEnd = i + int(l)
		case podSpec:
			spec = val
		case podStatus:
			status = val
		}
		i += int(l)
	}
	if meta == nil {
		return nil, nil, nil, nil, fmt.Errorf("no metadata field")
	}
	return meta, spec, status, raw[metaEnd:], nil
}

// scan walks a message's fields, calling visit for each one with its number,
// wire type and value bytes (for length-delimited) or varint value. It never
// allocates.
func scan(b []byte, visit func(num int, wire uint64, val []byte, v uint64) bool) error {
	i := 0
	for i < len(b) {
		key, n := binary.Uvarint(b[i:])
		if n <= 0 {
			return fmt.Errorf("bad tag at %d", i)
		}
		i += n
		num, wire := int(key>>3), key&7
		switch wire {
		case wireVarint:
			v, n := binary.Uvarint(b[i:])
			if n <= 0 {
				return fmt.Errorf("bad varint at %d", i)
			}
			i += n
			if !visit(num, wire, nil, v) {
				return nil
			}
		case wireBytes:
			l, n := binary.Uvarint(b[i:])
			if n <= 0 {
				return fmt.Errorf("bad length at %d", i)
			}
			i += n
			if i+int(l) > len(b) {
				return fmt.Errorf("field %d overruns buffer", num)
			}
			if !visit(num, wire, b[i:i+int(l)], 0) {
				return nil
			}
			i += int(l)
		case 5:
			i += 4
		case 1:
			i += 8
		default:
			return fmt.Errorf("field %d: unsupported wire type %d", num, wire)
		}
	}
	return nil
}

// SelectableFields reproduces pod.ToSelectableFields by reading the encoded
// spec and status directly, with no decode of either.
//
// The object-meta half is not here: it comes from the decoded ObjectMeta, which
// this design keeps anyway.
func SelectableFields(spec, status []byte, om *metav1.ObjectMeta) (fields.Set, error) {
	s := make(fields.Set, 10)
	s["spec.nodeName"] = ""
	s["spec.restartPolicy"] = ""
	s["spec.schedulerName"] = ""
	s["spec.serviceAccountName"] = ""
	s["spec.hostNetwork"] = "false"
	s["status.phase"] = ""
	s["status.podIP"] = ""
	s["status.nominatedNodeName"] = ""

	err := scan(spec, func(num int, wire uint64, val []byte, v uint64) bool {
		switch num {
		case specNodeName:
			s["spec.nodeName"] = string(val)
		case specRestartPolicy:
			s["spec.restartPolicy"] = string(val)
		case specSchedulerName:
			s["spec.schedulerName"] = string(val)
		case specServiceAccountName:
			s["spec.serviceAccountName"] = string(val)
		case specHostNetwork:
			if v != 0 {
				s["spec.hostNetwork"] = "true"
			}
		}
		return true
	})
	if err != nil {
		return nil, fmt.Errorf("scanning spec: %w", err)
	}

	firstPodIP := ""
	seenPodIPs := false
	err = scan(status, func(num int, wire uint64, val []byte, v uint64) bool {
		switch num {
		case statusPhase:
			s["status.phase"] = string(val)
		case statusNominatedNodeName:
			s["status.nominatedNodeName"] = string(val)
		case statusPodIPs:
			// Repeated PodIP; ToSelectableFields uses only the first.
			if !seenPodIPs {
				seenPodIPs = true
				_ = scan(val, func(n2 int, w2 uint64, v2 []byte, _ uint64) bool {
					if n2 == podIPIP {
						firstPodIP = string(v2)
					}
					return false
				})
			}
		}
		return true
	})
	if err != nil {
		return nil, fmt.Errorf("scanning status: %w", err)
	}
	s["status.podIP"] = firstPodIP

	s["metadata.name"] = om.Name
	s["metadata.namespace"] = om.Namespace
	return s, nil
}

// Labels come from the decoded ObjectMeta, free of charge.
func Labels(om *metav1.ObjectMeta) labels.Set { return labels.Set(om.Labels) }

// Assemble rebuilds a complete encoded v1.Pod from a re-encoded ObjectMeta and
// the untouched tail. This is what serving costs: one small encode plus a copy.
func Assemble(om *metav1.ObjectMeta, tail []byte) ([]byte, error) {
	// Size first so the whole response can be one right-sized allocation, with
	// the metadata marshalled straight into it. Marshalling to its own buffer
	// and then appending would allocate the metadata twice, which on a 60%
	// metadata pod costs more than the full encode this is meant to beat.
	metaSize := om.Size()
	out := make([]byte, 1+uvarintLen(uint64(metaSize))+metaSize+len(tail))
	i := binary.PutUvarint(out, uint64(podMetadata)<<3|wireBytes)
	i += binary.PutUvarint(out[i:], uint64(metaSize))
	n, err := om.MarshalTo(out[i:])
	if err != nil {
		return nil, err
	}
	i += n
	i += copy(out[i:], tail)
	return out[:i], nil
}

func uvarintLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

// Element is what the cache would retain per pod under this design: a decoded
// ObjectMeta, the opaque tail, and the precomputed attributes.
type Element struct {
	Meta   *metav1.ObjectMeta
	Tail   []byte
	Labels labels.Set
	Fields fields.Set
}

// Build does the whole ingest path: split, decode metadata only, extract the
// attributes from the encoded spec and status, stamp the resourceVersion.
func Build(raw []byte, resourceVersion string) (*Element, error) {
	meta, spec, status, tail, err := SplitPod(raw)
	if err != nil {
		return nil, err
	}
	om := &metav1.ObjectMeta{}
	if err := om.Unmarshal(meta); err != nil {
		return nil, fmt.Errorf("decoding metadata: %w", err)
	}
	om.ResourceVersion = resourceVersion
	f, err := SelectableFields(spec, status, om)
	if err != nil {
		return nil, err
	}
	return &Element{Meta: om, Tail: tail, Labels: Labels(om), Fields: f}, nil
}
