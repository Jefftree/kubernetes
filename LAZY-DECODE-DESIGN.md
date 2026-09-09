# Lazy decoding in the apiserver watch cache

Working design and results for branch `apiserver-lazy-decode`. Prototype scope, not a KEP.

## The problem

The watch cache stores one fully decoded object tree per cached instance. For a realistic
pod that is very expensive to *retain*, and GC mark cost scales with the number of heap
objects and pointers in the live heap, not with bytes.

Measured here (`TestLazyDecodeRetentionProbe`, 10,000 pods built from
`staging/src/k8s.io/apiserver/pkg/storage/testing/testdata/exemplar_pod.yaml` decoded as a
full `v1.Pod`, mean protobuf wire size 10,118 B), on the isolated data structures:

| shape held | live heap | heap objects | per pod |
| --- | --- | --- | --- |
| decoded `*v1.Pod` | 249.0 MB | 2,790,015 | 26,105 B / 279 objects |
| storage-encoded protobuf bytes | 97.9 MB | 10,001 | 10,265 B / 1 object |

## What the cache holds, and who reads it

`store.Element` (`storage/cacher/store/store.go`) is the unit of retention. Readers of
`Element.Object`:

| reader | site | what it needs |
| --- | --- | --- |
| LIST | `cacher.go` `GetList`, `reflect.ValueOf(obj).Elem()` into a typed slice | a concrete typed struct |
| GET | `cacher.go` `Get`, same | a concrete typed struct |
| LIST filtering | `Predicate.MatchesObjectAttributes(elem.Labels, elem.Fields)` | **nothing** |
| WATCH filtering | `filterWithAttrsAndPrefixFunction` | **nothing**, unless the alpha `ShardedListAndWatch` gate is on |
| WATCH initial events | `watch_cache_interval.go` -> `cache_watcher.go` `getMutableObject` | a deep copy per watcher, then encoded |
| WATCH live events | `cacher.go` `setCachingObjects` | encoded once, spliced to N watchers |
| indexes, `getAttrsFunc`, `keyFunc` | insert time only | typed, once |

The crux question, *can we filter without decoding*, answers itself: labels and fields are
already extracted and stored next to the object, so selector matching never touches the
typed object. Nothing had to be moved.

## Existing machinery, extended not duplicated

