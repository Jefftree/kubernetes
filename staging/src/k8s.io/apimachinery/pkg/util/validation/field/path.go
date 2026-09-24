/*
Copyright 2015 The Kubernetes Authors.

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

package field

import (
	"bytes"
	"fmt"
	"strconv"
	"sync"
	"unsafe"
)

type pathOptions struct {
	path *Path
}

// PathOption modifies a pathOptions
type PathOption func(o *pathOptions)

// WithPath generates a PathOption
func WithPath(p *Path) PathOption {
	return func(o *pathOptions) {
		o.path = p
	}
}

// ToPath produces *Path from a set of PathOption
func ToPath(opts ...PathOption) *Path {
	c := &pathOptions{}
	for _, opt := range opts {
		opt(c)
	}
	return c.path
}

// Path represents the path from some root to a particular field.
type Path struct {
	name   string // the name of this field or "" if this is an index
	index  string // if name == "", this is a subscript (index or map key) of the previous element
	parent *Path  // nil if this is the root element
}

type pathChildEntry struct {
	parent *Path
	name   string
	child  *Path
}

type pathChildShard struct {
	mu      sync.Mutex
	entries [16]pathChildEntry
	next    uint32
}

var pathChildShards [128]pathChildShard

func childSingle(parent *Path, name string) *Path {
	if len(name) <= 64 {
		h := uint64(uintptr(unsafe.Pointer(parent))) * 0x9e3779b97f4a7c15
		for i := 0; i < len(name); i++ {
			h = (h ^ uint64(name[i])) * 1099511628211
		}
		shard := &pathChildShards[(h^(h>>16))&127]
		shard.mu.Lock()
		for i := range shard.entries {
			e := &shard.entries[i]
			if e.child != nil && e.parent == parent && e.name == name {
				res := e.child
				shard.mu.Unlock()
				return res
			}
		}
		res := &Path{name: name, parent: parent}
		idx := shard.next & 15
		shard.entries[idx] = pathChildEntry{parent: parent, name: name, child: res}
		shard.next++
		shard.mu.Unlock()
		return res
	}
	return &Path{name: name, parent: parent}
}

type pathIndexEntry struct {
	parent *Path
	index  int
	child  *Path
}

type pathIndexShard struct {
	mu      sync.Mutex
	entries [8]pathIndexEntry
	next    uint32
}

var pathIndexShards [64]pathIndexShard

// NewPath creates a root Path object.
func NewPath(name string, moreNames ...string) *Path {
	r := childSingle(nil, name)
	for _, anotherName := range moreNames {
		r = childSingle(r, anotherName)
	}
	return r
}

// Root returns the root element of this Path.
func (p *Path) Root() *Path {
	for ; p.parent != nil; p = p.parent {
		// Do nothing.
	}
	return p
}

// Child creates a new Path that is a child of the method receiver.
func (p *Path) Child(name string, moreNames ...string) *Path {
	r := childSingle(p, name)
	for _, anotherName := range moreNames {
		r = childSingle(r, anotherName)
	}
	return r
}

// Index indicates that the previous Path is to be subscripted by an int.
// This sets the same underlying value as Key.
func (p *Path) Index(index int) *Path {
	if index >= 0 && index < 64 {
		h := (uint64(uintptr(unsafe.Pointer(p))) * 0x9e3779b97f4a7c15) ^ uint64(index)*0x517cc1b727220a95
		shard := &pathIndexShards[(h^(h>>16))&63]
		shard.mu.Lock()
		for i := range shard.entries {
			e := &shard.entries[i]
			if e.child != nil && e.parent == p && e.index == index {
				res := e.child
				shard.mu.Unlock()
				return res
			}
		}
		res := &Path{index: strconv.Itoa(index), parent: p}
		idx := shard.next & 7
		shard.entries[idx] = pathIndexEntry{parent: p, index: index, child: res}
		shard.next++
		shard.mu.Unlock()
		return res
	}
	return &Path{index: strconv.Itoa(index), parent: p}
}

// Key indicates that the previous Path is to be subscripted by a string.
// This sets the same underlying value as Index.
func (p *Path) Key(key string) *Path {
	return &Path{index: key, parent: p}
}

// String produces a string representation of the Path.
func (p *Path) String() string {
	if p == nil {
		return "<nil>"
	}
	// make a slice to iterate
	elems := []*Path{}
	for ; p != nil; p = p.parent {
		elems = append(elems, p)
	}

	// iterate, but it has to be backwards
	buf := bytes.NewBuffer(nil)
	for i := range elems {
		p := elems[len(elems)-1-i]
		if p.parent != nil && len(p.name) > 0 {
			// This is either the root or it is a subscript.
			buf.WriteString(".")
		}
		if len(p.name) > 0 {
			buf.WriteString(p.name)
		} else {
			fmt.Fprintf(buf, "[%s]", p.index)
		}
	}
	return buf.String()
}
