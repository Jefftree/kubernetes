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

package cache

import (
	"context"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/sharding"
	"k8s.io/apimachinery/pkg/watch"
)

type shardedListWatch struct {
	lw            ListerWatcherWithContext
	selector      sharding.Selector
	serverSharded atomic.Bool
}

// NewShardedListWatch wraps a ListerWatcher to attach selector to outgoing
// LIST and WATCH requests and transparently fall back to client-side filtering
// via selector.Matches when the server omits ListMeta.ShardInfo (e.g. when the
// ShardedListAndWatch feature gate is disabled on kube-apiserver).
func NewShardedListWatch(lw ListerWatcher, selector sharding.Selector) ListerWatcherWithContext {
	if selector == nil || selector.Empty() {
		return ToListerWatcherWithContext(lw)
	}
	return &shardedListWatch{
		lw:       ToListerWatcherWithContext(lw),
		selector: selector,
	}
}

func (s *shardedListWatch) List(options metav1.ListOptions) (runtime.Object, error) {
	return s.ListWithContext(context.Background(), options)
}

func (s *shardedListWatch) ListWithContext(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
	options.ShardSelector = s.selector.String()
	list, err := s.lw.ListWithContext(ctx, options)
	if err != nil {
		return nil, err
	}
	metaObj, err := meta.ListAccessor(list)
	if err != nil {
		return nil, err
	}

	serverSharded := false
	if shardedList, ok := metaObj.(metav1.ShardedListInterface); ok && shardedList.GetShardInfo() != nil {
		serverSharded = true
	}
	s.serverSharded.Store(serverSharded)
	if serverSharded {
		return list, nil
	}

	items, err := meta.ExtractList(list)
	if err != nil {
		return nil, err
	}
	filtered := items[:0]
	for _, item := range items {
		matched, err := s.selector.Matches(item)
		if err != nil {
			return nil, err
		}
		if matched {
			filtered = append(filtered, item)
		}
	}
	if err := meta.SetList(list, filtered); err != nil {
		return nil, err
	}
	return list, nil
}

func (s *shardedListWatch) Watch(options metav1.ListOptions) (watch.Interface, error) {
	return s.WatchWithContext(context.Background(), options)
}

func (s *shardedListWatch) WatchWithContext(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
	options.ShardSelector = s.selector.String()
	w, err := s.lw.WatchWithContext(ctx, options)
	if err != nil {
		return nil, err
	}
	if s.serverSharded.Load() {
		return w, nil
	}
	return watch.Filter(w, func(in watch.Event) (watch.Event, bool) {
		if in.Type == watch.Bookmark || in.Type == watch.Error {
			return in, true
		}
		matched, err := s.selector.Matches(in.Object)
		return in, err == nil && matched
	}), nil
}

// IsWatchListSemanticsUnSupported delegates to the underlying ListerWatcher.
func (s *shardedListWatch) IsWatchListSemanticsUnSupported() bool {
	type unsupported interface {
		IsWatchListSemanticsUnSupported() bool
	}
	if u, ok := s.lw.(unsupported); ok {
		return u.IsWatchListSemanticsUnSupported()
	}
	return false
}
