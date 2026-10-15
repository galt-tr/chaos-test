# arcade #362 + #363: three merkle-services under chaos (2026-10-15)

Arcade built from PR #362 (merkle-service Pool: fan-out /watch, per-endpoint breaker, catch-up
queue) with PR #363 (content-addressed STUMPs, idempotent BLOCK_PROCESSED) stacked, against three
merkle-services (`make gen M=3 SV=0`, each on its own Postgres database and Kafka topics). The
wallet-toolbox blaster sends 2 tx/s for 4 min. Each fault starts 60 s into the send and is healed
90 s later. Blocks come every 30 s. `internal/txtiming` polls every tx's arcade status at 1 s and
samples `/health` `merkle_endpoints[]`; `cmd/chaosreport` turns a run into the tables below.

Report page: https://claude.ai/artifact/KWwTvpJDV3UBkvd2hHHXqE

## Verdict

- No transaction was lost: every tx reached MINED in every scenario, including all three down for 90 s.
- With all three down, 0 of 181 txs submitted during the outage were broadcast before an endpoint
  returned (F-024). 135 parked at PENDING_RETRY and were mined after the restore.
- Latency was the problem. One hung endpoint held every /watch batch for the 30 s client timeout.
  Fixed in arcade 0366c51 (#362): SEEN p95 during a black-holed endpoint went from 64.8 s to 14.4 s.
- #363 treated every redelivered BLOCK_PROCESSED as an incomplete rebuild (coinbase placeholder vs
  real coinbase txid at subtree 0 offset 0) and cleared processed_at on every block. Fixed in 83ed4bf.

## Findings

| # | finding | where | status |
|---|---|---|---|
| 1 | hung endpoint holds each /watch batch for the full HTTP timeout; breaker trips 33–45 s in | #362 pool.go | fixed, 0366c51 |
| 2 | coinbase slot makes identical redeliveries look new and rebuilds look lossy; processed_at cleared every block | #363 bump_builder | fixed, 83ed4bf |
| 3 | correlated 5xx (all services busy on the same block) opens every breaker at once, so the pool refuses locally; seen with sqlite merkle-services, 34 parked txs with no fault injected | #362 pool.go | review comment |
| 4 | breaker threshold counts concurrent in-flight failures from one instant | #362 pool.go | review comment |
| 5 | after an all-down outage, parked txs wait out the PENDING_RETRY backoff: re-broadcast 45–54 s after the first endpoint returns | propagation reaper | review comment |
| 6 | 30 s /watch timeout not configurable | #362 app.go | review comment |

Reviews: https://github.com/bsv-blockchain/arcade/pull/362#pullrequestreview-5539704485 and
https://github.com/bsv-blockchain/arcade/pull/363#pullrequestreview-5539709899

## Caveats

- Under podman a stopped container's IP disappears, so connections hang. "stop" measures the same
  black-hole class as "pause". A deleted k8s pod usually refuses connections at once.
- SIGKILL left the container down (podman did not apply restart: on-failure).
- The pinned merkle-service image sends BLOCK_PROCESSED without the #195 enrichment, which adds
  about 10 s to every MINED (datahub fetch + second grace window), baseline included.
- The first baseline ran merkle-services on sqlite. It wedged under load (SQLITE_BUSY), so the
  harness moved them to Postgres (merkle-db). Finding 3 comes from that run.

## Before (3a36cd3 + c50b40b + 83ed4bf)

### merkle-baseline (20261015-192923-merkle-baseline-1) status=passed fault window=90s

| phase | txs | final | seen p50/p95/max (s) | mined p50/p95/max (s) | wallet call p50/p95 (s) | PENDING_RETRY | broadcast before window end |
|---|---|---|---|---|---|---|---|
| before | 121 | MINED=121 | 4.8/9.3/9.8 (n=121) | 20.8/34.3/35.8 (n=121) | 0.1/0.4 | 0 | 121 |
| during | 180 | MINED=180 | 5.3/9.8/10.8 (n=180) | 20.8/34.3/35.8 (n=180) | 0.2/0.3 | 0 | 179 |
| after | 180 | MINED=180 | 5.3/9.3/9.8 (n=180) | 20.8/34.3/35.8 (n=180) | 0.2/0.8 | 0 | 0 |
| all | 481 | MINED=481 | 5.3/9.8/10.8 (n=481) | 20.8/34.3/35.8 (n=481) | 0.2/0.4 | 0 | 300 |

