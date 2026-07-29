import contextlib
from datetime import datetime
import os
from pathlib import Path
from signal import SIGTERM
from subprocess import Popen
from typing import Optional

import yaml
from wyzebridge.build_config import MTX_TAG
from wyzebridge.config import KVS_SOURCE_ON_DEMAND, MTX_HLSVARIANT, MTX_READTIMEOUT, MTX_WEBRTICTRACKGATHERTIMEOUT, MTX_WRITEQUEUESIZE, RECORD_KEEP, RECORD_LENGTH, RECORD_PATTERN, STUN_SERVER, SUBJECT_ALT_NAME
from wyzebridge.bridge_utils import env_bool
from wyzebridge.gst_rtsp_server import GST_RTSP_ENABLED, GST_RTSP_PORT
from wyzebridge.logging import logger

MTX_CONFIG: str = "/app/mediamtx.yml"
MTX_PATH: str = "%path"

# Cameras whose "-sub" path should be a real downscaled stream rather than an
# alias to the full-resolution one. Opt-in per camera: a camera that is already
# low resolution gains nothing and would just burn CPU re-encoding itself.
SUBSTREAM_TRANSCODE_CAMS: list[str] = [
    cam.strip() for cam in env_bool("SUBSTREAM_TRANSCODE_CAMS").split(",") if cam.strip()
]
# 16:9 to match the 2560x1440 sources. The prebuffer plugin asks for 480x360,
# but that is 4:3 and would distort a widescreen source.
SUBSTREAM_SCALE: str = env_bool("SUBSTREAM_SCALE", "640:360", style="original")
SUBSTREAM_BITRATE: str = env_bool("SUBSTREAM_BITRATE", "400k", style="original")


def substream_transcode_cmd(base_uri: str, sub_uri: str) -> str:
    """ffmpeg command mediamtx runs to publish the downscaled substream.

    Software x264: this image's ffmpeg has no h264_qsv/h264_vaapi, and /dev/dri
    is not mapped into the container, so Quick Sync is unavailable. Audio is
    copied rather than re-encoded since it is already 8kHz PCMU.
    """
    return (
        "/usr/local/bin/ffmpeg -nostdin -loglevel warning "
        # genpts: the upstream audio carries non-monotonic DTS, which spams
        # "Non-monotonic DTS ... changing to" on every copied packet.
        "-fflags +genpts "
        f"-rtsp_transport tcp -i rtsp://127.0.0.1:8554/{base_uri} "
        "-c:a copy "
        f"-c:v libx264 -preset veryfast -tune zerolatency -vf scale={SUBSTREAM_SCALE} "
        f"-b:v {SUBSTREAM_BITRATE} -maxrate {SUBSTREAM_BITRATE} -bufsize 800k "
        "-g 40 -pix_fmt yuv420p "
        f"-f rtsp -rtsp_transport tcp rtsp://127.0.0.1:8554/{sub_uri}"
    )

class MtxInterface:
    __slots__ = "data", "_modified"

    def __init__(self):
        self.data = {}
        self._modified = False

    def __enter__(self):
        self.load_config()
        return self

    def __exit__(self, exc_type, exc_value, exc_traceback):
        self.save_config()

    def load_config(self):
        logger.debug(f"[MTX] Reading config from {MTX_CONFIG=}")
        with open(MTX_CONFIG, "r") as f:
            self.data = yaml.safe_load(f) or {}

    def save_config(self):
        if self._modified:
            logger.debug(f"[MTX] Writing config to {MTX_CONFIG=}")
            with open(MTX_CONFIG, "w") as f:
                yaml.safe_dump(self.data, f, sort_keys=False)

    def dump_to_yaml(self) -> str:
        return yaml.safe_dump(self.data, sort_keys=False)

    def get(self, path: str):
        keys = path.split(".")
        current = self.data
        for key in keys:
            if current is None:
                return None
            current = current.get(key)
        return current

    def set(self, path: str, value):
        keys = path.split(".")
        current = self.data
        for key in keys[:-1]:
            current = current.setdefault(key, {})
        current[keys[-1]] = value
        self._modified = True

    def add(self, path: str, value):
        if not isinstance(value, list):
            value = [value]
        current = self.data.get(path)
        if isinstance(current, list):
            current.extend([item for item in value if item not in current])
        else:
            self.data[path] = value
        self._modified = True

