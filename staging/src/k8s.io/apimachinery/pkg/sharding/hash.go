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
	"fmt"
	"hash/fnv"
)

const (
	// FNV-1a 64-bit parameters, from hash/fnv.
	fnvOffset64 = 14695981039346656037
	fnvPrime64  = 1099511628211
)

// HashFieldValue computes the FNV-1a 64-bit hash of value without allocating.
func HashFieldValue(value string) uint64 {
	h := uint64(fnvOffset64)
	for i := 0; i < len(value); i++ {
		h ^= uint64(value[i])
		h *= fnvPrime64
	}
	return h
}

// HashField computes a hash of value and returns it
// as a 16-character lowercase hex string (no "0x" prefix).
func HashField(value string) string {
	h := fnv.New64a()
	h.Write([]byte(value))
	return fmt.Sprintf("%016x", h.Sum64())
}