- http://10.190.0.41:8080: unhealthy at [], healthy at [], max catch-up 0, catch-up drained at -1.0s
- http://10.190.0.44:8080: unhealthy at [], healthy at [], max catch-up 0, catch-up drained at -1.0s
- http://10.190.0.45:8080: unhealthy at [], healthy at [], max catch-up 0, catch-up drained at -1.0s
- mark fault-window-start
- mark fault-window-end

### merkle-down-1-stop (20261015-193528-merkle-down-1-stop-2) status=passed fault window=93s

| phase | txs | final | seen p50/p95/max (s) | mined p50/p95/max (s) | wallet call p50/p95 (s) | PENDING_RETRY | broadcast before window end |
|---|---|---|---|---|---|---|---|
| before | 121 | MINED=121 | 3.1/6.6/7.1 (n=121) | 21.1/34.6/36.1 (n=121) | 0.1/0.2 | 0 | 121 |
| during | 185 | MINED=185 | 9.1/34.1/40.1 (n=185) | 28.6/60.1/72.1 (n=185) | 0.2/0.3 | 0 | 183 |
| after | 180 | MINED=180 | 6.1/10.6/11.6 (n=180) | 21.6/35.1/36.6 (n=180) | 0.2/0.4 | 0 | 0 |
| all | 486 | MINED=486 | 5.6/28.6/40.1 (n=486) | 23.6/52.6/72.1 (n=486) | 0.1/0.4 | 0 | 304 |

- http://10.190.0.41:8080: unhealthy at [33.1], healthy at [95.1], max catch-up 187, catch-up drained at 105.1s
- http://10.190.0.44:8080: unhealthy at [], healthy at [], max catch-up 0, catch-up drained at -1.0s
- http://10.190.0.45:8080: unhealthy at [], healthy at [], max catch-up 0, catch-up drained at -1.0s
- mark fault-window-start
- mark chaos stop merkle-service
- mark chaos start merkle-service
- mark fault-window-end

### merkle-down-1-pause (20261015-194104-merkle-down-1-pause-3) status=passed fault window=90s

| phase | txs | final | seen p50/p95/max (s) | mined p50/p95/max (s) | wallet call p50/p95 (s) | PENDING_RETRY | broadcast before window end |
|---|---|---|---|---|---|---|---|
| before | 121 | MINED=121 | 4.3/8.3/36.3 (n=121) | 20.8/34.3/43.3 (n=121) | 0.1/0.3 | 0 | 121 |
| during | 180 | MINED=180 | 18.3/64.8/69.3 (n=180) | 59.3/92.3/108.8 (n=180) | 0.1/0.3 | 0 | 179 |
| after | 180 | MINED=180 | 15.3/23.8/25.8 (n=180) | 32.8/48.8/62.8 (n=180) | 0.2/0.6 | 0 | 0 |
| all | 481 | MINED=481 | 13.3/49.3/69.3 (n=481) | 33.8/84.8/108.8 (n=481) | 0.2/0.5 | 0 | 300 |

- http://10.190.0.41:8080: unhealthy at [40.3], healthy at [91.3], max catch-up 181, catch-up drained at 104.3s
- http://10.190.0.44:8080: unhealthy at [], healthy at [], max catch-up 0, catch-up drained at -1.0s
- http://10.190.0.45:8080: unhealthy at [], healthy at [], max catch-up 0, catch-up drained at -1.0s
- mark fault-window-start
- mark chaos pause merkle-service
- mark chaos unpause merkle-service
- mark fault-window-end

### merkle-down-1-kill (20261015-194710-merkle-down-1-kill-4) status=passed fault window=90s

