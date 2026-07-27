# Handoff: backyard-cam Video Stream Investigation - June 11, 2026

## Current Status (as of last chat)
- **Branch:** `v4-merged/idisposable-improvements` on GitHub (remote origin)
- **Container:** `wyze-bridge` (running custom Go binary deployed via `docker cp`)
- **Problem:** Backyard-cam upstream WebRTC connects successfully, receives H264+audio tracks from KWS, but:
  - MediaMTX only sees audio track (not video)
  - Video drop rate ~7% (1816/25k frames) vs ~0.01% yesterday when it worked
  - `video_ready` stays `false` constantly despite upstream receiving H264 tracks

## What's Actually Working Now
- Upstream WebRTC connection IS established (`upstream_alive=true`)
- KWS sends both **H264 video** + **PCMU audio** to whep_proxy (confirmed by track logs at 20:43:53)
- Video samples arrive with SPS/PPS keyframes; RTP stats show ~2.6k frames written to WHEP clients
- Direct RTSP forwarding enabled on port 5600 (Go side confirms "Direct RTSP video forwarding enabled")
- GStreamer RTSP bridge mounted: `rtsp://127.0.0.1:8555/backyard-cam (video=5600 audio=5601)`

## What's Broken
- MediaMTX only sees **audio** track, not H264 video track today
- Video drop rate is ~7% vs <0.01% yesterday when it was working perfectly
- `video_ready` stays `false` constantly despite upstream receiving H264 tracks

## Timeline: June 10 (Working) → June 11 (Broken)
**June 10, 21:16 - Video WAS flowing to MediaMTX:**
```
[GST_RTSP] mounted rtsp://127.0.0.1:8555/backyard-cam (video=5600 audio=5601)
[WHEP_PROXY] Direct RTSP video forwarding enabled for backyard-cam on udp port 5600
INF [RTSP] [session ...] is reading from path 'backyard-cam', with TCP, 2 tracks (G711, H264)
RTP stats: read=100000 written=11536 dropped=7 clients=2  (<0.01% drop rate!)
```

**June 11, 20:43 - Video NOT flowing to MediaMTX:**
```
[GST_RTSP] mounted rtsp://127.0.0.1:8555/backyard-cam (video=5600 audio=5601)
[WHEP_PROXY] Direct RTSP video forwarding enabled for backyard-cam on udp port 5600
RTP stats: read=25000 written=2624 dropped=1816 clients=2  (~7% drop rate!)
MediaMTX only sees audio track
```

## Changes Made Between Working and Broken States
1. **SDP timeout** increased from 20s → 45s in `whep_proxy/main.go:1778` (commit 835d907) - confirmed NOT root cause
2. **Wake success check** fixed to use `resultList[0].code == 1` instead of `result == 'ok'` (commit bd30894)
3. **Cooldown/retry logic** added: 600s KVS cooldown, 12s post-wake delay, 15s fast-fail retry window (commit 4924af8)
4. **Go binary deployed via `docker cp`** to running container + Flask restart

## Root Cause Hypothesis
The timing mismatch between Go forwarding RTP on port 5600 and gst_rtsp_bridge starting to listen is likely causing packet loss:
- Python's `setup_mtx_proxy()` times out after 20s without `video_ready=true`
- Go receives track at ~20:43:53 but gst_rtsp_bridge may have been restarting around that time
- UDP packets sent before listener starts are silently dropped
- The Flask restart (via docker cp) may have triggered a GST_RTSP server restart cycle

## Key File Locations
| File | Purpose |
|------|---------|
| `whep_proxy/main.go:1608-1665` | `OnTrack()` callback - sets video source, starts forwarding goroutine |
| `whep_proxy/main.go:951-1041` | `forwardVideoTrack()` - reads RTP, forwards to both WHEP clients and direct RTSP |
| `whep_proxy/main.go:211-263` | `loadGStreamerRTSPStreamConfig()` / `gstreamerRTSPEnabledForStream()` |
| `app/wyzebridge/gst_rtsp_server.py:85-94` | `write_config()` - writes `/tmp/gst-rtsp-streams.conf` with port mappings |
| `app/wyzebridge/wyze_api.py:755-814` | `setup_mtx_proxy()` - Python side setup, 20s wait for video_ready |
| `whep_proxy/main.go:361-369` | `setVideoSource()` / `setAudioReady()` - sets ready flags |

## Debugging Commands
```bash
# Check if gst_rtsp_bridge is listening on port 5600 right now
docker exec wyze-bridge ss -ulnp | grep 5600

# Check GST_RTSP server restart events today
docker logs wyze-bridge 2>&1 | grep -iE "gst.*rtsp|GST_RTSP" 

# Compare current main.go vs yesterday's working version in git
git log --oneline -- whep_proxy/main.go

# Check if fish-cam has same issue today (was also affected by our changes)
docker logs wyze-bridge 2>&1 | grep -i "fish-cam.*video" | tail -20

# Add debug logging in forwardVideoTrack() around rtspSink.Write() call
# to confirm packets are actually being sent on port 5600
```

## Next Steps for New Chat
1. **Check if gst_rtsp_bridge is listening on port 5600 right now:**
   ```bash
   docker exec wyze-bridge ss -ulnp | grep 5600
   ```

2. **Compare current main.go vs yesterday's working version in git** to find what changed:
   ```bash
   git log --oneline -- whep_proxy/main.go
   git diff HEAD~3 -- whep_proxy/main.go
   ```

3. **Check if fish-cam has same issue today** (was also affected by our changes)

4. **Verify gst_rtsp_bridge process health:**
   ```bash
   docker logs wyze-bridge 2>&1 | grep -i "gst.*rtsp\|gst_rtsp_bridge"
   ```

5. **Add debug logging in forwardVideoTrack()** around the `rtspSink.Write()` call to confirm packets are actually being sent on port 5600

6. **Consider adding a startup synchronization step** where Go waits for gst_rtsp bridge to be ready before forwarding first RTP packet (or buffer initial keyframe until listener confirmed)

## Environment
- Docker compose stack at `/volume2/docker/wyze-bridge/`
- Build command: `docker build --no-cache -t wyze-bridge-local:latest .`
- Container name: `wyze-bridge`
- WHEP proxy listens on port 8080 (Go binary)
- Flask app listens on port 5000 (Python)
- MediaMTX handles RTSP/WebRTC forwarding

## Git History Context
```
bd30894 - Fix wake success check: use resultList[0].code == 1 instead of result == 'ok'
4924af8 - Add cooldown/retry logic for KVS wakes (600s cooldown, 12s post-wake delay)
835d907 - Increase SDP_ANSWER timeout from 20s to 45s
```
