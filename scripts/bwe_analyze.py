#!/usr/bin/env python3
"""BWE vs link, offline, at 1 s resolution (Prometheus scrapes are 5 s).

Joins the mediasoup bwe_log JSONL (SFU clock) with the generators' links.jsonl
(dip schedule, generator clock) and prints, per network profile:
  - estimate / sent bitrate over the downlink limit (capped: always; dippy: during dips),
  - RTX and probe share of all bytes sent,
and per dip of a dippy viewer:
  - est down:   dip start -> estimate at or below 1.1 x the dip rate
  - layer down: dip start -> a lower spatial layer forwarded
  - layer up:   dip end   -> the layer from before the dip again
  - est up:     dip end   -> estimate back to 90% of its pre-dip value
  - sent > link: share of the dip's 1 s samples where the SFU sent more than the dip rate
  - RTP / RTX sent in dips and between them (what fills the link during a dip)
It also draws one SVG per sample viewer (estimate, sent, limit; forwarded layer).

  scripts/bwe_analyze.py --sfu bwe-media.jsonl --run results/x/run-g1 --run results/x/run-g2 \\
      --out results/x/bwe-media [--offset-ms 0] [--plot-peers 1]

--offset-ms is SFU clock minus generator clock (chronyc / ntpdate -q); the dip
schedule is shifted by it. The run window is [first step start, last step end]
from the runs' steps.csv. Standard library only.
"""
import argparse, csv, json, math, os, sys
from bisect import bisect_right
from datetime import datetime, timezone


def ms(iso):
    return int(datetime.fromisoformat(iso.replace('Z', '+00:00')).timestamp() * 1000)


def quantile(xs, q):
    if not xs:
        return math.nan
    s = sorted(xs)
    pos = q * (len(s) - 1)
    lo = int(pos)
    if lo + 1 >= len(s):
        return s[-1]
    return s[lo] + (pos - lo) * (s[lo + 1] - s[lo])


def fmt(v, nd=2):
    if v is None or (isinstance(v, float) and math.isnan(v)):
        return '–'
    return f'{v:.{nd}f}'


def pct(n, d):
    return '–' if d == 0 else f'{100 * n / d:.1f}%'


