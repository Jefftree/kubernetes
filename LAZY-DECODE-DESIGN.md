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

`LazyObject` is the same contract with the stored bytes standing in for the map, and with
memoization deliberately removed because its lifetime is the cache entry's, not a dispatch's.

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

   So the zero-work splice never fires on identity alone, and on the watch path it is
   further out of reach (see the withdrawn claim below). `CacheEncode` decodes and encodes
   without retaining anything, sharing the stored slice when the bytes match. Aligning the
   two identifiers upstream would make the splice reachable for GET.

## Design as built

`store.LazyObject` is a `runtime.Object` + `runtime.CacheableObject` holding the encoded
bytes, the storage decoder, and the identity of the encoder that produced them.

- `Materialize()` decodes **fresh on every call**. The caller owns the result outright.
- `CacheEncode` splices on an identity hit and otherwise decodes and encodes, retaining
  nothing. It must not memoize: a LazyObject lives as long as its cache entry.
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

### GC cost, the mechanism claim

`TestLazyGCCost`: hold a 10,000-pod watch cache live, then allocate 4 GiB of identical
garbage in both arms and difference the `runtime/metrics` GC CPU counters over that window.
The workload is byte-identical, so the difference is the cost of marking the retained heap.
Total process CPU over the window agrees to 2.1% between arms, which confirms the workload
really is the same.

Live heap while running: **273.0 MB / 2,993,995 objects** off versus **133.4 MB / 454,000
objects** on, a 6.59x reduction in objects. GC cycles rise from 15 to 31, because the
smaller live heap lowers the GOGC trigger.

GC CPU seconds over the window, by class, n=3 per arm:

| class | off | on | change |
| --- | --- | --- | --- |
| total | 3.9715 | 1.7457 | **-56.0%** |
| mark, dedicated | 1.1447 | 0.6873 | -40.0% |
| mark, assist | 0.0052 | 0.0030 | -42.0% |
| mark, idle | 2.7859 | 0.9976 | -64.2% |
| pause | 0.0357 | 0.0576 | **+61.6%** |
| **non-idle (dedicated + assist + pause)** | **1.1856** | **0.7480** | **-36.9%** |

**The number to quote is -37%, not -56%.** Seventy percent of the "total" is the *idle*
class: GC work scheduled onto otherwise-idle Ps. This box has 32 cores and the benchmark
allocates on one goroutine, so idle-mark is nearly free in wall-clock terms and inflates
the total. A saturated apiserver has no idle Ps and that work reappears as dedicated and
assist. Non-idle GC CPU, the part that is actually taken away from request serving, falls
from 4.19% to 2.70% of process CPU.

An earlier version of this measurement used `MemStats.GCCPUFraction` and reported -40%.
That metric is a cumulative average over the whole process lifetime, including building the
cache, so it could not answer "what did this workload cost"; the counters above can.

Stop-the-world pause is the one regression: 36 ms to 58 ms over the window, the price of
running twice as many cycles. Small in absolute terms here, worth watching at scale.

The accounting model, "GC CPU is proportional to live heap objects per live byte", predicts
10,967 objects/MB against 3,403 objects/MB, so a 3.22x gain and a 69% reduction. Measured
total was 2.22x and 56%; measured non-idle was 1.59x and 37%. **The model overpredicted by
1.45x on the most generous reading and by 2.0x on the defensible one** -- consistent with
every accounting model on the `ladder` rig having overpredicted, now five for five.

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
- `LazyIngest` is the write path: +17.4 µs and +20.7 KiB per event, the encode. Of that,
  ~10 KB is an avoidable copy, because `runtime.NewCodec`'s wrapper embeds `Encoder` and so
  does not forward `EncodeWithAllocator`; the allocator fast path in `EncodeToLazyObject`
  never fires for a storage codec.

## A claim this design does not get to make

An earlier version of this document reported a 168x speedup on repeated per-object serving,
from `CacheEncode` splicing stored bytes instead of re-encoding. **That was wrong and has
been withdrawn.** A review of the delivery path showed the benchmark encoded a `LazyObject`
directly, which production never does:

- Every watch delivery goes through `getMutableObject`, which materializes, so a decoded
  object reaches the encoder. `TestLazyObjectNeverEscapesTheCacher` asserts exactly this
  and fails when the materialization is removed.
