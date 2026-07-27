# wyze-bridge-project Notes

## Build Failure Pattern: Unused Variables in whep_proxy/main.go

Go build fails silently during Docker multi-stage builds when `whep_proxy/main.go` has unused variables. The error appears as:

```
./main.go:<LINE>:8: declared and not used: <VARNAME>
ERROR: failed to solve: process "/bin/sh -c go build -o /build/whep_proxy/whep_proxy" did not complete successfully: exit code: 1
```

Fix: Remove the unused variable declaration. If the surrounding logic was also simplified/replaced, remove all dead code related to that feature (e.g., `lastSeq`, `lastSeqSet`, `gapLogCounter` were left over after switching from sequence gap tracking to keyframe-wait approach).

## Docker Build Command
```bash
docker build --no-cache -t wyze-bridge-local:latest . 2>&1 | tee /tmp/docker-build.log &
BUILD_PID=$!; while kill -0 $BUILD_PID 2>/dev/null; do sleep 60; echo "--- building ---"; done; wait $BUILD_PID 2>&1; tail -5 /tmp/docker-build.log
```

## Docker Compose (NAS)
Stack lives at `/volume2/docker/wyze-bridge/`. Use Arcane for compose management.
