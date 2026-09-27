import subprocess, tarfile, time, urllib.request
from pathlib import Path

def sh(cmd, **kw):
    return subprocess.run(cmd, shell=True, check=True, text=True, capture_output=True, **kw).stdout

src_dir = Path("/tmp/lua/src")

if not src_dir.exists():
    tar = "/tmp/lua.tar.gz"
    urllib.request.urlretrieve("https://www.lua.org/ftp/lua-5.4.7.tar.gz", tar)
    with tarfile.open(tar) as f:
        f.extractall("/tmp")
    Path("/tmp/lua-5.4.7").rename("/tmp/lua")

workers = " ".join(f"w{i}" for i in range(20))

sh(f"docker rm -f orch {workers} 2>/dev/null || true")
sh("docker network rm benchnet 2>/dev/null || true")
sh("docker network create benchnet")
sh("docker build -t distbuild .")
sh("docker build -t bench -", input="FROM distbuild\nRUN apt-get update && apt-get install -y gcc\n")

for i in range(20):
    sh(f"docker run -d --name w{i} --network benchnet --network-alias worker distbuild worker")

sh("docker run -d --name orch --network benchnet -e WORKER_SERVICE=worker distbuild orchestrator")
time.sleep(1)

cmd = """
cd /src
rm -f *.o
t0=$(date +%s%N)
ls *.c | xargs -n1 -P4 gcc -O2 -c -I.
t1=$(date +%s%N)

rm -f *.o
t2=$(date +%s%N)
ORCHESTRATOR_ADDR=http://orch:8080 OUT_DIR=/src DISTBUILD_CC=gcc /usr/local/bin/client -O2 -I. *.c
t3=$(date +%s%N)

echo $(( t1 - t0 )) $(( t3 - t2 ))
"""

try:
    raw = subprocess.check_output(
        f"docker run --rm --network benchnet -v '{src_dir}':/src bench bash -c '{cmd}'",
        shell=True, text=True
    ).strip().split()
    local, dist = int(raw[-2]) / 1e9, int(raw[-1]) / 1e9
    print(f"Local:       {local}s")
    print(f"Distributed: {dist}s")
    print(f"Speedup:     {local / dist}x")
finally:
    sh(f"docker rm -f orch {workers}")
    sh("docker network rm benchnet")
    