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
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
)

// Selector represents a shard selector that can match objects based on
// hash ranges of their metadata fields. It follows the labels.Selector
// pattern from the Kubernetes API.
type Selector interface {
	// Matches returns true if the given object matches the shard selector.
	Matches(obj runtime.Object) (bool, error)

	// Empty returns true if the selector matches everything (no filtering).
	Empty() bool

	// String returns the wire-format string representation that can be
	// round-tripped through Parse.
	String() string

	// Requirements returns the list of shard range requirements.
	Requirements() []ShardRangeRequirement

	// DeepCopySelector returns a deep copy of the selector.
	DeepCopySelector() Selector
}

// Everything returns a selector that matches all objects.
func Everything() Selector {
	return &everythingSelector{}
}

type everythingSelector struct{}

func (s *everythingSelector) Matches(_ runtime.Object) (bool, error) { return true, nil }
func (s *everythingSelector) Empty() bool                            { return true }
func (s *everythingSelector) String() string                         { return "" }
func (s *everythingSelector) Requirements() []ShardRangeRequirement  { return nil }
func (s *everythingSelector) DeepCopySelector() Selector             { return &everythingSelector{} }

// parsedRequirement is a ShardRangeRequirement with its bounds resolved to
// integers, so that matching an object is a pair of integer comparisons
// rather than hex string formatting and comparison.
type parsedRequirement struct {
	req        ShardRangeRequirement
	start      uint64
	end        uint64
	startIsMax bool
	endIsMax   bool
}

// shardSelector implements Selector with one or more shard range requirements.
type shardSelector struct {
	requirements []parsedRequirement
	// key is shared by every requirement, so Matches resolves the object
	// field and hashes it once.
	key string
	// err records a malformed selector. NewSelector has no error return and
	// callers can build requirements without going through Parse, so the
	// problem is reported from Matches.
	err error
}

func (s *shardSelector) Matches(obj runtime.Object) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	if len(s.requirements) == 0 {
		return true, nil
	}

	value, err := ResolveFieldValue(obj, s.key)
	if err != nil {
		return false, err
	}
	hash := HashFieldValue(value)

	for i := range s.requirements {
		r := &s.requirements[i]
		// A range starting at 2^64 is empty: no hash can reach it.
		if r.startIsMax || hash < r.start {
			continue
		}
		if r.endIsMax || hash < r.end {
			return true, nil
		}
	}
	return false, nil
}

// parseBound converts a 0x-prefixed hex bound to an integer. The second
// result reports a bound that sits above the entire hash space and therefore
// has no uint64 value. 2^64 is the canonical such bound and the only one the
// wire parser accepts, but NewSelector is public and any over-long hex was
// treated the same way by the string comparison this replaces.
func parseBound(bound string) (uint64, bool, error) {
	digits, ok := strings.CutPrefix(bound, "0x")
	if !ok {
		return 0, false, fmt.Errorf("shard range bound %q must be 0x-prefixed", bound)
	}
	value, err := strconv.ParseUint(digits, 16, 64)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return 0, true, nil
		}
		return 0, false, fmt.Errorf("shard range bound %q is not a valid hex value", bound)
	}
	return value, false, nil
}

// HexLess compares two 0x-prefixed lowercase hex strings numerically.
// Both values must be normalized to 16 hex digits (e.g. "0x0000000000000000"),
// except for the special upper bound "0x10000000000000000" (2^64) which has 17.
func HexLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

func (s *shardSelector) Empty() bool {
	return len(s.requirements) == 0
}

func (s *shardSelector) String() string {
	parts := make([]string, 0, len(s.requirements))
	for i := range s.requirements {
		req := s.requirements[i].req
		parts = append(parts, fmt.Sprintf("shardRange(%s, '%s', '%s')", req.Key, req.Start, req.End))
	}
	return strings.Join(parts, " || ")
}

func (s *shardSelector) Requirements() []ShardRangeRequirement {
	result := make([]ShardRangeRequirement, len(s.requirements))
	for i := range s.requirements {
		result[i] = s.requirements[i].req
	}
	return result
}

func (s *shardSelector) DeepCopySelector() Selector {
	reqs := make([]parsedRequirement, len(s.requirements))
	copy(reqs, s.requirements)
	return &shardSelector{requirements: reqs, key: s.key, err: s.err}
}

// NewSelector creates a Selector from the given requirements.
// All requirements must use the same Key, and their bounds must be
// 0x-prefixed hex; violations are reported from Matches.
// If no requirements are provided, returns Everything().
func NewSelector(reqs ...ShardRangeRequirement) Selector {
	if len(reqs) == 0 {
		return Everything()
	}
	s := &shardSelector{
		requirements: make([]parsedRequirement, 0, len(reqs)),
		key:          reqs[0].Key,
	}
	for _, req := range reqs {
		parsed := parsedRequirement{req: req}
		if req.Key != s.key && s.err == nil {
			s.err = fmt.Errorf("inconsistent shard keys: %q vs %q", s.key, req.Key)
		}
		start, startIsMax, err := parseBound(req.Start)
		if err != nil && s.err == nil {
			s.err = err
		}
		end, endIsMax, err := parseBound(req.End)
		if err != nil && s.err == nil {
			s.err = err
		}
		parsed.start, parsed.startIsMax = start, startIsMax
		parsed.end, parsed.endIsMax = end, endIsMax
		// Kept even when malformed so String() and Requirements() can still
		// round-trip what the client asked for.
		s.requirements = append(s.requirements, parsed)
	}
	return s
}

// NewShardRangeSelector returns a Selector for shardIndex in [0, totalShards)
// evenly partitioning the 64-bit FNV-1a hash space [0x0000000000000000, 0x10000000000000000).
func NewShardRangeSelector(key string, shardIndex, totalShards int) (Selector, error) {
	if key == "" || totalShards <= 0 || shardIndex < 0 || shardIndex >= totalShards {
		return nil, fmt.Errorf("invalid shard range parameters: key=%q shardIndex=%d totalShards=%d", key, shardIndex, totalShards)
	}
	maxHash := new(big.Int).Lsh(big.NewInt(1), 64)
	total := big.NewInt(int64(totalShards))
	start := new(big.Int).Div(new(big.Int).Mul(big.NewInt(int64(shardIndex)), maxHash), total)
	endHex := "0x10000000000000000"
	if shardIndex < totalShards-1 {
		end := new(big.Int).Div(new(big.Int).Mul(big.NewInt(int64(shardIndex+1)), maxHash), total)
		endHex = fmt.Sprintf("0x%016x", end.Uint64())
	}
	return NewSelector(ShardRangeRequirement{
		Key:   key,
		Start: fmt.Sprintf("0x%016x", start.Uint64()),
		End:   endHex,
	}), nil
}
