# Wyze Android 3.19.0.935 — APK Reversing / Drift Study (started 2026-08-16 night)

Goal: decompile the latest public Wyze Android build, diff its internal credential/signaling/stream
constants against our fork's reverse-engineered flow (auth → DMS wake → KVS MediaReception), and
assess breakage risk + learn the "real" current protocol so the bridge can be redone more faithfully.

## Toolchain (all under /tmp/opencode — ephemeral, re-download if gone)
- JDK: `/tmp/opencode/tools/jdk/bin/java` (Temurin 21 headless)
- jadx: `/tmp/opencode/tools/jadx/lib/jadx-1.5.3-all.jar` — MUST run via CLI class, not `-jar`
  (Main-Class is the GUI → HeadlessException on server):
  ```
  nohup /tmp/opencode/tools/jdk/bin/java -Djava.awt.headless=true \
    -cp /tmp/opencode/tools/jadx/lib/jadx-1.5.3-all.jar jadx.cli.JadxCLI \
    --no-res -d /tmp/opencode/wyze/decompiled /tmp/opencode/wyze/wyze-3.19.0.935.apk \
    > /tmp/opencode/wyze/jadx.log 2>&1 &
  ```
- APK: `/tmp/opencode/wyze/wyze-3.19.0.935.apk` (264,001,589 bytes, MD5 `8276aff51ec49e28e6f92acc16ca8f05`)
  - Sourced from Aptoide pool URL (APKMirror variant/download pages are Cloudflare-managed-challenge).
- Extracted dex: `/tmp/opencode/wyze/extracted/` (37 files: classes.dex..classes36.dex + resources.arsc)
- Decompiled Java: `/tmp/opencode/wyze/decompiled/sources/` (~68,356 .java; 199 minor decode errors — ignore)
- **APK contains ZERO `.so` files** (universal single-ABI build). All native code is CDN-delivered at runtime.

## Version timeline (public releases, from Play/Aptoide listing)
| build | date | note |
|---|---|---|
| 3.14.0.807 | 2026-05-27 | **what our fork emulates** (`APP_VERSION=3.14.0`) |
| 3.15.0.876 | 2026-06-16 | |
| 3.16.0.890 | 2026-06-25 | |
| 3.17.0.904 | 2026-07-14 | |
| 3.18.0.918 | 2026-07-23 | |
| **3.19.0.935** | 2026-08-11 | latest; decompiled here (vercode 62636, pkg `com.hualai`) |

## Byte-scan of extracted dex: constants present vs missing in 3.19
### Still PRESENT (fork's core identity intact)
- appid `9319141212m2ik` — 41 hits
- string `wyze_app_secret_key_132` — classes5.dex, but in 3.19 it is only an **analytics stat-index constant** (`WyzeEventStatIndex.SECRET_KEY_APP`, camera/doorbell/event/plugins) — NOT proof the HMAC secret is hardcoded. The actual signer secrets live only in the CDN .so. (Our fork happens to use this same string as its HMAC seed; server accepts it today.)
- DMS id `cfpp_fb0fdcbb42204523` (LD_CFP) — 4 hits
- sc `9f275790cab9...` — 3 hits; sv for get_event_list `782ced69...` — classes15.dex:1
- endpoints: all 6 of fork's set incl. `/v4/camera/get_streams`, `action/run_action` (15), `get_iot_prop` (22), `signaling/device` (4)

### MISSING from dex entirely
- x-api-key value `WMXHYf79...`; sv-default `e1fe3929...`
- sc `01dd431d...` + its three paired svs: run_action `2c0edc...`, get_device_Info `0bc2c3be...`, set_device_Info `e8e1db44...`
- `use_trickle` — string absent even bare

## SIGNING LAYER (fully mapped)

### Current HTTP signing service: `com.wyze.platformkit.network.WpkSignature2Service`
Headers it adds per request:
| header | value |
|---|---|
| `appid` | e.g. `9319141212m2ik` (per-call) |
| `access_token` | Center.access_token raw |
| `appinfo` | `<Center.app_name>_android_<Center.app_version>` → `wyze_android_3.19.0.935` |
| `phoneid` | device id |
| `requestid` | **MD5(MD5(nonce))** — double MD5 of ms timestamp string |
| `Signature2` | native signature (below) |

- POST/PUT: body = compact-sorted params JSON with injected `nonce` (ms); GET: sorted `k1=v1&k2=v2...` + nonce appended.
- NO sc/sv headers, no env header on this service.

### Legacy signing service: `WpkWyzeSignatureService` (header key `signature`)
`packageHeader()` = same base set; then either dynamic (`isDynamicSignature(true)` → Signature2) or old static signature under `signature`. Used by `AgoraApiUtils` for `/app/v4/device/wakeup`, `/app/v4/wcsa/*`.

### The signer is NATIVE — and NOT in the APK
- `WpkSecurityUtil.getSignature(appId, payload)` → JNI `WpkSecurity.getSignature(appId, Center.access_token, payload)`; also `getOldSignature(...)` and `getDataByString(...)`, all `native`; loads lib **`wyze_olive-lib`**.
- **No `.so` in the APK** → the signer .so (algorithm + secret table) is downloaded from a CDN at runtime.
- Consequence: Wyze can silently rotate signature algorithm/secrets per-appid without any app release. This is the single biggest future breakage vector and the one thing we CANNOT see statically. Our fork's md5+HMAC-MD5 scheme works today ⇒ server still accepts it for our endpoints; risk = async rotation via .so push.

