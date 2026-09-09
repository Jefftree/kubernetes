# Lazy decoding in the apiserver watch cache

Working design for branch `apiserver-lazy-decode`. Prototype scope, not a KEP.

## The measured problem

The watch cache stores one fully decoded object tree per cached resource instance.
For a realistic pod that is very expensive to *retain*, and GC mark cost scales with
the number of heap objects and pointers in the live heap, not with bytes.

Measured on this branch (`TestLazyDecodeRetentionProbe`, 10,000 pods built from
`staging/src/k8s.io/apiserver/pkg/storage/testing/testdata/exemplar_pod.yaml`
decoded as a full `v1.Pod`, mean protobuf wire size 10,118 B):

| shape held | live heap | heap objects | per pod |
| --- | --- | --- | --- |
| decoded `*v1.Pod` (what the cache holds today) | 249.0 MB | 2,790,015 | 26,105 B / 279 objects |
| storage-encoded protobuf bytes | 97.9 MB | 10,001 | 10,265 B / 1 object |

2.54x fewer bytes, **279x fewer heap objects**.

## What the cache actually holds, and who reads it

`store.Element` (`storage/cacher/store/store.go:117`) is the unit of retention:

```go
type Element struct {
    Key    string
    Object runtime.Object   // <- the 279 objects
    Labels labels.Set       // precomputed at insert
    Fields fields.Set       // precomputed at insert
}
```

Readers of `Element.Object`:

| reader | site | what it needs |
| --- | --- | --- |
| LIST | `cacher.go:810`, `cacher.go:849` — `listVal.Index(i).Set(reflect.ValueOf(elem.Object).Elem())` | a concrete typed struct to shallow-copy into `[]T` |
| GET | `cacher.go:723` — same reflect set | a concrete typed struct |
| LIST filtering | `cacher.go:832` — `Predicate.MatchesObjectAttributes(elem.Labels, elem.Fields)` | **nothing** — precomputed attrs only |
| WATCH filtering | `cacher.go:1263` — `p.MatchesObjectAttributes(label, field)` | **nothing**, unless the alpha `ShardedListAndWatch` gate is on (`cacher.go:1253`) |
| WATCH initial events | `watch_cache_interval.go:126` -> `cache_watcher.go:397` `getMutableObject` | a **deep copy per watcher**, then encoded to the wire |
| WATCH live events | `watch_cache.go:227` builds `Element`, `cacher.go:969` wraps in `cachingObject` | encoded to the wire once, spliced to N watchers |
| indexes (node name, namespace) | `store.ElementIndexFunc` at insert time | typed, but only at insert |
| `getAttrsFunc` / `keyFunc` | `watch_cache.go:223,228` at insert time | typed, but only at insert |

The crux question — *can we filter without decoding?* — answers itself: labels and
fields are already extracted and stored next to the object. Selector matching never
touches the typed object on the default feature set. Nothing has to be moved.

## Existing machinery we extend rather than duplicate

`runtime.CacheableObject` (`apimachinery/pkg/runtime/interfaces.go:344`) and its only
implementation `cachingObject` (`storage/cacher/caching_object.go`) already do half of
this: an object that carries a map from `runtime.Identifier` (encoder identity: target
group-version + media type + serializer options) to the bytes that encoder would
produce, and hands those bytes to the writer via `Splice` instead of re-encoding. Every
serializer honours it (`protobuf.go:194`, `json.go`, `versioning`), so an object that
arrives at the response writer already holding the right bytes is written verbatim.

`cachingObject` fills that map *lazily on first encode*. The insight here is that for
the storage encoder identity we do not have to encode at all — etcd already handed us
exactly those bytes. Storage protobuf framing (`k8s\0` magic + `runtime.Unknown{TypeMeta,
Raw}`) is byte-identical to what the protobuf serializer emits for a single object, so
the etcd value is a valid pre-seeded serialization for any request that negotiates the
storage version and media type.

