# Wyze Bridge - KVS WHEP Pipeline Notes (Last active: 2026-08-18)

## Current State
- BOTH cams streaming to HomeKit (verified 2026-08-18 ~18:50: backyard 2560x1440, fish 640x360).
- Architecture: KVS (`v-*.kinesisvideo.us-west-2.amazonaws.com` WHEP/WS) → Go `whep_proxy/main.go` → UDP 5600/5601 (backyard), 5602/5603 (fish) per `/tmp/gst-rtsp-streams.conf` → `gst_rtsp_bridge :8555` ← mediaMTX path source (`rtsp://127.0.0.1:8555/<cam>`) → **:8554** → Scrypted prebuffer (H264+G711; backyard 2560x1440@20, fish 640x360@10). mediaMTX API is disabled (`api: false`).

## App-Alignment Changes (2026-08-18, deployed as image `04018e7a`)
Aligning the bridge with how the official Wyze app actually behaves (research in `RESEARCH_wyze_3.19_reversing.md`):
1. **Keep-alive wakes removed** — `KVS_KEEPALIVE_SECONDS=0` in compose (env knob in `config.py`/`stream_manager.py`, default 480). The app never re-wakes a held session; phase-2 test proved the warm reconnect path recovers independently (two full 607s cycles, both attempt 1, zero keep-alive lines).
2. **Reconnect escalation** (`whep_proxy/main.go` `scheduleReconnect`): attempts 1–5 use the old hot backoff (2s/4s/…); from attempt 6 onward the cadence drops to **60s** — the app gives up on a session that won't connect and falls back to slow camera-state checks instead of hammering (today's dead cam took 19+ offer storms at ~75s cadence; now it's 12/hour, and each attempt is still the full app-style flow: wake-on-cooldown + fresh config + fresh offer). Log line when it trips: `not recovering after 5 attempts; slowing to 60s poll cadence`.
3. **Periodic keyframe refresh disabled** — `WHEP_PERIODIC_KEYFRAME_MS=0` (0 now means "off" in Go, was "invalid → default 60s"). The app never sends periodic PLIs; camera GOP is ~2s.
4. **Snapshot ffmpeg leak fixed** (`stream_manager.py`): snapshots run in their own session (`start_new_session=True`) and are killed by process group (the old `kill()` hit only the `/bin/sh -ec` wrapper and orphaned the ffmpeg holding its :8554 RTSP session — 4 leaked overnight); the monitor thread now force-kills any capture alive past 30s. Verified: snapshot 200 in ~5s, only the substream transcoder ffmpeg remains.
- Rollback tags: `wyze-bridge-local:rollback-20260818-knob` (keep-alive knob only, old Go) and `wyze-bridge-local:rollback-20260817` (pre-everything).

