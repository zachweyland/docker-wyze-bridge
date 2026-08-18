import contextlib
import json
import os
import signal
import time
from subprocess import Popen, TimeoutExpired
from threading import Thread
from typing import  Callable, Optional

import requests

from wyzebridge.wyze_api import WyzeApi
from wyzebridge.stream import Stream
from wyzebridge.config import (
    KVS_KEEPALIVE_SECONDS,
    KVS_SNAPSHOT_REQUEST_KEYFRAME,
    MOTION,
    MQTT_DISCOVERY,
    SNAPSHOT_TYPE,
)
from wyzebridge.ffmpeg import rtsp_snap_cmd, wait_for_purges
from wyzebridge.logging import logger
from wyzebridge.mqtt import bridge_status, cam_control, publish_topic, update_preview
from wyzebridge.mtx_event import RtspEvent
from wyzebridge.wyze_events import WyzeEvents
from wyzebridge.bridge_utils_sunset import should_take_snapshot, should_skip_snapshot

# KVS live-view channels expire on a ~10-minute clock that keep-alive wakes do
# not extend (observed re-cycles at ~607s regardless of wake alignment), so the
# proactive wake is stress without benefit. Interval is now env-tunable; 0 off.
KVS_KEEPALIVE_INTERVAL = KVS_KEEPALIVE_SECONDS

# Hard lifetime for a snapshot ffmpeg run. A normal capture lands the first
# keyframe within the camera's ~2s GOP; anything still alive well past this is
# a hung RTSP reader (upstream died mid-capture) that will hold a :8554
# session forever if left alone.
SNAPSHOT_MAX_LIFETIME = 30.0