| phase | txs | final | seen p50/p95/max (s) | mined p50/p95/max (s) | wallet call p50/p95 (s) | PENDING_RETRY | broadcast before window end |
|---|---|---|---|---|---|---|---|
| before | 119 | MINED=119 | 3.7/8.6/12.2 (n=119) | 22.7/36.2/37.7 (n=119) | 0.1/1.4 | 0 | 119 |
| during | 181 | MINED=181 | 14.1/34.1/46.6 (n=181) | 47.6/64.7/76.6 (n=181) | 0.2/0.4 | 0 | 179 |
| after | 180 | MINED=180 | 7.6/13.1/15.6 (n=180) | 41.1/54.1/58.1 (n=180) | 0.2/0.3 | 0 | 0 |
| all | 480 | MINED=480 | 8.1/30.1/46.6 (n=480) | 38.6/58.6/76.6 (n=480) | 0.2/0.5 | 0 | 298 |

- http://10.190.0.41:8080: unhealthy at [32.7], healthy at [], max catch-up 361, catch-up drained at -1.0s
- http://10.190.0.44:8080: unhealthy at [], healthy at [], max catch-up 0, catch-up drained at -1.0s
- http://10.190.0.45:8080: unhealthy at [], healthy at [], max catch-up 0, catch-up drained at -1.0s
- mark fault-window-start
- mark chaos kill merkle-service
- mark fault-window-end

### merkle-down-2 (20261015-195316-merkle-down-2-5) status=passed fault window=93s

| phase | txs | final | seen p50/p95/max (s) | mined p50/p95/max (s) | wallet call p50/p95 (s) | PENDING_RETRY | broadcast before window end |
|---|---|---|---|---|---|---|---|
| before | 121 | MINED=121 | 3.6/7.1/8.1 (n=121) | 21.6/35.1/36.5 (n=121) | 0.1/0.2 | 0 | 121 |
| during | 185 | MINED=185 | 32.1/71.1/79.6 (n=185) | 53.6/98.1/125.6 (n=185) | 0.1/0.4 | 0 | 182 |
| after | 178 | MINED=178 | 10.0/30.5/33.5 (n=178) | 23.0/36.0/39.6 (n=178) | 0.2/0.8 | 0 | 0 |
| all | 484 | MINED=484 | 10.0/57.1/79.6 (n=484) | 27.6/86.1/125.6 (n=484) | 0.2/0.4 | 0 | 303 |

- http://10.190.0.41:8080: unhealthy at [33.6], healthy at [93.6], max catch-up 185, catch-up drained at 115.6s
- http://10.190.0.44:8080: unhealthy at [44.6], healthy at [93.6], max catch-up 183, catch-up drained at 110.6s
- http://10.190.0.45:8080: unhealthy at [], healthy at [], max catch-up 0, catch-up drained at -1.0s
- mark fault-window-start
- mark chaos stop merkle-service
- mark chaos pause merkle-service-2
- mark chaos start merkle-service
- mark chaos unpause merkle-service-2
- mark fault-window-end

### merkle-down-3 (20261015-195851-merkle-down-3-6) status=passed fault window=156s

| phase | txs | final | seen p50/p95/max (s) | mined p50/p95/max (s) | wallet call p50/p95 (s) | PENDING_RETRY | broadcast before window end |
|---|---|---|---|---|---|---|---|
| before | 121 | MINED=121 | 3.9/9.4/162.4 (n=121) | 29.4/180.9/183.9 (n=121) | 0.1/0.3 | 0 | 121 |
| during | 311 | MINED=311 | 50.4/144.9/189.4 (n=311) | 56.4/157.4/189.4 (n=311) | 0.1/0.3 | 142 | 309 |
| after | 180 | MINED=180 | 5.9/10.9/21.4 (n=180) | 22.4/35.9/39.4 (n=180) | 0.2/0.4 | 0 | 0 |
| all | 612 | MINED=612 | 10.9/138.9/189.4 (n=612) | 36.9/169.9/189.4 (n=612) | 0.2/0.4 | 142 | 430 |