Where it does *not* work: protobuf LIST bodies inline items as embedded messages inside
`PodList`, with no per-item envelope (`serializer/protobuf/collections.go:147`), and the
streaming collection encoder calls `item.MarshalTo` on the concrete type. So LIST cannot
splice per-item bytes without new machinery. LIST pays a decode.

## Design

Store the storage bytes; materialize typed objects on demand and never retain them.

### `lazyObject`

A `runtime.Object` + `runtime.CacheableObject` holding:

- `raw []byte` — the post-transformer etcd value, immutable, shared freely
- `decoder runtime.Decoder` + `storageID runtime.Identifier`
- memoized cheap metadata captured at insert (`resourceVersion`, `name`, `namespace`, `uid`)

Behaviour:

- `CacheEncode(id, encode, w)`: `id == storageID` -> splice `raw` verbatim, zero work.
  Otherwise fall back to `encode(GetObject(), w)`, exactly as `cachingObject` does on a
  cache miss.
- `GetObject()` -> a **fresh decode every call**. The caller owns it outright.
- `DeepCopyObject()` -> also a fresh decode, which satisfies the deep-copy contract.
- `metav1.Object` setters (only `SetResourceVersion` is used, on DELETE events) ->
  copy-on-write: decode, mutate the copy, drop laziness.

Mutation safety is *structural* here, which is the point. The failure mode the brief
warns about — a shared buffer or shared decoded object that someone silently mutates —
cannot happen for the decoded form, because no two callers ever get the same decoded
object. The shared thing is a `[]byte` that is only ever read. The one dangerous case is
a caller that mutates a decoded object and expects the cache to see it; no reader does
that today, and the tests below assert it.

### Where the bytes come from

- **Watch path (hot).** `etcd3/watcher.go:834` already has the post-transformer `data`
  in hand immediately before `decodeObj`. Carry it alongside the decoded object using
  the same wrapper pattern the tree already uses for `storage.WatchEventWithRecordTime`
  (`etcd3/watcher.go:741`), and unwrap it in `watchCache.processEvent`. No extra encode.
- **Initial LIST / `Replace` (startup only).** The reflector's list goes through a typed
  slice, so the bytes are not recoverable without deeper surgery. Re-encode there. It
  happens once per cache initialisation; the cost is measured, not hidden.

The decoded object is still produced on the write path, because `getAttrsFunc` and
`keyFunc` need it. It simply becomes immediately-dead young garbage instead of being
promoted and retained. Allocation rate is roughly unchanged; **retention collapses**.
Extracting `spec.nodeName` / `status.phase` straight from protobuf to skip that decode
too is a later step, deliberately out of scope.

### Consequences per path

| path | today | with lazy decode |
| --- | --- | --- |
| retention | 279 objects/pod | 1 object/pod |
| write (`processEvent`) | decode, retain | decode, discard. Extra: none |
| LIST | shallow struct copy per item | **+1 decode per item** (regression) |
| GET | shallow struct copy | +1 decode (`DecodeInto` straight into the caller's pointer) |
| WATCH live event | encode once per event, splice to N watchers | **splice, no encode at all** |
| WATCH initial events | deep copy per watcher, then encode | **splice, no copy and no encode** |
| selector matching | precomputed attrs | unchanged |

LIST is the one regression and it is the honest cost of the trade. The workload this
targets (10k pods, sustained patches, 10 watchers) does almost no LIST.

## Scope of this first cut

1. `lazyObject` + unit tests including mutation-safety tests that fail if aliasing is wrong.
2. `store.Element` materialisation via an accessor, so every reader is caught by the compiler.
3. Bytes plumbed from the etcd3 watcher; re-encode on `Replace`.
4. Feature gate, default off.

Deliberately **not** in this cut: per-item LIST splicing, attribute extraction without
decode, and any change to `GuaranteedUpdate`.

## What would falsify this

- If LIST decode cost dominates in the target workload, the trade is bad.
- If heap-object reduction does not translate into lower `gcDrain` time on a real
  apiserver, the accounting model overpredicted — as every accounting model on the
  `ladder` rig has, four for four.