class MtxServer:
    """Setup and interact with the backend mediamtx."""

    __slots__ = "sub_process"

    def __init__(self) -> None:
        self.sub_process: Optional[Popen] = None
        self.setup_path_defaults()

    def setup_path_defaults(self):
        logger.info(f"[MTX] Setting up default {RECORD_PATTERN=}")
        record_path = ensure_record_path().format(cam_name=MTX_PATH, CAM_NAME=MTX_PATH)

        with MtxInterface() as mtx:
            mtx.set("paths", {})
            for event in {"Read", "Unread", "Ready", "NotReady", "Init"}:
                bash_cmd = f"echo $MTX_PATH,{event}! > /tmp/mtx_event;"
                mtx.set(f"pathDefaults.runOn{event}", f"bash -c '{bash_cmd}'")
            mtx.set("pathDefaults.runOnDemandStartTimeout", "30s")
            mtx.set("pathDefaults.runOnDemandCloseAfter", "60s")
            # Sources are the loopback gst_rtsp_bridge, so TCP costs nothing and
            # makes keyframe bursts lossless. Under "automatic" mediamtx pulls
            # over UDP and drops the tail of a 2K IDR, which reaches decoders as
            # a truncated I-frame that smears down the rest of the picture.
            mtx.set("pathDefaults.rtspTransport", "tcp")
            mtx.set("pathDefaults.recordPath", record_path)
            mtx.set("pathDefaults.recordSegmentDuration", RECORD_LENGTH)
            mtx.set("pathDefaults.recordDeleteAfter", RECORD_KEEP)
            
            # explicitly defaults these because we used to force them in the config.yml enviroment
            mtx.set("hlsVariant", MTX_HLSVARIANT)
            mtx.set("readTimeout", MTX_READTIMEOUT)
            mtx.set("webrtcTrackGatherTimeout", MTX_WEBRTICTRACKGATHERTIMEOUT)
            mtx.set("writeQueueSize", MTX_WRITEQUEUESIZE)

            if STUN_SERVER != "":
                logger.info(f"[MTX] enabling STUN server at: {STUN_SERVER}")
                mtx.set("webrtcICEServers2", [{"url": f"{STUN_SERVER}"}])

            mtx.save_config()

    def setup_auth(self, api: Optional[str], stream: Optional[str]):
        administrator: dict = {
                "user": "any",
                "ips": ["127.0.0.1", "::1"],
                "permissions": [{"action": "api"}, {"action": "metrics"}, {"action": "pprof"}]
            }
        publisher: dict = {
                "user": "any",
                "ips": ["127.0.0.1", "::1"],
                "permissions": [{"action": "publish"}]
            }
        player: dict = {
                "user": "any",
                "permissions": [{"action": "read"}, {"action": "playback"}]
            }

        with MtxInterface() as mtx:
            mtx.set("authInternalUsers", [])
            mtx.add("authInternalUsers", administrator)
            mtx.add("authInternalUsers", publisher)
            mtx.add("authInternalUsers", player)
            if (api or not stream):
                client: dict = { }
                if api:
                    client.update({"user": "wb", "pass": api})
                else:
                    client.update({"user": "any"})
                client.update({"permissions": [{"action": "read"}, {"action": "playback"}]})
                mtx.add("authInternalUsers", client)
            if stream:
                logger.info("[MTX] Custom stream auth enabled")
                for client in parse_auth(stream):
                    mtx.add("authInternalUsers", client)

            mtx.save_config()

    def add_path(self, uri: str, on_demand: bool = True, is_kvs: bool = False):
        with MtxInterface() as mtx:
            if is_kvs:
                source_on_demand = on_demand and KVS_SOURCE_ON_DEMAND
                if GST_RTSP_ENABLED:
                    # Source from the gst_rtsp_bridge: its SDP always advertises
                    # both tracks, unlike the WHEP source which only learns
                    # tracks from live RTP and can wedge audio-only.
                    mtx.set(f"paths.{uri}.source", f"rtsp://127.0.0.1:{GST_RTSP_PORT}/{uri}")
                else:
                    mtx.set(f"paths.{uri}.source", f"whep://localhost:8080/whep/{uri}")
                mtx.set(f"paths.{uri}.sourceOnDemand", source_on_demand)
                if source_on_demand:
                    mtx.set(f"paths.{uri}.sourceOnDemandStartTimeout", "30s")
            elif on_demand:
                bash_cmd = "bash -c 'echo $MTX_PATH,{}! > /tmp/mtx_event'"
                mtx.set(f"paths.{uri}.runOnDemand", bash_cmd.format("start"))
                mtx.set(f"paths.{uri}.runOnUnDemand", bash_cmd.format("stop"))
            else:
                mtx.set(f"paths.{uri}", {})

            mtx.save_config()

    def add_source(self, uri: str, value: str):
        with MtxInterface() as mtx:
            mtx.set(f"paths.{uri}.source", value)

        mtx.save_config()

    def add_substream_alias(self, base_uri: str):
        """Add a substream path that mirrors the main stream (for Scrypted compatibility)."""
        sub_uri = f"{base_uri}-sub"
        with MtxInterface() as mtx:
            # For KVS cameras, substream should pull from the same source
            path_config = mtx.get(f"paths.{base_uri}")
            if base_uri in SUBSTREAM_TRANSCODE_CAMS:
                # A plain alias hands consumers the full-resolution feed while
                # telling them it is the small one. Scrypted then runs motion
                # detection -- and serves Apple Watch / low-bandwidth HomeKit --
                # at 2560x1440, and HomeKit copies 2K verbatim to clients that
                # top out at 1080p. Publish a genuinely downscaled path instead.
                # runOnInit, not runOnDemand: on-demand tears the transcode down
                # 60s after the last reader, and Scrypted polls sporadically, so
                # it thrashed start/stop and every read landing in the spin-up
                # window failed with 404 / connection refused. The encode is
                # ~0.3 cores, cheap enough to just leave running.
                mtx.set(f"paths.{sub_uri}.runOnInit", substream_transcode_cmd(base_uri, sub_uri))
                mtx.set(f"paths.{sub_uri}.runOnInitRestart", True)
                logger.info(
                    "[MTX] Added transcoded substream: %s -> %s (%s)",
                    sub_uri,
                    base_uri,
                    SUBSTREAM_SCALE,
                )
                return
            if path_config and path_config.get("source"):
                # KVS camera - substream pulls from same source
                mtx.set(f"paths.{sub_uri}.source", path_config.get("source"))
                mtx.set(f"paths.{sub_uri}.sourceOnDemand", False)
            else:
                # TUTK camera - substream uses same on-demand config
                run_on_demand = mtx.get(f"paths.{base_uri}.runOnDemand")
                if run_on_demand:
                    mtx.set(f"paths.{sub_uri}.runOnDemand", run_on_demand)
                run_on_undemand = mtx.get(f"paths.{base_uri}.runOnUnDemand")
                if run_on_undemand:
                    mtx.set(f"paths.{sub_uri}.runOnUnDemand", run_on_undemand)
        
        logger.info(f"[MTX] Added substream alias: {sub_uri} -> {base_uri}")

    def record(self, uri: str):
        logger.info(f"[MTX] Starting record for {uri}")
        
        base = ensure_record_path()
        record_path = base.format(cam_name=MTX_PATH, CAM_NAME=MTX_PATH)
        logger.info(f"[MTX] 📹 Will record {RECORD_LENGTH} clips for {uri} to {record_path} where {MTX_PATH} will be {uri}")
        
        file = datetime.now().strftime(base)
        recording = file.format(cam_name=uri, CAM_NAME=uri.upper())
        os.makedirs(os.path.dirname(recording), exist_ok=True)

        with MtxInterface() as mtx:
            mtx.set(f"paths.{uri}.record", True)
            mtx.set(f"paths.{uri}.recordPath", record_path)
            mtx.save_config()

    def dump_config(self) -> str:
        with MtxInterface() as mtx:
            return mtx.dump_to_yaml()

    def start(self) -> bool:
        if not self.sub_process_alive():
            logger.info(f"[MTX] starting MediaMTX {MTX_TAG}")
            self.sub_process = Popen(["./mediamtx", "./mediamtx.yml"], stdout=None, stderr=None) # None means inherit from parent process
        return self.sub_process_alive()

    def stop(self):
        if not self.sub_process:
            return
        if self.sub_process.poll() is None:
            logger.info("[MTX] Stopping MediaMTX...")
            with contextlib.suppress(ValueError, AttributeError, RuntimeError):
                self.sub_process.terminate()
                self.sub_process.communicate()
        self.sub_process = None

    def restart(self) -> bool:
        self.stop()
        return self.start()

    def health_check(self) -> bool:
        if self.sub_process is not None and not self.sub_process_alive():
            logger.error(f"[MediaMTX] Process exited with {self.sub_process.poll()}")
            self.restart()

        return self.sub_process_alive()

    def sub_process_alive(self) -> bool:
         return self.sub_process is not None and self.sub_process.poll() is None

    def setup_webrtc(self, bridge_ip: Optional[str]):
        if not bridge_ip:
            logger.warning("SET WB_IP to allow WEBRTC connections.")
            return
        ips = bridge_ip.split(",")
        logger.debug(f"Using {' and '.join(ips)} for webrtc")

        with MtxInterface() as mtx:
            mtx.add("webrtcAdditionalHosts", ips)
            mtx.save_config()

    def setup_llhls(self, token_path: str = "/tokens/", hass: bool = False):
        logger.info("[MTX] Configuring LL-HLS")
        with MtxInterface() as mtx:
            mtx.set("hlsVariant", "lowLatency")
            mtx.set("hlsEncryption", True)
            if env_bool("MTX_HLSSERVERKEY"):
                return

            key = "/ssl/privkey.pem"
            cert = "/ssl/fullchain.pem"
            if hass and Path(key).is_file() and Path(cert).is_file():
                logger.info(
                    f"[MTX] 🔐 Using existing SSL certificate from Home Assistant {key=} {cert=}"
                )
                mtx.set("hlsServerKey", key)
                mtx.set("hlsServerCert", cert)
                return

            cert_path = f"{token_path}hls_server"
            generate_certificates(cert_path)
            mtx.set("hlsServerKey", f"{cert_path}.key")
            mtx.set("hlsServerCert", f"{cert_path}.crt")
            mtx.save_config()

