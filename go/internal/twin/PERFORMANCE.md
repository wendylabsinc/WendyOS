# Room storage and response performance

Measured on an Apple M4 Max, Darwin arm64, November 26, 2026. These are local synthetic benchmarks, not robot or network latency measurements.

The fixture stores 20,000 points, a point layer containing the same number of points, 3,000 reference surfaces and 45,000 localization mesh scalars. Benchmarks repeatedly read an unchanged room revision; the first cache fill is included and amortized. A new revision, external file change, cache eviction or process restart pays the decode/derivation cost again.

Run:

```sh
go test ./go/internal/twin -run '^$' -bench BenchmarkDenseRoom -benchmem -benchtime=500ms -count=3
```

Median results from three runs:

| Operation | Before | After | Before bytes/op | After bytes/op |
| --- | ---: | ---: | ---: | ---: |
| Room list | 15.00 ms | 12.60 µs | 10,165,401 | 2,547 |
| Detached room state | 15.86 ms | 66.67 µs | 13,834,257 | 1,477,926 |
| Detached room frame | 14.71 ms | 66.78 µs | 10,164,209 | 1,477,756 |
| Atomic frame validation | 14.91 ms | 1.80 µs | 10,164,134 | 611 |
| Encode an uncached gzip response | 8.54 ms | 8.93 ms | 815,586 | 13,605 |

An explicitly evicted cold frame read measured 15.75 ms and 11,645,439 bytes/op, versus the original uncached frame read at 14.71 ms and 10,164,209 bytes/op. The extra copy preserves immutable cached ownership; this change targets repeated reads, not first-load speed.

Gzip writer reuse reduces allocation; it does not reduce the underlying JSON serialization cost. The unchanged-revision HTTP state response therefore also caches its compressed wire representation. Compared with the intermediate decoded-room-cache version, that endpoint went from 9.29 ms and 3,231,136 bytes/op to 2.23 µs and 1,141 bytes/op. This excludes network transfer time and benefits repeated polling of an unchanged revision.

CPU and allocation profiles were captured with Go's `-cpuprofile` and `-memprofile` flags. After caching, detached state/frame allocation is dominated by copies of points, mesh scalars and reference surfaces. Those copies deliberately remain: callers can mutate returned rooms without changing the cache or another request.

## Bounds and consistency

- At most eight room entries and 32 MiB of conservatively accounted retained arrays, maps and metadata. Derived floor tiles and gzip payload capacities count against the same budget. An individual compressed payload larger than 8 MiB is not retained.
- Model assets and camera images are not added to this cache. `sync.Pool` reuses gzip encoder workspaces, and encoders release their response writer reference before returning to the pool.
- Every successful atomic room write invalidates decoded state, derived floor tiles and the wire image together.
- Each cache lookup validates inode identity, file size, modification time and change time. Darwin and Linux provide change time; other platforms read without retaining snapshots.
- Cold reads use an opened file and compare its metadata before and after reading. Concurrent file changes produce an error rather than binding unrelated bytes to cached metadata.
- Frame validation still runs under the store mutex. Room revision checks, controller ownership, physical motion limits and observation freshness policies are unchanged.

Validation includes full twin package regression, focused race tests for mutable return isolation, byte/entry eviction, derived-floor invalidation, concurrent readers/writers, gzip response equivalence, deletion/corruption, atomic replacement and same-inode rewrites with restored modification times. The package also cross-compiles for Linux arm64 with CGO disabled.
