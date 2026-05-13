import contextlib
import os
from pathlib import Path
from subprocess import Popen, TimeoutExpired
from typing import Optional

from wyzebridge.bridge_utils import env_bool
from wyzebridge.logging import logger

GST_RTSP_CONFIG: str = "/tmp/gst-rtsp-streams.conf"
GST_RTSP_BINARY: str = "/usr/local/bin/gst_rtsp_bridge"
GST_RTSP_ENABLED: bool = env_bool("KVS_GSTREAMER_RTSP", style="bool")
GST_RTSP_PORT: int = env_bool("KVS_GSTREAMER_RTSP_PORT", "8555", style="int") or 8555
GST_RTSP_UDP_BASE: int = env_bool("KVS_GSTREAMER_UDP_BASE", "5600", style="int") or 5600
GST_RTSP_URL: str = env_bool("WB_KVS_RTSP_URL").strip("/")


def _stream_lines() -> list[tuple[str, int, int]]:
    try:
        with open(GST_RTSP_CONFIG, "r", encoding="utf-8") as config_file:
            lines = []
            for raw_line in config_file:
                line = raw_line.strip()
                if not line or line.startswith("#"):
                    continue
                parts = line.split()
                if len(parts) < 3:
                    continue
                try:
                    lines.append((parts[0], int(parts[1]), int(parts[2])))
                except ValueError:
                    continue
            return lines
    except FileNotFoundError:
        return []


def stream_ports(uri: str) -> Optional[tuple[int, int]]:
    for stream_uri, video_port, audio_port in _stream_lines():
        if stream_uri == uri:
            return video_port, audio_port
    return None


def direct_rtsp_enabled_for_stream(uri: str) -> bool:
    return stream_ports(uri) is not None


def rtsp_stream_url(uri: str, hostname: str, default_base: str) -> str:
    if direct_rtsp_enabled_for_stream(uri):
        base = GST_RTSP_URL or f"rtsp://{hostname}:{GST_RTSP_PORT}"
        return f"{base}/{uri}"
    return f"{default_base}/{uri}"


def rtsp_snap_input_url(uri: str) -> str:
    if direct_rtsp_enabled_for_stream(uri):
        return f"rtsp://127.0.0.1:{GST_RTSP_PORT}/{uri}"
    return f"rtsp://127.0.0.1:8554/{uri}"


class GstRtspServer:
    __slots__ = "sub_process", "streams"

    def __init__(self) -> None:
        self.sub_process: Optional[Popen] = None
        self.streams: dict[str, tuple[int, int]] = {}

    @property
    def enabled(self) -> bool:
        return GST_RTSP_ENABLED

    def add_path(self, uri: str, audio: bool = True):
        if not self.enabled or uri in self.streams:
            return

        offset = len(self.streams) * 2
        video_port = GST_RTSP_UDP_BASE + offset
        audio_port = GST_RTSP_UDP_BASE + offset + 1 if audio else 0
        self.streams[uri] = (video_port, audio_port)

    def has_path(self, uri: str) -> bool:
        return uri in self.streams

    def write_config(self):
        Path(GST_RTSP_CONFIG).parent.mkdir(parents=True, exist_ok=True)
        tmp_path = f"{GST_RTSP_CONFIG}.tmp"
        with open(tmp_path, "w", encoding="utf-8") as config_file:
            config_file.write("# stream_id video_port audio_port\n")
            for uri in sorted(self.streams):
                video_port, audio_port = self.streams[uri]
                config_file.write(f"{uri} {video_port} {audio_port}\n")
        os.replace(tmp_path, GST_RTSP_CONFIG)

    def start(self) -> bool:
        if not self.enabled:
            return True

        self.write_config()
        if not self.streams:
            logger.info("[GST_RTSP] Enabled, but no KVS streams were configured")
            return True
        if self.sub_process_alive():
            return True
        if not Path(GST_RTSP_BINARY).is_file():
            logger.error("[GST_RTSP] Missing helper binary at %s", GST_RTSP_BINARY)
            return False

        logger.info("[GST_RTSP] Starting direct RTSP server on port %s", GST_RTSP_PORT)
        self.sub_process = Popen(
            [GST_RTSP_BINARY, "--config", GST_RTSP_CONFIG, "--port", str(GST_RTSP_PORT)],
            stdout=None,
            stderr=None,
        )
        return self.sub_process_alive()

    def stop(self):
        if not self.sub_process:
            return
        if self.sub_process.poll() is None:
            logger.info("[GST_RTSP] Stopping direct RTSP server...")
            with contextlib.suppress(ValueError, AttributeError, RuntimeError, TimeoutExpired):
                self.sub_process.terminate()
                self.sub_process.communicate(timeout=5)
        self.sub_process = None

    def restart(self) -> bool:
        self.stop()
        return self.start()

    def health_check(self) -> bool:
        if not self.enabled:
            return True
        if self.sub_process is not None and not self.sub_process_alive():
            logger.error("[GST_RTSP] Process exited with %s", self.sub_process.poll())
            return self.restart()
        return self.sub_process_alive() or not self.streams

    def sub_process_alive(self) -> bool:
        return self.sub_process is not None and self.sub_process.poll() is None