def ensure_record_path() -> str:
    record_path = RECORD_PATTERN

    if "%s" in record_path or all(x in record_path for x in ["%Y", "%m", "%d", "%H", "%M", "%S"]):
        logger.info(f"[MTX] The computed record_path: '{record_path}' IS VALID")
    else:
        logger.warning(f"[MTX] The computed record_path: '{record_path}' IS NOT VALID, appending the %%s to the pattern")
        record_path += "_%s"

    return record_path

def mtx_version() -> str:
    try:
        with open("/MTX_TAG", "r") as tag:
            return tag.read().strip()
    except FileNotFoundError:
        return ""

def generate_certificates(cert_path):
    if not Path(f"{cert_path}.key").is_file():
        logger.info("[MTX] 🔐 Generating key for LL-HLS")
        Popen(
            ["openssl", "genrsa", "-out", f"{cert_path}.key", "2048"],
            stdout=None, stderr=None   # None means inherit from parent process
        ).wait()
    if not Path(f"{cert_path}.crt").is_file():
        logger.info("[MTX] 🔏 Generating certificate for LL-HLS")
        dns = SUBJECT_ALT_NAME
        Popen(
            ["openssl", "req", "-new", "-x509", "-sha256"]
            + ["-key", f"{cert_path}.key"]
            + ["-subj", "/C=US/ST=WA/L=Kirkland/O=WYZE BRIDGE/CN=wyze-bridge"]
            + (["-addext", f"subjectAltName = DNS:{dns}"] if dns else [])
            + ["-out", f"{cert_path}.crt"]
            + ["-days", "3650"],
            stdout=None, stderr=None   # None means inherit from parent process
        ).wait()

def parse_auth(auth: str) -> list[dict[str, str]]:
    entries = []
    for entry in auth.split("|"):
        creds, *endpoints = entry.split("@")
        if ":" not in creds:
            continue
        user, password, *ips = creds.split(":", 2)
        if ips:
            ips = ips[0].split(",")
        data: dict = {"user": user or "any", "pass": password, "ips": ips, "permissions": []}
        if endpoints:
            paths = []
            for endpoint in endpoints[0].split(","):
                paths.append(endpoint)
                data["permissions"].append({"action": "read", "path": endpoint})
                data["permissions"].append({"action": "playback", "path": endpoint})
        else:
            paths = "all"
            data["permissions"].append({"action": "read"})
            data["permissions"].append({"action": "playback"})
        logger.info(f"[MTX] Auth [{data['user']}:{data['pass']}] {paths=}")
        entries.append(data)
    return entries
