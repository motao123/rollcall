#!/usr/bin/env python3
"""Rollcall 稳态容量基线：先构造"历史环已满"的 state.json，再启动服务端测量。

为什么需要这个脚本：bench_capacity.py 只跑几十秒，24h 原始环是空的，
而生产环境下每节点环里有 2880 个点（30s × 24h）。快照序列化的成本完全取决于
环是否满，所以必须先把数据填满再测，否则会低估一个数量级。

用法：
    python bench_steady.py --binary C:\\tmp\\rc-bench.exe --nodes 40 100 200
"""
import argparse
import ctypes
import ctypes.wintypes as wt
import gzip
import json
import os
import random
import shutil
import statistics
import subprocess
import sys
import time
import urllib.error
import urllib.request

RING = 2880  # 24h @ 30s
PROCESS_QUERY_INFORMATION = 0x0400
PROCESS_VM_READ = 0x0010


class PMC(ctypes.Structure):
    _fields_ = [("cb", wt.DWORD), ("PageFaultCount", wt.DWORD),
                ("PeakWorkingSetSize", ctypes.c_size_t), ("WorkingSetSize", ctypes.c_size_t),
                ("QuotaPeakPagedPoolUsage", ctypes.c_size_t), ("QuotaPagedPoolUsage", ctypes.c_size_t),
                ("QuotaPeakNonPagedPoolUsage", ctypes.c_size_t), ("QuotaNonPagedPoolUsage", ctypes.c_size_t),
                ("PagefileUsage", ctypes.c_size_t), ("PeakPagefileUsage", ctypes.c_size_t)]


def proc_mem(pid):
    """返回 (working_set_bytes, private_bytes)。"""
    if os.name != "nt":
        try:
            info = {}
            with open("/proc/%d/status" % pid, encoding="utf-8") as f:
                for line in f:
                    if line.startswith(("VmRSS:", "VmSize:")):
                        k, v = line.split(":", 1)
                        info[k] = int(v.split()[0]) * 1024
            return info.get("VmRSS", 0), info.get("VmSize", 0)
        except OSError:
            return 0, 0

    k32, psapi = ctypes.windll.kernel32, ctypes.windll.psapi
    h = k32.OpenProcess(PROCESS_QUERY_INFORMATION | PROCESS_VM_READ, False, pid)
    if not h:
        return 0, 0
    try:
        mc = PMC()
        mc.cb = ctypes.sizeof(mc)
        if not psapi.GetProcessMemoryInfo(h, ctypes.byref(mc), mc.cb):
            return 0, 0
        return mc.WorkingSetSize, mc.PagefileUsage
    finally:
        k32.CloseHandle(h)


def proc_cpu(pid):
    """返回进程累计 CPU 秒数。"""
    if os.name != "nt":
        try:
            with open("/proc/%d/stat" % pid, encoding="utf-8") as f:
                parts = f.read().split()
            hz = os.sysconf("SC_CLK_TCK")
            return (int(parts[13]) + int(parts[14])) / hz
        except (OSError, ValueError, IndexError):
            return 0.0

    k32 = ctypes.windll.kernel32
    h = k32.OpenProcess(PROCESS_QUERY_INFORMATION, False, pid)
    if not h:
        return 0.0
    try:
        c, e, k, u = (wt.FILETIME() for _ in range(4))
        k32.GetProcessTimes(h, ctypes.byref(c), ctypes.byref(e), ctypes.byref(k), ctypes.byref(u))
        s = lambda ft: ((ft.dwHighDateTime << 32) | ft.dwLowDateTime) / 1e7
        return s(k) + s(u)
    finally:
        k32.CloseHandle(h)


def load_state(path):
    with gzip.open(path, "rb") as f:
        return json.loads(f.read().decode("utf-8"))


def save_state(path, doc):
    raw = json.dumps(doc, ensure_ascii=False).encode("utf-8")
    with gzip.open(path, "wb", compresslevel=1) as f:
        f.write(raw)