- http://10.190.0.41:8080: unhealthy at [38.4], healthy at [97.4], max catch-up 2, catch-up drained at 105.4s
- http://10.190.0.44:8080: unhealthy at [44.4], healthy at [127.4], max catch-up 246, catch-up drained at 148.4s
- http://10.190.0.45:8080: unhealthy at [44.4], healthy at [157.4], max catch-up 309, catch-up drained at 176.4s
- mark fault-window-start
- mark chaos stop merkle-service
- mark chaos stop merkle-service-2
- mark chaos stop merkle-service-3
- mark chaos start merkle-service
- mark chaos start merkle-service-2
- mark chaos start merkle-service-3
- mark fault-window-end

### merkle-flap (20261015-200527-merkle-flap-7) status=passed fault window=107s

| phase | txs | final | seen p50/p95/max (s) | mined p50/p95/max (s) | wallet call p50/p95 (s) | PENDING_RETRY | broadcast before window end |
|---|---|---|---|---|---|---|---|
| before | 121 | MINED=121 | 3.3/7.3/10.3 (n=121) | 21.8/35.3/36.8 (n=121) | 0.1/0.7 | 0 | 121 |
| during | 213 | MINED=213 | 14.3/23.3/31.9 (n=213) | 35.3/48.8/52.3 (n=213) | 0.2/0.3 | 0 | 198 |
| after | 180 | MINED=180 | 4.3/8.8/12.3 (n=180) | 20.8/34.3/35.8 (n=180) | 0.2/0.4 | 0 | 0 |
| all | 514 | MINED=514 | 5.3/21.8/31.9 (n=514) | 27.3/45.9/52.3 (n=514) | 0.2/0.4 | 0 | 319 |

- http://10.190.0.41:8080: unhealthy at [], healthy at [], max catch-up 1, catch-up drained at 43.3s
- http://10.190.0.44:8080: unhealthy at [], healthy at [], max catch-up 0, catch-up drained at -1.0s
- http://10.190.0.45:8080: unhealthy at [], healthy at [], max catch-up 0, catch-up drained at -1.0s
- mark fault-window-start
- mark chaos stop merkle-service
- mark chaos start merkle-service
- mark chaos stop merkle-service
- mark chaos start merkle-service
- mark chaos stop merkle-service
- mark chaos start merkle-service
- mark chaos stop merkle-service
- mark chaos start merkle-service
- mark fault-window-end

## After (0366c51 / 50f8765)

### merkle-baseline (20261015-201204-merkle-baseline-8) status=passed fault window=90s

| phase | txs | final | seen p50/p95/max (s) | mined p50/p95/max (s) | wallet call p50/p95 (s) | PENDING_RETRY | broadcast before window end |
|---|---|---|---|---|---|---|---|
| before | 121 | MINED=121 | 3.2/7.7/8.2 (n=121) | 20.3/33.8/35.2 (n=121) | 0.1/0.2 | 0 | 121 |
| during | 180 | MINED=180 | 4.2/8.2/10.2 (n=180) | 20.7/34.2/36.2 (n=180) | 0.2/0.3 | 0 | 178 |
| after | 180 | MINED=180 | 4.2/8.3/13.2 (n=180) | 21.2/34.7/40.2 (n=180) | 0.2/0.3 | 0 | 0 |
| all | 481 | MINED=481 | 3.8/8.2/13.2 (n=481) | 20.8/34.2/40.2 (n=481) | 0.2/0.3 | 0 | 299 |

- http://10.190.0.41:8080: unhealthy at [], healthy at [], max catch-up 0, catch-up drained at -1.0s
- http://10.190.0.44:8080: unhealthy at [], healthy at [], max catch-up 0, catch-up drained at -1.0s
- http://10.190.0.45:8080: unhealthy at [], healthy at [], max catch-up 0, catch-up drained at -1.0s
- mark fault-window-start
- mark fault-window-end

### merkle-down-1-stop (20261015-201740-merkle-down-1-stop-9) status=passed fault window=94s

