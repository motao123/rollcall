#!/usr/bin/env python3
"""Rollcall 容量基线：按 N 个节点并发上报，测内存、CPU、接口延迟与快照耗时。

用法：
    python bench_capacity.py --binary C:\\tmp\\rc-bench.exe --nodes 40 100 200 --duration 60

每个场景用独立数据目录与全新进程，避免上一轮的历史/内存影响结果。
"""
import argparse
import ctypes
import ctypes.wintypes as wt
import json
import os
import random
import shutil
import statistics
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request

PROCESS_QUERY_INFORMATION = 0x0400
PROCESS_VM_READ = 0x0010


class PROCESS_MEMORY_COUNTERS(ctypes.Structure):
    _fields_ = [
        ("cb", wt.DWORD),
        ("PageFaultCount", wt.DWORD),
        ("PeakWorkingSetSize", ctypes.c_size_t),
        ("WorkingSetSize", ctypes.c_size_t),
        ("QuotaPeakPagedPoolUsage", ctypes.c_size_t),
        ("QuotaPagedPoolUsage", ctypes.c_size_t),
        ("QuotaPeakNonPagedPoolUsage", ctypes.c_size_t),
        ("QuotaNonPagedPoolUsage", ctypes.c_size_t),
        ("PagefileUsage", ctypes.c_size_t),
        ("PeakPagefileUsage", ctypes.c_size_t),
    ]


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
    return proc_mem_nt(pid)


def proc_mem_nt(pid):
    k32 = ctypes.windll.kernel32
    psapi = ctypes.windll.psapi
    h = k32.OpenProcess(PROCESS_QUERY_INFORMATION | PROCESS_VM_READ, False, pid)
    if not h:
        return 0, 0
    try:
        mc = PROCESS_MEMORY_COUNTERS()
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
    return proc_cpu_nt(pid)


def proc_cpu_nt(pid):
    k32 = ctypes.windll.kernel32
    h = k32.OpenProcess(PROCESS_QUERY_INFORMATION, False, pid)
    if not h:
        return 0.0
    try:
        creation, exit_, kernel, user = (wt.FILETIME() for _ in range(4))
        if not k32.GetProcessTimes(h, ctypes.byref(creation), ctypes.byref(exit_),
                                   ctypes.byref(kernel), ctypes.byref(user)):
            return 0.0
        to_s = lambda ft: (ft.dwHighDateTime << 32 | ft.dwLowDateTime) / 1e7
        return to_s(kernel) + to_s(user)
    finally:
        k32.CloseHandle(h)


class Client:
    def __init__(self, base, token):
        self.base = base
        self.token = token
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def req(self, method, path, body=None, admin=True, node_token=None, timeout=10):
        data = json.dumps(body).encode() if body is not None else None
        r = urllib.request.Request(self.base + path, data=data, method=method)
        r.add_header("Content-Type", "application/json")
        if admin:
            r.add_header("X-Admin-Token", self.token)
        if node_token:
            r.add_header("X-Node-Token", node_token)
            r.add_header("X-Agent-Version", "bench-1.0.0")
        t0 = time.perf_counter()
        try:
            with self.opener.open(r, timeout=timeout) as resp:
                payload = resp.read()
                return resp.status, payload, (time.perf_counter() - t0) * 1000
        except urllib.error.HTTPError as e:
            return e.code, e.read(), (time.perf_counter() - t0) * 1000


def wait_ready(client, deadline=30):
    end = time.time() + deadline
    while time.time() < end:
        try:
            st, _, _ = client.req("GET", "/healthz", admin=False, timeout=2)
            if st == 200:
                return True
        except Exception:
            pass
        time.sleep(0.2)
    return False


def report_once(client, node, seq):
    """构造一份贴近真实 Agent 的上报。"""
    m = {
        "ts": int(time.time()),
        "cpuPct": round(random.uniform(1, 85), 2),
        "load1": round(random.uniform(0.1, 4), 2),
        "load5": round(random.uniform(0.1, 4), 2),
        "load15": round(random.uniform(0.1, 4), 2),
        "memUsed": 4 << 30,
        "memTotal": 8 << 30,
        "swapUsed": 0,
        "swapTotal": 0,
        "diskUsed": 40 << 30,
        "diskTotal": 100 << 30,
        "netRx": 1_000_000_000 + seq * 10_000,
        "netTx": 500_000_000 + seq * 5_000,
        "tcpConn": random.randint(10, 300),
        "uptime": 86400 * 30,
        "procs": 180,
        "diskReadBytes": seq * 4096,
        "diskWriteBytes": seq * 2048,
        "mounts": [
            {"path": "/", "used": 40 << 30, "total": 100 << 30},
            {"path": "/data", "used": 100 << 30, "total": 500 << 30},
        ],
        "nics": [
            {"name": "eth0", "rx": 1_000_000_000 + seq * 10_000, "tx": 500_000_000 + seq * 5_000},
            {"name": "eth1", "rx": 10_000 + seq, "tx": 20_000 + seq},
        ],
    }
    host = {"hostname": node["name"], "os": "Ubuntu 24.04", "kernel": "6.8.0", "arch": "amd64"}
    body = {"nodeId": node["id"], "host": host, "metrics": m}
    return client.req("POST", "/api/report", body, admin=False,
                      node_token=node["token"], timeout=10)


