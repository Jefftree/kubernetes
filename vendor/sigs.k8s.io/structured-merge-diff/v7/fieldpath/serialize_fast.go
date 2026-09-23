/*
Copyright 2019 The Kubernetes Authors.

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

package fieldpath

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"sigs.k8s.io/structured-merge-diff/v7/value"
)

// setReader lets readIterV1 take field names straight out of the input instead
// of allocating a copy of each one. A nil *setReader reads the ordinary way.
type setReader struct {
	// src is the whole input the decoder reads from. Strings handed out point
	// into it, so it must never be modified.
	src string
	// names holds FieldName targets, a chunk at a time, so that pointing a path
	// element at its name does not take an allocation per name. sets, members
	// and nodes do the same for the decoded sets and their contents.
	names   []string
	sets    []Set
	members []PathElement
	nodes   []setNode
	// free holds sets to build into. A set is built in one of these, then
	// copied out at its final size.
	free []*Set
	// Chunk capacities, scaled to the input so that one chunk usually suffices.
	nameChunk, memberChunk, setChunk int
}

// beginSet returns an empty set to build into, and finishSet the set built.
func (sr *setReader) beginSet() *Set {
	if sr == nil {
		return &Set{}
	}
	if n := len(sr.free); n > 0 {
		s := sr.free[n-1]
		sr.free = sr.free[:n-1]
		return s
	}
	return &Set{}
}

func (sr *setReader) finishSet(building *Set) *Set {
	if sr == nil {
		return building
	}
	if len(sr.sets) == cap(sr.sets) {
		sr.sets = make([]Set, 0, sr.setChunk)
	}
	sr.sets = append(sr.sets, Set{})
	s := &sr.sets[len(sr.sets)-1]
	s.Members.members = copyInto(&sr.members, building.Members.members, sr.memberChunk)
	s.Children.members = copyInto(&sr.nodes, building.Children.members, sr.setChunk)
	building.Members.members = building.Members.members[:0]
	building.Children.members = building.Children.members[:0]
	sr.free = append(sr.free, building)
	return s
}

// copyInto copies src to the end of *chunk, starting a new chunk if it does not
// fit, and returns the copy. The copy's capacity is its length, so appending to
// it never writes over whatever follows it in the chunk.
func copyInto[T any](chunk *[]T, src []T, size int) []T {
	n := len(src)
	if n == 0 {
		return nil
	}
	if cap(*chunk)-len(*chunk) < n {
		*chunk = make([]T, 0, max(size, n))
	}
	start := len(*chunk)
	*chunk = append(*chunk, src...)
	return (*chunk)[start : start+n : start+n]
}

type setDecoder struct {
	buf bytes.Buffer
	dec jsontext.Decoder
	// free carries setReader.free over from one decoding to the next.
	free []*Set
}

// maxPooledSetCap bounds the capacity of sets kept in setDecoder.free.
const maxPooledSetCap = 256

// keepFree cleared sets from free in d, so that they neither refer to what was
// decoded nor hold on to much memory.
func (d *setDecoder) keepFree(free []*Set) {
	d.free = free[:0]
	for _, s := range free {
		m, c := s.Members.members, s.Children.members
		if cap(m) > maxPooledSetCap || cap(c) > maxPooledSetCap {
			continue
		}
		clear(m[:cap(m)])
		clear(c[:cap(c)])
		d.free = append(d.free, s)
	}
	clear(free[len(d.free):cap(free)])
}

var setDecoderPool = sync.Pool{New: func() any { return &setDecoder{} }}

// fromJSONFast decodes data, which it takes ownership of, into s. It reports
// false when it cannot, for any reason, in which case s is unchanged and the
// caller must decode data the ordinary way, which also yields the error.
func (s *Set) fromJSONFast(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	d := setDecoderPool.Get().(*setDecoder)
	sr := &setReader{free: d.free}
	defer func() {
		d.buf = bytes.Buffer{}
		d.dec.Reset(&d.buf)
		d.keepFree(sr.free)
		setDecoderPool.Put(d)
	}()
	d.buf = *bytes.NewBuffer(data)
	d.dec.Reset(&d.buf, allowInvalidUTF8, allowDuplicates)
	*sr = setReader{
		free: sr.free,
		src:  unsafe.String(unsafe.SliceData(data), len(data)),
		// Serialized managedFields of built-in types run to 20-30 bytes per
		// field name and member, and 60-90 per set.
		nameChunk:   max(4, len(data)/24),
		memberChunk: max(4, len(data)/20),
		setChunk:    max(4, len(data)/60),
	}
	found, _, err := readIterV1(&d.dec, sr)
	if err != nil {
		return false
	}
	if _, err := d.dec.ReadToken(); err != io.EOF {
		return false
	}
	if found == nil {
		*s = Set{}
	} else {
		*s = *found
	}
	return true
}

// rawKeyContent returns the encoded content of the object name that the decoder
// just read, starting at input offset start, without its quotes.
func (sr *setReader) rawKeyContent(dec *jsontext.Decoder, start int64) (string, bool) {
	if sr == nil {
		return "", false
	}
	end := dec.InputOffset()
	if start < 0 || end > int64(len(sr.src)) || start >= end {
		return "", false
	}
	// Only whitespace and a separator can precede the opening quote.
	raw := sr.src[start:end]
	i := strings.IndexByte(raw, '"')
	if i < 0 || len(raw)-i < 2 || raw[len(raw)-1] != '"' {
		return "", false
	}
	return raw[i+1 : len(raw)-1], true
}

// keyString returns the string value of tok, an object name that the decoder
// read starting at input offset start.
func (sr *setReader) keyString(dec *jsontext.Decoder, tok jsontext.Token, start int64) string {
	if content, ok := sr.rawKeyContent(dec, start); ok && isVerbatim(content) {
		return content
	}
	return tok.String()
}

var errNotEscapedKey = errors.New("not a key path element the fast way")

// escapedKeyPathElement parses the object name that the decoder just read as a
// key path element straight from its encoded form, in which the quotes of the
// key's own JSON are escaped, saving unescaping it into a new string first. It
// returns an error, and the caller must take the ordinary way, for anything
// else, including any escape other than \".
func (sr *setReader) escapedKeyPathElement(dec *jsontext.Decoder, start int64) (PathElement, error) {
	content, ok := sr.rawKeyContent(dec, start)
	if !ok || len(content) < 2 || content[0] != peKey || content[1] != peSeparator {
		return PathElement{}, errNotEscapedKey
	}
	key, ok := parseKeyFieldsQuoted(content[2:], `\"`)
	if !ok {
		return PathElement{}, errNotEscapedKey
	}
	return PathElement{Key: key}, nil
}

// isVerbatim reports whether the JSON encoding of a string is its content
// unchanged, in which case the decoder would produce an identical string.
func isVerbatim(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c == '\\' || c == '"' || c < 0x20 || c >= 0x80 {
			return false
		}
	}
	return true
}

func (sr *setReader) fieldName(name string) *string {
	if sr == nil {
		p := new(string)
		*p = name
		return p
	}
	if len(sr.names) == cap(sr.names) {
		sr.names = make([]string, 0, sr.nameChunk)
	}
	sr.names = append(sr.names, name)
	return &sr.names[len(sr.names)-1]
}

// deserializePathElement is DeserializePathElement, taking field names and
// keys the cheaper way when sr is not nil.
func (sr *setReader) deserializePathElement(s string) (PathElement, error) {
	if sr == nil || len(s) < 2 || s[1] != peSeparator {
		return DeserializePathElement(s)
	}
	switch s[0] {
	case peField:
		return PathElement{FieldName: sr.fieldName(s[2:])}, nil
	case peKey:
		if key, ok := parseKeyFields(s[2:]); ok {
			return PathElement{Key: key}, nil
		}
	}
	return DeserializePathElement(s)
}

type fieldListStorage struct {
	list    value.FieldList
	array   [2]value.Field
	strings [2]value.StringValue
}

// parseKeyFields parses the JSON object of a key path element in the form the
// serializer writes it: no whitespace, strings that need no unescaping, and
// scalar values. It reports false for anything else, which DeserializePathElement
// then handles. Numbers become float64, as they do when decoded to an any.
func parseKeyFields(s string) (*value.FieldList, bool) {
	return parseKeyFieldsQuoted(s, `"`)
}

// parseKeyFieldsQuoted is parseKeyFields for JSON whose strings are delimited
// by quote, which is either a quote or an escaped quote.
func parseKeyFieldsQuoted(s, quote string) (*value.FieldList, bool) {
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return nil, false
	}
	storage := &fieldListStorage{}
	fields := value.FieldList(storage.array[:0])
	rest := s[1 : len(s)-1]
	for rest != "" {
		name, after, ok := cutVerbatimString(rest, quote)
		if !ok || after == "" || after[0] != ':' {
			return nil, false
		}
		rest = after[1:]
		var val value.Value
		switch {
		case rest == "":
			return nil, false
		case strings.HasPrefix(rest, quote):
			str, after, ok := cutVerbatimString(rest, quote)
			if !ok {
				return nil, false
			}
			rest = after
			if i := len(fields); i < len(storage.strings) {
				storage.strings[i] = value.StringValue(str)
				val = &storage.strings[i]
			} else {
				sv := value.StringValue(str)
				val = &sv
			}
		case strings.HasPrefix(rest, "true"):
			val, rest = value.NewValueInterface(true), rest[len("true"):]
		case strings.HasPrefix(rest, "false"):
			val, rest = value.NewValueInterface(false), rest[len("false"):]
		case strings.HasPrefix(rest, "null"):
			val, rest = value.NewValueInterface(nil), rest[len("null"):]
		default:
			n := jsonNumberLength(rest)
			if n == 0 {
				return nil, false
			}
			f, err := strconv.ParseFloat(rest[:n], 64)
			if err != nil {
				return nil, false
			}
			val, rest = value.NewValueInterface(f), rest[n:]
		}
		fields = append(fields, value.Field{Name: name, Value: val})
		if rest == "" {
			break
		}
		if rest[0] != ',' || len(rest) == 1 {
			return nil, false
		}
		rest = rest[1:]
	}
	fields.Sort()
	storage.list = fields
	return &storage.list, true
}

// cutVerbatimString cuts a JSON string delimited by quote, whose content needs
// no unescaping, off the front of s, returning its content and the remainder.
func cutVerbatimString(s, quote string) (content, rest string, ok bool) {
	if !strings.HasPrefix(s, quote) {
		return "", "", false
	}
	s = s[len(quote):]
	end := strings.Index(s, quote)
	if end < 0 {
		return "", "", false
	}
	content = s[:end]
	if !isVerbatim(content) {
		return "", "", false
	}
	return content, s[end+len(quote):], true
}

// jsonNumberLength returns the length of the JSON number at the front of s, or
// zero if there is none.
func jsonNumberLength(s string) int {
	i := 0
	if i < len(s) && s[i] == '-' {
		i++
	}
	switch {
	case i < len(s) && s[i] == '0':
		i++
	case i < len(s) && s[i] >= '1' && s[i] <= '9':
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	default:
		return 0
	}
	if i < len(s) && s[i] == '.' {
		i++
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == start {
			return 0
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == start {
			return 0
		}
	}
	return i
}

// toJSONFast encodes s as ToJSON does, appending straight to a buffer rather
// than going through an encoder. It reports false when it cannot, in which case
// the caller must encode s the ordinary way, which also yields the error.
func (s *Set) toJSONFast() ([]byte, bool) {
	serializer := pool.Get().(*pathElementSerializer)
	defer func() {
		serializer.reset()
		pool.Put(serializer)
	}()
	w := setWriter{serializer: serializer, buf: make([]byte, 0, 64+s.Size()*24)}
	if !w.emit(s, false) {
		return nil, false
	}
	return w.buf, true
}

type setWriter struct {
	serializer *pathElementSerializer
	buf        []byte
	// keys holds the spans of buf holding the quoted key and value path
	// elements of each object being written, to detect duplicate names, which
	// ToJSON rejects.
	keys [][2]int
}

func (w *setWriter) emit(s *Set, includeSelf bool) bool {
	w.buf = append(w.buf, '{')
	first := true
	keysStart := len(w.keys)
	defer func() { w.keys = w.keys[:keysStart] }()
	if includeSelf && !(len(s.Members.members) == 0 && len(s.Children.members) == 0) {
		w.buf = append(w.buf, `".":{}`...)
		first = false
	}
	mi, ci := 0, 0
	for mi < len(s.Members.members) || ci < len(s.Children.members) {
		var pe PathElement
		var child *Set
		includeChildSelf := false
		switch {
		case ci == len(s.Children.members):
			pe = s.Members.members[mi]
			mi++
		case mi == len(s.Members.members):
			pe, child = s.Children.members[ci].pathElement, s.Children.members[ci].set
			ci++
		default:
			mpe, cpe := s.Members.members[mi], s.Children.members[ci].pathElement
			switch c := mpe.Compare(cpe); {
			case c < 0:
				pe = mpe
				mi++
			case c > 0:
				pe, child = cpe, s.Children.members[ci].set
				ci++
			default:
				pe, child, includeChildSelf = cpe, s.Children.members[ci].set, true
				mi++
				ci++
			}
		}
		if !first {
			w.buf = append(w.buf, ',')
		}
		first = false
		if !w.writeKey(pe, keysStart) {
			return false
		}
		w.buf = append(w.buf, ':')
		if child == nil {
			w.buf = append(w.buf, "{}"...)
		} else if !w.emit(child, includeChildSelf) {
			return false
		}
	}
	w.buf = append(w.buf, '}')
	return true
}

func (w *setWriter) writeKey(pe PathElement, keysStart int) bool {
	w.serializer.reset()
	if err := w.serializer.serialize(pe); err != nil {
		return false
	}
	start := len(w.buf)
	var err error
	w.buf, err = jsontext.AppendQuote(w.buf, w.serializer.builder.Bytes())
	if err != nil {
		return false
	}
	if pe.Key != nil || pe.Value != nil {
		// Distinct keys or values can serialize alike. Field names and indexes
		// cannot, and cannot collide with these, which carry another prefix.
		quoted := w.buf[start:]
		for _, span := range w.keys[keysStart:] {
			if bytes.Equal(w.buf[span[0]:span[1]], quoted) {
				return false
			}
		}
		w.keys = append(w.keys, [2]int{start, len(w.buf)})
	}
	return true
}
