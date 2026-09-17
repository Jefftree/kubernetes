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

package api

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/apitesting/fuzzer"
	genericfuzzer "k8s.io/apimachinery/pkg/apis/meta/fuzzer"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

// ghostMarshaler is the known-bad shape, used to prove the detector can report a
// non-empty result. It serialises a key that has no field to decode back into.
type ghostMarshaler struct {
	// known is the only field that decodes back, MarshalJSON also emits "ghost".
	// +required
	Known string `json:"known"`
}

func (g ghostMarshaler) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf(`{"known":%q,"ghost":"seen"}`, g.Known)), nil
}

type ghostRoot struct {
	// untouched carries the known-bad type, so the detector has one to find.
	// +optional
	Untouched *ghostMarshaler `json:"untouched,omitempty"`
}

// TestMarshalerUnmarshalerSymmetry asserts that no type reachable from a built-in API
// type implements json.Marshaler without also implementing json.Unmarshaler.
//
// runtime.DefaultUnstructuredConverter is asymmetric about custom JSON methods:
// toUnstructured delegates to MarshalJSON, but fromUnstructured only delegates to
// UnmarshalJSON if the type has one, otherwise it walks the type and reports any source
// key matching no json tag. So a type with only MarshalJSON can emit a key the converter
// then calls unknown on the way back. That breaks the pruned strategic-merge-patch path
// in apiserver/pkg/endpoints/handlers/patch.go, whose soundness argument is that a
// subtree originating from a typed struct cannot contain unknown fields.
//
// To fix a failure, give the named type an UnmarshalJSON accepting what its MarshalJSON
// produces. A type implementing both is safe whatever it emits, because the converter
// then routes it through JSON in both directions and never runs the unknown-field walk.
func TestMarshalerUnmarshalerSymmetry(t *testing.T) {
	marshaler := reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	unmarshaler := reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()

	seen := map[reflect.Type]bool{}
	var offenders []string
	var walk func(rt reflect.Type, path string)
	walk = func(rt reflect.Type, path string) {
		if seen[rt] {
			return
		}
		seen[rt] = true

		ptr := reflect.PointerTo(rt)
		if (rt.Implements(marshaler) || ptr.Implements(marshaler)) &&
			!(rt.Implements(unmarshaler) || ptr.Implements(unmarshaler)) {
			offenders = append(offenders, fmt.Sprintf("%s (reached via %s)", rt.String(), path))
		}

		switch rt.Kind() {
		case reflect.Struct:
			for i := 0; i < rt.NumField(); i++ {
				f := rt.Field(i)
				if f.PkgPath != "" {
					continue // unexported, never serialised
				}
				walk(f.Type, path+"."+f.Name)
			}
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			walk(rt.Elem(), path+"[]")
		}
	}

	// A detector that reports zero must first be shown able to report one, or a green
	// result cannot be distinguished from a walk that never ran.
	walk(reflect.TypeOf(ghostRoot{}), "self-check")
	if len(offenders) != 2 { // the pointer field and its element type
		t.Fatalf("self-check failed: detector found %v on the known-bad type, want it in both pointer and value form", offenders)
	}
	for _, o := range offenders {
		if !strings.Contains(o, "ghostMarshaler") {
			t.Fatalf("self-check failed: unexpected offender %q", o)
		}
	}
	offenders = nil

	scheme := runtime.NewScheme()
	for _, builder := range groups {
		if err := builder.AddToScheme(scheme); err != nil {
			t.Fatalf("unexpected error adding to scheme: %v", err)
		}
	}
	kinds := scheme.AllKnownTypes()
	if len(kinds) < 100 {
		t.Fatalf("scheme has %d known types, expected every built-in group", len(kinds))
	}
	for gvk, rt := range kinds {
		walk(rt, gvk.String())
	}
	if len(seen) < 500 {
		t.Fatalf("walked only %d distinct types, expected the reachable graph to be far larger", len(seen))
	}
	t.Logf("walked %d distinct types reachable from %d registered kinds", len(seen), len(kinds))

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("these types implement json.Marshaler but not json.Unmarshaler, which breaks "+
			"unstructured round-trip equivalence (see this test's doc comment):\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// TestUnstructuredRoundTripReportsNoUnknownFields asserts the same property
// behaviourally rather than through the structural proxy above: for every registered
// kind, converting a populated instance to unstructured and back with unknown-field
// reporting on must report nothing. It catches routes the proxy cannot see, such as a
// field a Marshaler emits and its own Unmarshaler drops.
func TestUnstructuredRoundTripReportsNoUnknownFields(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, builder := range groups {
		if err := builder.AddToScheme(scheme); err != nil {
			t.Fatalf("unexpected error adding to scheme: %v", err)
		}
	}
	codecs := serializer.NewCodecFactory(scheme)
	seed := rand.Int63()
	t.Logf("fuzzing with seed %d", seed)
	f := fuzzer.FuzzerFor(genericfuzzer.Funcs, rand.NewSource(seed), codecs)

	kinds := scheme.AllKnownTypes()
	if len(kinds) < 100 {
		t.Fatalf("scheme has %d known types, expected every built-in group", len(kinds))
	}

	var checked, skipped int
	for gvk, rt := range kinds {
		if !unstructuredConvertible(rt, map[reflect.Type]bool{}) {
			// WatchEvent and anything else holding a runtime.Object behind an
			// interface. The patch handler never sees one; a patched object is
			// always a concrete registered kind.
			skipped++
			continue
		}
		t.Run(gvk.String(), func(t *testing.T) {
			for i := 0; i < 3; i++ {
				obj := reflect.New(rt).Interface()
				f.Fill(obj)

				m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
				if err != nil {
					t.Fatalf("ToUnstructured: %v", err)
				}
				back := reflect.New(rt).Interface()
				if err := runtime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(m, back, true); err != nil {
					t.Fatalf("round trip reported unknown fields, which breaks the pruned "+
						"patch path's soundness argument (see this test's doc comment): %v", err)
				}
			}
		})
		checked++
	}
	t.Logf("round-tripped %d of %d registered kinds, skipped %d as not convertible", checked, len(kinds), skipped)
	if checked < 100 {
		t.Fatalf("only %d kinds were round-tripped; the rest were skipped, so this proves too little", checked)
	}
}

// unstructuredConvertible reports whether the unstructured converter can restore rt.
// It cannot restore an interface-typed field, which is what runtime.Object and
// runtime.RawExtension.Object are.
func unstructuredConvertible(rt reflect.Type, seen map[reflect.Type]bool) bool {
	if seen[rt] {
		return true
	}
	seen[rt] = true

	switch rt.Kind() {
	case reflect.Interface:
		return false
	case reflect.Struct:
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if f.PkgPath != "" {
				continue // unexported, never serialised
			}
			if !unstructuredConvertible(f.Type, seen) {
				return false
			}
		}
		return true
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		return unstructuredConvertible(rt.Elem(), seen)
	default:
		return true
	}
}