`runtime.CacheableObject` (`apimachinery/pkg/runtime/interfaces.go`) and its only
implementation `cachingObject` (`storage/cacher/caching_object.go`, from
https://github.com/kubernetes/kubernetes/pull/81914, merged 2019-10-01) already do half of
this: an object carrying a map from `runtime.Identifier` (encoder identity) to the bytes
that encoder would produce, spliced to the writer instead of re-encoded. Every serializer
honours it, and `watchEmbeddedEncoder.Encode` (`endpoints/handlers/response.go`) checks for
it *before* transforming, so an object that arrives holding the right bytes is written
verbatim.

`LazyObject` is the same contract with the map pre-seeded from the cache's own bytes.

### Two things that do not work, both measured

1. **The etcd value cannot be spliced.** `APIObjectVersioner.PrepareObjectForStorage`
   clears `resourceVersion` before writing, and `etcd3/watcher.go` `decodeObj` stamps it
   back from the etcd revision after reading. The bytes in etcd therefore carry an empty
   resourceVersion and are not what any client may be served. The cache has to re-encode
   after the storage layer decodes. That encode is the change's main CPU cost.

2. **Storage and response encoders agree on bytes but not on identity.**
   `TestLazyIdentityProtobufGet` shows the storage encoder and the encoder serving a
   protobuf v1 GET produce byte-identical output (10,131 bytes) but report different
   `runtime.Identifier`s, because the storage side targets a `multiGroupVersioner`:

   ```
   storage:  {"encodeGV":"{\"accepted\":\",\",\"coerce\":\"false\",\"name\":\"multi\",\"target\":\"v1\"}","encoder":"protobuf","name":"versioning"}
   response: {"encodeGV":"v1","encoder":"protobuf","name":"versioning"}
   ```

   So the zero-work splice never fires on identity alone. `LazyObject.CacheEncode` falls
   back to decode-and-encode once, then compares the result with the stored bytes and
   shares the slice when they match, so the memo costs no second copy. Aligning the two
   identifiers upstream would remove even the one-off encode.

## Design as built

`store.LazyObject` is a `runtime.Object` + `runtime.CacheableObject` holding the encoded
bytes, the storage decoder, and the identity of the encoder that produced them.

- `Materialize()` decodes **fresh on every call**. The caller owns the result outright.
- `CacheEncode` splices on an identity hit, otherwise decodes, encodes, and memoizes,
  sharing the stored slice when the bytes are equal.
- `DeepCopyObject()` shares the bytes, which is sound only because `LazyObject` exposes no
  setter.

Mutation safety is **structural**. The failure mode the brief warns about, a shared
decoded object silently mutated by one reader, cannot occur: no two callers are ever handed
the same decoded object, and the one shared thing is a `[]byte` that is only ever read.

`store.Element` also carries `IndexValues` and `TriggerValue`, computed once on ingest.
Without them the indexer and the watch dispatcher would decode on the write path, which is
precisely what this is trying to avoid.

Both the btree store **and** the history ring buffer hold the same `*LazyObject`. That
matters: they share one decoded object per live key today, so encoding only the store would
add a second representation rather than replace one. This was measured before it was
believed (see the history sweep below).

`LazyObject` never escapes the cacher. Every watch delivery funnels through
`getMutableObject`, which materializes. `TestLazyObjectNeverEscapesTheCacher` pins that and
fails if the materialization is removed.

Safety rails: a startup probe (`codecRoundTripsCleanly`) disables the feature for any codec
whose encode/decode pair is not the identity, and an encode failure at ingest falls back to
storing the decoded object and increments
`apiserver_watch_cache_lazy_encode_fallback_total` rather than failing the write.

## What is deliberately not done

- LIST cannot splice. `Cacher.GetList` copies items into a typed slice and the streaming
  collection encoder (`serializer/protobuf/collections.go`) marshals each item directly, so
  a LIST never reaches `CacheableObject`. LIST pays a decode per returned item.
- No change to `GuaranteedUpdate`.
- No attribute extraction straight from protobuf, so ingest still decodes once for
  `getAttrsFunc`.

## Results

Machine: 32-core Intel Xeon @ 2.80GHz, 125 GB RAM, Go 1.27.0, linux/amd64. Feature switched
with `LAZY_DECODE_WATCH_CACHE=true`, same binary both arms.

### GC cost, the mechanism claim (n=3 per arm)

`TestLazyGCCost`: hold a 10,000-pod watch cache live, then allocate 4 GB of identical
garbage in both arms and read off GC CPU. Same workload, so any difference is the cost of
marking the retained heap.

| | live heap | heap objects | GC cycles | GCCPUFraction | GC CPU of run |
| --- | --- | --- | --- | --- | --- |
| off | 273.0 MB | 2,993,990 | 15 | .0354 / .0358 / .0354 | 32 / 32 / 32 ms |
| on | 133.4 MB | 454,018 | 31 | .0223 / .0212 / .0208 | 19 / 18 / 17 ms |

**GC CPU falls 40% while the collector runs twice as many cycles.** The smaller live heap
lowers the GOGC trigger, so cycles get more frequent; each is far cheaper because there are
6.6x fewer objects to mark. Total stop-the-world pause rises from 1.6 ms to 3.2 ms over the
run, which is the cost of the extra cycles.

### Retention (n=1 each, deterministic; negative control flat)

`TestLazyWatchCacheRetention`, 10,000 pods, store only:

| | live heap | heap objects | per pod |
| --- | --- | --- | --- |
| off | 266.7 MB | 2,966,497 | 27,962 B / 296.6 objects |
| on | 127.1 MB | 426,491 | 13,330 B / 42.6 objects |

**Negative control**, the keys and label/field sets that this change does not touch:
270,067 objects / 14.6 MB off versus 270,080 objects / 14.6 MB on. Flat to 0.005%.

`TestLazyWatchCacheChurnRetention`, 2,000 pods under 5 updates each, swept over the history
ring buffer size, whole cache:

| history capacity | off MB | off objects | on MB | on objects | bytes | objects |
| --- | --- | --- | --- | --- | --- | --- |
| 100 | 50.0 | 587,720 | 24.0 | 62,975 | 2.08x | 9.33x |
| 1,000 | 68.8 | 696,966 | 35.1 | 84,453 | 1.96x | 8.25x |
| 4,000 | 131.7 | 1,061,460 | 72.2 | 156,460 | 1.82x | 6.78x |
| 12,000 | 262.9 | 1,808,416 | 149.0 | 324,977 | 1.76x | 5.56x |

An earlier build that left the history buffer decoded was *worse* than baseline at
capacity 4,000 and above (154.1 MB and 381.5 MB against 131.7 MB and 262.9 MB), because the
store and the history stopped sharing objects. That is why the history events are lazy too.

`TestLazyServingDoesNotGrowRetention`: serving 2,000 objects to the wire adds 0.63 MB and
8,010 heap objects, 330 B and 4 objects each, not a second 10 KB copy. The byte-equality
dedup fires.

### CPU (`benchstat`, n=6 per arm, `-benchmem`)

```
                               │      off       │                     on                     │
                               │     sec/op     │    sec/op      vs base                     │
LazyIngest-32                    2.305µ ±  6%     19.732µ ± 16%     +756.03% (p=0.002 n=6)
LazyListAll-32                   11.09µ ±  3%   27873.15µ ±  3%  +251167.89% (p=0.002 n=6)
LazyListSelector1Pct-32          46.96µ ±  8%     421.54µ ±  7%     +797.68% (p=0.002 n=6)
LazyListSelectorNoMatch-32       51.71µ ± 12%      50.92µ ± 10%            ~ (p=0.937 n=6)
LazyServeObjectsToWire-32     12032.08µ ±  6%      71.40µ ± 10%      -99.41% (p=0.002 n=6)
LazyServeObjectsToWireFirst-32   12.62m ±  4%      47.20m ±  9%     +273.90% (p=0.002 n=6)
LazyGet-32                       309.5n ±  4%    29225.5n ±  5%    +9344.34% (p=0.002 n=6)
LazyWatchInitialEvents-32        13.12m ±  7%      28.38m ±  1%     +116.27% (p=0.002 n=6)
```

Reading these honestly:

- **`LazyListSelectorNoMatch` is the CPU negative control and it did not move**
  (p=0.937, identical B/op and allocs/op). Neither arm decodes when the selector matches
  nothing, and the harness confirms it.
- `LazyListAll`, `LazyListSelector1Pct` and `LazyGet` stop at the cache read, so they
  compare "decode a 10 KB pod" against "copy a pointer". They are upper bounds on the added
  cost, not request-level numbers: a real request also encodes, which both arms pay.
- `LazyServeObjectsToWire` is the watch-list bootstrap shape, repeated per-object serving.
  Encoding once and splicing beats re-encoding per watcher by 168x, with allocations down
  66% and bytes down 99.5%.
- `LazyServeObjectsToWireFirstTouch` is the same path with nothing memoized, the first
  watcher after a cache rebuild: 3.7x slower.
- `LazyIngest` is the write path: +17.4 µs and +20.7 KiB per event, the encode. Of that,
  ~10 KB is an avoidable copy, because `runtime.NewCodec`'s wrapper embeds `Encoder` and so
  does not forward `EncodeWithAllocator`; the allocator fast path in `EncodeToLazyObject`
  never fires for a storage codec.

## What this does not settle

The local numbers say: 5.6x to 9.3x fewer live heap objects, 1.8x to 2.1x fewer live bytes,
40% less GC CPU for a fixed allocation workload, against +17 µs and +21 KiB per write and a
decode per LIST item. Whether that nets out positive on a real apiserver depends on the
ratio of writes to LISTs and on how much of the process's CPU is actually GC, and only an
end-to-end run answers it. Every accounting model on the `ladder` rig has overpredicted,
four for four, and there is no reason to think this one is different.
