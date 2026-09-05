---
tags:
  - Reference
  - Endpoints
  - Monitoring
---

# Go Runtime Profiling

Configure the profiling server with these environment variables:

| Key             | Type    | Description                                              | Default |
|-----------------|---------|----------------------------------------------------------|---------|
| `PPROF_ENABLED` | boolean | Enables Go's built-in runtime profiling endpoints.       | `false` |
| `PPROF_PORT`    | number  | Loopback port used by the profiling server when enabled. | `6060`  |

!!! note "Profiling server is not exposed"
    The profiling server is intentionally bound to loopback and cannot be exposed through Docker port publishing. 

The doco-cd image does not include a shell, so collect profiles through a helper container that shares its network namespace:

```bash
docker run --rm --network container:doco-cd curlimages/curl:latest \
  -sSf 'http://127.0.0.1:6060/debug/pprof/heap?gc=1' > heap.pb.gz
```

Use `gc=1` for baseline and memory-growth comparisons. It forces garbage collection before the profile is collected, 
excluding objects that are no longer reachable but have not yet been reclaimed. 
Omit it only when investigating short-lived allocation spikes or garbage-collection behavior.

To capture a GC-forced heap profile automatically once the container exceeds 60 MiB of Docker-reported memory, run:

```bash
./scripts/capture-heap-on-memory.sh
```

The script polls every 10 seconds, writes `after.pb.gz`, and exits after a successful capture. 
Override its defaults with environment variables:

```bash
INTERVAL_SECONDS=30 THRESHOLD_MIB=100 OUTPUT_FILE=high-memory.pb.gz \
  ./scripts/capture-heap-on-memory.sh
```

Compare two heap profiles with the Go toolchain:

```bash
go tool pprof -base before.pb.gz heap.pb.gz
```

See Go's [pprof documentation](https://pkg.go.dev/net/http/pprof) for all available profiles and query parameters.