| phase | txs | final | seen p50/p95/max (s) | mined p50/p95/max (s) | wallet call p50/p95 (s) | PENDING_RETRY | broadcast before window end |
|---|---|---|---|---|---|---|---|
| before | 121 | MINED=121 | 3.2/8.2/8.7 (n=121) | 20.7/34.2/35.7 (n=121) | 0.1/0.4 | 0 | 121 |
| during | 187 | MINED=187 | 4.2/11.7/16.7 (n=187) | 22.2/36.2/40.7 (n=187) | 0.2/0.3 | 0 | 184 |
| after | 180 | MINED=180 | 3.7/8.2/11.2 (n=180) | 20.7/34.2/35.7 (n=180) | 0.1/0.3 | 0 | 0 |
| all | 488 | MINED=488 | 3.7/8.2/16.7 (n=488) | 21.2/35.2/40.7 (n=488) | 0.1/0.3 | 0 | 305 |

- http://10.190.0.41:8080: unhealthy at [12.7], healthy at [94.7], max catch-up 184, catch-up drained at 108.7s
- http://10.190.0.44:8080: unhealthy at [], healthy at [], max catch-up 0, catch-up drained at -1.0s
- http://10.190.0.45:8080: unhealthy at [], healthy at [], max catch-up 0, catch-up drained at -1.0s
- mark fault-window-start
- mark chaos stop merkle-service
- mark chaos start merkle-service
- mark fault-window-end

### merkle-down-1-pause (20261015-202316-merkle-down-1-pause-10) status=passed fault window=90s

| phase | txs | final | seen p50/p95/max (s) | mined p50/p95/max (s) | wallet call p50/p95 (s) | PENDING_RETRY | broadcast before window end |
|---|---|---|---|---|---|---|---|
| before | 121 | MINED=121 | 3.4/6.4/8.4 (n=121) | 20.9/34.4/35.9 (n=121) | 0.1/0.9 | 0 | 121 |
| during | 178 | MINED=178 | 5.4/14.4/16.9 (n=178) | 22.9/38.9/42.9 (n=178) | 0.2/0.7 | 0 | 176 |
| after | 179 | MINED=179 | 3.6/7.1/9.1 (n=179) | 20.6/34.6/36.6 (n=179) | 0.2/0.5 | 0 | 0 |
| all | 478 | MINED=478 | 4.1/10.8/16.9 (n=478) | 21.6/35.6/42.9 (n=478) | 0.2/0.8 | 0 | 297 |

- http://10.190.0.41:8080: unhealthy at [10.4], healthy at [91.4], max catch-up 179, catch-up drained at 102.4s
- http://10.190.0.44:8080: unhealthy at [], healthy at [], max catch-up 0, catch-up drained at -1.0s
- http://10.190.0.45:8080: unhealthy at [], healthy at [], max catch-up 0, catch-up drained at -1.0s
- mark fault-window-start
- mark chaos pause merkle-service
- mark chaos unpause merkle-service
- mark fault-window-end

### merkle-down-2 (20261015-202852-merkle-down-2-11) status=passed fault window=93s

| phase | txs | final | seen p50/p95/max (s) | mined p50/p95/max (s) | wallet call p50/p95 (s) | PENDING_RETRY | broadcast before window end |
|---|---|---|---|---|---|---|---|
| before | 121 | MINED=121 | 4.5/9.5/10.0 (n=121) | 21.0/34.5/36.0 (n=121) | 0.1/0.2 | 0 | 121 |
| during | 186 | MINED=186 | 6.5/19.5/24.0 (n=186) | 22.5/37.0/41.5 (n=186) | 0.1/0.3 | 0 | 184 |
| after | 180 | MINED=180 | 5.5/10.0/12.0 (n=180) | 21.5/35.0/37.0 (n=180) | 0.1/0.2 | 0 | 0 |
| all | 487 | MINED=487 | 5.5/13.5/24.0 (n=487) | 22.0/35.5/41.5 (n=487) | 0.1/0.3 | 0 | 305 |