## DMS IN 3.19: PLAIN BEARER TOKEN — sc/sv ELIMINATED
`com.wyze.camerasdk.network.net.services.settings.repo.RemoteSettingsDataSource.packageDeviceManagementHeaders()`:
```
Authorization : <access_token>          (raw, not "Bearer ..." prefix on this path)
appinfo       : ServiceCenter.app_ver   → format "com.hualai.WyzeCam___3.19.0.935"
phoneid       : Center.phone_id
nonce         : currentTimeMillis (string)
requestid     : MD5(MD5(nonce))
```
**No appid, no sc/sv, no signature headers at all.** Our fork already sends the same `app_ver` format for DMS. Interpretation of the missing sc/sv trio: DMS stopped per-endpoint signing entirely (or moved it server-side). Old values we found still in dex are repurposed elsewhere (see below), not used on the camera DMS path.

### Where old sc/sv hex survived
- `9f275790cab9...` (old default sc) now pairs with **new** sv `c86fa16fc99d4d6580f82ef3b942e586` under appid **`lsvp_627dad6585fa6`** — a doorbell/linkage ("chime") service, as request PARAMETERS not headers.
- camplus device_list property endpoints still carry per-call sv values (WyzeCloudApi): `1df2807c...` (get_property_list), `44b6d564...` (set_property), `ddb9baef...` (set_property_list).

## AUTH CHAIN
- **Android 3.19 app login = browser OAuth PKCE** (`defpackage/fjt.java`): client_id `68900a09-abfa-472d-a30d-27ae53c205a6`, S256 code challenge, redirect `wyze://wyzeapp:8080/v2?id=wyze/auth/login`; token at `https://auth.wyze.com/oauth/token`. client_secret itself decoded natively via `WpkSecurity.getDataByString(client_id)`.
- **HMS person-hub** (`com.wyze.hms.WyzeHmsApi`, 996 lines): host `https://auth-prod.api.wyze.com` (beta: auth-beta, test: gamma.../auth); header `X-API-Key`; keys in class: official `RckMFKbsds5p6QY3COEXc2ABwNTYY0q18ziEiSEm`, another prod value `DlqfzAbf1N4ppJpq31WHT7al5z4pBuhE8qaOjwAY`; new appid **`hmss_b172509edbf6948f`**.
- **Our fork login** (api.py): POST `https://auth-prod.api.wyze.com/api/user/login` {email, password: triple-md5} + per-account developer `keyid`/`apikey` from env → still accepted by the server today. Legacy path alive; no urgency.
- App identity in 3.19: `Center.app_name="wyze"`, version from PackageInfo ⇒ UA/appinfo `wyze_android_3.19.0.935`. Fork spoofs `wyze_ios_3.14.0`.

## THE 3.19 LIVE-VIEW FLOW (complete, camera path)
All under host **`https://app.wyzecam.com`** (`AppConfig.BASE_URL_NEW`; beta/test variants exist).
Two clients coexist in camerasdk: legacy `WebRtcClient` and newer Agora-based `AgoraApiUtils`.

### 1. Wake — `POST /app/v4/device/wakeup` (AgoraApiUtils.o)
Signed via WpkWyzeExService appid `9319141212m2ik`, dynamic Signature2:
```json
{ "device_id": "<mac>", "device_model": "<model>",
  "params": { "mode": <int>, "user_id": Center.user_id,
              "rtc_client_uid": <per-device cached int (agora/rtc_uid)> } }
```
Fork today: DMS `run_action` functions[{name:"wakeup", in:{wakeup-live-view:true}}] w/ LD_CFP sc/sv. Both alive server-side; Wyze's app path moved to the direct endpoint + RTC-uid binding (the uid is cached per device and reused across wake→stream).

