/*
Copyright 2018 The Kubernetes Authors.

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

package value

// StringValue is a Value holding a string, like NewValueInterface of that
// string. A *StringValue is a Value too, which lets a StringValue be allocated
// together with other data, rather than on its own.
type StringValue string

var _ Value = StringValue("")
var _ Value = (*StringValue)(nil)

func (s StringValue) IsMap() bool    { return false }
func (s StringValue) IsList() bool   { return false }
func (s StringValue) IsBool() bool   { return false }
func (s StringValue) IsInt() bool    { return false }
func (s StringValue) IsFloat() bool  { return false }
func (s StringValue) IsString() bool { return true }
func (s StringValue) IsNull() bool   { return false }

func (s StringValue) AsString() string { return string(s) }

func (s StringValue) Unstructured() interface{} { return string(s) }

// The rest fail as they do for NewValueInterface of a string.

func (s StringValue) AsMap() Map                   { return s.unstructured().AsMap() }
func (s StringValue) AsMapUsing(a Allocator) Map   { return s.unstructured().AsMapUsing(a) }
func (s StringValue) AsList() List                 { return s.unstructured().AsList() }
func (s StringValue) AsListUsing(a Allocator) List { return s.unstructured().AsListUsing(a) }
func (s StringValue) AsBool() bool                 { return s.unstructured().AsBool() }
func (s StringValue) AsInt() int64                 { return s.unstructured().AsInt() }
func (s StringValue) AsFloat() float64             { return s.unstructured().AsFloat() }

func (s StringValue) unstructured() valueUnstructured {
	return valueUnstructured{Value: string(s)}
}