- http://10.190.0.41:8080: unhealthy at [15.5], healthy at [93.5], max catch-up 183, catch-up drained at 106.5s
- http://10.190.0.44:8080: unhealthy at [17.5], healthy at [93.5], max catch-up 182, catch-up drained at 102.5s
- http://10.190.0.45:8080: unhealthy at [], healthy at [], max catch-up 0, catch-up drained at -1.0s
- mark fault-window-start
- mark chaos stop merkle-service
- mark chaos pause merkle-service-2
- mark chaos start merkle-service
- mark chaos unpause merkle-service-2
- mark fault-window-end

### merkle-down-3 (20261015-203427-merkle-down-3-12) status=passed fault window=160s

| phase | txs | final | seen p50/p95/max (s) | mined p50/p95/max (s) | wallet call p50/p95 (s) | PENDING_RETRY | broadcast before window end |
|---|---|---|---|---|---|---|---|
| before | 119 | MINED=119 | 3.4/7.4/9.4 (n=119) | 31.6/164.6/168.6 (n=119) | 0.1/0.3 | 0 | 119 |
| during | 319 | MINED=319 | 48.9/130.9/170.9 (n=319) | 54.9/143.9/170.9 (n=319) | 0.1/0.6 | 143 | 303 |
| after | 181 | MINED=181 | 5.9/14.1/22.8 (n=181) | 22.9/35.4/40.9 (n=181) | 0.2/1.0 | 0 | 0 |
| all | 619 | MINED=619 | 12.9/125.4/170.9 (n=619) | 34.1/154.4/170.9 (n=619) | 0.2/0.7 | 143 | 422 |

- http://10.190.0.41:8080: unhealthy at [31.1], healthy at [98.1], max catch-up 2, catch-up drained at 106.1s
- http://10.190.0.44:8080: unhealthy at [38.1], healthy at [133.1], max catch-up 256, catch-up drained at 154.1s
- http://10.190.0.45:8080: unhealthy at [38.1], healthy at [163.1], max catch-up 309, catch-up drained at 181.1s
- mark fault-window-start
- mark chaos stop merkle-service
- mark chaos stop merkle-service-2
- mark chaos stop merkle-service-3
- mark chaos start merkle-service
- mark chaos start merkle-service-2
- mark chaos start merkle-service-3
- mark fault-window-end

### merkle-flap (20261015-204133-merkle-flap-13) status=passed fault window=114s

| phase | txs | final | seen p50/p95/max (s) | mined p50/p95/max (s) | wallet call p50/p95 (s) | PENDING_RETRY | broadcast before window end |
|---|---|---|---|---|---|---|---|
| before | 121 | MINED=121 | 5.1/9.7/11.7 (n=121) | 21.7/35.2/37.7 (n=121) | 0.1/0.2 | 0 | 121 |
| during | 228 | MINED=228 | 7.7/21.1/25.7 (n=228) | 27.2/41.2/43.2 (n=228) | 0.2/0.4 | 0 | 227 |
| after | 180 | MINED=180 | 5.2/9.7/10.7 (n=180) | 21.2/34.7/36.2 (n=180) | 0.2/0.3 | 0 | 0 |
| all | 529 | MINED=529 | 6.2/15.7/25.7 (n=529) | 24.2/38.7/43.2 (n=529) | 0.2/0.3 | 0 | 348 |

- http://10.190.0.41:8080: unhealthy at [11.1 41.1 66.1 93.1], healthy at [17.1 46.1 75.1 103.1], max catch-up 31, catch-up drained at 105.1s
- http://10.190.0.44:8080: unhealthy at [], healthy at [], max catch-up 0, catch-up drained at -1.0s
- http://10.190.0.45:8080: unhealthy at [], healthy at [], max catch-up 0, catch-up drained at -1.0s
- mark fault-window-start
- mark chaos stop merkle-service
- mark chaos start merkle-service
- mark chaos stop merkle-service
- mark chaos start merkle-service
- mark chaos stop merkle-service
- mark chaos start merkle-service
- mark chaos stop merkle-service
- mark chaos start merkle-service
- mark fault-window-end