def run_scenario(binary, nodes, duration, interval, port):
    data_dir = os.path.join(os.environ.get("TEMP", "/tmp"), "rc-bench-%d" % nodes)
    shutil.rmtree(data_dir, ignore_errors=True)
    os.makedirs(data_dir, exist_ok=True)

    proc = subprocess.Popen([binary, "-addr", "127.0.0.1:%d" % port, "-data", data_dir],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        base = "http://127.0.0.1:%d" % port
        tok_path = os.path.join(data_dir, "admin.token")
        for _ in range(60):
            if os.path.exists(tok_path):
                break
            time.sleep(0.1)
        with open(tok_path, encoding="utf-8") as f:
            token = f.read().strip()
        client = Client(base, token)
        if not wait_ready(client):
            raise SystemExit("服务端未就绪")

        # 建节点
        created = []
        for i in range(nodes):
            st, body, _ = client.req("POST", "/api/admin/nodes", {
                "name": "bench-%03d" % i, "provider": "bench",
                "region": "test", "group": "bench",
            })
            if st != 201:
                raise SystemExit("建节点失败: %d %s" % (st, body[:200]))
            created.append(json.loads(body))

        # 预热：先上报一轮，让环缓冲分配完成
        for n in created:
            report_once(client, n, 0)
        time.sleep(2)

        stop = threading.Event()
        seq_lock = threading.Lock()
        seq = {"v": 0}
        errors = {"report": 0, "latency": 0}
        lat = {"nodes": [], "status": []}

        def reporter(node):
            while not stop.is_set():
                with seq_lock:
                    seq["v"] += 1
                    s = seq["v"]
                try:
                    st, _, _ = report_once(client, node, s)
                    if st != 200:
                        errors["report"] += 1
                except Exception:
                    errors["report"] += 1
                stop.wait(interval)

        def sampler():
            while not stop.is_set():
                for path, key in (("/api/nodes", "nodes"), ("/api/status", "status")):
                    try:
                        admin = path != "/api/status"
                        st, _, ms = client.req("GET", path, admin=admin, timeout=10)
                        if st == 200:
                            lat[key].append(ms)
                        else:
                            errors["latency"] += 1
                    except Exception:
                        errors["latency"] += 1
                stop.wait(0.5)

        threads = [threading.Thread(target=reporter, args=(n,), daemon=True) for n in created]
        for t in threads:
            t.start()
        smp = threading.Thread(target=sampler, daemon=True)
        smp.start()

        cpu0 = proc_cpu(proc.pid)
        time.sleep(duration)
        stop.set()
        for t in threads:
            t.join(timeout=12)
        smp.join(timeout=5)
        cpu1 = proc_cpu(proc.pid)

        ws, priv = proc_mem(proc.pid)
        st, metrics, _ = client.req("GET", "/metrics", admin=True)
        save_total = save_dur = None
        if st == 200:
            for line in metrics.decode("utf-8", "ignore").splitlines():
                if line.startswith("rollcall_save_total "):
                    save_total = float(line.split()[1])
                if line.startswith("rollcall_save_duration_seconds "):
                    save_dur = float(line.split()[1])

        def pct(vals, p):
            if not vals:
                return None
            vals = sorted(vals)
            return round(vals[min(len(vals) - 1, int(len(vals) * p))], 1)

        return {
            "nodes": nodes,
            "duration_s": duration,
            "report_interval_s": interval,
            "reports": seq["v"],
            "report_errors": errors["report"],
            "rss_mb": round(ws / 1048576, 1),
            "private_mb": round(priv / 1048576, 1),
            "cpu_cores_avg": round((cpu1 - cpu0) / duration, 2),
            "api_nodes_p50_ms": pct(lat["nodes"], 0.5),
            "api_nodes_p95_ms": pct(lat["nodes"], 0.95),
            "api_status_p50_ms": pct(lat["status"], 0.5),
            "api_status_p95_ms": pct(lat["status"], 0.95),
            "api_samples": len(lat["nodes"]),
            "save_total": save_total,
            "save_duration_ms": round(save_dur * 1000, 1) if save_dur else None,
        }
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
        shutil.rmtree(data_dir, ignore_errors=True)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--binary", required=True)
    ap.add_argument("--nodes", type=int, nargs="+", default=[40, 100, 200])
    ap.add_argument("--duration", type=int, default=60)
    ap.add_argument("--interval", type=float, default=2.0)
    ap.add_argument("--port", type=int, default=9195)
    args = ap.parse_args()

    results = []
    for n in args.nodes:
        print("=== 场景 %d 节点 ===" % n, flush=True)
        r = run_scenario(args.binary, n, args.duration, args.interval, args.port)
        results.append(r)
        print(json.dumps(r, ensure_ascii=False), flush=True)

    print("\n| 节点数 | RSS | 私有内存 | CPU 核数 | /api/nodes p50/p95 | /api/status p50/p95 | 快照耗时 |")
    print("|---|---|---|---|---|---|---|")
    for r in results:
        print("| %d | %.1f MB | %.1f MB | %.2f | %s/%s ms | %s/%s ms | %s ms |" % (
            r["nodes"], r["rss_mb"], r["private_mb"], r["cpu_cores_avg"],
            r["api_nodes_p50_ms"], r["api_nodes_p95_ms"],
            r["api_status_p50_ms"], r["api_status_p95_ms"], r["save_duration_ms"]))


if __name__ == "__main__":
    main()
