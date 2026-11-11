# gundb optimization report

This report covers 14 optimizations of gundb, and the `--obf` feature. Each optimization was built, tested, measured against the code before it, and committed on its own on the branch `optimizations`.

## Contents

- [Summary](#summary)
- [Original code against final code](#original-code-against-final-code)
- [How it was measured](#how-it-was-measured)
- [Step by step](#step-by-step)
- [The --obf feature](#the---obf-feature)
- [What did not improve](#what-did-not-improve)
- [Reproduce](#reproduce)

## Summary

The final code against the original, on the benchmark each optimization targets. Times are medians of 6 runs. All differences shown are significant (p ≤ 0.05).

| # | Optimization | Benchmark | Original | Final | Faster |
|---|---|---|--:|--:|--:|
| 1 | Verify only signatures that can win | A relay receives a signed node it already has | 168 µs | 10.7 µs | 15.8× |
| 2 | Sync outside the lock, once per frame | A frame of 100 puts, Pebble syncing every write | 416 ms | 6.54 ms | 63.7× |
| | | Writes to one node from many goroutines, synced | 4.51 ms | 157 µs | 28.7× |
| 3 | Tune Pebble | Look up a field that does not exist yet | 6.68 µs | 1.35 µs | 4.9× |
| | | Add a field to a node of a 100,000-node store | 16.5 µs | 6.78 µs | 2.4× |
| 4 | Read one field per path step | One field through a node of 10,000 fields, in memory | 122 µs | 294 ns | 416× |
| | | The same on Pebble | 1.32 ms | 2.19 µs | 604× |
| 5 | Incremental `Ref.On` | A change in a watched document of 100 nodes | 109 µs | 71 µs | 1.5× |
| 6 | `Options.RouteByInterest` | A miss on a relay with 1,000 clients | 274 µs | 1.47 µs | 186× |
| | | The relay's own write, 1,000 clients connected | 222 µs | 2.96 µs | 75× |
| 7 | Cheap reconnects | Reconnecting with 10,000 nodes of interest, to a peer seen before | 14.6 ms | 4.0 ms | 3.7× |
| | | The same to a new peer | 14.6 ms | 9.64 ms | 1.5× |
| 8 | Range-aware soul listing | 100 souls out of 100,000, in memory | 2.8 ms | 3.56 µs | 786× |
| | | The same on Pebble | 50.8 ms | 43.5 µs | 1,168× |
| | | Opening a DB with a quota on 100,000 nodes | 740 ms | 63 ms | 11.7× |
| 9 | One-pass decoding, relay raw bytes | Decode a frame of 10 gets | 10.1 µs | 3.6 µs | 2.8× |
| | | A relay receives a put and acks it | 5.63 µs | 4.05 µs | 1.4× |
| 10 | Peer counts | (no measurable change) | | | |
| 11 | Sharded dedup table | Track message IDs from 16 cores at once | 286 ns | 34.5 ns | 8.3× |
| 12 | WebSocket buffers, compression | Receive a frame of 10 puts | 12.4 µs | 9.98 µs | 1.2× |
| | | Bytes on the wire for it, with `Compress` | 2,359 B | 242 B | 9.7× less |
| 13 | Chunk large nodes | Largest frame answering a get for 100,000 fields | 4.36 MiB | 380 KiB | 11.7× less |
| 14 | Fewer allocations | Write one field | 1.88 µs | 1.25 µs | 1.5× |
| | | `Set` an item | 6.08 µs | 4.94 µs | 1.2× |

Rows 6 and 7 compare the original code with the final code using the new option or path: `GetMiss` against `GetMissRouted`, and `Resubscribe` against `ResubscribeBack`. With `RouteByInterest` off, a miss costs what it did before.

Across every benchmark, the geometric mean of the time per operation fell by **46%** in the core package, **89%** in `pebblestore` and **20%** in the WebSocket transport. Allocations fell by **35%**, **79%** and **63%**.

## Original code against final code

Every benchmark of the three packages, the original code against the final code. "~" means no significant difference; "new" marks a benchmark of a feature the original code did not have.

| Benchmark | Time | Change | Memory | Allocs |
|---|--:|--:|--:|--:|
| HandlePutSigned/new | 159 µs → 156 µs | **-1.81%** | 64.2 KiB → 52.6 KiB | 807 → 592 |
| HandlePutSigned/known | 168 µs → 10.7 µs | **-93.66%** (15.8×) | 70.9 KiB → 15.5 KiB | 845 → 98 |
| OnceFieldLargeNode/fields=10 | 589 ns → 280 ns | **-52.45%** (2.1×) | 1.88 KiB → 128 B | 18 → 4 |
| OnceFieldLargeNode/fields=10000 | 122 µs → 294 ns | **-99.76%** (416.4×) | 1.04 MiB → 128 B | 78 → 4 |
| OnPutNested/nodes=10 | 19.1 µs → 11.7 µs | **-38.87%** (1.6×) | 29 KiB → 14.2 KiB | 336 → 120 |
| OnPutNested/nodes=100 | 109 µs → 71 µs | **-35.19%** (1.5×) | 177 KiB → 86.9 KiB | 1,701 → 378 |
| OnPutNested/nodes=1000 | 913 µs → 694 µs | **-23.96%** (1.3×) | 1.38 MiB → 939 KiB | 11,272 → 3,156 |
| GetMiss | 274 µs → 269 µs | ~ | 124 KiB → 123 KiB | 1,063 → 1,052 |
| PutLocalManyPeers | 222 µs → 220 µs | ~ | 41.7 KiB → 37.6 KiB | 1,086 → 1,056 |
| Resubscribe/souls=1000 | 1.35 ms → 899 µs | **-33.52%** (1.5×) | 1.78 MiB → 1.62 MiB | 20,253 → 11,720 |
| Resubscribe/souls=10000 | 14.6 ms → 9.64 ms | **-33.79%** (1.5×) | 17.1 MiB → 16.2 MiB | 202,921 → 117,424 |
| SoulsRange | 2.8 ms → 3.56 µs | **-99.87%** (785.8×) | 8.51 MiB → 4.66 KiB | 32 → 12 |
| HandlePutLargeNode | 908 µs → 613 µs | **-32.55%** (1.5×) | 1.28 MiB → 923 KiB | 4,207 → 4,139 |
| DupTrackParallel | 286 ns → 34.5 ns | **-87.95%** (8.3×) | 2 B → 0 B | 0 |
| GetLargeNode | 16.3 ms → 16.2 ms | ~ | 35.2 MiB → 14.6 MiB | 587 → 841 |
| GetLargeNode (max-frame-B) | 4.36 MiB → 380 KiB | **-91.49%** | | |
| FrameDecodeGet | 10.1 µs → 3.6 µs | **-64.48%** (2.8×) | 10.8 KiB → 8.35 KiB | 117 → 56 |
| ReadFrame/single | 1.81 µs → 1.43 µs | **-20.91%** (1.3×) | 1.77 KiB → 1.61 KiB | 34 → 33 |
| ReadFrame/batch10 | 23.3 µs → 15 µs | **-35.42%** (1.5×) | 21 KiB → 20.8 KiB | 337 → 316 |
| EncodePut | 1.23 µs → 1.26 µs | **+2.23%** | 1.12 KiB | 16 |
| HandlePut | 5.63 µs → 4.05 µs | **-28.09%** (1.4×) | 5.15 KiB → 4.28 KiB | 71 → 57 |
| PutLocal | 3.75 µs → 3.78 µs | ~ | 6.2 KiB → 5.51 KiB | 68 → 60 |
| OnceLocal | 2.4 µs → 2.22 µs | **-7.55%** | 2.48 KiB → 1.78 KiB | 39 → 26 |
| DupTrack | 47.9 ns → 45.5 ns | **-5.08%** | 0 B | 0 |
| PutFieldLargeNode/fields=10 | 1.88 µs → 1.25 µs | **-33.24%** (1.5×) | 3.15 KiB → 2.46 KiB | 35 → 24 |
| PutFieldLargeNode/fields=1000 | 1.86 µs → 1.24 µs | **-33.01%** (1.5×) | 3.15 KiB → 2.46 KiB | 35 → 24 |
| PutFieldLargeNode/fields=10000 | 1.79 µs → 1.14 µs | **-35.90%** (1.6×) | 3.15 KiB → 2.46 KiB | 35 → 24 |
| MapOnUpdate/items=100 | 5.21 µs → 4.56 µs | **-12.42%** (1.1×) | 5.49 KiB → 4.77 KiB | 71 → 57 |
| MapOnUpdate/items=1000 | 6.04 µs → 5.25 µs | **-13.14%** (1.2×) | 5.48 KiB → 4.76 KiB | 71 → 57 |
| MapOnUpdate/items=10000 | 12.6 µs → 11.6 µs | **-8.03%** | 5.66 KiB → 4.92 KiB | 73 → 58 |
| OnPut | 4.8 µs → 3.96 µs | **-17.44%** (1.2×) | 5.46 KiB → 4.41 KiB | 69 → 50 |
| UserPut | 287 µs → 286 µs | ~ | 113 KiB → 112 KiB | 1,350 → 1,344 |
| UserOnce | 4.96 µs → 4.54 µs | **-8.33%** | 4.02 KiB → 3.2 KiB | 57 → 45 |
| CheckSEA | 121 µs → 117 µs | **-2.84%** | 44.3 KiB → 34 KiB | 689 → 473 |
| GetFieldLargeNode/fields=10 | 2.23 µs → 1.46 µs | **-34.59%** (1.5×) | 2.31 KiB → 1.51 KiB | 31 → 23 |
| GetFieldLargeNode/fields=10000 | 2.22 µs → 1.48 µs | **-33.21%** (1.5×) | 2.3 KiB → 1.51 KiB | 31 → 23 |
| Fanout/canRead=none | 214 µs → 214 µs | ~ | 38 KiB → 37.1 KiB | 1,076 → 1,064 |
| Fanout/canRead=allow | 215 µs → 224 µs | **+4.09%** | 38.8 KiB → 38 KiB | 1,094 → 1,082 |
| Fanout/canRead=hide | 218 µs → 222 µs | ~ | 39.7 KiB → 38.8 KiB | 1,106 → 1,094 |
| Set | 6.08 µs → 4.94 µs | **-18.77%** (1.2×) | 8.79 KiB → 7.83 KiB | 118 → 92 |
| EncodeLargeNode | 1.36 ms → 1.32 ms | **-2.54%** | 2.58 MiB | 32 |
| Quota/field/quota=off | 1.73 µs → 1.25 µs | **-27.73%** (1.4×) | 3.15 KiB → 2.46 KiB | 35 → 24 |
| Quota/set/quota=off | 6.04 µs → 5.11 µs | **-15.44%** (1.2×) | 8.79 KiB → 7.83 KiB | 118 → 92 |
| Quota/field/quota=on | 2.01 µs → 1.51 µs | **-24.83%** (1.3×) | 3.53 KiB → 2.84 KiB | 39 → 28 |
| Quota/set/quota=on | 6.96 µs → 5.76 µs | **-17.32%** (1.2×) | 9.62 KiB → 8.46 KiB | 127 → 101 |
| GetMissRouted | — → 1.47 µs | new | — → 1.19 KiB | — → 16 |
| PutLocalManyPeersRouted | — → 2.96 µs | new | — → 5.32 KiB | — → 55 |
| ResubscribeBack/souls=1000 | — → 397 µs | new | — → 339 KiB | — → 5,434 |
| ResubscribeBack/souls=10000 | — → 4 ms | new | — → 2.87 MiB | — → 54,526 |
| pebblestore: PutFieldLargeNode/fields=10 | 3.31 µs → 2.75 µs | **-17.01%** (1.2×) | 3.34 KiB → 2.63 KiB | 46 → 32 |
| pebblestore: PutFieldLargeNode/fields=10000 | 3.3 µs → 2.73 µs | **-17.19%** (1.2×) | 3.34 KiB → 2.63 KiB | 46 → 32 |
| pebblestore: GetFieldsMissing | 6.68 µs → 1.35 µs | **-79.78%** (4.9×) | 130 B | 5 |
| pebblestore: PutNewField | 16.5 µs → 6.78 µs | **-58.96%** (2.4×) | 3.61 KiB → 3.02 KiB | 50 → 37 |
| pebblestore: HotSoulSynced | 4.51 ms → 157 µs | **-96.52%** (28.7×) | 4.57 KiB → 2.77 KiB | 48 → 32 |
| pebblestore: FrameOfPutsSynced | 416 ms → 6.54 ms | **-98.43%** (63.7×) | 455 KiB → 442 KiB | 5,998 → 4,710 |
| pebblestore: OnceFieldLargeNodePebble/fields=10 | 2.58 µs → 1.17 µs | **-54.67%** (2.2×) | 2.75 KiB → 1.48 KiB | 57 → 24 |
| pebblestore: OnceFieldLargeNodePebble/fields=10000 | 1.32 ms → 2.19 µs | **-99.83%** (603.9×) | 2.39 MiB → 1.48 KiB | 30,182 → 24 |
| pebblestore: SoulsRangePebble | 50.8 ms → 43.5 µs | **-99.91%** (1167.5×) | 12.5 MiB → 10 KiB | 300,789 → 319 |
| pebblestore: QuotaLoad | 740 ms → 63 ms | **-91.49%** (11.7×) | 101 MiB → 84.8 MiB | 2,610,344 → 2,101,470 |
| pebblestore: PutFields | 2.44 µs → 2.39 µs | ~ |  |  |
| pebblestore: Get | 7.69 µs → 2.46 µs | **-67.96%** (3.1×) |  |  |
| pebblestore: SetSynced | 4.29 ms → 4.73 ms | ~ | 9.06 KiB → 6.7 KiB | 133 → 86 |
| pebblestore: PutFieldsNoHistory | — → 1.25 µs | new | — → 740 B | — → 7 |
| ws: SendRecv/default | 12.4 µs → 9.98 µs | **-19.74%** (1.2×) | 6.6 KiB → 3.21 KiB | 46 → 17 |
| ws: SendRecv/default (wire-B/op) | 2.3 KiB → 2.3 KiB | ~ | | |
| ws: SendRecv/deflate | — → 25.3 µs | new | — → 6.77 KiB | — → 43 |
| ws: SendRecv/deflate (wire-B/op) | — → 242 B | new | | |

## How it was measured

- **Go:** 1.27.3, every run on the same machine.
- **New benchmarks first.** The existing benchmarks did not cover most of the optimizations, so a commit before any change added one or more per optimization (`bench_opt_test.go`, `storage/pebblestore/bench_opt_test.go`, `transport/ws/bench_test.go`). The baseline includes them, so every number is measured against the original code.
- **Each step.** The benchmarks a step touches ran 6 times on the commit before it, in a temporary worktree, then 6 times on the changed code, one after the other so they never compete for the CPU. `benchstat` compares them: the median, its range, and a p-value from a Mann-Whitney U test.
- **Baseline and final.** Every benchmark of the three packages ran 6 times, on the original code and on the final code.
- **Noise.** The same code measured in two separate runs differs by up to 3–5% on small benchmarks. Benchmarks that wait for a goroutine to call back (`OnPut`, `MapOnUpdate`) differ by up to 20%, as do those dominated by an fsync (`SetSynced`, `FrameOfPutsSynced`). A benchmark that runs right after the 1,000-peer benchmarks in the same process is slower, because those leave more heap for the garbage collector: `PutLocal` takes 2.8 µs alone and 3.7 µs there, on the same code. The baseline and final runs run every benchmark in the same order, so they compare fairly; differences of a few percent are not meaningful.
- **Order.** Optimization 3 was measured and committed before 2, since it touched only Pebble and was ready first. Each step is measured against the commit before it, whatever its number.

## Step by step

Each table compares the commit before the step with the step's commit, on the benchmarks the step touches.

### 1. Verify only signatures of fields that can win HAM

A relay verified the ECDSA signature of every signed field it received, before reading anything from its store. Offers and resubscribes send whole nodes the relay mostly has already, and HAM then drops those fields. Now the relay reads the stored states first and verifies only the fields that are newer, in parallel as before. A field that becomes newer only because the store changed meanwhile is verified during the merge. Parsed public keys are cached, and each soul is classified once per node instead of up to three times per field.

A put with a stale forged field is now acknowledged instead of refused; the forged field is neither stored nor passed on. The relay passes on only what changed, and verifies replies it relays in full. Tests cover all three cases.

| Benchmark | Time | Change | Memory | Allocs |
|---|--:|--:|--:|--:|
| HandlePutSigned/new | 165 µs → 164 µs | ~ | 64 KiB → 59.8 KiB | 806 → 616 |
| HandlePutSigned/known | 164 µs → 29.7 µs | **-81.88%** (5.5×) | 69.8 KiB → 37.1 KiB | 843 → 234 |
| HandlePut | 5.52 µs → 5.39 µs | **-2.36%** | 5.14 KiB | 71 |
| UserPut | 274 µs → 275 µs | ~ | 113 KiB → 114 KiB | 1,350 → 1,357 |
| UserOnce | 4.42 µs → 4.49 µs | **+1.61%** | 4.02 KiB | 57 |
| CheckSEA | 117 µs → 114 µs | **-2.31%** | 44.5 KiB → 34.3 KiB | 690 → 474 |

### 2. Sync to disk outside the merge lock, once per frame

`pebblestore` synced every merge while holding the node's lock, and a peer's frame of puts was synced one put at a time. A new optional store interface, `SyncStore`, separates writing from syncing. The DB writes under the lock, syncs after releasing it, and syncs a frame's puts once before acknowledging them. An ack still means the write is on disk. `pebblestore` implements it with Pebble's `LogData` with sync, which syncs the write-ahead log.

| Benchmark | Time | Change | Memory | Allocs |
|---|--:|--:|--:|--:|
| HandlePut | 5.37 µs → 5.5 µs | **+2.44%** | 5.15 KiB | 71 |
| PutLocal | 2.77 µs → 2.8 µs | ~ | 6.19 KiB | 68 |
| Set | 5.9 µs → 6.1 µs | **+3.26%** | 8.78 KiB | 118 |
| pebblestore: HotSoulSynced | 4.52 ms → 166 µs | **-96.33%** (27.2×) | 4.53 KiB → 3.59 KiB | 42 → 41 |
| pebblestore: FrameOfPutsSynced | 438 ms → 6.24 ms | **-98.58%** (70.2×) | 447 KiB → 464 KiB | 5,388 → 5,618 |
| pebblestore: SetSynced | 4.4 ms → 4.29 ms | ~ | 8.92 KiB | 122 |

`HandlePut` and `Set` (in memory, nothing to sync) are 2–3% slower: an interface check and the closure that holds the ack back. That is within the noise of separate runs.

### 3. Tune Pebble

Pebble ran with its defaults: no Bloom filter and an 8 MB block cache. Every merge looks up the fields it writes, most of them new, so each lookup of a missing key read a table per level. The store now uses a 10-bit Bloom filter on every level and a 64 MB cache (`Options.CacheSize`), smaller than the 256 MB the plan suggested, since the cache is only a ceiling. It also gains `Options.NoHistory`, which halves what each write stores, and `PruneHistory`, which bounds history by count or age. One buffer now holds the keys and value of a whole batch.

| Benchmark | Time | Change | Memory | Allocs |
|---|--:|--:|--:|--:|
| PutFieldLargeNode/fields=10 | 1.33 µs → 1.32 µs | ~ | 3.15 KiB | 35 |
| PutFieldLargeNode/fields=1000 | 1.35 µs → 1.33 µs | **-1.30%** | 3.15 KiB | 35 |
| PutFieldLargeNode/fields=10000 | 1.61 µs → 1.59 µs | **-1.33%** | 3.16 KiB | 35 |
| pebblestore: PutFieldLargeNode/fields=10 | 3.35 µs → 3.26 µs | **-2.67%** | 3.34 KiB → 3.3 KiB | 46 → 40 |
| pebblestore: PutFieldLargeNode/fields=10000 | 3.28 µs → 3.21 µs | **-2.38%** | 3.33 KiB → 3.31 KiB | 46 → 40 |
| pebblestore: GetFieldsMissing | 9.09 µs → 1.4 µs | **-84.55%** (6.5×) | 131 B | 5 |
| pebblestore: PutNewField | 15.8 µs → 7.35 µs | **-53.58%** (2.2×) | 3.6 KiB → 3.71 KiB | 50 → 45 |
| pebblestore: OnceFieldLargeNodePebble/fields=10 | 2.58 µs → 2.51 µs | ~ | 2.75 KiB | 57 |
| pebblestore: OnceFieldLargeNodePebble/fields=10000 | 1.34 ms → 1.37 ms | ~ | 2.39 MiB → 2.39 MiB | 30,182 |
| pebblestore: PutFields | 2.5 µs → 2.29 µs | ~ |  |  |
| pebblestore: Get | 7.95 µs → 2.52 µs | **-68.33%** (3.2×) |  |  |
| pebblestore: SetSynced | 4.33 ms → 4.28 ms | ~ | 9.05 KiB → 8.94 KiB | 133 → 122 |
| pebblestore: PutFieldsNoHistory | — → 1.27 µs | new | — → 741 B | — → 7 |

### 4. Read one field per step when following a path

`db.Get("list").Get(id).Get("title")` read every node on the path whole, and asked peers for each whole node, to use one field. Each step now reads only that field: with `GetFields` from the store, directly from the default memory store, and from peers with a get for that one field unless the whole node was asked for already. Interest is now kept per node and field, and `Put` reads the links of a node's nested objects field by field, switching to one whole-node read past 8.

| Benchmark | Time | Change | Memory | Allocs |
|---|--:|--:|--:|--:|
| OnceFieldLargeNode/fields=10 | 529 ns → 266 ns | **-49.63%** (2.0×) | 1.88 KiB → 96 B | 18 → 4 |
| OnceFieldLargeNode/fields=10000 | 117 µs → 285 ns | **-99.76%** (410.3×) | 1.04 MiB → 96 B | 78 → 4 |
| PutLocal | 2.74 µs → 2.67 µs | **-2.61%** | 6.19 KiB → 5.58 KiB | 68 → 63 |
| OnceLocal | 2.05 µs → 1.96 µs | **-4.48%** | 2.48 KiB → 1.87 KiB | 39 → 34 |
| PutFieldLargeNode/fields=10 | 1.34 µs → 1.35 µs | **+1.04%** | 3.15 KiB | 35 |
| PutFieldLargeNode/fields=1000 | 1.36 µs → 1.37 µs | **+1.18%** | 3.15 KiB | 35 |
| PutFieldLargeNode/fields=10000 | 1.59 µs → 1.61 µs | ~ | 3.16 KiB | 35 |
| OnPut | 3.81 µs → 3.9 µs | **+2.30%** | 5.46 KiB | 69 |
| UserOnce | 4.3 µs → 4.18 µs | **-2.78%** | 4.02 KiB → 3.41 KiB | 57 → 52 |
| Set | 5.92 µs → 5.99 µs | ~ | 8.78 KiB | 118 |
| pebblestore: PutFieldLargeNode/fields=10 | 3.27 µs → 3.25 µs | ~ | 3.31 KiB | 41 |
| pebblestore: PutFieldLargeNode/fields=10000 | 3.24 µs → 3.23 µs | ~ | 3.31 KiB → 3.31 KiB | 41 |
| pebblestore: OnceFieldLargeNodePebble/fields=10 | 2.47 µs → 1.12 µs | **-54.78%** (2.2×) | 2.75 KiB → 1.44 KiB | 57 → 24 |
| pebblestore: OnceFieldLargeNodePebble/fields=10000 | 1.33 ms → 2.16 µs | **-99.84%** (616.4×) | 2.39 MiB → 1.44 KiB | 30,182 → 24 |

### 5. Re-render only what changed in Ref.On

`On` read every node of the value and encoded it again on every change. A subscription now keeps the JSON of each node it rendered, drops a node and the nodes that embed it when it changes, and copies the rest. JSON that depends on the path it was reached by (a link cut by a cycle or the depth limit) is never kept.

| Benchmark | Time | Change | Memory | Allocs |
|---|--:|--:|--:|--:|
| OnPutNested/nodes=10 | 18 µs → 12 µs | **-33.49%** (1.5×) | 27.8 KiB → 15.1 KiB | 327 → 142 |
| OnPutNested/nodes=100 | 105 µs → 69.5 µs | **-33.83%** (1.5×) | 169 KiB → 88.7 KiB | 1,691 → 402 |
| OnPutNested/nodes=1000 | 864 µs → 703 µs | **-18.59%** (1.2×) | 1.24 MiB → 944 KiB | 11,158 → 3,190 |
| OnceLocal | 2.07 µs → 2.14 µs | **+3.61%** | 1.87 KiB → 1.88 KiB | 34 |
| MapOnUpdate/items=100 | 4.79 µs → 4.97 µs | **+3.75%** | 5.49 KiB → 5.51 KiB | 71 |
| MapOnUpdate/items=1000 | 5.98 µs → 6.04 µs | **+1.04%** | 5.49 KiB → 5.51 KiB | 71 |
| MapOnUpdate/items=10000 | 12.3 µs → 12.7 µs | **+2.94%** | 5.66 KiB → 5.68 KiB | 73 |
| OnPut | 4.65 µs → 5.15 µs | **+10.86%** | 5.46 KiB → 5.16 KiB | 69 → 65 |

The gain shrinks with size because the rest is decoding the whole value into `T`, which `On` must do since it delivers the whole value, and garbage collection: a profile at 1,000 nodes shows the render itself is now a small part. `OnceLocal` and `MapOnUpdate` do not use the cache; their 1–4% are run-to-run noise.

### 6. Options.RouteByInterest, and no goroutine per relayed miss

A relay passed every get it could not answer, and its own writes, to every connected peer. With `Options.RouteByInterest`, they go to the peers it dialled and the peers that asked for the same node, the way it already passed on clients' writes. It is off by default, since a mesh may hold data only in some client. `gundb server` turns it on (`--ask-every-peer` turns it off). A relayed get now ends on timers instead of a goroutine that waited up to `Options.Wait`.

| Benchmark | Time | Change | Memory | Allocs |
|---|--:|--:|--:|--:|
| GetMiss | 273 µs → 272 µs | ~ | 124 KiB → 124 KiB | 1,064 |
| PutLocalManyPeers | 223 µs → 223 µs | ~ | 38.2 KiB | 1,077 |
| Fanout/canRead=none | 215 µs → 214 µs | ~ | 38 KiB | 1,076 |
| Fanout/canRead=allow | 221 µs → 221 µs | ~ | 38.8 KiB | 1,094 |
| Fanout/canRead=hide | 219 µs → 223 µs | ~ | 39.7 KiB | 1,106 |
| GetMissRouted | — → 2.02 µs | new | — → 1.71 KiB | — → 24 |
| PutLocalManyPeersRouted | — → 3.79 µs | new | — → 6.47 KiB | — → 76 |

Against the code before this step, with the option on: a miss is **272.7 µs → 2.0 µs (135×)** and the relay's own write **222.9 µs → 3.8 µs (59×)**.

### 7. Cheap reconnects: offer only what changed, forget idle reads

Every soul ever read stayed of interest. Every reconnect read, encoded and offered each one, before the peer's messages were read at all. Now:

- **Offers:** a peer that comes back with a PID seen before is offered only the nodes that changed while it was away, plus two minutes for what may have been in flight. A new or restarted peer has a new PID and is offered everything, as before.
- **Batching:** offers go out 100 nodes per put.
- **Non-blocking:** resubscribing runs on its own goroutine.
- **Eviction:** interests nobody watches are dropped after 10 idle minutes.

| Benchmark | Time | Change | Memory | Allocs |
|---|--:|--:|--:|--:|
| Resubscribe/souls=1000 | 1.36 ms → 925 µs | **-31.85%** (1.5×) | 1.81 MiB → 1.65 MiB | 20,256 → 11,792 |
| Resubscribe/souls=10000 | 15.1 ms → 10.2 ms | **-32.11%** (1.5×) | 17.3 MiB → 16.8 MiB | 203,297 → 118,318 |
| HandlePut | 5.42 µs → 5.44 µs | ~ | 5.14 KiB | 71 |
| PutLocal | 2.72 µs → 2.95 µs | **+8.37%** | 5.58 KiB → 5.58 KiB | 63 |
| OnceLocal | 1.96 µs → 2 µs | **+2.04%** | 1.88 KiB | 34 |
| ResubscribeBack/souls=1000 | — → 428 µs | new | — → 373 KiB | — → 5,500 |
| ResubscribeBack/souls=10000 | — → 4.39 ms | new | — → 3.34 MiB | — → 55,458 |

`PutLocal` is 3–5% slower in repeated runs: every write now notes, under the interest lock, which nodes of interest changed.

### 8. List only the souls in range, and count quotas in one pass

A LEX query listed every soul with its prefix, then sorted and filtered them. Two new optional interfaces fix that:

- **`SoulRangeStore`** lists only a range. The memory stores keep a sorted index of souls, merged lazily, and Pebble seeks straight to the range. It is used only when the bounds compare the same in byte order and in JS's UTF-16 order (characters below U+D800); a test checks the cases where the two orders differ.
- **`ScanStore`** reads every node in one pass, so opening a DB with a quota no longer lists the souls and reads each one.

| Benchmark | Time | Change | Memory | Allocs |
|---|--:|--:|--:|--:|
| SoulsRange | 2.88 ms → 3.51 µs | **-99.88%** (818.5×) | 8.51 MiB → 4.66 KiB | 32 → 12 |
| PutLocal | 2.78 µs → 2.85 µs | **+2.32%** | 5.58 KiB | 63 |
| Set | 5.97 µs → 5.89 µs | ~ | 8.79 KiB → 8.87 KiB | 118 |
| Quota/field/quota=off | 1.62 µs → 1.63 µs | ~ | 3.16 KiB | 35 |
| Quota/set/quota=off | 5.93 µs → 5.94 µs | ~ | 8.78 KiB → 8.87 KiB | 118 |
| Quota/field/quota=on | 1.87 µs → 1.89 µs | ~ | 3.54 KiB | 39 |
| Quota/set/quota=on | 6.83 µs → 6.82 µs | ~ | 9.62 KiB → 9.69 KiB | 127 |
| pebblestore: SoulsRangePebble | 43.5 ms → 43.9 µs | **-99.90%** (991.1×) | 12.9 MiB → 10 KiB | 300,904 → 319 |
| pebblestore: QuotaLoad | 232 ms → 62 ms | **-73.29%** (3.7×) | 106 MiB → 84.8 MiB | 2,604,964 → 2,101,353 |

### 9. Decode frames in one pass, and relay puts as received

Decoding was slow in three ways:
- a batch was parsed into raw messages, and each message parsed again;
- a get went through a map and four more `Unmarshal` calls;
- every put a relay passed on was encoded again.

Frames are now read with one pooled `jsontext` decoder that parses the frame in place, and an unchanged message is passed on as the bytes it arrived as. A message without an ID, which the relay must give one, is encoded again. `TestReadFrameMatchesV1` checks the new decoder against the old one on a corpus of normal and malformed frames, and a fuzz test compared them on 7.6 million inputs. The one intended difference: keys now match exactly (`"put"`, not `"Put"`), as in GUN.js.

| Benchmark | Time | Change | Memory | Allocs |
|---|--:|--:|--:|--:|
| HandlePutSigned/new | 165 µs → 157 µs | **-4.53%** | 59.8 KiB → 54.7 KiB | 616 → 606 |
| HandlePutSigned/known | 29.7 µs → 28.4 µs | **-4.23%** | 37.1 KiB → 36.9 KiB | 234 → 232 |
| GetMiss | 269 µs → 271 µs | ~ | 124 KiB → 123 KiB | 1,064 → 1,052 |
| HandlePutLargeNode | 897 µs → 698 µs | **-22.16%** (1.3×) | 1.28 MiB → 1.06 MiB | 4,210 → 4,182 |
| FrameDecodeGet | 10.4 µs → 3.54 µs | **-65.92%** (2.9×) | 10.8 KiB → 8.35 KiB | 117 → 56 |
| ReadFrame/single | 1.77 µs → 1.44 µs | **-18.63%** (1.2×) | 1.77 KiB → 1.61 KiB | 34 → 33 |
| ReadFrame/batch10 | 22.8 µs → 15.1 µs | **-33.67%** (1.5×) | 21 KiB → 20.8 KiB | 337 → 316 |
| HandlePut | 5.47 µs → 4.16 µs | **-24.01%** (1.3×) | 5.15 KiB → 4.48 KiB | 71 → 61 |
| GetFieldLargeNode/fields=10 | 2.23 µs → 1.49 µs | **-33.14%** (1.5×) | 2.32 KiB → 1.61 KiB | 31 → 23 |
| GetFieldLargeNode/fields=10000 | 2.24 µs → 1.5 µs | **-33.13%** (1.5×) | 2.31 KiB → 1.61 KiB | 31 → 23 |
| Fanout/canRead=none | 214 µs → 215 µs | ~ | 38 KiB → 37.2 KiB | 1,076 → 1,066 |
| Fanout/canRead=allow | 219 µs → 222 µs | ~ | 38 KiB | 1,094 → 1,084 |
| Fanout/canRead=hide | 219 µs → 222 µs | ~ | 39.7 KiB → 38.9 KiB | 1,106 → 1,096 |

### 10. Count connected peers per direction

`connected` walked every connected peer on each network read and `PutAck`, and `alone` did when no peer matched. Both now read counts kept as peers come and go. No benchmark moved: `alone` already stopped at the first matching peer, and `connected` matters only with thousands of peers on a path no benchmark covers. It is kept because it removes a scan from those paths at no cost.

| Benchmark | Time | Change | Memory | Allocs |
|---|--:|--:|--:|--:|
| GetMiss | 271 µs → 266 µs | ~ | 123 KiB | 1,052 |
| GetMissRouted | 1.5 µs → 1.5 µs | ~ | 1.22 KiB | 16 |
| PutLocalManyPeers | 222 µs → 219 µs | ~ | 38.1 KiB | 1,077 |
| PutLocalManyPeersRouted | 3.9 µs → 3.87 µs | ~ | 6.47 KiB | 76 |
| PutLocal | 3.61 µs → 3.66 µs | **+1.30%** | 5.6 KiB → 5.6 KiB | 63 |
| OnceLocal | 2.25 µs → 2.26 µs | ~ | 1.89 KiB → 1.89 KiB | 34 |

### 11. Shard the message ID table

Every message received, replied to or relayed took the one lock of the dedup table. The table is now 64 shards by the ID's hash, each with its own lock and cache line. Rotation still drops a generation of the whole table at once, so IDs expire exactly as before.

| Benchmark | Time | Change | Memory | Allocs |
|---|--:|--:|--:|--:|
| DupTrackParallel | 285 ns → 29.5 ns | **-89.67%** (9.7×) | 2 B → 0 B | 0 |
| HandlePut | 4.14 µs → 4.18 µs | **+1.11%** | 4.46 KiB | 61 |
| DupTrack | 45.2 ns → 46.2 ns | **+2.15%** | 0 B | 0 |
| GetFieldLargeNode/fields=10 | 1.51 µs → 1.51 µs | ~ | 1.61 KiB | 23 |
| GetFieldLargeNode/fields=10000 | 1.5 µs → 1.51 µs | **+0.97%** | 1.61 KiB → 1.61 KiB | 23 |

### 12. Reused read buffers, and Options.Compress

`Recv` grew a new buffer for every frame with `io.ReadAll`. It now reads into a pooled buffer and returns one exact copy. Since step 9, messages keep slices of their frame, so a reused buffer cannot be handed out. `Options.Compress` (`gundb server --compress`, `ws.AcceptWith`, `ws.Dialer.Compress`) negotiates permessage-deflate with peers that offer it, as browsers and GUN.js on Node do; an interop test runs GUN.js through a compressing relay.

| Benchmark | Time | Change | Memory | Allocs |
|---|--:|--:|--:|--:|
| HandlePut | 4.18 µs → 4.21 µs | **+0.75%** | 4.46 KiB | 61 |
| ws: SendRecv/default | 13 µs → 10.2 µs | **-21.88%** (1.3×) | 6.6 KiB → 3.21 KiB | 46 → 17 |
| ws: SendRecv/default (wire-B/op) | 2.3 KiB → 2.3 KiB | ~ | | |
| ws: SendRecv/deflate | — → 25.4 µs | new | — → 6.78 KiB | — → 43 |
| ws: SendRecv/deflate (wire-B/op) | — → 242 B | new | | |

Compression is off by default. A batch of 10 puts crosses the network in 242 bytes instead of 2,359 (9.7× less), but sending and receiving it takes 25 µs instead of 10 µs.

### 13. Send nodes of more than 1,000 fields in chunks

A get for a list of 100,000 items was answered with one message of megabytes, built whole in memory. Replies and offers now carry at most 1,000 fields each, all but the last marked `%`, as replies to LEX queries already were. Each chunk is written straight from the stored node. Go clients, relays that pass the chunks on, and GUN.js (`.once()` included) read them back whole; tests cover all three.

| Benchmark | Time | Change | Memory | Allocs |
|---|--:|--:|--:|--:|
| Resubscribe/souls=1000 | 940 µs → 937 µs | ~ | 1.66 MiB | 11,832 |
| Resubscribe/souls=10000 | 11 ms → 10.2 ms | **-7.38%** | 16.8 MiB | 118,822 → 118,736 |
| ResubscribeBack/souls=1000 | 429 µs → 420 µs | ~ | 341 KiB | 5,544 |
| ResubscribeBack/souls=10000 | 4.55 ms → 4.34 ms | **-4.66%** | 3.38 MiB → 3.32 MiB | 55,896 |
| GetLargeNode | 15.8 ms → 15.4 ms | **-2.55%** | 35.2 MiB → 14.6 MiB | 576 → 841 |
| GetLargeNode (max-frame-B) | 4.36 MiB → 268 KiB | **-93.99%** | | |
| GetFieldLargeNode/fields=10 | 1.54 µs → 1.52 µs | **-1.52%** | 1.61 KiB | 23 |
| GetFieldLargeNode/fields=10000 | 1.53 µs → 1.52 µs | ~ | 1.62 KiB | 23 |

### 14. Cut allocations on the write and relay paths

- **Default store merges:** read the stored fields straight from the incoming node, without building a list of names.
- **`applyFrom`:** makes the graph of writes from the future only when there is one, and doesn't order a one-node write.
- **HAM:** calls two equal values the same without writing them as JSON. This is most of the gain for re-sent signed data.
- **`toTree`:** skips the encoding round trip for values that are already plain. A test checks it gives what encoding gives, and `float32`, invalid UTF-8, NaN, `json.Number` and cycles still go through `encoding/json`.
- **`randomID`:** one call to the random generator per ID instead of nine.
- **`writeLoop`:** joins batches into one buffer reused from frame to frame.

| Benchmark | Time | Change | Memory | Allocs |
|---|--:|--:|--:|--:|
| HandlePutSigned/new | 158 µs → 156 µs | **-1.25%** | 54.7 KiB → 52.7 KiB | 606 → 592 |
| HandlePutSigned/known | 28 µs → 10.9 µs | **-61.20%** (2.6×) | 36.9 KiB → 15.5 KiB | 232 → 98 |
| HandlePut | 4.27 µs → 4.02 µs | **-5.97%** | 4.47 KiB → 4.27 KiB | 61 → 57 |
| PutLocal | 2.88 µs → 2.95 µs | **+2.27%** | 5.58 KiB → 5.48 KiB | 63 → 60 |
| PutFieldLargeNode/fields=10 | 1.39 µs → 957 ns | **-31.30%** (1.5×) | 3.15 KiB → 2.46 KiB | 35 → 24 |
| PutFieldLargeNode/fields=1000 | 1.42 µs → 974 ns | **-31.22%** (1.5×) | 3.15 KiB → 2.46 KiB | 35 → 24 |
| PutFieldLargeNode/fields=10000 | 1.61 µs → 1.12 µs | **-30.42%** (1.4×) | 3.16 KiB → 2.46 KiB | 35 → 24 |
| MapOnUpdate/items=100 | 4.55 µs → 4.03 µs | **-11.23%** (1.1×) | 5.5 KiB → 4.79 KiB | 71 → 61 |
| MapOnUpdate/items=1000 | 6.25 µs → 5.25 µs | **-16.00%** (1.2×) | 5.51 KiB → 4.8 KiB | 71 → 61 |
| MapOnUpdate/items=10000 | 12.8 µs → 11.9 µs | **-6.74%** | 5.68 KiB → 4.98 KiB | 73 → 63 |
| UserPut | 292 µs → 287 µs | ~ | 113 KiB → 112 KiB | 1,352 → 1,344 |
| Fanout/canRead=none | 218 µs → 217 µs | ~ | 37.2 KiB → 37.1 KiB | 1,066 → 1,064 |
| Fanout/canRead=allow | 227 µs → 223 µs | ~ | 38 KiB → 38 KiB | 1,084 → 1,082 |
| Fanout/canRead=hide | 251 µs → 222 µs | ~ (1.1×) | 38.9 KiB → 38.8 KiB | 1,096 → 1,094 |
| Set | 6.05 µs → 5.06 µs | **-16.42%** (1.2×) | 8.88 KiB → 7.81 KiB | 118 → 92 |
| pebblestore: PutFieldLargeNode/fields=10 | 3.23 µs → 2.74 µs | **-15.12%** (1.2×) | 3.31 KiB → 2.63 KiB | 41 → 32 |
| pebblestore: PutFieldLargeNode/fields=10000 | 3.2 µs → 2.72 µs | **-15.00%** (1.2×) | 3.31 KiB → 2.63 KiB | 41 → 32 |
| pebblestore: FrameOfPutsSynced | 5.7 ms → 6.94 ms | **+21.69%** | 450 KiB → 444 KiB | 4,724 → 4,712 |

`FrameOfPutsSynced` measured +22% here with ±20% noise. Measured again with 10 runs on each side, it was 6.56 ms against 6.68 ms (p = 0.58): no difference.

## The --obf feature

`--obf` keeps the names of a part of a user's space from the server, with a flag on the existing commands instead of a new subcommand.

- **`sea/pathmap`** seals a name into a token: an HMAC-SHA256 synthetic IV, then AES-256-CTR, as encrypted file systems seal file names. Tokens are deterministic, so offline writes merge; bound to their node, so a name repeated in two nodes cannot be counted; and padded to 16 bytes. A test vector was checked against an independent implementation with Node's `crypto`. Grants carry the key to readers, encrypted with the secret two users share, under a field only they can work out.
- **The core** gained `Ref.Sealed(KeyCodec)`, which seals every key below a ref wherever it meets a soul and opens the names on every read (`Once`, `On`, `Map().On`, `Node`, `Links`), and `User.Soul`, a signed ref to a node nothing links to. Relays need no change.
- **The CLI** gained `--obf` (`--obfuscate`) on `put`, `set`, `unset`, `get` and `sub`:
  - the first write to one of your scopes creates it;
  - `--enc` and `--dec` use the scope's key, so everyone you share it with can decrypt;
  - `--to` grants access;
  - `get --links` shows the map from names to tokens, and `get ~ --obf` lists your scopes;
  - `put --rotate` gives a scope a new key and root and keeps only the readers named;
  - a write without `--obf` to one of your scopes is refused.
- **Documentation:** GUIDE.md section 9 ("Hide the names: sealed scopes", with [examples/obfuscate](examples/obfuscate/main.go)) and section 14 ("Hide the names too: --obf"), and SCENARIOS.md scenario 17.

What sealing costs, on a path of three keys (`BenchmarkSealed`):

| | Plain | Sealed | Memory (plain → sealed) | Allocs |
|---|--:|--:|--:|--:|
| `Put` | 3.01 µs | 4.20 µs | 6.9 KiB → 9.9 KiB | 65 → 102 |
| `Once` | 0.89 µs | 1.97 µs | 1.3 KiB → 4.1 KiB | 21 → 54 |

## What did not improve

- **Signed writes that are new** (`HandlePutSigned/new`, `UserPut`, `CheckSEA`) cost what they did: ECDSA verification and signing dominate, and new data must be verified.
- **Fan-out to 1,000 subscribers** (`Fanout`) is unchanged within a few percent (one variant measured +4%). Its cost is queueing a message for each peer.
- **A miss or an own write without `RouteByInterest`** (`GetMiss`, `PutLocalManyPeers`) is unchanged, by design: the default keeps GUN.js relay behavior.
- **`PutLocal`** takes the same time with 12% fewer allocations. Step 7's interest bookkeeping took back what other steps saved on that path.
- **Optimization 10** changed no benchmark (see above).
- **`EncodePut`** is 2% slower, within noise; encoding was not changed.

## Reproduce

```sh
git switch optimizations
go test -run '^$' -bench . -count 6 . ./storage/pebblestore ./transport/ws > new.txt
git stash; git checkout main   # or any commit to compare with
go test -run '^$' -bench . -count 6 . ./storage/pebblestore ./transport/ws > old.txt
benchstat old.txt new.txt
```

The benchmarks added for this work are in `bench_opt_test.go`, `storage/pebblestore/bench_opt_test.go`, `transport/ws/bench_test.go` and `sealed_test.go`. On `main` only the ones that existed before run.
