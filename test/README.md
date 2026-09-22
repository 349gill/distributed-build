# Benchmark

```bash
test/bench.sh
```

This starts a Docker cluster: an orchestrator plus 10 workers. It then compiles 50 standalone C files from the Linux tree in two ways, once with local gcc and once through the distbuild client, and prints output like this (from an 8-CPU Docker VM):

```
Local build       : 8.684s
Distributed build : 1.720s  (10 workers)
Speedup           : 5.05x
Ceiling           : 6.69x  (preprocess is 15% of the local build)
Correctness       : 49/49 byte-identical, PASS
```

Any file that fails to compile locally is skipped; on arm64 hosts that is one file with x86 inline asm. The script exits non-zero if any distributed object differs from the local one.

The 50 files are userspace helpers from `scripts/` and `tools/` that compile without kernel headers. This checks the distribution pipeline; it does not build the kernel.

## Knobs

| Env / flag     | Default | Meaning                                            |
|----------------|---------|----------------------------------------------------|
| `WORKERS`      | 10      | worker containers                                  |
| `WORKER_JOBS`  | 1       | concurrent compiles per worker                     |
| `BENCH_CPUS`   | 0.8     | CPU cap on the client container                    |
| `--files N`    | 50      | number of C files                                  |
| `--cflags ...` | `-O2`   | flags for both builds                              |

For example: `WORKERS=4 test/bench.sh --files 100`.

## Reading the numbers

- **Ceiling:** the client preprocesses every file locally, because only it has the headers, so that step never gets distributed. That caps the speedup at `local / preprocess` (Amdahl's law). At `-O0` the ceiling is close to 1x, which is why the default is `-O2`.
- **Shared host:** every container runs on the same Docker host. `BENCH_CPUS=0.8` limits the client to model a thin machine offloading to a build farm. To get real numbers, run the workers on separate machines, for example with `k8s/`.
- **Linux source:** the clone is kept in the `distbuild-bench-linux-src` Docker volume and reused between runs. Delete it with `docker volume rm distbuild-bench-linux-src`.