## Key Findings (24h stress audit, 2026-08-18)
- KVS live-view channels die on a hard **~607s TTL**. Proactive keep-alive wakes (`KVS_KEEPALIVE_INTERVAL=480` in `stream_manager.py`) do NOT extend it — reconnect cycles stayed ~605–611s no matter where the 480s wake landed. So every 8 min was pure camera stress (~72 wasted wakes/day/cam): 391 "Waking KVS camera" + 301 `/kvs-config` polls + 157 backyard reconnects in 24h.
- On channel expiry, Go `scheduleReconnect()` (main.go:1504) backoffs 2s/4s/../cap 30s, each attempt GETs `http://127.0.0.1:5000/kvs-config/<streamID>` (warm = media <120s old), which wakes the camera via `_maybe_wake_kvs_camera` (30s warm / 600s non-warm cooldown). Recovery normally succeeds on attempt 1 — recovery does NOT depend on the keep-alive wake. Since the 2026-08-18 app-alignment change, from attempt 6 onward the cadence drops to 60s (see App-Alignment #2).
- PLI throttling is already committed & deployed (`130daa2`, `4dc2d42`; binary has `WHEP_PLI_MIN_INTERVAL_MS`, default 3s floor; discontinuity only triggers when `PrevDroppedPackets > 5`). Periodic keyframe refresh = 60s/cam (redundant: camera natural GOP is ~2s).
- Direct TUTK P2P to the cams is infeasible (`IOTC_ER_UNLICENSE -10` with Wyze's own 4.3.8 libs, tested under qemu aarch64) — these cams are cloud/KVS-only for direct connect. Closed; see `RESEARCH_wyze_3.19_reversing.md`.

## Keep-Alive Experiment — PHASE 2 PASSED (2026-08-18, running with `KVS_KEEPALIVE_SECONDS=0`)
- Code change (uncommitted, in `app/wyzebridge/config.py` + `stream_manager.py`): keep-alive interval is now env-tunable via **`KVS_KEEPALIVE_SECONDS`** (default 480 = old behavior; 0 = off). Image built as `ddc519ba`; rollback image tagged `wyze-bridge-local:rollback-20260817`.
- Phase 1 (`=480`, new code): boot clean, keep-alive fired normally via the new path — no regression.
- **Phase 2 (`=0`, deployed ~08:25 local)**: fish-cam ran two full KVS cycles (expired 12:31:49 and 12:41:56 UTC, exact 607s apart) and recovered on `Upstream reconnected ... attempt 1` each time with **zero keep-alive wake lines** since boot. Hypothesis confirmed: the warm reconnect path wakes/re-recovers independently; proactive 8-min pokes were pure stress (~72/cam/day saved).
- Rollback of anything bad: set `KVS_KEEPALIVE_SECONDS=480` (or delete line) + `docker compose up -d`. No rebuild needed for env changes.
- Also removed dead config from docker-compose.yml this session: `MAIN_TRANSCODE_CAMS`, `STREAM_KEEP_ALIVE`, `MAIN_TRANSCODE_BANDWIDTH` are NOT read anywhere in the code (only `SUBSTREAM_TRANSCODE_CAMS` is).

## RESOLVED INCIDENT (2026-08-18 ~07:30 local): backyard-cam upstream dead at KVS/WHEP layer
- Symptoms: HomeKit "preview not available" for Backyard; :8554 returned 404; Go logged repeated `ICE gathering timeout` → `SDP_ANSWER timeout` on EVERY attempt (19+ by 08:43) across 3 container boots. `/kvs-config/backyard-cam` returned 200 throughout — bridge-side auth/wake/grant healthy; Wyze's KVS edge never answered the WHEP offer ⇒ **camera-side RTC agent not joining**.
- **Resolved by power-cycling the camera** (~18:00 local): upstream re-acquired 18:01 UTC on attempt 3 and has cycled cleanly (attempt 1) ever since.
- Earlier the same morning there was a distinct gst failure at one boot: mount preroll race for the 2K stream (`gst_rtsp_media_prepare: failed to preroll pipeline` + heavy RTP reorder drops) that left :8554 404 until restart — forensics saved in `/tmp/opencode/wyze-boot-failure-20260818.log`.

## Known Bugs / Next Steps
1. ~~Snapshot ffmpeg leak~~ — **FIXED 2026-08-18** (process-group kill + 30s hard lifetime, see App-Alignment #4).
2. **Wedge detection (partially addressed)**: the reconnect loop no longer hammers a wedged cam (60s cadence after 5 failures), but it never declares the cam "offline" to MQTT/Scrypted or escalates to a deeper reset (full re-wake with fresh stream grant / surface for power-cycle). Today's incident still required a human power-cycle.
3. ~~`WHEP_PERIODIC_KEYFRAME_MS` 60s refresh redundant~~ — **FIXED 2026-08-18** (disabled via `=0`).
4. Repo-root `uncommitted-pli-throttle-2026-07-07.patch` is a stale leftover; those changes are committed (`130daa2`/`4dc2d42`).
5. **gst mount preroll race at boot**: one boot this week left the 2K mount stuck at `failed to preroll pipeline` (:8554 404) for 25+ min until container restart, despite KVS media flowing. Rare but real; candidate hardening = detect preroll failure + restart that mount's pipeline.

## Quick Commands
```bash
# Stream health (expect backyard 2560x1440, fish 640x360)
ffprobe -v error -select_streams v:0 -show_entries stream=width,height rtsp://192.168.6.10:8554/backyard-cam
docker logs --since 1h wyze-bridge 2>&1 | grep -E "keep-alive|Reconnecting upstream|Upstream reconnected" | tail

# Restart the bridge (fixes wedged KVS channels)
docker restart wyze-bridge

# Rebuild/deploy code changes
cd /volume2/docker/wyze-bridge-project && docker build -t wyze-bridge-local . && docker compose up -d
```