def make_point(ts, seq):
    return {
        "ts": ts,
        "cpuPct": round(random.uniform(1, 90), 2),
        "load1": round(random.uniform(0.1, 4), 2),
        "load5": round(random.uniform(0.1, 4), 2),
        "load15": round(random.uniform(0.1, 4), 2),
        "memUsed": 4 << 30, "memTotal": 8 << 30,
        "swapUsed": 0, "swapTotal": 0,
        "diskUsed": 40 << 30, "diskTotal": 100 << 30,
        "netRx": 1_000_000_000 + seq * 10_000, "netTx": 500_000_000 + seq * 5_000,
        "tcpConn": 120, "uptime": 86400 * 30, "procs": 180,
        "diskReadBytes": seq * 4096, "diskWriteBytes": seq * 2048,
        "mounts": [{"path": "/", "used": 40 << 30, "total": 100 << 30},
                   {"path": "/data", "used": 100 << 30, "total": 500 << 30}],
        "nics": [{"name": "eth0", "rx": 1_000_000_000 + seq * 10_000, "tx": 500_000_000 + seq * 5_000},
                 {"name": "eth1", "rx": 10_000 + seq, "tx": 20_000 + seq}],
    }


def build_state(binary, nodes, workdir, port):
    """先用空目录启动一次，拿到合法快照骨架，再按节点数填满原始环。"""
    shutil.rmtree(workdir, ignore_errors=True)
    os.makedirs(workdir)
    proc = subprocess.Popen([binary, "-addr", "127.0.0.1:%d" % port, "-data", workdir],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        tok_path = os.path.join(workdir, "admin.token")
        for _ in range(150):
            if os.path.exists(tok_path):
                break
            time.sleep(0.1)
        token = open(tok_path, encoding="utf-8").read().strip()
        # 先建一个节点，让快照里有可复制的节点骨架
        body = json.dumps({"name": "tpl", "provider": "x"}).encode()
        req = urllib.request.Request("http://127.0.0.1:%d/api/admin/nodes" % port, data=body, method="POST")
        req.add_header("Content-Type", "application/json")
        req.add_header("X-Admin-Token", token)
        urllib.request.build_opener(urllib.request.ProxyHandler({})).open(req, timeout=10).read()
        time.sleep(2.0)  # 等自动保存把节点写进快照
    finally:
        proc.terminate()
        proc.wait(timeout=10)

    path = os.path.join(workdir, "state.json")
    doc = load_state(path)
    template = next(iter(doc["nodes"].values()))

    now = int(time.time())
    nodes_map, series, tokens = {}, {}, {}
    for i in range(nodes):
        nid = "n_bench%06d" % i
        node = dict(template)
        node.update({"id": nid, "name": "bench-%04d" % i, "provider": "bench",
                     "region": "test", "group": "bench", "agentVersion": "bench-1.0.0"})
        token = "bench" + "".join(random.choice("0123456789abcdef") for _ in range(40))
        node["token"] = token
        nodes_map[nid] = node
        tokens[nid] = token
        series[nid] = [make_point(now - (RING - 1 - k) * 30, k) for k in range(RING)]

    doc["nodes"] = nodes_map
    doc["tokens"] = tokens
    doc["series"] = series
    doc["hosts"] = {nid: {"hostname": "bench-%04d" % i, "os": "Ubuntu 24.04", "kernel": "6.8.0",
                          "arch": "amd64"} for i, nid in enumerate(nodes_map)}
    save_state(path, doc)
    return path


def http(base, token, path, timeout=15):
    r = urllib.request.Request(base + path)
    r.add_header("X-Admin-Token", token)
    op = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    t0 = time.perf_counter()
    try:
        with op.open(r, timeout=timeout) as resp:
            return resp.status, resp.read(), (time.perf_counter() - t0) * 1000
    except urllib.error.HTTPError as e:
        return e.code, e.read(), (time.perf_counter() - t0) * 1000


def measure(binary, nodes, port, workdir, settle=75):
    state_path = build_state(binary, nodes, workdir, port)
    print("  填充后 state.json: %.1f MB" % (os.path.getsize(state_path) / 1048576), flush=True)

    proc = subprocess.Popen([binary, "-addr", "127.0.0.1:%d" % port, "-data", workdir],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        base = "http://127.0.0.1:%d" % port
        for _ in range(150):
            try:
                if http(base, "", "/healthz", timeout=2)[0] == 200:
                    break
            except Exception:
                pass
            time.sleep(0.2)
        token = open(os.path.join(workdir, "admin.token"), encoding="utf-8").read().strip()

        lat_nodes, lat_status = [], []
        cpu0 = proc_cpu(proc.pid)
        t_start = time.time()
        rss_mid = None
        while time.time() - t_start < settle:
            if rss_mid is None and time.time() - t_start >= 20:
                rss_mid = round(proc_mem(proc.pid)[0] / 1048576, 1)   # 加载后 20s 的 RSS，用来看是否持续增长
            for p, bucket in (("/api/nodes", lat_nodes), ("/api/status", lat_status)):
                st, _, ms = http(base, token, p)
                if st == 200:
                    bucket.append(ms)
            time.sleep(1.0)
        cpu1 = proc_cpu(proc.pid)
        elapsed = time.time() - t_start

        st, metrics, _ = http(base, token, "/metrics")
        save_total = save_dur = None
        phases = {}
        want = ("rollcall_save_copy_seconds", "rollcall_save_marshal_seconds",
                "rollcall_save_encode_seconds", "rollcall_save_sync_seconds",
                "rollcall_save_bytes")
        for line in metrics.decode("utf-8", "ignore").splitlines():
            if line.startswith("rollcall_save_total "):
                save_total = float(line.split()[1])
            if line.startswith("rollcall_save_duration_seconds "):
                save_dur = float(line.split()[1])
            for k in want:
                if line.startswith(k + " "):
                    phases[k.replace("rollcall_save_", "")] = round(float(line.split()[1]), 4)

        ws, priv = proc_mem(proc.pid)
        p = lambda v, q: round(sorted(v)[min(len(v) - 1, int(len(v) * q))], 1) if v else None
        return {
            "nodes": nodes,
            "state_mb": round(os.path.getsize(state_path) / 1048576, 1),
            "rss_mb": round(ws / 1048576, 1),
            "rss_mb_at_20s": rss_mid,
            "private_mb": round(priv / 1048576, 1),
            "cpu_cores_avg": round((cpu1 - cpu0) / elapsed, 3),
            "api_nodes_p50_ms": p(lat_nodes, 0.5), "api_nodes_p95_ms": p(lat_nodes, 0.95),
            "api_status_p50_ms": p(lat_status, 0.5), "api_status_p95_ms": p(lat_status, 0.95),
            "save_total": save_total,
            "save_duration_ms": round(save_dur * 1000, 1) if save_dur else None,
            "save_phases_ms": {k: round(v * 1000, 1) if k != "bytes" else int(v)
                               for k, v in phases.items()},
        }
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=15)
        except subprocess.TimeoutExpired:
            proc.kill()
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                pass  # 已经 kill，再等不到就放弃：不能让清理异常盖住真正的错误


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--binary", required=True)
    ap.add_argument("--nodes", type=int, nargs="+", default=[40, 100, 200])
    ap.add_argument("--port", type=int, default=9197)
    ap.add_argument("--settle", type=int, default=75)
    args = ap.parse_args()

    results = []
    for n in args.nodes:
        print("=== 稳态 %d 节点（每节点 %d 点）===" % (n, RING), flush=True)
        wd = os.path.join(os.environ.get("TEMP", "/tmp"), "rc-steady-%d" % n)
        r = measure(args.binary, n, args.port, wd, args.settle)
        results.append(r)
        print(json.dumps(r, ensure_ascii=False), flush=True)
        shutil.rmtree(wd, ignore_errors=True)

    print("\n| 节点数 | state.json | RSS | 私有内存 | CPU 核数 | /api/nodes p50/p95 | /api/status p50/p95 | 快照耗时 |")
    print("|---|---|---|---|---|---|---|---|")
    for r in results:
        print("| %d | %.1f MB | %.1f MB | %.1f MB | %.3f | %s/%s ms | %s/%s ms | %s ms |" % (
            r["nodes"], r["state_mb"], r["rss_mb"], r["private_mb"], r["cpu_cores_avg"],
            r["api_nodes_p50_ms"], r["api_nodes_p95_ms"],
            r["api_status_p50_ms"], r["api_status_p95_ms"], r["save_duration_ms"]))


if __name__ == "__main__":
    main()
