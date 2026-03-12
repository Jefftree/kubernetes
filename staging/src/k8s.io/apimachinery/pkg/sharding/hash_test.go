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

package sharding

import (
	"testing"
)

func TestHashField(t *testing.T) {
	tests := []struct {
		input string
	}{
		{""},
		{"abc"},
		{"test-uid-12345"},
		{"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"},
	}

	for _, tt := range tests {
		result := HashField(tt.input)
		if len(result) != 18 {
			t.Errorf("HashField(%q) returned %q (len %d), expected 18 chars (0x + 16 hex)", tt.input, result, len(result))
		}
		if result[:2] != "0x" {
			t.Errorf("HashField(%q) returned %q, expected 0x prefix", tt.input, result)
		}
		// Verify digits after prefix are all hex chars
		for _, c := range result[2:] {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				t.Errorf("HashField(%q) returned %q which contains non-hex char %q", tt.input, result, string(c))
			}
		}
	}

	// Determinism
	h1 := HashField("test")
	h2 := HashField("test")
	if h1 != h2 {
		t.Errorf("HashField is not deterministic: %q != %q", h1, h2)
	}

	// Different inputs produce different outputs
	h3 := HashField("different")
	if h1 == h3 {
		t.Errorf("HashField produced same hash for different inputs: %q", h1)
	}
}
