# Session Memory — 2026-05-26

## Goal
Enable backyard camera in Scrypted with working video, audio, snapshots, and motion detection via built-in frame scanning. Fix periodic video stutter (every 3-4 seconds) in WHEP proxy streaming.

## Current Status (as of end of session)
- **Backyard motion detection FIXED.** Root cause: `motionSensorSupplementation` was `"Assist"` instead of `"Replace"`. "Assist" mode requires a companion raw motion sensor to fire first, but the MQTT broker at `192.168.7.250:1883` is unreachable from the NAS, so the MQTT sensor (device 151) never triggered. Changed to "Replace" — OpenCV fires motion events directly from frame analysis.
- DB was accidentally wiped during the edit (rm -rf on bind mount deleted contents before hitting mount point). Restored from `/volume2/docker/scrypted/backup.db` (May 25 22:07). Any config changes made between May 25 22:07 and May 26 ~19:16 are lost.
- After fix: Backyard shows `"Libav + OpenCV Motion Detection"` running clean, no errors, HomeKit connections active.
- **Video stutter fixed.** Bumped `rtpjitterbuffer latency` from 200ms to 3500ms in `gst_rtsp_bridge.c` to smooth across ~2-second KVS stream burst gaps. Video flowing at 0.02% packet loss.
- **Container IP fixed.** Qwen3.6 redeployed without `--ip`, getting DHCP lease at 192.168.4.4 instead of the expected 192.168.6.10. Recreated with `--ip 192.168.6.10` on macvlan_net (subnet 192.168.4.0/22).
- **Qwen3.6 regressions reverted:** `wait_for_video=True` → `False` (commit 1431bb3) to stop MediaMTX source timeout loop. `maxLate` was 2048 in HEAD (Qwen3.6 change was uncommitted), no revert needed.

## Camera RTSP Source Map
- **Backyard (150)**: `rtsp://192.168.6.10:8555/backyard-cam` — wyze-bridge GST RTSP server (KVS camera)
- **Doorbell (82)**: `rtsp://user:...@192.168.4.20:8554/Video-42` — **Vivint panel**, NOT wyze-bridge
- **Driveway (21)**: `rtsp://user:...@192.168.4.20:8554/Video-43` — **Vivint panel**, NOT wyze-bridge
- **Fish Cam (104)**: wyze-bridge MediaMTX (non-KVS, no motion configured)

## Docker / Wyze Bridge
- Stack: `/volume2/docker/wyze-bridge-project/`
- Container: `wyze-bridge`, image `wyze-bridge-local:latest`, IP 192.168.6.10 on macvlan_net
- Env vars: `ON_DEMAND=true`, `ENABLE_AUDIO_ALL=true`, `KVS_GSTREAMER_RTSP=true`, `WB_IP=192.168.6.10`
- Ports: `5000/tcp` (Flask frontend), `8189/udp` (WebRTC ICE), `8889/tcp` (WHEP), `8555/tcp` (GST RTSP)
- **Rebuild command:** `docker build --no-cache -t wyze-bridge-local:latest .`
- **Redeploy command:** see docker run in session log (12 env vars, 2 volume mounts, macvlan_net with --ip 192.168.6.10)
- Key code changes in `wyze_bridge.py`: KVS cameras call `setup_mtx_proxy()` with `wait_for_video=False`. `mtx.add_path()` always called.
- Key code change in `gst_rtsp_bridge.c`: `config-interval=-1`, caps `video/x-h264,stream-format=avc,alignment=au`. **`rtpjitterbuffer latency=3500`** (up from 200ms) to handle KVS burst gaps.
- **Port routing**: Port 8554 (MediaMTX) serves corrupted H264 for KVS streams. Port 8555 (gst_rtsp_bridge) correctly reports width=2560, height=1440 — used by Backyard in Scrypted.
- All 6 commits pushed to Gitea: `https://git.sixteen33.com/zachweyland/docker-wyze-bridge`
- **Known cosmetic noise:** `[path backyard-cam] [WebRTC source] deadline exceeded while waiting tracks` — MediaMTX's separate WebRTC source can't share the KVS connection with the WHEP proxy. Does NOT affect RTSP video path (8555).
- Wyze-bridge log shows healthy KVS streaming for backyard-cam: ~0.02% packet loss (4 dropped / 20,000 read).

## Scrypted
- Container: `scrypted-app-1`, image `koush/scrypted:latest`, IP 192.168.6.3, API on port 10443 (HTTPS)
- Volume: `/volume2/docker/scrypted` → `/server/volume`
- LevelDB at `/server/volume/scrypted.db` (host: `/volume2/docker/scrypted/scrypted.db/`)
- Backups (multiple): `/volume2/docker/scrypted/scrypted.db.bak-*` and `backup.db`
- Auth: POST to `https://192.168.6.3:10443/login` with `{username, password}` returns bearer token.
  - Credentials: user=`claude`, pass=`vad4rtf@fvj5zpj3DJH`
- Scrypted uses gRPC-web / Engine.IO RPC, NOT standard REST.
- Managed via: direct docker commands (not Arcane CLI — not installed on NAS). Compose at `/volume2/docker/scrypted-1/compose.yaml`.