class StreamManager:
    __slots__ = "api", "stop_flag", "streams", "rtsp_snapshots", "last_snap", "monitor_snapshots_thread", "_last_kvs_keepalive", "_snap_started"

    def __init__(self, api: WyzeApi):
        self.api: WyzeApi = api
        self.stop_flag: bool = False
        self.streams: dict[str, Stream] = {}
        self.rtsp_snapshots: dict[str, Popen] = {}
        self.last_snap: float = 0
        self.monitor_snapshots_thread: Optional[Thread] = None
        self._last_kvs_keepalive: float = 0
        self._snap_started: dict[str, float] = {}

    @property
    def total(self):
        return len(self.streams)

    @property
    def active(self):
        return len([s for s in self.streams.values() if s.enabled])

    def add(self, stream: Stream) -> str:
        uri = stream.uri
        self.streams[uri] = stream
        return uri

    def get(self, uri: str) -> Optional[Stream]:
        return self.streams.get(uri)

    def get_info(self, uri: str) -> dict:
        return stream.get_info() if (stream := self.get(uri)) else {}

    def get_all_cam_info(self) -> dict:
        return {uri: s.get_info() for uri, s in self.streams.items()}

    def stop_all(self) -> None:
        logger.info(f"[STREAM] Stopping {self.total} stream{'s'[:self.total^1]}")
        self.stop_flag = True

        for stream in self.streams.values():
            stream.stop()

        if self.monitor_snapshots_thread is not None:
            logger.info("[STREAM] Stopping monitor_snapshots thread")
            with contextlib.suppress(ValueError, AttributeError, RuntimeError):
                self.monitor_snapshots_thread.join(timeout=5)
            self.monitor_snapshots_thread = None
                
        wait_for_purges()

    def monitor_streams(self, mtx_health: Callable) -> None:
        self.stop_flag = False

        if MQTT_DISCOVERY:
            self.monitor_snapshots()

        mqtt = cam_control(self.streams, self.send_cmd)
        logger.info(f"🎬 {self.total} stream{'s'[:self.total^1]} enabled")
        event = RtspEvent(self.streams)
        events = WyzeEvents(self.streams) if MOTION else None

        while not self.stop_flag:
            event.read(timeout=1)
            self.snap_all(self.active_streams())

            if events:
                events.check_motion()

            if int(time.time()) % 15 == 0:
                mtx_health()
                bridge_status(mqtt)

            # Guard on >0 as well: with the interval disabled a bare elapsed-time
            # check would re-wake every camera on the very first loop iteration.
            if KVS_KEEPALIVE_INTERVAL > 0 and time.time() - self._last_kvs_keepalive >= KVS_KEEPALIVE_INTERVAL:
                self._last_kvs_keepalive = time.time()
                self._wake_active_kvs_cameras()

        if mqtt:
            logger.info("[STREAM] Stopping mqtt loop")
            mqtt.loop_stop()
            mqtt = None

        logger.info("[STREAM] Stream monitoring stopped")

    def monitor_snapshots(self) -> None:
        def wrapped():
            logger.info("[STREAM] Starting monitor_snapshots thread")
            try:
                # emit to MQTT the current snapshots on file system
                for cam in self.streams:
                    if not self.stop_flag:
                        update_preview(cam)

                while not self.stop_flag:
                    for cam, ffmpeg in list(self.rtsp_snapshots.items()):
                        if self.stop_flag:
                            break
                        if ffmpeg is None:
                            continue
                        # Hard lifetime: a capture still alive this long is a hung
                        # RTSP reader (upstream died mid-capture). Killing the
                        # process group frees the :8554 session it is holding.
                        if ffmpeg.poll() is None and time.time() - self._snap_started.get(cam, time.time()) > SNAPSHOT_MAX_LIFETIME:
                            logger.warning(f"[STREAM] [{cam}] snapshot hung past {SNAPSHOT_MAX_LIFETIME:.0f}s; killing")
                            self.stop_subprocess(cam)
                            continue
                        if (returncode := ffmpeg.returncode) is not None:
                            if returncode == 0:
                                update_preview(cam)
                            # we have some response, remove from queue
                            self.remove_from_rtsp_snapshots(cam)
                    time.sleep(1)
            except Exception as e:
                logger.error(f"[STREAM] Unexpected error in monitor_snapshots: {e}")

        if self.monitor_snapshots_thread is not None:
            logger.info("[STREAM] Stopping previous monitor_snapshots thread")
            with contextlib.suppress(ValueError, AttributeError, RuntimeError):
                self.monitor_snapshots_thread.join(timeout=5)
            self.monitor_snapshots_thread = None
                
        self.monitor_snapshots_thread = Thread(target=wrapped, name="monitor_snapshots")
        self.monitor_snapshots_thread.daemon = True # allow this thread to be abandoned
        self.monitor_snapshots_thread.start()
        
    def remove_from_rtsp_snapshots(self, cam: str):
        try:
            del self.rtsp_snapshots[cam]
        except KeyError:
            logger.warning(f"[STREAM] {cam} not found in rtsp snapshots.")
        except Exception as ex:
            logger.error(f"[STREAM] [{type(ex).__name__}] removing {cam=} {ex}.")
        self._snap_started.pop(cam, None)

    def active_streams(self) -> list[str]:
        """
        Health check on all streams and return a list of enabled
        streams that are NOT battery powered.

        Returns:
        - list(str): uri-friendly name of streams that are enabled.
        """
        if self.stop_flag:
            return []
        return [cam for cam, s in self.streams.items() if s.health_check() > 0]

    def _wake_active_kvs_cameras(self) -> None:
        """Proactively wake KVS cameras before the 10-minute session timeout expires."""
        for cam_name, stream in self.streams.items():
            if stream.camera.is_kvs and stream.health_check() > 0:
                try:
                    cam = self.api.get_camera(cam_name, existing=True)
                    if cam:
                        logger.info(f"[STREAM] ☁️ KVS keep-alive wake for {cam_name}")
                        # min_interval must stay below KVS_KEEPALIVE_INTERVAL or the
                        # renewal never fires before the ~10-min session expires.
                        self.api._maybe_wake_kvs_camera(cam, min_interval=KVS_KEEPALIVE_INTERVAL - 60)
                except Exception as ex:
                    logger.warning(f"[STREAM] KVS keep-alive failed for {cam_name}: {ex}")

    def snap_all(self, cams: Optional[list[str]] = None, force: bool = False):
        """
        Take an rtsp snapshot of the streams in the list.

        Args:
        - cams (list[str], optional): names of the streams to take a snapshot of.
        - force (bool, optional): Ignore interval and force snapshot. Defaults to False.
        """
        if force or should_take_snapshot(SNAPSHOT_TYPE, self.last_snap):
            self.last_snap = time.time()
            for cam_name in cams or self.active_streams():
                if should_skip_snapshot(cam_name):
                    continue
                if (stream := self.get(cam_name)) and stream.camera.is_kvs:
                    self.api.save_thumbnail(cam_name, "")
                    continue
                if SNAPSHOT_TYPE == "rtsp":
                    self.stop_subprocess(cam_name)
                    self.rtsp_snap_popen(cam_name, True)
                elif SNAPSHOT_TYPE == "api":
                    self.api.save_thumbnail(cam_name, "")

    def get_sse_status(self) -> dict:
        return {
            uri: {"status": cam.status(), "motion": cam.motion}
            for uri, cam in self.streams.items()
        }

    def send_cmd(
        self, cam_name: str, cmd: str, payload: str | list | dict = ""
    ) -> dict:
        """
        Send a command directly to the camera and wait for a response.

        Parameters:
        - cam_name (str): uri-friendly name of the camera.
        - cmd (str): The camera/tutk command to send.
        - payload (str): value for the tutk command.

        Returns:
        - dictionary: Results that can be converted to JSON.
        """
        resp = {"status": "error", "command": cmd, "payload": payload}

        if cam_name == "all" and cmd == "update_snapshot":
            self.snap_all(force=True)
            return resp | {"status": "success"}

        if not (stream := self.get(cam_name)):
            return resp | {"response": "Camera not found"}

        if cam_resp := stream.send_cmd(cmd, payload):
            status = cam_resp.get("value") if cam_resp.get("status") == "success" else 0

            if isinstance(status, dict):
                status = json.dumps(status)

            if "update_snapshot" in cam_resp:
                demand_opened = not stream.connected
                if stream.camera.is_kvs:
                    snap = bool(self.api.save_thumbnail(cam_name, ""))
                else:
                    snap = self.get_rtsp_snap(cam_name)
                if demand_opened:
                    stream.stop()

                publish_topic(f"{cam_name}/{cmd}", int(time.time()) if snap else 0)
                return dict(resp, status="success", value=snap, response=snap)

            publish_topic(f"{cam_name}/{cmd}", status)

        return cam_resp if "status" in cam_resp else resp | cam_resp

    def rtsp_snap_popen(self, cam_name: str, interval: bool = False) -> Optional[Popen]:
        if not (stream := self.get(cam_name)):
            return
        stream.start()
        if stream.camera.is_kvs and KVS_SNAPSHOT_REQUEST_KEYFRAME and not interval:
            with contextlib.suppress(requests.RequestException):
                requests.post(
                    f"http://localhost:8080/request-keyframe/{cam_name}",
                    timeout=2,
                )
                # Give the fresh IDR a short window to arrive before ffmpeg snapshots.
                time.sleep(1)
        ffmpeg = self.rtsp_snapshots.get(cam_name)
        if not ffmpeg or ffmpeg.poll() is not None:
            # start_new_session puts the sh+ffmpeg wrapper in its own process
            # group so stop_subprocess can kill the ffmpeg child, not just the
            # shell around it (killing the wrapper orphaned ffmpeg and it kept
            # its :8554 RTSP session indefinitely).
            ffmpeg = Popen(rtsp_snap_cmd(cam_name, interval), stderr=None, start_new_session=True)
            self.rtsp_snapshots[cam_name] = ffmpeg
            self._snap_started[cam_name] = time.time()
        return ffmpeg

    def get_rtsp_snap(self, cam_name: str) -> bool:
        if not (stream := self.get(cam_name)) or stream.health_check() < 1:
            return False
        if not (ffmpeg := self.rtsp_snap_popen(cam_name)):
            return False
        try:
            if ffmpeg.wait(timeout=15) == 0:
                return True
        except TimeoutExpired:
            logger.info(f"❗ [{cam_name}] Snapshot timed out")
        except Exception as ex:
            logger.error(f"❗ [{cam_name}] [{type(ex).__name__}] {ex}")
        finally:
            self.stop_subprocess(cam_name)
        return False

    def stop_subprocess(self, cam: str):
        ffmpeg = self.rtsp_snapshots.get(cam)

        if ffmpeg is not None:
            self.remove_from_rtsp_snapshots(cam)

            if ffmpeg.poll() is None:
                # Kill the whole process group: the Popen'd process is a
                # /bin/sh -ec wrapper and killing only the shell orphaned the
                # ffmpeg child, which kept its RTSP session on :8554 forever.
                with contextlib.suppress(ProcessLookupError, PermissionError):
                    os.killpg(ffmpeg.pid, signal.SIGKILL)
                with contextlib.suppress(TimeoutExpired, OSError):
                    ffmpeg.wait(timeout=5)