- The watch encoder would not have hit the fast path anyway. `newWatchEncoder`
  (`endpoints/handlers/response.go`) calls `CacheEncode` with the identifier of the *whole
  framed watch event*, `{"name":"watch","embeddedEncoder":...,"encoder":...,"eventType":...}`,
  which can never equal a plain codec identifier.

So `CacheEncode` is currently unreachable through the cacher: latent capability, not an
active optimization, and the honest number for the watch-list path is
`LazyWatchInitialEvents` at **+116%**.

The same review found a real hazard in the version that memoized. Because the watch
identifier keys on the framed event, a memoizing `LazyObject` would have retained a full
framed copy per event type, for the lifetime of the cache entry -- unlike `cachingObject`,
which is built per dispatch and deliberately discarded for precisely this reason.
`CacheEncode` now never memoizes: it shares the stored slice when the bytes match and
otherwise encodes without retaining. `TestLazyObjectCacheEncodeRetainsNothing` pins it at
+4 heap objects across 6,000 encodes.

## Prior art

Nobody upstream has built a serialized watch cache. The idea is on the record with a named
blocker, and the nearest working prototype is on the client-go side.

- https://github.com/kubernetes/kubernetes/issues/124680 (closed, 2024-05). wojtek-t
  proposes this design almost verbatim -- "store a serialized object, potentially with
  deserialized ObjectMeta for efficient filtering" -- and names the blocker: **CRD field
  selectors**, "a large effort on its own". This prototype sidesteps it rather than solving
  it, by keeping the precomputed `Labels`/`Fields` on the element.
- https://github.com/kubernetes/kubernetes/pull/141683 (open draft, 2026-08-29). justinsb's
  client-go informer `bytecache`: objects as `[]byte`, decode on Get. Reports informer heap
  89.4 MB to 2.2 MB and **889k heap objects to ~10k**, independently reproducing the
  object-count collapse measured here. A KEP is promised.
- https://github.com/kubernetes/kubernetes/issues/137109 (open, triage/accepted). serathius's
  interning work, -43% apiserver memory at 50k pods. Carries the bar this design must clear:
  liggitt's objection that sharing mutable `[]byte` between objects is "really dangerous".
  Here the `[]byte` is written once, never handed out, and every reader gets a private
  decode, which is the answer to that objection.
- https://github.com/kubernetes/kubernetes/issues/90179 (frozen, 2020-04). smarterclayton
  proposed "decode from storage exactly one time per object revision"; closed at
  awaiting-more-evidence after wojtek-t judged memory not to be a real problem, a premise
  the 2026 data has overturned.
- https://github.com/kubernetes/kubernetes/pull/81914 (merged 2019-10-01). The origin of
  `cachingObject`. It caches encodings of an already-decoded object so N watchers serialize
  once: the inverse of lazy decode, not a version of it.
- KEP-4988 snapshottable cache (shipped 1.34) holds pointers to decoded objects, so it is
  what this must interoperate with; the prototype does, since snapshots hold `Element`s.
- KEP-5116 streaming list encoding (GA 1.34) is complementary but is also why LIST cannot
  splice: it marshals each item directly rather than through `CacheableObject`.

There is no KEP for any of this yet.

## What this does not settle

The local numbers say: 5.6x to 9.3x fewer live heap objects, 1.8x to 2.1x fewer live bytes,
and 37% less non-idle GC CPU for a fixed allocation workload, against +17 µs and +21 KiB
per write, a decode per LIST item, and 60% more stop-the-world pause.

Whether that nets out positive on a real apiserver depends on three things this box cannot
answer:

1. **How much of the apiserver's CPU is actually GC.** The profile that motivated this said
   `gcDrain` was 38-40% of apiserver CPU after the fieldsv1string fix. If that holds, a 37%
   cut to non-idle GC CPU is worth roughly 14 points of total CPU. That multiplication is
   an accounting model and should be treated as a hypothesis.
2. **The write-to-read ratio.** Every write pays an encode; only some reads get anything
   back. The target write-throughput workload is the favourable end.
3. **Whether idle-mark headroom exists.** On a box with spare cores the win is smaller than
   -56% suggests and larger than -37% suggests; -37% is the pessimistic bound.

An end-to-end run on `ladder-c4-144` with `--feature-gates=LazyDecodeWatchCache=true`
against the ClusterLoader2 write-throughput test would settle all three, reading delivered
throughput, apiserver CPU, `gcDrain` share in the profile, and RSS. That box is serialized
and was in use, so it was not touched.
