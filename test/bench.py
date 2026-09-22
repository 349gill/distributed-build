#!/usr/bin/env python3
"""Local vs. distributed compile benchmark. Run it through test/bench.sh.

Compiles N standalone C files from the Linux tree (userspace helpers in
scripts/ and tools/ -- a pipeline benchmark, not a kernel build) twice:
once with local gcc, once through the distbuild client. Then checks that
both produced byte-identical objects and prints the speedup.
"""

import argparse
from concurrent.futures import ThreadPoolExecutor
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request


def run(cmd, **kw):
    return subprocess.run(cmd, text=True, capture_output=True, **kw)


def wait_for(url: str, timeout=30):
    deadline = time.monotonic() + timeout
    while True:
        try:
            urllib.request.urlopen(f"{url}/compile", timeout=1)
            return
        except urllib.error.HTTPError:
            return  # it answered, so it is up
        except OSError:
            if time.monotonic() > deadline:
                raise SystemExit(f"orchestrator at {url} not reachable")
            time.sleep(0.5)


def clone(repo: str, dest: Path):
    if (dest / ".git").exists():
        print(f"Reusing Linux clone at {dest}")
        return
    print(f"Cloning {repo} (depth 1) into {dest}...")
    subprocess.run(["git", "clone", "--depth", "1", repo, str(dest)], check=True)


def collect_files(src: Path, count: int) -> list:
    """First `count` C files (unique names) that compile without kernel headers."""
    found, seen = [], set()
    for top in ("scripts", "tools"):
        for root, _, files in os.walk(src / top):
            for name in sorted(files):
                if not name.endswith(".c") or name in seen:
                    continue
                seen.add(name)
                path = Path(root) / name
                if run(["gcc", "-fsyntax-only", "-w", str(path)]).returncode == 0:
                    found.append(path)
                    if len(found) == count:
                        return found
    return found


def parallel(fn, items, jobs):
    with ThreadPoolExecutor(max_workers=jobs) as pool:
        return list(pool.map(fn, items))


def timed(fn) -> float:
    t0 = time.perf_counter()
    fn()
    return time.perf_counter() - t0


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--orchestrator", required=True)
    p.add_argument("--worker-service", default="worker")
    p.add_argument("--client-bin", default="/usr/local/bin/client")
    p.add_argument("--src", default="/src/linux")
    p.add_argument("--repo", default="https://github.com/torvalds/linux.git")
    p.add_argument("--files", type=int, default=50)
    p.add_argument("--cflags", default="-O2")
    p.add_argument("--jobs", type=int, default=os.cpu_count())
    args = p.parse_args()

    src = Path(args.src)
    cflags = args.cflags.split()
    wait_for(args.orchestrator)
    clone(args.repo, src)
    files = collect_files(src, args.files)
    workers = len({a[4][0] for a in socket.getaddrinfo(args.worker_service, None, proto=socket.IPPROTO_TCP)})
    print(f"{len(files)} candidate files, {workers} workers, {args.jobs} local jobs, cflags: {args.cflags}")

    local_dir = Path(tempfile.mkdtemp(prefix="local-"))
    dist_dir = Path(tempfile.mkdtemp(prefix="dist-"))

    def compile_local(f):
        return run(["gcc", *cflags, "-c", str(f), "-o", str(local_dir / f"{f.stem}.o")])

    results = []
    local = timed(lambda: results.extend(parallel(compile_local, files, args.jobs)))
    # A few files pass the syntax check but fail to assemble (e.g. x86 inline asm on arm64).
    files = [f for f, r in zip(files, results) if r.returncode == 0]
    # Preprocessing stays on the client in both builds, so it caps the speedup.
    preprocess = timed(lambda: parallel(lambda f: run(["gcc", "-E", *cflags, str(f)]), files, args.jobs))

    env = dict(os.environ, ORCHESTRATOR_ADDR=args.orchestrator, OUT_DIR=str(dist_dir))

    def compile_dist():
        res = run([args.client_bin, *cflags, *map(str, files)], env=env)
        if res.returncode != 0:
            raise SystemExit(f"client failed:\n{res.stderr}")

    dist = timed(compile_dist)

    matched = sum(
        (dist_dir / f"{f.stem}.o").exists()
        and (dist_dir / f"{f.stem}.o").read_bytes() == (local_dir / f"{f.stem}.o").read_bytes()
        for f in files
    )
    shutil.rmtree(local_dir)
    shutil.rmtree(dist_dir)

    ceiling = local / preprocess
    ok = matched == len(files)
    print(f"""
Local build       : {local:.3f}s
Distributed build : {dist:.3f}s  ({workers} workers)
Speedup           : {local / dist:.2f}x
Ceiling           : {ceiling:.2f}x  (preprocess is {100 * preprocess / local:.0f}% of the local build)
Correctness       : {matched}/{len(files)} byte-identical, {'PASS' if ok else 'FAIL'}
""")
    raise SystemExit(0 if ok else 1)


if __name__ == "__main__":
    main()