## Device IDs in Scrypted
- Device 150 = Backyard camera (`PluginDevice/150`, pluginId `@scrypted/rtsp`)
- Device 151 = Backyard MQTT sensor (dead — broker unreachable)
- Device 149 = OpenCV Motion Detection (`PluginDevice/149`)
- Device 113 = Video Analysis Plugin (`PluginDevice/113`) — stores mixin settings for all cameras
- Device 82 = Doorbell (Vivint RTSP + OpenCV motion)
- Device 21 = Driveway (Vivint RTSP + OpenCV motion)
- Device 104 = Fish Cam (wyze-bridge, no motion)

## ObjectDetector Settings for Backyard Camera (device 150)
Stored as mixin keys under `PluginDevice/113` storage (on `p.storage`, NOT `p.state.storage.value`):
- `mixin:150:149:motionSensorSupplementation`: **"Replace"** ✅ (changed from "Assist" 2026-05-26)
- `mixin:150:149:motionDuration`: **12** (was changed to 3 in prior session; reverted by backup restore; needs re-change if 3s is still desired)
- `mixin:150:149:threshold`: **70**
- `mixin:150:149:blur`: **7**
- `mixin:150:149:area`: **450**
- `mixin:150:149:zones`: Zone 1 with 30 points
- `mixin:150:149:newPipeline`: `"FFmpeg Frame Generator"`

### Doorbell (82) — working reference
- `mixin:82:149:motionSensorSupplementation`: **"Replace"** ✅
- `mixin:82:149:newPipeline`: `"FFmpeg Frame Generator"`

## Constraints & Preferences
- ON_DEMAND=false required — Scrypted prebuffer session needs constant stream availability.
- Motion detection must use Scrypted's built-in frame scanning plugin, NOT MQTT or MOTION_API.
- **CRITICAL**: Scrypted MUST be stopped before modifying LevelDB. Manual edits while running corrupt the database.
- **DB modification pattern**: Copy DB to temp dir → modify → write temp back to host → swap. NEVER `rm -rf` a bind mount directory — it deletes contents before failing on the mount point itself. Use `rm -rf /path/*` then `cp -a temp/. /path/`.

## LevelDB Modification Script Pattern
```javascript
const src = "/host/scrypted.db";
const tmp = "/host/scrypted.db.new";
execSync("rm -rf " + tmp + " && mkdir -p " + tmp + " && cp -a " + src + "/. " + tmp + "/ && rm -f " + tmp + "/LOCK");
const db = new Level(tmp, { valueEncoding: "utf8" });
await db.open();
// ... read/modify/write ...
await db.close();
execSync("rm -rf " + src + "/* && cp -a " + tmp + "/. " + src + "/ && rm -rf " + tmp);
```
Note: storage may be at `p.storage` (top-level) or `p.state.storage.value` depending on DB version. Always check first.

## Reconnect Fix (2026-05-28)
- **Root cause:** After first KWS WS disconnect/reconnect, downstream tracks (`videoTrack`, `audioTrack`) persisted in memory but upstream session was dead. On subsequent reconnects, `canReuse()` only checked track existence → returned true → early-return without creating new WS/peer connection → zero packets delivered (silent failure).
- **Fix:** Added `stream.upstreamAlive.Load() &&` to `canReuse()` check at `whep_proxy/main.go:357`. Now stale streams with dead upstreams are destroyed and rebuilt.
- **Commit:** `78afd56` on main, pushed to Gitea.
- **Verified:** 13+ disconnect/reconnect cycles (every ~10 min from KVS) completed successfully through 18:22 UTC. No stale stream issues.

## GStreamer config-interval History (CORRECTED)
- **Correct semantics:** `config-interval=-1` = send SPS/PPS inline before EVERY IDR frame. `config-interval=0` = SPS/PPS only in RTSP SDP, never inline. `config-interval=N>0` = at most every N seconds.
- **2026-05-29 change:** Changed `-1` → `0` (commit `a565552`). Intent was to fix late-joining RTSP clients. The ACTUAL reason the previous `-1` caused problems was the 3500ms jitter buffer delaying IDR delivery past Scrypted's timeout — NOT the config-interval. The session memory at the time had the semantics backwards.
- **2026-06-02 change:** Changed `0` → `-1` (commit `69b3299`). After a 25-hour camera outage, the stream restarted fresh. Scrypted's prebuffer-mixin couldn't find sync frames with `config-interval=0` because its ring buffer had no inline SPS+PPS+IDR sequences to detect. With `-1`, inline SPS/PPS before every IDR fixes sync frame detection after long outages. `wait-for-keyframe=true` on rtph264depay handles the late-joiner keyframe case.
- **Remaining issue:** Scrypted's prebuffer snapshot (`select='eq(pict_type,I)'`) times out at 1440p resolution on the NAS — HomeKit's 4-second deadline is too short for software H264 decode at that resolution. Scrypted falls back to cached snapshot (acceptable). Not a wyze-bridge issue.

## Next Steps
1. Re-apply `motionDuration: 3` if user wants shorter clips for cats/raccoons
2. Monitor Backyard motion detection stability over time (verify prebuffer errors are gone)
3. Consider fixing Doorbell prebuffer errors (`"Could not find codec parameters"`) for Vivint source — may require Vivint firmware update or Scrypted FFmpeg input args tweak
