#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
"""Analysis helper of experiment E9 (tests/p0/e9-ha-proxy-writer.sh).

Usage: e9-analyze.py DIR

DIR holds, for one scenario:
  pa.smp, pb.smp   proxy status samples, one per 100 ms:
                   HH:MM:SS.mmm LABEL HTTP-CODE WRITER HAS_WRITER EPOCH PENDING
                   ("-" = empty; code 0 = proxy not reachable)
  rows.tsv         committed client rows: HH:MM:SS.mmm<TAB>NODE<TAB>LABEL
                   (the commit-time stamp of the serving database node)

Prints (a) the compressed state timeline of each proxy, (b) the spans without
a writer, (c) the number of samples in which both proxies reported different
writers at the same time (a split), and (d) the run-length sequence of the
serving node over time with the number of interleavings (a node that serves
again after another node served = overlap of two writers).
"""
import os
import sys


def secs(ts):
    h, m, s = ts.split(":")
    return int(h) * 3600 + int(m) * 60 + float(s)


def load_smp(path):
    out = []
    if not os.path.exists(path):
        return out
    for line in open(path):
        f = line.split()
        if len(f) < 7:
            continue
        out.append((secs(f[0]), f[0], f[2], f[3], f[4], f[5], f[6]))
    return out


def timeline(name, samples, limit=40):
    print(f"## {name}: state timeline ({len(samples)} samples)")
    if not samples:
        print("   no samples")
        return
    runs = []
    for t, ts, code, writer, has, epoch, pend in samples:
        key = (code, writer, has, epoch, pend)
        if runs and runs[-1][0] == key:
            runs[-1][2] = ts
            runs[-1][3] += 1
        else:
            runs.append([key, ts, ts, 1])
    for key, first, last, n in runs[:limit]:
        print(f"   {first}..{last} x{n:<4} http={key[0]} writer={key[1]} has_writer={key[2]} epoch={key[3]} pending={key[4]}")
    if len(runs) > limit:
        print(f"   ... {len(runs) - limit} more state changes")


def nowriter_spans(name, samples, min_len=0.0):
    spans = []
    start = None
    prev = None
    for t, ts, code, writer, has, epoch, pend in samples:
        bad = code != "200" or writer == "-" or has != "true"
        if bad and start is None:
            start = (t, ts)
        if not bad and start is not None:
            spans.append((start[1], prev[1], t - start[0]))
            start = None
        prev = (t, ts)
    if start is not None and prev is not None:
        spans.append((start[1], prev[1], prev[0] - start[0]))
    print(f"## {name}: spans without a writer (status unreachable, no writer or freeze)")
    spans = [s for s in spans if s[2] >= min_len]
    if not spans:
        print("   none")
    for a, b, d in spans:
        print(f"   {a}..{b} ({d:.2f} s)")
    if spans:
        print(f"   longest {max(s[2] for s in spans):.2f} s, total {sum(s[2] for s in spans):.2f} s")


def splits(pa, pb):
    print("## writer agreement of the two proxies (100 ms buckets, latest sample of the other within 150 ms)")
    if not pa or not pb:
        print("   not enough samples")
        return
    j = 0
    compared = split = 0
    first = []
    for t, ts, code, writer, has, epoch, pend in pa:
        while j + 1 < len(pb) and pb[j + 1][0] <= t:
            j += 1
        o = pb[j]
        if abs(o[0] - t) > 0.15:
            continue
        if writer == "-" or o[3] == "-" or code != "200" or o[2] != "200":
            continue
        compared += 1
        if writer != o[3]:
            split += 1
            if len(first) < 5:
                first.append(f"{ts} a={writer}/e{epoch} b={o[3]}/e{o[5]}")
    print(f"   compared {compared} aligned samples with a writer on both: {split} with different writers")
    for f in first:
        print(f"   split: {f}")


def vip(da):
    print("## VIP holders (100 ms samples of 'holds the VIP' on both proxy VMs)")
    files = {n: os.path.join(da, f"vip-{n}.smp") for n in ("pa", "pb")}
    data = {}
    for n, f in files.items():
        data[n] = []
        if os.path.exists(f):
            for line in open(f):
                p = line.split()
                if len(p) == 2:
                    data[n].append((secs(p[0]), p[0], p[1]))
    if not data["pa"] or not data["pb"]:
        print("   not enough samples")
        return
    j = 0
    both = none = n_all = 0
    first = []
    for t, ts, h in data["pa"]:
        while j + 1 < len(data["pb"]) and data["pb"][j + 1][0] <= t:
            j += 1
        o = data["pb"][j]
        if abs(o[0] - t) > 0.15:
            continue
        n_all += 1
        if h == "1" and o[2] == "1":
            both += 1
            if len(first) < 5:
                first.append(ts)
        if h == "0" and o[2] == "0":
            none += 1
    print(f"   {n_all} aligned samples: both VMs hold the VIP in {both}, neither holds it in {none}")
    for f in first:
        print(f"   both hold the VIP at {f}")


def rows(path):
    print("## serving node of the committed client writes, in commit-time order")
    if not os.path.exists(path):
        print("   no rows file")
        return
    data = []
    for line in open(path):
        f = line.rstrip("\n").split("\t")
        if len(f) >= 2 and f[0] and f[1]:
            data.append((secs(f[0]), f[0], f[1]))
    data.sort()
    runs = []
    for t, ts, node in data:
        if runs and runs[-1][0] == node:
            runs[-1][2] = ts
            runs[-1][3] = t
            runs[-1][4] += 1
        else:
            runs.append([node, ts, ts, t, 1, t])
    distinct = len({r[0] for r in runs})
    for node, first, last, _, n, _ in runs[:30]:
        print(f"   {node} {first[:12]}..{last[:12]} x{n}")
    if len(runs) > 30:
        print(f"   ... {len(runs) - 30} more runs")
    print(f"   {len(data)} rows, {len(runs)} runs, {distinct} distinct nodes, interleavings (runs - distinct nodes) = {len(runs) - distinct}")
    # smallest gap between the last write of a node and the first write of the next node
    gaps = []
    for a, b in zip(runs, runs[1:]):
        gaps.append((b[5] - a[3], a[0], b[0], a[2], b[1]))
    for g, an, bn, alast, bfirst in gaps[:10]:
        print(f"   hand-over {an} -> {bn}: last {an} write {alast[:12]}, first {bn} write {bfirst[:12]} (gap {g * 1000:.0f} ms)")


def main():
    if len(sys.argv) != 2:
        print(__doc__)
        return 2
    d = sys.argv[1]
    pa = load_smp(os.path.join(d, "pa.smp"))
    pb = load_smp(os.path.join(d, "pb.smp"))
    timeline("proxy-a", pa)
    timeline("proxy-b", pb)
    nowriter_spans("proxy-a", pa)
    nowriter_spans("proxy-b", pb)
    splits(pa, pb)
    vip(d)
    rows(os.path.join(d, "rows.tsv"))
    return 0


if __name__ == "__main__":
    sys.exit(main())