class Link:
    def __init__(self, d, offset):
        self.d = d
        self.profile = d['profile']
        self.kbps = d.get('kbps', 0)
        self.dip_kbps = d.get('dip_kbps', 0)
        self.dip_for = d.get('dip_for_ms', 0)
        self.dip_every = d.get('dip_every_ms', 0)
        # first dip start on the SFU clock
        self.t0 = d.get('start_ms', 0) + d.get('phase_ms', 0) + offset

    def dips(self, a, b):
        """Dip windows [s, e) overlapping [a, b], SFU clock."""
        if not (self.dip_every > 0 and self.dip_for > 0 and self.dip_kbps > 0):
            return []
        k = max(0, (a - self.t0) // self.dip_every)
        out = []
        while True:
            s = self.t0 + k * self.dip_every
            if s > b:
                return out
            if s + self.dip_for >= a:
                out.append((s, s + self.dip_for))
            k += 1

    def limit(self, t):
        """Downlink limit in bps at SFU time t, 0 = none."""
        if self.dip_every > 0 and self.dip_for > 0 and self.dip_kbps > 0 and t >= self.t0:
            if (t - self.t0) % self.dip_every < self.dip_for:
                return self.dip_kbps * 1000
        return self.kbps * 1000


class Peer:
    def __init__(self):
        self.bwe_t, self.bwe_avail = [], []          # trace events
        self.st = []                                  # stats samples (dicts)

    def est(self, t):
        """Latest estimate at t: last bwe trace event, else the stats sample's value."""
        i = bisect_right(self.bwe_t, t)
        return self.bwe_avail[i - 1] if i else None


def load(args):
    links, win = {}, [None, None]
    for run in args.run:
        with open(os.path.join(run, 'links.jsonl')) as f:
            for line in f:
                d = json.loads(line)
                if not d.get('publish'):
                    links[d['peer']] = Link(d, args.offset_ms)
        with open(os.path.join(run, 'steps.csv')) as f:
            for r in csv.DictReader(f):
                a, b = ms(r['start']), ms(r['end'])
                win[0] = a if win[0] is None else min(win[0], a)
                win[1] = b if win[1] is None else max(win[1], b)
    peers = {}
    with open(args.sfu) as f:
        for line in f:
            try:
                d = json.loads(line)
            except ValueError:
                continue
            t, p = d.get('t', 0), d.get('peer')
            if p not in links or not (win[0] <= t <= win[1]):
                continue
            pe = peers.setdefault(p, Peer())
            if d['ev'] == 'bwe':
                pe.bwe_t.append(t)
                pe.bwe_avail.append(d['avail'])
            elif d['ev'] == 'stats':
                pe.st.append(d)
    for pe in peers.values():
        order = sorted(range(len(pe.bwe_t)), key=pe.bwe_t.__getitem__)
        pe.bwe_t = [pe.bwe_t[i] for i in order]
        pe.bwe_avail = [pe.bwe_avail[i] for i in order]
        pe.st.sort(key=lambda d: d['t'])
    return links, peers, win


def est_at(pe, s):
    e = pe.est(s['t'])
    return e if e is not None else s.get('avail')


def per_profile(links, peers):
    prof = {}
    for name, pe in peers.items():
        L = links[name]
        r = prof.setdefault(L.profile, dict(peers=0, est=[], er=[], sr=[], send=0, rtx=0, probe=0, lay=[]))
        r['peers'] += 1
        for s in pe.st:
            e = est_at(pe, s)
            if e is not None:
                r['est'].append(e)
            r['send'] += s['send']
            r['rtx'] += s['rtx']
            r['probe'] += s['probe']
            r['lay'].append(s['layer'])
            lim = L.limit(s['t'] - 500)  # sample covers [t-1s, t]
            if lim > 0:
                if e is not None:
                    r['er'].append(e / lim)
                r['sr'].append(s['send'] / lim)
    return prof


def reactions(links, peers):
    rows = []
    for name, pe in sorted(peers.items()):
        L = links[name]
        if not pe.st:
            continue
        first, last = pe.st[0]['t'] + 10_000, pe.st[-1]['t']
        ts = [s['t'] for s in pe.st]
        for ds, de in L.dips(first, last):
            if ds < first or de + 2_000 > last:
                continue
            i = bisect_right(ts, ds)
            if i == 0:
                continue
            pre_layer = pe.st[i - 1]['layer']
            pre_est = pe.est(ds) or pe.st[i - 1].get('avail')
            dip_bps = L.dip_kbps * 1000
            nxt = ds + L.dip_every
            r = dict(peer=name, ds=ds, pre_layer=pre_layer)
            # estimate down: trace events and stats samples within the dip; the
            # estimate settles a few % above the shaper rate, hence the 10% slack
            near = 1.1 * dip_bps
            cand = [t for t, v in zip(pe.bwe_t, pe.bwe_avail) if ds <= t <= de and v <= near]
            cand += [s['t'] for s in pe.st if ds <= s['t'] <= de and (s.get('avail') or math.inf) <= near]
            r['est_down'] = (min(cand) - ds) / 1000 if cand else None
            if pre_layer >= 0:
                lo = [s['t'] for s in pe.st if ds <= s['t'] <= de and 0 <= s['layer'] < pre_layer]
                r['layer_down'] = (min(lo) - ds) / 1000 if lo else None
                up = [s['t'] for s in pe.st if de <= s['t'] < nxt and s['layer'] >= pre_layer]
                if up or nxt <= last:  # no recovery yet and the data ends before the next dip: unknown, not missed
                    r['layer_up'] = (min(up) - de) / 1000 if up else None
            if pre_est:
                eu = [t for t, v in zip(pe.bwe_t, pe.bwe_avail) if de <= t < nxt and v >= 0.9 * pre_est]
                if eu or nxt <= last:
                    r['est_up'] = (min(eu) - de) / 1000 if eu else None
            in_dip = [s for s in pe.st if ds + 1000 <= s['t'] <= de]
            r['over'] = sum(1 for s in in_dip if s['send'] > dip_bps)
            r['n'] = len(in_dip)
            # bytes in the dip (after its first second) and between dips (from 5 s after it ends)
            between = [s for s in pe.st if de + 5000 <= s['t'] < min(nxt, last)]
            for tag, ss in (('dip', in_dip), ('free', between)):
                r[tag] = dict(n=len(ss), **{k: sum(s.get(k) or 0 for s in ss) for k in ('rtp', 'rtx', 'probe', 'send')})
            r['dip_bps'] = dip_bps
            rows.append(r)
    return rows


# ---- SVG ------------------------------------------------------------------------

STYLE = """<style>
  svg { --surface:#fcfcfb; --ink:#0b0b0b; --ink2:#52514e; --grid:#e4e3df; --shade:#efeeea;
        --s1:#2a78d6; --s2:#eb6834; --s3:#1baf7a; font-family: system-ui, sans-serif; }
  @media (prefers-color-scheme: dark) {
    svg { --surface:#1a1a19; --ink:#f0efec; --ink2:#c3c2b7; --grid:#33322f; --shade:#272724;
          --s1:#3987e5; --s2:#d95926; --s3:#199e70; }
  }
  .bg { fill: var(--surface); } .grid { stroke: var(--grid); stroke-width: 1; }
  .t { fill: var(--ink2); font-size: 12px; } .h { fill: var(--ink); font-size: 14px; font-weight: 600; }
  .dip { fill: var(--shade); }
  .est { stroke: var(--s1); } .sent { stroke: var(--s2); } .lay { stroke: var(--s3); }
  .lim { stroke: var(--ink); stroke-dasharray: 6 4; }
  .ln { fill: none; stroke-width: 2; stroke-linejoin: round; }
  .sw-est { fill: var(--s1); } .sw-sent { fill: var(--s2); }
</style>"""


def plot(path, title, L, pe, t0, t1):
    W, X0, X1 = 960, 56, 940
    T0, T1 = 40, 300          # bitrate panel
    B0, B1 = 340, 420         # layer panel
    H = 450
    span = max(1, t1 - t0)
    x = lambda t: X0 + (t - t0) / span * (X1 - X0)
    est = [(s['t'], est_at(pe, s)) for s in pe.st if est_at(pe, s) is not None]
    sent = [(s['t'], s['send']) for s in pe.st]
    ymax = max([v for _, v in est] + [v for _, v in sent] + [L.kbps * 1000, 1e6])
    ymax = min(ymax, 4e6) if ymax > 4e6 else ymax          # keep a 10 Mbps start from flattening the rest
    ystep = 0.5e6 if ymax <= 3e6 else 1e6
    ymax = math.ceil(ymax / ystep) * ystep
    y = lambda v: T1 - min(v, ymax) / ymax * (T1 - T0)
    lmax = max([s['layer'] for s in pe.st] + [2])
    yl = lambda v: B1 - (max(v, -1) + 1) / (lmax + 1) * (B1 - B0)
    o = [f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {H}" width="{W}" height="{H}">', STYLE,
         f'<rect class="bg" width="{W}" height="{H}"/>',
         f'<text class="h" x="{X0}" y="22">{title}</text>']
    for ds, de in L.dips(t0, t1):
        a, b = x(max(ds, t0)), x(min(de, t1))
        o.append(f'<rect class="dip" x="{a:.1f}" y="{T0}" width="{b - a:.1f}" height="{B1 - T0}"/>')
    v = 0.0
    while v <= ymax + 1:
        o.append(f'<line class="grid" x1="{X0}" x2="{X1}" y1="{y(v):.1f}" y2="{y(v):.1f}"/>')
        o.append(f'<text class="t" x="{X0 - 8}" y="{y(v) + 4:.1f}" text-anchor="end">{v / 1e6:g}</text>')
        v += ystep
    o.append(f'<text class="t" x="{X0 - 8}" y="{T0 - 10}" text-anchor="end">Mbps</text>')
    for l in range(-1, lmax + 1):
        o.append(f'<line class="grid" x1="{X0}" x2="{X1}" y1="{yl(l):.1f}" y2="{yl(l):.1f}"/>')
        o.append(f'<text class="t" x="{X0 - 8}" y="{yl(l) + 4:.1f}" text-anchor="end">{"off" if l < 0 else l}</text>')
    o.append(f'<text class="t" x="{X0}" y="{B0 - 10}">Spatial layer forwarded (lowest of the viewer\'s video consumers)</text>')
    step = 60_000 if span > 180_000 else 10_000
    k = 0
    while k <= span:
        o.append(f'<text class="t" x="{x(t0 + k):.1f}" y="{B1 + 18}" text-anchor="middle">{k // 1000}s</text>')
        k += step

    def path_d(pts, stepwise=False):
        d, prev = [], None
        for t, val in pts:
            px, py = x(t), val
            if prev is not None and stepwise:
                d.append(f'L{px:.1f},{prev:.1f}')
            d.append(f'{"M" if not d else "L"}{px:.1f},{py:.1f}')
            prev = py
        return ''.join(d)

    # link limit, as a step function sampled every 250 ms
    lim = []
    t = t0
    while t <= t1:
        lim.append((t, L.limit(t)))
        t += 250
    segs, cur = [], []  # one path per stretch with a limit (dippy: one per dip)
    for t, val in lim:
        if val > 0:
            cur.append((t, y(val)))
        else:
            if cur:
                segs.append(cur)
            cur = []
    if cur:
        segs.append(cur)
    for sgm in segs:
        o.append(f'<path class="ln lim" d="{path_d(sgm, True)}"/>')
    o.append(f'<path class="ln sent" d="{path_d([(t, y(v)) for t, v in sent])}"/>')
    o.append(f'<path class="ln est" d="{path_d([(t, y(v)) for t, v in est], True)}"/>')
    o.append(f'<path class="ln lay" d="{path_d([(s["t"], yl(s["layer"])) for s in pe.st], True)}"/>')
    # legend
    lx = X1 - 470
    o.append(f'<rect class="sw-est" x="{lx}" y="12" width="14" height="4" rx="2"/><text class="t" x="{lx + 20}" y="18">SFU estimate</text>')
    o.append(f'<rect class="sw-sent" x="{lx + 120}" y="12" width="14" height="4" rx="2"/><text class="t" x="{lx + 140}" y="18">sent (1 s)</text>')
    o.append(f'<line class="lim" x1="{lx + 225}" x2="{lx + 245}" y1="14" y2="14" stroke-width="2"/><text class="t" x="{lx + 251}" y="18">link limit</text>')
    o.append(f'<rect class="dip" x="{lx + 330}" y="8" width="14" height="12" stroke="var(--ink2)" stroke-width="0.5"/><text class="t" x="{lx + 350}" y="18">dip</text>')
    o.append('</svg>')
    with open(path, 'w') as f:
        f.write('\n'.join(o))


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--sfu', required=True, help='mediasoup bwe_log JSONL')
    ap.add_argument('--run', action='append', required=True, help='run dir with links.jsonl and steps.csv (repeat per generator)')
    ap.add_argument('--out', required=True, help='output dir for analysis.md and SVGs')
    ap.add_argument('--offset-ms', type=int, default=0, help='SFU clock minus generator clock, ms')
    ap.add_argument('--plot-peers', type=int, default=1, help='sample viewers to plot per profile')
    args = ap.parse_args()
    links, peers, win = load(args)
    if not peers:
        sys.exit('no SFU samples for the runs\' viewers in the run window (wrong JSONL, or clock/peer mismatch)')
    os.makedirs(args.out, exist_ok=True)
    md = [f'# BWE vs link, 1 s resolution\n',
          f'Window {datetime.fromtimestamp(win[0] / 1000, timezone.utc):%Y-%m-%d %H:%M:%S} – {datetime.fromtimestamp(win[1] / 1000, timezone.utc):%H:%M:%S} UTC, '
          f'{len(peers)} viewers with SFU samples, clock offset {args.offset_ms} ms.\n',
          '\n## Per profile\n',
          'est = SFU send estimate, sent = all bytes sent to the viewer per 1 s, limit = generator downlink limit '
          '(capped: always; dippy: only during dips). RTX/probe share of all bytes sent. Top layer = samples on the '
          'highest layer seen, of samples with video forwarded.\n',
          '| profile | viewers | est p5 Mbps | est p50 Mbps | est÷limit p5 | p50 | p95 | est > limit | sent÷limit p50 | p95 | sent > limit | RTX share | probe share | top layer |',
          '|---|---|---|---|---|---|---|---|---|---|---|---|---|---|']
    prof = per_profile(links, peers)
    top = max((l for r in prof.values() for l in r['lay']), default=-1)
    for p in sorted(prof):
        r = prof[p]
        on = [l for l in r['lay'] if l >= 0]
        md.append(f"| {p} | {r['peers']} | {fmt(quantile(r['est'], .05) / 1e6)} | {fmt(quantile(r['est'], .5) / 1e6)} | "
                  f"{fmt(quantile(r['er'], .05))} | {fmt(quantile(r['er'], .5))} | {fmt(quantile(r['er'], .95))} | "
                  f"{pct(sum(1 for v in r['er'] if v > 1), len(r['er']))} | "
                  f"{fmt(quantile(r['sr'], .5))} | {fmt(quantile(r['sr'], .95))} | {pct(sum(1 for v in r['sr'] if v > 1), len(r['sr']))} | "
                  f"{pct(r['rtx'], r['send'])} | {pct(r['probe'], r['send'])} | {pct(sum(1 for l in on if l == top), len(on))} |")

    rx = reactions(links, peers)
    if rx:
        md.append('\n## Dips (dippy viewers)\n')
        md.append('Seconds from dip start (down) or dip end (up). est and layer are sampled every ~100 ms (trace) and 1 s (stats). '
                  'missed = did not happen within the dip (down) or before the next dip (up). '
                  'sent > link = 1 s samples in the dip (after its first second) where the SFU sent more than the dip rate.\n')
        md.append('| | dips | p50 s | p95 s | max s | missed |')
        md.append('|---|---|---|---|---|---|')
        for key, label in [('est_down', 'est down (≤ 1.1 × dip rate)'), ('layer_down', 'layer down'), ('layer_up', 'layer up (pre-dip layer)'), ('est_up', 'est up (≥ 90% pre-dip)')]:
            have = [r for r in rx if key in r]
            vals = [r[key] for r in have if r[key] is not None]
            md.append(f'| {label} | {len(have)} | {fmt(quantile(vals, .5), 1)} | {fmt(quantile(vals, .95), 1)} | '
                      f'{fmt(max(vals) if vals else math.nan, 1)} | {len(have) - len(vals)} |')
        n, over = sum(r['n'] for r in rx), sum(r['over'] for r in rx)
        md.append(f'| sent > link in dip | {len(rx)} | | | | {pct(over, n)} of {n} samples |')
        pre = {}
        for r in rx:
            pre[r['pre_layer']] = pre.get(r['pre_layer'], 0) + 1
        md.append(f"\nLayer before the dip: {', '.join(f'{k}: {v}' for k, v in sorted(pre.items()))}.\n")
        md.append('What the SFU sent to dippy viewers, mean of 1 s samples: in dips (after their first second) and between dips '
                  '(from 5 s after a dip ends). ÷ dip rate > 1 means more than the link carries during a dip.\n')
        md.append('| | samples | RTP Mbps | RTX Mbps | probe Mbps | all Mbps | all ÷ dip rate | RTX ÷ RTP |')
        md.append('|---|---|---|---|---|---|---|---|')
        for tag, label in (('dip', 'in dips'), ('free', 'between dips')):
            n = sum(r[tag]['n'] for r in rx)
            if not n:
                continue
            tot = {k: sum(r[tag][k] for r in rx) / n for k in ('rtp', 'rtx', 'probe', 'send')}
            rate = sum(r['dip_bps'] * r[tag]['n'] for r in rx) / n
            md.append(f"| {label} | {n} | {fmt(tot['rtp'] / 1e6, 2)} | {fmt(tot['rtx'] / 1e6, 2)} | {fmt(tot['probe'] / 1e6, 2)} | "
                      f"{fmt(tot['send'] / 1e6, 2)} | {fmt(tot['send'] / rate, 2) if tag == 'dip' else '–'} | {pct(tot['rtx'], tot['rtp'])} |")
        md.append('')

    md.append('\n## Sample viewers\n')
    for p in sorted(prof):
        cands = sorted((n for n in peers if links[n].profile == p), key=lambda n: (-len(peers[n].st), n))
        for name in cands[:args.plot_peers]:
            pe = peers[name]
            f = f'{p}-{name}.svg'
            plot(os.path.join(args.out, f), f'{p}: {name}', links[name], pe, pe.st[0]['t'] if pe.st else win[0], pe.st[-1]['t'] if pe.st else win[1])
            md.append(f'![{p} {name}]({f})\n')
    text = '\n'.join(md) + '\n'
    with open(os.path.join(args.out, 'analysis.md'), 'w') as f:
        f.write(text)
    print(text)


if __name__ == '__main__':
    main()