### 2. Stream info — `POST /app/v4/camera/get_streams`
Via WpkSignature2Service appid `9319141212m2ik`. Body:
```json
{ "device_list": [ {"device_id","device_model","provider"} , ... ] }   // batch; provider e.g. "webrtc"
```
(single-device overload also takes a per-device `parameters` map — that's where older builds put use_trickle).
**Response model (`WpkStreamInfo`) — NO KVS URLs ANYMORE:**
```
{ code, msg, ts, data: [ Stream ] }
Stream { deviceId, provider,
         params: ParamsBean,      // ← WebRTC session material (see below)
         property: PropertyBean } // only iot-device::iot-power / iot-state flags
ParamsBean { accessId, accessToken, authToken, channel, dtls:int, encryptionMode:int,
             encryptionKey, encryptionSalt, enr, expireTime:long,
             iceServers:[{url,username,credential}], p2pId, p2pKey, parentDeviceId,
             parentDtls, parentEnr, parentP2pId, signalingUrl }
```
This is Agora-style material: P2P attempt (p2p*/parent* + enr) with cloud-relay fallback (channel/authToken/accessId = Agora Cloud), ICE servers included. The app then runs its own Agora/WebRTC stack (native lib, CDN-delivered).

### 3. WebRTC session
- signalingUrl from the get_streams response itself — **3.19 no longer calls `/signaling/device/{mac}?use_trickle=true` for cameras**. That endpoint's host constant (`https://prod-webrtc.api.wyze.com/signaling/device/`) survives in `NeptuneCenter` but is only wired to the Neptune doorbell flow. No camera caller found in decompiled code.
- Legacy non-KVS path in OUR fork still uses it (works; probably version-gated on server).

### 4. New WCSA endpoints (AgoraApiUtils) — Wyze Cloud Signaling & Admission, all `/app/v4/wcsa/*` appid 9319141212m2ik
- `create-connection` {device_id, device_model} (+uid in renew)
- `renew-token` {device_id, device_model, uid:int}
- `start-playback` {device_id, device_model}

### 5. KVS is REPLAY-ONLY in 3.19
`KvsAPI` (host `kvs-service.wyzecam.com`) exposes exactly two endpoints:
- GET `app/live_replay_url?device_id&product_model&start_time&nonce[&resource_version]` → KVSReplayUrlOutPut
- GET `app/replay_url?...&end_time...`
(plus `get_image_url` for snapshots). Live media no longer flows through KVS MediaReception in the Android app.

## FORK vs 3.19 — endpoint-by-endpoint status (as of this study)
| fork call | host/path (fork) | 3.19 equivalent | drift verdict |
|---|---|---|---|
| login | auth-prod `/api/user/login` + keyid/apikey env keys | OAuth PKCE browser flow; HMS appid hmss_… | **Legacy path still served.** Low urgency; watch for deprecation of per-account dev keys. |
| refresh | api.wyzecam.com/app `/user/refresh_token` | (token lifecycle via oauth now) | alive today |
| wake | DMS `run_action` wakeup fn, LD_CFP+sc/sv | direct `/app/v4/device/wakeup` w/ rtc_client_uid | both served; fork's fine. Note: Wyze binds rtc uid at wake — our WebRTC client picks its own uid (works because server tolerates) |
| get_streams | app.wyzecam.com/app `/v4/camera/get_streams`, body + `parameters:{use_trickle:true}` per device, appid 9319141212m2ik | identical path/appid; batch entries {device_id,device_model,provider} (optional parameters) | **body shape matches.** Response: fork parses partial model (signaling_url/auth_token/ice_servers); server returns full Agora params. Server still fills signalingUrl for our spoofed client today |
| signaling (non-KVS cams) | GET webrtc.api.wyze.com `/signaling/device/{mac}?use_trickle=true` Bearer | constant moved to `prod-webrtc.api.wyze.com`; only Neptune doorbell uses it in-app; `use_trickle` gone from dex | **endpoint likely version-gated on server**; switch host to prod-webrtc when/if it 404s. Param may still be accepted |
| DMS props (get_iot_prop etc.) | DMS + per-endpoint sc/sv headers | DMS plain Bearer+appinfo/phoneid/nonce/requestid, no sc/sv | **fork's extra sc/sv tolerated**; 3.19-style header set is the forward-compatible one |
| events | (sc/sv get_event_list `782ced69...` still in dex) | unchanged for that appid | low risk |

### Risk ranking (breakage → impact)
1. **CDN-delivered native signer rotation** (`wyze_olive-lib`) — invisible to us; could change Signature2 algo/secrets per appid server-gated. Impact: total if we don't track it. Mitigation: monitor 401/invalid-signature spikes; mirror header set of WpkSignature2Service exactly (fork mostly already does) so a server-side "require new scheme" switch hits us at the algorithm level, not shape level.
2. **KVS live-stream response field removal** — if server ever stops returning signalingUrl for ios-3.14 clients (full migration to Agora-only), fork's media path dies overnight. Mitigation: implement Agora/WebRTC session from ParamsBean fields (standard WebRTC + their relay; p2pId/p2pKey = direct candidate exchange).
3. **prod-webrtc host switch / use_trickle removal** — non-KVS camera signaling only. Easy fix when it bites (host swap, drop param).
4. **Legacy dev-key login deprecation** — would require re-implementing OAuth PKCE (browser flow) or keeping a per-account key until forced out.

## What "redoing the app faithfully" would mean (3.19-native architecture)
- Auth: OAuth2 PKCE via in-app browser (client_id 68900a09-abfa-472d-a30d-27ae53c205a6; client_secret natively derived — we'd need the .so or a captured token flow), OR keep legacy key login as long as served.
- Wake: `/app/v4/device/wakeup` with stable per-device rtc uid (generate once, persist).
- Stream: `get_streams` → ParamsBean → run real WebRTC: offer/answer over `signalingUrl` (wss), ICE from iceServers, P2P via p2pId/p2pKey when available else Agora relay (channel/authToken/accessId + encryptionMode/Key/Salt). Media out to GStreamer RTSP as we do now. This would make us immune to KVS deprecation and match what every modern Wyze client does.
- Device control: DMS with plain Bearer header set (drop sc/sv dict) — simpler AND forward-compatible; or adopt `/app/v4/iot3/run-action` for the new plane.

## iot3 plane (`IotV3Constants`, host `https://app.wyzecam.com`)
`/app/v4/iot3/{run-action, run-action-sync, get-property, set-property, set-property-retain, set-property-noiot, action-history, property-history, daily-metrics-history, event-history, get-shadow, get-app-setting, get-connection-token}`

## Neptune / AN_RSCW note
Fork lists `AN_RSCW` = "Battery Cam Pro" (BATTERY_CAMS). In 3.19 the **Neptune** plugin family controls an AN_RSCW-targeted device through *different* hosts: jupiter (`service.jupiter.wyzecam.com/plugin/doorbell/v2/getActionResult`, `/plugin/doorbell/set_iot_prop[_by_topic]`), DMS host `devicemgmt-service(-beta).wyze.com` with appid **`rscp_f1f18d470bb7658d`**, and connection-status at `camera-connection-monitor.wyzecam.com/plugin/video/connection/status`. If a newer AN_RSCW hardware revision stops answering classic DMS/WSS, this is where its control flow now lives.

## Fork endpoint × 3.19 reference cross-check (grep of decompiled tree)
| fork call (host/path) | refs in 3.19 | verdict |
|---|---|---|
| api.wyzecam.com/app `/user/get_user_info` | 3 files | alive (WpkDeviceManager etc.) |
| app `/user/refresh_token` | 1 file | alive |
| app `/v2/home_page/get_object_list` | 3 files | alive — camera listing matches current app |
| app `/v2/auto/run_action` (`/app/v2/auto/run_action`) | 4 files (WpkAutoManager, batch OTA) | alive; used for platform-level actions in-app |
| app `/v2/device/set_property`, `set_device_info`, v2 `get_device_Info` | all present (GlobalEditionApi/WpkDeviceManager) | alive — fork's post_device v1/v2 paths match current plane |
| **app-core.cloud.wyze.com/app** `/v4/device/get_event_list` + get_event/_tags/sub_devices/wakeup | kmplugin/eventplayer/event/cloud (3 files each) | **alive — this IS the 3.19 events-fetching plane**; our fork's api_version=4 mapping is current |
| DMS run_action / get_iot_prop | 2 / 21 files | alive (settings pages + Neptune) |
| app `/v4/camera/get_streams` | 1 file (WpkStreamManager — the only stream path in-app) | alive, exact match to fork's call |
| auth-prod `/api/user/login` (email/pw+keyid/apikey) | **0 files** | legacy backdoor: app uses OAuth PKCE; server still serves us. Expect eventual sunset of per-account dev keys |
| webrtc.api.wyze.com signaling `?use_trickle=true` | host constant survives (`prod-webrtc...`) but 0 camera callers; `use_trickle` string gone | dead for cameras in-app; fork uses it only for non-KVS cams — and **our deployment is all-KVS (LD_CFP/HL_CAM4) so this path is unused code for us** |

## 3.19 production host inventory (grep of https:// literals, top hosts)
app-resource.wyze.com (834, static CDN), app.wyzecam.com (89; beta-app / test-api variants), wyze-device-binding S3 buckets (firmware/OTA assets), devicemgmt-service(-beta/-gamma).wyze.com, kvs-service.wyzecam.com, **core-api.brain.wyze.com** (AI automation rules — WyzeAIRulePlatform, not camera media), **wyze-mars-service.wyzecam.com** (event pipeline: thumbnails WssCpEventPlatform, clips, onboarding/feature-gate `/external/v1/onboarding/*`, `/features-restriction`; event layer uses its own appid `wyze_event` in addition to 9319…), **prod-service.snowberg.wyzecam.com** (doorbell "uranus" flow + `com.wyze.sun` device API), wyze-platform-service.wyzecam.com (camplus device_list property endpoints, per-call sv values), ai-face-recognition-v2-api / ai-wkg-api / ai-subscription-service (AI features), wyze-membership-service / wyze-mars membership checks (`check_service_available_by_device` = Wyze Plus gating), auth-prod/beta/gamma.api.wyze.com + hms-gamma, camera-connection-monitor.wyzecam.com.
Note: plugin families `plugin/venus|earth|sun/set_iot_action` are **non-camera IoT categories** (venus = sweep robot, earth/sun = other device classes) — NOT the camera control plane; cameras stay on DMS + iot3 + WCSA.

## Event push architecture (why app notifications are instant; what it means for us)
- Motion/event push = **FCM**. `com.hualai.home.fcm.FCMService.onMessageReceived` → filters Klaviyo/Braze marketing msgs → `WyzeFcmReceiver` reads data keys `event_id`, `event_ts`. App then refreshes event list / notifies locally.
- FCM token registration: `/user/set_push_info` (+ per-device `device/set_push_info`) on the cloud API; token rotation via EventBus `event_msg_push_token_refresh` (SHApplication). Event layer also has its own appid **`wyze_event`**.
- **Implication for the fork**: we cannot consume FCM directly (needs a Google-registered client id). Polling `/v4/device/get_event_list` remains our realtime mechanism — it is exactly what every 3.19 client refreshes after an FCM nudge, so polling stays compatible with how Wyze's own clients operate post-push.
- Gwell (`plugin/{service}/gen_user_token`, `regist_gw_user`) = plugin-scoped user-token registration (gateway plugins), not camera events. AWSIotConnectHelper = MQTT for specific IoT plugins via `/plugin/%s/get_iot_connect_info` temp credentials — not the camera event path either.

## Legacy CloudApi surface still alive in 3.19 (`com.HLApi.CloudAPI`)
- `CloudApi.java` (1946 lines): full legacy REST client — device CRUD, share/accept, binding tokens (`device/get_binding_token`, sub-device), scenes (`scene/*`), **automations** (`auto/add_auto`, `upload_auto_action`, `get_auto_list`...), alarms (`device/delete_alarm_info`), logs (`log/get_device_state_log_list`), push info get/set.
- Login: `CloudApi.userLogin()` / `userLogin2FA()` → HttpModel.request(CLOUD_LOGIN, urlProps("URL_LOGIN")...) — URLs injected from runtime properties (PropertiesTool). So **email/password + 2FA login is still client-side code in 3.19** alongside OAuth PKCE; our fork's auth approach (`auth-prod /api/user/login` w/ keyid/apikey) matches this lineage, not an orphaned one. `oauth.wyzecam.com/login` host constant exists but has zero callers (dead).
- New hosts surfaced: `upgrade-api.wyzecam.com:8605` (firmware update), `predeploy-api.wyzecam.com/app/`, `ai-anything-recognition-api.wyzecam.com` (+ gamma API-gateway variant).
- Internal next-build hint: `SHApplication.u()` writes `Center.app_version = "3.20.0.b936"` — 3.20 beta already in progress as of this APK.

## Small items resolved
- `rtc_client_uid` (wake params) = `new Random().nextInt(Integer.MAX_VALUE)` generated once per device and cached (`WpkDeviceScopedCache` "agora/rtc_uid"). No derivation to match — any stable positive int works (matters only if we ever switch wake calls to `/app/v4/device/wakeup`; our DMS wake doesn't carry it).

## Open items / next steps

1. ~~WpkSecurity algo~~ → native+CDN, not statically obtainable. ACCEPTED LIMITATION; monitor behaviorally.
2. Optionally capture a real 3.19 `get_streams` response from a device (mitm) to diff ParamsBean field values (enr format, encryptionMode values).
3. Decide: keep legacy KVS path + add prod-webrtc fallback now (cheap), vs start Agora/WebRTC session work (bigger win, bigger lift). → recommend cheap one tonight if time; Agora = separate project.
4. Neptune/doorbell AN_RSCW flow (soj.java) — only relevant if Zach adds the new Wyze doorbell model; endpoints: `/plugin/doorbell/v2/getActionResult`, set_iot_prop(_by_topic), connection-status `rscp_f1f18d470bb7658d`.
5. iot3 plane (`/app/v4/iot3/*`) — document when/if we want to move DMS actions forward-compatible.

 ## On-device RTSP feature (camSDK) — found 2026-08-17, fourth pass

 Wyze ships an **on-device RTSP server toggle** in the new camSDK stack. This is what appears as a capability on the backyard cam's beta firmware and is the likely escape hatch from KVS media entirely: once enabled, the camera serves plain RTSP (per-user credentials, optional TLS via RTSPS) and any NVR/Scrypted can pull it like an ordinary IP camera — no Agora/KVS involved.

 ### UI + flow
 - Package `com/wyze/plugin/camsdksetting/rtsp/` (`page/`, `utils/`, `viewmodel/`). Setup sequence: user → password (with "auto-fill" = random credentials) → secure-mode toggle (RTSP vs RTSPS). Subsequent visits edit pwd / mode.
 - After any save the UI shows the camera's reported stream URL (log line `"Save pwd success, ... Url: <linkUrls[0].url>"`, `CamsdkRtspViewModel.java:187`) — i.e. **the full RTSP endpoint incl. user/pwd is displayed in-app**. The camera's LAN IP therefore is NOT hardcoded anywhere; the device reports it itself via the property response.

 ### Firmware gating
 - `WpkCamplusHelper.filterRTSP()` (com/wyze/platformkit/component/service/camplus/utils/WpkCamplusHelper.java) drops devices whose firmware starts with `4.19.` / `4.20.` / `4.28.` / `4.29.` → feature requires newer fw; consistent with it only appearing on the beta-firmware backyard cam (LD_CFP).

 ### Property model
 - `com/wyze/camerasdk/core/property/RtspInfoCameraProperty.java`: `{ name, pwd, rtspMode: OFF|RTSP|RTSPS, linkUrls: [{url, resolution}] }`. Wire property ID = class simple name `"RtspInfoCameraProperty"` (from `defpackage/pdf.java` default `getId()`).
 - Response model def `b1n`: `{name, linkUrls[]bwg{url,resolution}, rtspMode:int?, result:int}`.

 ### Transport: direct camera session — NOT cloud/DMS
 - No `::rtsp` DMS property strings exist anywhere in dex. Get/set go through `ConnectionBaseViewModel` (com/wyze/plugin/camsdksetting/viewmodel/): `iCameraVideoView.s(property, listener)` / `.E(...)`, 15s timeout, pending-queue when session not connected, all pending fail on disconnect ("Camera disconnected").
 - So: enabling RTSP requires an established camsdk/hualai P2P session with the device. This is the *new* direct-connect protocol — distinct from the legacy TUTK path our fork uses for non-KVS cameras (`app/wyzecam/tutk/`).

 ### Wire codec (defpackage/z0n.java, extends u9f)
 - Message type IDs: **10729 / 10731** (`super(10729, 10731)`). Codec base `u9f`: fields a/b = the two ids.
 - GET request payload: protobuf envelope `qnm.a()` → `uz3.i0()` (empty message).
 - SET request payload: protobuf `rnm` → `uz3.R(JSONObject)`, JSON keys `"name"`, `"pwd"`, `"rtspMode"` — **only non-null fields are encoded**, so partial updates work. rtspMode int mapping in codec switch: 1→RTSP, 2→RTSPS, else OFF (OFF sent as 0).
 - Response decode (`mtm`): requires `result == 1`; parses name/rtspMode/linkUrls into RtspInfoCameraProperty; a second response variant decodes via `otm`.

 ### ViewModel behavior (CamsdkRtspViewModel.java)
 - `G(deviceId)` → GET empty property = "getRtspInfo".
 - `I(props, isFirstSetup)` → save name+pwd+mode; **first setup also POSTs the sinker flag below**.
 - `L(pwd)` / `M(secure, autoFill)` → partial saves (pwd-only / mode-only); auto-fill re-posts sinker=true.
 - `H()` → remove: sinker=false + set rtspMode OFF.

 ### Cloud "sinker" endpoint (telemetry/flag only)
 - `POST https://<app_wyzecam host>/app/v4/sinker/set`, appid `9319141212m2ik` (WpkWyzeExService, dynamic Signature2), body `{deviceId, deviceModel, value:bool}` (`CamsdkRtspViewModel.java:279-291`). Sent on first setup / auto-fill (true) and remove (false). Response only logged — not gating. This is the ONLY cloud touchpoint; all real config is on-device over P2P.

 ### Fork implications
 1. **Zero-code path**: enable RTSP once via the Wyze app UI → note/copy the displayed URL → point mediamtx at `rtsp://user:pwd@<cam-ip>:port/<path>` directly (same-LAN pull, WB host is on 192.168.6.x). wyze-bridge stops being a media source for this cam; Scrypted/HomeKit can consume it as a plain IP camera (also sidesteps the prebuffer/keyframe fight if we feed RTSP straight in).
 2. **Full fork path**: implement the hualai direct session + z0n codec (10729/10731) from Python to enable/poll the property ourselves. Non-trivial: new P2P protocol stack, not compatible with existing tutk module; protobuf envelopes (`uz3.java`, `mtm`/`otm` field numbers) still need pinning down.
 3. Unknowns before either path: does LD_CFP's beta fw actually run the RTSP server on LAN (vs VPN/relay only), what port/path linkUrls returns, and whether enabling it interacts with KVS streaming (likely independent).

  ## Live probe results (backyard-cam 192.168.4.223, LD_CFP fw 2.2.1.1345) — 2026-08-17

  **Device-side facts**
  - Only two ports open on the LAN: **554 (native RTSP)** and **8000 (HTTP+RTSP-tunnel wrapper)**. Serial full-port scan; parallel scans get rate-limited by the device. No SSDP/UPnP responders.
  - :554 is a live555 server, always-on regardless of app state: `OPTIONS` → 200 (unauth), every path (`/`, `/live`, `/stream`, …) DESCRIBE → `401 Digest realm="LIVE555 Streaming Media"`. Uniform 401 for all usernames — no user enumeration. So the RTSP server runs by default; only credentials are gated.
  - :8000 speaks raw RTSP on fallback AND offers an HTTP tunnel: OPTIONS `/` → `Access-Control-Allow-Headers: x-sessioncookie, …`; **any non-empty** `x-sessioncookie` value + GET / → `200 OK Content-Type: application/x-rtsp-tunnelled`, then raw RTSP over the same socket. Still hits the identical Digest 401 — no auth bypass. String "x-sessioncookie" appears nowhere in dex (not driven by this APK; firmware-native or another app generation).
  - **Credential guessing failed across ~15 candidates** (admin/wyze/root/empty × 888888/admin/enr/p2p-id/mac-lowercase): per-device random password, only obtainable via the vendor "ask" protocol.
  - Cloud object for this cam HAS P2P fields: `p2p_id=LD_CFP_D03F276DDB8A` (= its `mac` field), `enr=3442e655a16a4992`, `dtls=1`.
  - Cloud media payload (`/v4/camera/get_streams`, provider=webrtc) is **pure AWS Kinesis Video Streams**: signed wss signaling URL + ICE/TURN only. No RTSP info anywhere in the API surface we can reach.

  **"Emulate the app's ask" — attempted and blocked**
  - Legacy TUTK path (fork already implements it: `K10604GetRtspParam` → reply embeds switch byte + full `rtsp://user:pwd@…` URL; `K10600SetRtspSwitch(value=1|2)`): session itself fails with **`IOTC_ER_UNLICENSE`** on both connect paths (`iotc_connect_by_uid_ex` DTLS, and `_parallel`).
  - Root cause of the UNLICENSE in this deployment: `SDK_KEY` was never set. It exists only in repo-committed `app/.env` (Zach's commit 7a9a044) but compose mounts `.secrets/wyze-bridge.env`, which lacks it; container env had no SDK_KEY at all. Retest with the key injected: `TUTK_SDK_Set_License_Key` returns **0 (accepted)**, yet LD_CFP still refuses per-connection → either the key doesn't cover this device generation or the bundled `libIOTCAPIs_ALL.so` build is out of scope for it. No non-KVS camera in this fleet to A/B-test with.
  - New camSDK path: **`HualaiClient`/`HualaiNetworkThread` are built on `com.tutk.IOTC.*` JNI** (IOTCAPIs/AVAPIs) — the "new protocol" is a higher layer over stock TUTK P2P, not a proprietary socket. BUT **the 3.19 APK contains zero native libraries** (verified: no lib/ entries, 0 `.so` in the 252MB zip; size is ~100MB of classesN.dex + tflite assets). The app must fetch its TUTK .so at runtime (plugin/split delivery) or those code paths are dormant for KVS-only accounts.
  - DTLS authkey algo (Tutk.Companion.e): `Base64(sha256(enr + MAC.upper())[:6])` then charmap **`+`→Z, `/`→9, `=`→A** — our fork sends plain base64 without the charmap (relevant if/when a licensed TUTK build works here).

  **Option ranking after probing**
  1. ~~Zero-code via app UI~~ — CLOSED: Wyze's own gating hides RTSP for LD_CFP fw 2.2.x in-app.
  2. Fork's K10604 ask — blocked on a working TUTK license/.so for this device generation (need: valid SDK_KEY for newer devices, or the .so Wyze itself ships at runtime).
  3. Extract Wyze's runtime-delivered TUTK module (.so + embedded license) from their CDN → ctypes it and send K10604/z0n ask. Medium effort; download mechanism still to be pinned (candidates: `assets/PLUGIN_URL` pattern, camplus feature delivery).
  4. Pure-Python port of the new P2P stack — now looks BIGGEST (DTLS + wire format from scratch) since the "new" stack is still JNI-TUTK underneath. Deprioritized.
  5. Pivot: fix Scrypted/prebuffer against wyze-bridge's existing RTSP re-broadcast (`rtsp://192.168.6.10:8554/backyard-cam`) — unblocks HomeKit today without any camera-protocol work; device RTSP remains the post-KVS-deprecation optimization.

  ### Option A — FINAL RESULT: infeasible for LD_CFP (2026-08-17)

  **Verdict:** Direct TUTK P2P to `LD_CFP_D03F276DDB8A` is not licensed/available from either client
  stack. This closes options #2 and #3 above definitively — the last unknown ("maybe Wyze's own
  runtime .so + key is what we're missing") has now been eliminated by actually running Wyze's
  extracted native stack.

  **Correction to line "base APK contains zero .so":** the base `classes*.dex` APK indeed ships no
  `.so`, but the full **XAPK** bundles them in a per-ABI split: `config.arm64_v8a.apk` (53 MiB) →
  `lib/arm64-v8a/lib{TUTKGlobalAPIs,IOTCAPIs,AVAPIs,RDTAPIs}.so`. Extracted to
  `/tmp/opencode/wyze/xapk/arm64v8a/...`. So Wyze's native TUTK stack IS shipped in the download —
  no runtime CDN fetch needed. The "0 .so" finding was a base-APK-only scan.

  **Wyze's own license key = ours, byte-for-byte.** Hardcoded at `com/tutk/wyze/Tutk.java:182`
  (`Set_License_Key("AQAAAIZ4…")`, 220 chars). Verified identical to the repo-committed `app/.env`
  SDK_KEY and to what was injected into the container. So "our key doesn't cover this generation" is
  not a key-content problem — we are literally using Wyze's key with Wyze's libs.

  **Version mismatch (not the cause, but documented):** fork bundles TUTK `4.2.1.1-H` x86-64; Wyze
  XAPK bundles `4.3.8.0-5-gf554f2f-R2_openssl_android` aarch64. 4.3 adds `TUTK_SDK_Set_Region[_Code]`
  and `IOTC_Check_Device_OnlineEx`; app does not call them. New APIs present but unused ⇒ not the unlock.

  **Two independent connect tests, both UNLICENSE (-10 `IOTC_ER_UNLICENSE`):**
  1. **Wyze's own stack under qemu-aarch64** (static QEMU + arm64 python:3.11 rootfs; bionic shim `.so`
     for the ~8 missing symbols `__errno/__FD_SET_chk/__get_h_errno/__strncpy_chk2/
     __system_property_get/__sF/res_init/__strlen_chk`; ELF VERNEED strip via `stripvers.py`). Result:
     libs load, `Set_License_Key→0`, `IOTC_Initialize2(0)→0`, version string returns;
     `IOTC_Connect_ByUID_Parallel(uid,sid)` → **-10**. (Parallel = non-secure path; wrong for a dtls cam,
     so not conclusive alone — but shows no local license rejection.)
  2. **Production stack on the real NAS** (`/tmp/opencode/p2p_test.py`, run in `wyze-bridge` container):
     fork TUTK x86-64 + env SDK_KEY + `TUTK_SDK_Set_Region(US)` + full
     `WyzeIOTC.connect_and_auth()` → takes the **DTLS ByUIDEx** branch (dtls=true) with the correct
     charmapped authkey. Result: `*** CONNECT FAILED code=-10 IOTC_ER_UNLICENSE`. This is the decisive
     one — it's exactly how production reaches non-KVS cams, applied to LD_CFP.

  **Why this is conclusive:** both the older (4.2.1) and newer (4.3.8) TUTK builds, using Wyze's own
  key, fail direct-P2P connect to this UID on the correct secure path. The TUTK directory server
  refuses the integration for `LD_CFP_*` under Wyze's license ⇒ LD_CFP is a **cloud-only (KVS) device
  generation** as far as direct P2P goes. Without a control session there is no channel to send the
  RTSP-provisioning IOCTLs (`K10604GetRtspParam`/`K10600SetRtspSwitch`, or camSDK property `z0n`
  10729/10731). Consistent with: `LD_CFP ∈ KVS_CAMS`; Wyze app hides the RTSP option for fw 2.2.x; and
  production traffic shows backyard-cam is only ever served via **AWS Kinesis WebRTC (WHEP)**, never a
  TUTK process (`wyze_stream.py:start()` routes `is_kvs` cams to `setup_mtx_proxy`, skipping the tutk
  worker entirely).

  **Camera-side RTSP server still exists but is unprovisionable:** 554 + 8000 answer Digest
  `realm=LIBLIVE555` with a per-device random password obtainable only via the vendor ask over a P2P
  control session we cannot open. ~15 credential guesses failed earlier; no LAN bypass (SSDP/UPnP none,
  no plaintext creds in any reachable API surface).

  **Net effect / what actually works:** backyard-cam reaches HomeKit today through
  `Wyze cloud KVS → WHEP proxy → local mediamtx RTSP (…:8554/backyard-cam) → Scrypted`. Live logs at
  2026-08-18 00:09–00:10 confirm a healthy session (ICE connected; H264 + audio tracks; "Direct RTSP
  video/audio forwarding enabled"). The AGENTS.md "preview not available" note is stale (last active
  2026-08-13).

  **Levers remaining if local/direct RTSP becomes a hard requirement** (latency, Wyze/KVS-independent):
  none achievable on this device+fw today. Only: (a) wait for a future LD_CFP firmware that enables the
  in-app RTSP option + a licensed direct-P2P control channel (then re-run `p2p_test.py` / K10604); or
  (b) use a camera generation whose direct P2P is licensed (non-KVS cams already stream locally via the
  fork's TUTK path).

  ## Status log
 - [x] APK acquired + verified, extracted, decompiled (~68k classes)
- [x] constant presence scan vs fork spec
- [x] signing layer fully mapped (WpkSignature2Service / WpkWyzeSignatureService → native CDN .so; requestid=MD5(MD5(nonce)))
- [x] DMS 3.19 header scheme identified (plain Bearer, sc/sv gone) + old hex values recontextualized
- [x] auth chain mapped (OAuth PKCE, HMS keys/appids); fork's legacy login confirmed still served
- [x] live-view flow complete: wake→get_streams(ParamsBean/Agora)→WebRTC; KVS=replay-only in 3.19
- [x] WCSA endpoint family documented
- [x] drift report table + risk ranking written (above)
- [x] second pass: fork endpoint × 3.19 cross-reference, full prod host inventory, mars/brain/snowberg roles, onboarding/feature-gate discovery; confirmed all-KVS deployment ⇒ webrtc.api.wyze.com path unused for us
- [x] third pass: event-push architecture (FCM → WyzeFcmReceiver → get_event_list refresh; /user/set_push_info token registration), legacy CloudApi surface + alive 2FA login lineage, rtc_client_uid = plain cached random int, kvs get_image_url only used by garage-AI promo (fork snapshots via RTSP frame — no drift)
- [x] fourth pass: camSDK on-device RTSP feature fully mapped (z0n 10729/10731 codec, sinker endpoint, firmware gating); live probe of LD_CFP 192.168.4.223 — RTSP server confirmed running on 554+8000 with per-device random Digest creds; all ask paths blocked (app UI gate closed for fw 2.2.x; fork TUTK IOTC = UNLICENSE even with repo's SDK_KEY; APK ships no native libs so Wyze's own direct session needs a runtime-delivered .so). See "Live probe results" section + option ranking
- [x] fifth pass (bridge ops / KVS stress audit, 2026-08-17→18): quantified live-view duty cycle from 24h of logs — KVS channels die on a hard **~607s TTL**; the 480s proactive keep-alive wakes do NOT extend it (391 wakes + 301 `/kvs-config` polls + 157 backyard reconnects, cycles 605–611s regardless of wake alignment) ⇒ ~72 pure-stress pokes/cam/day. Recovery already rides the warm-reconnect path (`scheduleReconnect` → `GET /kvs-config/<id>?warm=1` → `_maybe_wake_kvs_camera` 30s/600s cooldown), so keep-alive was redundant stress. Added env knob **`KVS_KEEPALIVE_SECONDS`** (default 480 = old behavior; 0 off) in `config.py`+`stream_manager.py`; phase-2 experiment running with it off. Documented three wedge failure modes needing escalation logic: (a) P-frame-only upstream that Go reconnect never heals, (b) SDP_ANSWER-timeout storm against an unresponsive cam (infinite offer+wake hammer), (c) gst mount preroll race at boot for 2K streams (`failed to preroll pipeline` → :8554 stuck 404 until container restart). PLI throttle confirmed already deployed (`130daa2`/`4dc2d42`, `WHEP_PLI_MIN_INTERVAL_MS`). State, rollback (`wyze-bridge-local:rollback-20260817`) and quick commands in AGENTS.md; boot forensics at `/tmp/opencode/wyze-boot-failure-20260818.log`
