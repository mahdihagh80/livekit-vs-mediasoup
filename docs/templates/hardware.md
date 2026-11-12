# Hardware and setup record (fill in from results/inventory/)

| | SFU (xFusion) | gen-1 | gen-2 |
|---|---|---|---|
| hostname / IP | | | |
| model | xFusion ... | | |
| CPU model, sockets × cores × threads | | | |
| SMT on/off | | | |
| RAM | | | |
| NIC model / driver / firmware | | | |
| NIC NUMA node | | | |
| link speed (bond?) | | | |
| kernel | | | |
| BIOS power profile / turbo | | | |

## CPU plan (bench.yaml)

| | value | notes |
|---|---|---|
| cpu.sfu_irq | | NUMA node of NIC? |
| cpu.sfu | | |
| mediasoup.control_cpus | | |
| mediasoup.worker_cpus | | workers = |
| LiveKit layout chosen | | from sweep run ... |
| cpu.gen_irq / cpu.gen | | |

## Software versions

| | version |
|---|---|
| LiveKit | open-source 1.13.11 (native binary / official image), livekit.base.yaml changes: ... |
| mediasoup | 3.31.x |
| Node.js | |
| loadgen git rev | |
| server-sdk-go / pion | v2.18.1 / v4.2.x |
