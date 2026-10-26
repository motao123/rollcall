#!/usr/bin/env python3
"""按新的时区偏移平移流量周期的起点，保住本期已累计的用量。

为什么需要它
------------
流量周期的起点是「本月 resetDay 00:00（服务端本地时区）」，存的是 Unix 秒。
一旦服务端时区变了（例如从 UTC 换到 Asia/Shanghai），同一个日历周期的 00:00
对应的 Unix 秒会整体移动；而写入路径把「Start 与当前计算值不同」当作**新周期**，
于是本期用量会被清零。

本脚本把 state.json 里每个周期的 `start` 平移同样的偏移量，使它与新时区下
`trafficCycleStart` 的计算结果一致 —— 用量、基线（LastRx/LastTx）都不动。

用法
----
    systemctl stop rollcall-server
    cp -p /opt/rollcall/data/state.json /root/state.json.bak-$(date +%s)
    python3 repin-traffic-tz.py /opt/rollcall/data/state.json -28800
    chown rollcall:rollcall /opt/rollcall/data/state.json
    systemctl start rollcall-server

`-28800` = -8 小时（UTC → UTC+8）。若从 UTC+8 改回 UTC，用 `+28800`。

安全边界
--------
- 必须在**服务停止**时运行：运行中的服务会在下次快照里覆盖你的修改。
- 只改 `traffic[*].start`，其余字段原样保留；写回用 gzip（state.json 是压缩的）。
- 若判定逻辑与预期不符（例如某个节点用的是别的 resetDay），最坏结果是服务端
  把它当成新周期重新起算 —— 与你手工切换时区的默认行为一致，不会产生错误数据。
"""

import argparse
import gzip
import json
import os
import shutil
import sys
from datetime import datetime, timedelta, timezone


def load(path):
    with gzip.open(path, "rt", encoding="utf-8") as f:
        return json.load(f)


def save(path, data):
    tmp = path + ".new"
    with gzip.open(tmp, "wt", encoding="utf-8") as f:
        json.dump(data, f, ensure_ascii=False, separators=(",", ":"))
    # 保持原文件权限
    try:
        shutil.copystat(path, tmp)
    except OSError:
        pass
    shutil.move(tmp, path)


def cycle_start(now, reset_day, tz):
    """与 store.trafficCycleStart 同构：本月 resetDay 00:00（本地），未到则上个月。"""
    if not 1 <= reset_day <= 28:
        reset_day = 1
    start = now.replace(day=reset_day, hour=0, minute=0, second=0, microsecond=0)
    if now < start:
        prev = (start.replace(day=1) - timedelta(days=1))
        start = prev.replace(day=reset_day, hour=0, minute=0, second=0, microsecond=0)
    return int(start.timestamp())


def main():
    ap = argparse.ArgumentParser(description="平移流量周期起点（切换时区时保用量）")
    ap.add_argument("state", help="state.json 路径（gzip）")
    ap.add_argument("offset", type=int, help="偏移秒数，UTC→UTC+8 为 -28800")
    ap.add_argument("--tz", type=float, default=8.0,
                    help="切换后的时区小时数（默认 8，仅用于校验输出）")
    ap.add_argument("--dry-run", action="store_true", help="只打印将要做的改动")
    args = ap.parse_args()

    if not os.path.exists(args.state):
        sys.exit("找不到 %s" % args.state)

    data = load(args.state)
    traffic = data.get("traffic") or {}
    nodes = data.get("nodes") or {}
    if not traffic:
        print("没有流量周期记录，无需处理")
        return

    tz = timezone(timedelta(hours=args.tz))
    now = datetime.now(tz)
    total_before = 0
    moved = 0
    drift = 0
    for nid, cyc in traffic.items():
        total_before += cyc.get("usedRx", 0) + cyc.get("usedTx", 0)
        start = cyc.get("start") or 0
        if not start:
            continue
        want = cycle_start(now, (nodes.get(nid) or {}).get("trafficResetDay") or 1, tz)
        # 只处理"确实是同一个日历周期、只差一个时区偏移"的记录；
        # 其他情况（真的跨周期）不碰，交给服务端自己滚。
        if start + args.offset != want:
            print("跳过 %s：平移后仍与当前周期不符（start=%d want=%d）"
                  % (nid[:8], start + args.offset, want))
            drift += 1
            continue
        cyc["start"] = start + args.offset
        moved += 1

    print("周期 %d 条，平移 %d 条，跳过 %d 条；用量合计 %.3f GB"
          % (len(traffic), moved, drift, total_before / 1073741824))

    if args.dry_run:
        print("（dry-run，未写入）")
        return
    save(args.state, data)
    print("已写入 %s" % args.state)


if __name__ == "__main__":
    main()
