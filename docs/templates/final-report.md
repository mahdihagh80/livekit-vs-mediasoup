# LiveKit vs mediasoup on xFusion: final report

## 1. Question and decision rule
(copied from docs/01-methodology.md *before* the runs)

## 2. Setup
- Hardware: (docs/templates/hardware.md)
- CPU plan:
- Versions:
- LiveKit config diff vs production:
- mediasoup bench server: commit, mediasoup.media.router_max_peers chosen:
- bench.yaml used (attach it):

## 3. Validity
- Generator calibration: max valid load = ...
- chrony offsets (gen-1, gen-2):
- LiveKit run-to-run spread (run A vs run C):
- Excluded runs and why:

## 4. Results

### 1:1
| | LiveKit (best layout) | mediasoup | Δ |
|---|---|---|---|
| capacity at SLO | | | |
| cores per 1000 video tracks @ N | | | |
| video delay p99 @ N | | | |
| freeze % @ N | | | |
| first failing check | | | |

### Group-6
...
### Webinar
...
### Audio rooms
...
### Join storm
...

## 5. Known differences and their effect

## 6. Decision and fleet impact

## 7. If we migrate: gaps between the benchmark build and a production mediasoup service
