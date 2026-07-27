# wyze-bridge KWS WebSocket Auth Fix — Handoff Notes

## Problem
`backyard-cam` shows no video. The whep_proxy connects to KWS signaling via WebSocket, but KWS immediately closes it with `close 1001 (going away)` **before any SDP_OFFER can be sent**. This creates a reconnect loop every ~5 seconds from Scrypted.

## Root Cause
Two issues in `whep_proxy/main.go` function `establishUpstream()` at line 1594:

### Issue 1: auth_token never passed to KWS WebSocket handshake
- `WebRTCConfig` struct (line ~87) has an `AuthToken string` field that IS populated from `/kvs-config/` endpoint
- The JWT token looks like: `{"device":"LD_CFP_D03F276DDB8A","session_id":"...","sub":"WebRTC"}` signed with HS512
- But at line 1609, the WebSocket dial only sends `User-Agent` header — auth_token is never included in the handshake
- KWS needs this token for authentication. Without it, the connection is accepted then immediately closed

### Issue 2: Waiting up to 10s for ICE gathering before sending SDP_OFFER
- Line ~1798: `case <-time.After(10 * time.Second):` — waits for ICE candidates or times out after 10s
- KWS closes idle WebSocket connections quickly; by the time we send SDP_OFFER, the connection is already dead

## What Already Works (verified)
- Container runs on `macvlan_net` with static IP `192.168.6.10` — network is fine
- API auth flow: login → token refresh → payload signing (`sign_payload()`) all succeed
- Camera discovery works, camera status checks pass
- `/kvs-config/backyard-cam` returns full config with `auth_token`, `signaling_url`, 5 ICE servers
- Direct API test at `app.wyzecam.com/app/v4/camera/get_streams` returns 200 with correct payload

## Log Pattern (current broken state)
```
[WHEP_PROXY] Connecting websocket: wss://v-61a22014.kinesisvideo.us-west-2.amazonaws.com/...
[WHEP_PROXY] Upstream session ended for backyard-cam: websocket closed: close 1001 (going away)
```
No SDP messages, no ICE candidates — connection dies instantly.

## Key File Paths
- `whep_proxy/main.go` — **fix target**. Specifically `establishUpstream()` at line 1594 and the SDP_OFFER send logic around line 1780+
- `app/wyzecam/api.py` — auth flow, `sign_payload()`, `get_camera_stream()` (working correctly)
- `app/frontend.py` — routes `/kvs-config/<name>` (line 209) and `/signaling/<name>` (line 194)

## What Needs to Be Done

### Fix 1: Pass auth_token during WebSocket dial
In `establishUpstream()` around line 1603-1609, the WebSocket dial needs to include the auth_token. The KWS signaling URL from `/kvs-config/` already has SigV4 query params (`X-Amz-Algorithm=AWS4-HMAC-SHA256&...`). The auth_token likely needs to be added as an additional query parameter:
```
?X-Amz-Algorithm=AWS4-HMAC-SHA256&...&X-Amz-Token=<auth_token>
```
OR possibly as a header. Need to check the Wyze app's actual WebSocket opening logic (it uses `okhttp/4.12.0` which we already mimic).

The `config.AuthToken` field is available in `establishUpstream()` via `config := stream.getConfig()`. Modify the decoded URL or headers to include it before line 1609.

### Fix 2: Remove or reduce ICE gathering wait
Around line ~1780-1798, there's a blocking wait for ICE candidates before sending SDP_OFFER. KWS closes connections too quickly for this approach. Consider:
- Sending SDP_OFFER immediately after creating the peer connection and setting up handlers (before waiting for ICE)
- OR reducing the timeout significantly (e.g., 1-2 seconds instead of 10s)

### Build & Deploy
After changes, rebuild:
```bash
docker build --no-cache -t wyze-bridge-local:latest . 2>&1 | tee /tmp/docker-build.log &
BUILD_PID=$!; while kill -0 $BUILD_PID 2>/dev/null; do sleep 60; echo "--- building ---"; done; wait $BUILD_PID 2>&1; tail -5 /tmp/docker-build.log
```

Then restart the container. Watch for:
- Successful WebSocket connection (no immediate close)
- SDP_OFFER sent and SDP_ANSWER received
- ICE candidates exchanged
- Video track appearing

## Important Constraints
- Go build fails silently during Docker multi-stage builds if there are unused variables in `main.go` — remove all dead code
- Git author is always `zach@weyland.me`, not `hey@sixteen33.com`
- Secrets at `/volume2/docker/_admin/.secrets/` (700 dir / 600 file) — never commit them
