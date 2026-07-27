# WyzeBridge GST RTSP Setup - Handoff

## Current Status
- **Goal**: Use GST RTSP server for KVS cameras to eliminate stable timestamp issues and packet loss from WHEP→MediaMTX path
- **Issue**: Scrypted prebuffer session failing to connect with `Connection refused` on rtsp://192.168.6.10:8555/backyard-cam
- **Root cause**: WHEP proxy still running and writing Direct RTSP to UDP port 5600, causing sequence gaps
- **KVS stream Never actually starts**: `setup_mtx_proxy` requires WHEP proxy, but the stream initialization fails silently without RTP data flowing to GST RTSP server

## Configuration Changes Made

### 1. Enabled GST RTSP Mode (`/volume2/docker/wyze-bridge-project/app/run`)
```bash
if [ "$KVS_GSTREAMER_RTSP" = "true" ]; then
    whep_proxy &  # Still needed for stream initialization, but only KVS streams use it
fi
```

### 2. Modified RTSP URL Routing (`/volume2/docker/wyze-bridge-project/app/wyze_bridge.py` line 113)
```python
elif not self.gst_rtsp.enabled:  # Keep WHEP proxy for non-KVS or GST disabled
```

### 3. Environment Variable Set (`docker-compose.yml`)
```yaml
- KVS_GSTREAMER_RTSP=true
```

## Current State After Last Build
- GST RTSP server: `running on port 8555`
- WHEP proxy: `running on port 8080` 
- Stream config: `/tmp/gst-rtsp-streams.conf` contains `backyard-cam 5600 0`
- Log shows: `[GST_RTSP] mounted rtsp://127.0.0.1:8555/backyard-cam`
- Log shows: `[WHEP_PROXY] Requested keyframe (direct rtsp startup)` - indicating WHEP is writing Direct RTSP
- Log shows: `[WHEP_PROXY] Direct RTSP sequence gap` - packet loss still occurring

## Problem Analysis

### The Packet Loss Loop
1. WHEP proxy reads KVS stream from WebRTC (MediaMTX port 8889)
2. WHEP proxy writes Direct RTSP to UDP port 5600
3. GST RTSP server reads from UDP port 5600 and serves via RTSP on 8555
4. **BUT**: WHEP proxy's Direct RTSP writer has sequence gaps (packet loss)

### Why Direct RTSP Has Packet Loss
Looking at `/volume2/docker/wyze-bridge-project/whep_proxy/main.go` lines 988-1099:
- The video forwarding loop has `sequence gap` detection (lines 1035-1052)
- Sequence gaps trigger keyframe requests and increment `droppedCount`
- The logic shows: `expected=xxx got=yyy missing=zzz` pattern in logs
- This happens because RTP stream from WHEP has discontinuities before GST RTSP server can consume them

## Next Steps (Priority Order)

### Short Term Fix (1-2 hours)
**Disable WHEP proxy's Direct RTSP writer for KVS streams when GST RTSP is enabled**
- Modify `/volume2/docker/wyze-bridge-project/whep_proxy/main.go` line 1001 to skip Direct RTSP setup
- Or use environment variable check: `if !gstreamerRTSPEnabledForStream(streamID)`
- This makes WHEP proxy read from WebRTC but not write to UDP, avoiding the sequence gaps
- KVS stream needs alternative way to get data to GST RTSP server

### Alternative: Direct TUTK Stream (2-4 hours)
**Bypass WHEP proxy entirely for KVS streams with GST RTSP**
- Modify `WyzeStream.start()` in `/volume2/docker/wyze-bridge-project/app/wyzebridge/wyze_stream.py` line 149
- Skip `setup_mtx_proxy` when KVS and GST RTSP enabled
- Use TUTK connection directly to stream UDP 5600 (requires code changes in wyze-bridge)

### Long Term: Custom WHEP Proxy Build (4+ hours)
**Build new WHEP proxy binary with Direct RTSP disabled globally or per-stream**
- Clone and modify `/volume2/docker/wyze-bridge-project/whep_proxy/main.go`
- Add config flag: `WHEP_DISABLE_DIRECT_RTSP=true`
- Rebuild and test

## Scrypted Configuration Required
For KVS cameras (e.g., Backyard):
- RTSP URL: `rtsp://192.168.6.10:8555/backyard-cam`
- FFmpeg Input Arguments: `-rtsp_transport tcp`
- Enable Prebuffer: ON

For non-KVS cameras (e.g., Fish):
- Keep using MediaMTX on port 8554
- No changes needed

## Critical Files Modified
1. `/app/run` - WHEP proxy conditional startup logic
2. `/app/wyze_bridge.py` line 113 - KVS stream routing
3. `/volume2/docker/wyze-bridge-project/app/run` - Local source (needs Dockerfile rebuild)
4. `/volume2/docker/wyze-bridge-project/app/wyze_bridge.py` - Local source
5. `/volume2/docker/wyze-bridge-project/Dockerfile` - Build configuration
6. `/volume2/docker/wyze-bridge-project/whep_proxy/main.go` - WHEP proxy source (needs Direct RTSP fix)

## Verification Commands After Fix
```bash
# Check GST RTSP server is listening
docker exec wyze-bridge netstat -tlnp | grep 8555

# Verify no sequence gaps after fix
docker logs wyze-bridge | grep -E 'Direct RTSP.*gap|expected=.*got=' | tail -5

# Test Scrypted connection
timeout 10 ffplay -loglevel error rtsp://192.168.6.10:8555/backyard-cam
```

## Known Good Config File
`/tmp/gst-rtsp-streams.conf` (inside container):
```
backyard-cam 5600 0
fish-cam 8002 8003
```
- First column: stream_id (matches Wyze camera name_uri)
- Second column: video_port (UDP port for RTP)
- Third column: audio_port (0 = disabled, or UDP port number)

## Build Process
```bash
cd /volume2/docker/wyze-bridge-project
docker build -t wyze-bridge-local .
# Need to force container recreation due to compose state corruption
```

## Notes
- GST RTSP bridge binary: `/usr/local/bin/gst_rtsp_bridge` (lines 29-68 in gst_rtsp_bridge.c define GStreamer launch pipeline)
- The pipeline reads from `udpsrc port=5600` and serves via RTSP
- Direct RTSP writer in WHEP proxy (lines 988-1099) is the packet loss source
- MediaMTX remains active on port 8554 for non-KVS cameras
- Cannot merge ports due to conflicting stream types (KVS uses TUTK, non-KVS uses MediaMTX)

---
**Last Updated**: 2026-05-13 22:09 UTC
**Active Build**: wyze-bridge-local (rebuilt 14 minutes ago)
**Blocker**: WHEP proxy Direct RTSP writer sequence gaps
