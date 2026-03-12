package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/pion/ice/v2"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/pion/webrtc/v3/pkg/media/samplebuilder"
)

type WebRTCStream struct {
	streamID          string
	configMu          sync.RWMutex
	config            WebRTCConfig
	upstreamMu        sync.RWMutex
	upstream          *UpstreamSession
	mediaMu           sync.RWMutex
	etag              string
	videoTrack        *webrtc.TrackLocalStaticRTP
	audioTrack        *webrtc.TrackLocalStaticRTP
	videoTrackMu      sync.Mutex
	audioTrackMu      sync.Mutex
	forwardWg         sync.WaitGroup
	videoSource       *webrtc.TrackRemote
	whepClients       atomic.Int32
	videoPLIRequested atomic.Bool
	lastSnapshotPLIAt atomic.Int64
	videoSPSNALU      []byte
	videoPPSNALU      []byte
	videoSPSBytes     int
	videoPPSBytes     int
	videoPacketizer   rtp.Packetizer
	videoLastInTS     uint32
	videoLastInTSSet  bool
	audioOutSeq       uint16
	audioSeqOffset    uint16
	audioOutSeqSet    bool
	audioSeqOffsetSet bool
	videoReady        atomic.Bool
	videoStarted      atomic.Bool
	audioReady        atomic.Bool
	upstreamAlive     atomic.Bool
	reconnecting      atomic.Bool
	destroyed         atomic.Bool
	videoReplayLogged atomic.Bool
	videoIDRLogged    atomic.Bool
	videoParamsMissed atomic.Bool
}

type UpstreamSession struct {
	peerConnection    *webrtc.PeerConnection
	wsConn            *websocket.Conn
	wsMu              sync.Mutex
	pendingCandidates []webrtc.ICECandidateInit
	remoteDescription *webrtc.SessionDescription
	correlationID     string
}

type ICEServer struct {
	URL        string `json:"url"`
	Username   string `json:"username"`
	Credential string `json:"credential"`
}

type WebRTCConfig struct {
	SignalingURL string      `json:"signaling_url"`
	ICEServers   []ICEServer `json:"ice_servers"`
	AuthToken    string      `json:"auth_token"`
	PhoneID      string      `json:"phone_id"`
}

var streams = make(map[string]*WebRTCStream)
var streamsMu sync.Mutex

const defaultH264PacketizationMode1Fmtp = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"

func periodicKeyframeInterval() time.Duration {
	const (
		defaultInterval = 60 * time.Second
		minInterval     = 2 * time.Second
	)

	raw := strings.TrimSpace(os.Getenv("WHEP_PERIODIC_KEYFRAME_MS"))
	if raw == "" {
		return defaultInterval
	}

	ms, err := strconv.Atoi(raw)
	if err != nil || ms <= 0 {
		log.Printf("[WHEP_PROXY] Invalid WHEP_PERIODIC_KEYFRAME_MS=%q, using default %v", raw, defaultInterval)
		return defaultInterval
	}

	interval := time.Duration(ms) * time.Millisecond
	if interval < minInterval {
		log.Printf("[WHEP_PROXY] WHEP_PERIODIC_KEYFRAME_MS=%q too low; clamping to %v", raw, minInterval)
		return minInterval
	}

	return interval
}

func downstreamReadyTimeout() time.Duration {
	const defaultTimeout = 8 * time.Second

	raw := strings.TrimSpace(os.Getenv("WHEP_DOWNSTREAM_READY_TIMEOUT_MS"))
	if raw == "" {
		return defaultTimeout
	}

	ms, err := strconv.Atoi(raw)
	if err != nil || ms <= 0 {
		log.Printf("[WHEP_PROXY] Invalid WHEP_DOWNSTREAM_READY_TIMEOUT_MS=%q, using default %v", raw, defaultTimeout)
		return defaultTimeout
	}

	return time.Duration(ms) * time.Millisecond
}

func videoSampleBuilderMaxLate() uint16 {
	const (
		defaultMaxLate = 2048
		minMaxLate     = 256
		maxMaxLate     = 8192
	)

	raw := strings.TrimSpace(os.Getenv("WHEP_H264_SAMPLEBUILDER_MAX_LATE"))
	if raw == "" {
		return defaultMaxLate
	}

	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		log.Printf(
			"[WHEP_PROXY] Invalid WHEP_H264_SAMPLEBUILDER_MAX_LATE=%q, using default %d",
			raw,
			defaultMaxLate,
		)
		return defaultMaxLate
	}

	if value < minMaxLate {
		log.Printf(
			"[WHEP_PROXY] WHEP_H264_SAMPLEBUILDER_MAX_LATE=%q too low; clamping to %d",
			raw,
			minMaxLate,
		)
		return minMaxLate
	}

	if value > maxMaxLate {
		log.Printf(
			"[WHEP_PROXY] WHEP_H264_SAMPLEBUILDER_MAX_LATE=%q too high; clamping to %d",
			raw,
			maxMaxLate,
		)
		return maxMaxLate
	}

	return uint16(value)
}

func apiSnapshotKeyframeMinInterval() time.Duration {
	const defaultInterval = 3 * time.Second

	raw := strings.TrimSpace(os.Getenv("WHEP_API_KEYFRAME_MIN_INTERVAL_MS"))
	if raw == "" {
		return defaultInterval
	}

	ms, err := strconv.Atoi(raw)
	if err != nil || ms < 0 {
		log.Printf(
			"[WHEP_PROXY] Invalid WHEP_API_KEYFRAME_MIN_INTERVAL_MS=%q, using default %v",
			raw,
			defaultInterval,
		)
		return defaultInterval
	}

	return time.Duration(ms) * time.Millisecond
}

func videoPacketizerPause() (every int, pause time.Duration) {
	const (
		defaultEvery = 32
		defaultPause = time.Millisecond
	)

	rawEvery := strings.TrimSpace(os.Getenv("WHEP_H264_PACKET_BURST"))
	if rawEvery != "" {
		if value, err := strconv.Atoi(rawEvery); err == nil && value > 0 {
			every = value
		}
	}
	if every == 0 {
		every = defaultEvery
	}

	rawPause := strings.TrimSpace(os.Getenv("WHEP_H264_PACKET_PAUSE_MS"))
	if rawPause != "" {
		if value, err := strconv.Atoi(rawPause); err == nil && value >= 0 {
			pause = time.Duration(value) * time.Millisecond
		}
	}
	if pause == 0 {
		pause = defaultPause
	}

	return every, pause
}

func (stream *WebRTCStream) ensureETag() string {
	stream.mediaMu.Lock()
	defer stream.mediaMu.Unlock()

	if stream.etag == "" {
		stream.etag = fmt.Sprintf("\"%x\"", time.Now().UnixNano())
	}
	return stream.etag
}

func (stream *WebRTCStream) canReuse() bool {
	if stream == nil || stream.destroyed.Load() {
		return false
	}
	return stream.videoTrack != nil || stream.audioTrack != nil
}

func (stream *WebRTCStream) setVideoSource(track *webrtc.TrackRemote) {
	stream.mediaMu.Lock()
	defer stream.mediaMu.Unlock()
	stream.videoSource = track
	stream.videoReady.Store(track != nil)
}

func (stream *WebRTCStream) setAudioReady(ready bool) {
	stream.audioReady.Store(ready)
}

func (stream *WebRTCStream) resetDownstreamStartupState() {
	stream.videoStarted.Store(false)
	stream.videoReplayLogged.Store(false)
	stream.videoIDRLogged.Store(false)
	stream.videoParamsMissed.Store(false)

	stream.audioTrackMu.Lock()
	stream.audioSeqOffsetSet = false
	stream.audioTrackMu.Unlock()
}

func (stream *WebRTCStream) status() map[string]interface{} {
	upstreamState := ""
	if session := stream.currentUpstream(); session != nil && session.peerConnection != nil {
		upstreamState = session.peerConnection.ConnectionState().String()
	}

	return map[string]interface{}{
		"upstream_state": upstreamState,
		"upstream_alive": stream.upstreamAlive.Load(),
		"can_reuse":      stream.canReuse(),
		"video_ready":    stream.videoReady.Load(),
		"audio_ready":    stream.audioReady.Load(),
		"whep_clients":   stream.whepClients.Load(),
	}
}

func (stream *WebRTCStream) hasOutputReady() bool {
	if stream == nil {
		return false
	}
	if stream.videoTrack != nil {
		return stream.videoReady.Load()
	}
	return stream.audioTrack != nil && stream.audioReady.Load()
}

func (stream *WebRTCStream) waitForOutputReady(timeout time.Duration) bool {
	if stream.hasOutputReady() {
		return true
	}
	if timeout <= 0 {
		return stream.hasOutputReady()
	}

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		if stream.hasOutputReady() {
			return true
		}
		if time.Now().After(deadline) {
			return stream.hasOutputReady()
		}
		<-ticker.C
	}
}

func (stream *WebRTCStream) requestVideoKeyframe(reason string) error {
	if reason == "api snapshot preflight" {
		minInterval := apiSnapshotKeyframeMinInterval()
		if minInterval > 0 {
			now := time.Now().UnixNano()
			last := stream.lastSnapshotPLIAt.Load()
			if last != 0 && time.Duration(now-last) < minInterval {
				return nil
			}
			stream.lastSnapshotPLIAt.Store(now)
		}
	}

	stream.mediaMu.RLock()
	videoSource := stream.videoSource
	stream.mediaMu.RUnlock()
	session := stream.currentUpstream()
	var peerConnection *webrtc.PeerConnection
	if session != nil {
		peerConnection = session.peerConnection
	}

	if videoSource == nil || peerConnection == nil {
		log.Printf("[WHEP_PROXY] Skipping keyframe request (%s): video source unavailable", reason)
		return nil
	}

	err := peerConnection.WriteRTCP([]rtcp.Packet{
		&rtcp.PictureLossIndication{MediaSSRC: uint32(videoSource.SSRC())},
	})
	if err != nil {
		return err
	}

	if reason != "downstream rtcp feedback" {
		log.Printf("[WHEP_PROXY] Requested keyframe (%s) for SSRC=%d", reason, videoSource.SSRC())
	}
	return nil
}

func (stream *WebRTCStream) currentUpstream() *UpstreamSession {
	stream.upstreamMu.RLock()
	defer stream.upstreamMu.RUnlock()
	return stream.upstream
}

func (stream *WebRTCStream) getConfig() WebRTCConfig {
	stream.configMu.RLock()
	defer stream.configMu.RUnlock()
	return stream.config
}

func (stream *WebRTCStream) setConfig(config WebRTCConfig) {
	stream.configMu.Lock()
	stream.config = config
	stream.configMu.Unlock()
}

func (stream *WebRTCStream) setUpstream(session *UpstreamSession) {
	stream.upstreamMu.Lock()
	stream.upstream = session
	stream.upstreamMu.Unlock()
	stream.upstreamAlive.Store(session != nil)
}

func (stream *WebRTCStream) clearUpstreamIfCurrent(session *UpstreamSession) bool {
	stream.upstreamMu.Lock()
	defer stream.upstreamMu.Unlock()
	if stream.upstream != session {
		return false
	}
	stream.upstream = nil
	stream.upstreamAlive.Store(false)
	return true
}

func (stream *WebRTCStream) resetUpstreamMediaState() {
	stream.setVideoSource(nil)
	stream.setAudioReady(false)
	stream.videoPLIRequested.Store(false)
	stream.videoReplayLogged.Store(false)
	stream.videoIDRLogged.Store(false)
	stream.videoParamsMissed.Store(false)
	stream.videoStarted.Store(false)
	stream.mediaMu.Lock()
	stream.videoSPSNALU = nil
	stream.videoPPSNALU = nil
	stream.videoSPSBytes = 0
	stream.videoPPSBytes = 0
	stream.audioSeqOffsetSet = false
	stream.mediaMu.Unlock()
}

func closeUpstreamSession(session *UpstreamSession) {
	if session == nil {
		return
	}
	if session.wsConn != nil {
		session.wsMu.Lock()
		_ = session.wsConn.Close()
		session.wsConn = nil
		session.wsMu.Unlock()
	}
	if session.peerConnection != nil {
		_ = session.peerConnection.Close()
		session.peerConnection = nil
	}
}

func h264PacketInfo(payload []byte) (isIDR bool, desc string) {
	if len(payload) == 0 {
		return false, "empty"
	}

	naluType := payload[0] & 0x1F
	switch naluType {
	case 5:
		return true, "single-idr"
	case 24:
		types := make([]string, 0, 4)
		hasIDR := false
		for i := 1; i+2 <= len(payload); {
			naluSize := int(payload[i])<<8 | int(payload[i+1])
			i += 2
			if naluSize <= 0 || i+naluSize > len(payload) {
				break
			}
			aggType := payload[i] & 0x1F
			types = append(types, strconv.Itoa(int(aggType)))
			if aggType == 5 {
				hasIDR = true
			}
			i += naluSize
		}
		if len(types) == 0 {
			return false, "stap-a"
		}
		return hasIDR, "stap-a[" + strings.Join(types, ",") + "]"
	case 28:
		if len(payload) < 2 {
			return false, "fu-a-short"
		}
		start := payload[1]&0x80 != 0
		end := payload[1]&0x40 != 0
		origType := payload[1] & 0x1F
		if origType == 5 && start {
			return true, "fu-a-idr-start"
		}
		if origType == 5 && end {
			return false, "fu-a-idr-end"
		}
		return false, fmt.Sprintf("fu-a-%d", origType)
	default:
		return false, fmt.Sprintf("nalu-%d", naluType)
	}
}

func h264NeedsKeyframe(payload []byte) bool {
	if len(payload) == 0 {
		return true
	}

	naluType := payload[0] & 0x1F
	switch naluType {
	case 5:
		return false
	case 24:
		for i := 1; i+2 <= len(payload); {
			naluSize := int(payload[i])<<8 | int(payload[i+1])
			i += 2
			if naluSize <= 0 || i+naluSize > len(payload) {
				return true
			}
			if payload[i]&0x1F == 5 {
				return false
			}
			i += naluSize
		}
		return true
	case 28:
		if len(payload) < 2 {
			return true
		}
		return !(payload[1]&0x80 != 0 && payload[1]&0x1F == 5)
	default:
		return true
	}
}

func cloneRTPPacket(pkt *rtp.Packet) *rtp.Packet {
	if pkt == nil {
		return nil
	}

	clone := &rtp.Packet{
		Header:      pkt.Header,
		PaddingSize: pkt.PaddingSize,
	}
	clone.CSRC = append([]uint32(nil), pkt.CSRC...)
	clone.Extensions = append([]rtp.Extension(nil), pkt.Extensions...)
	clone.Payload = append([]byte(nil), pkt.Payload...)
	clone.Raw = append([]byte(nil), pkt.Raw...)
	return clone
}

func cloneBytes(src []byte) []byte {
	if len(src) == 0 {
		return nil
	}
	return append([]byte(nil), src...)
}

func parseSTAPAParameterSets(payload []byte) ([]byte, []byte) {
	if len(payload) < 3 {
		return nil, nil
	}

	var spsNALU []byte
	var ppsNALU []byte
	for i := 1; i+2 <= len(payload); {
		naluSize := int(payload[i])<<8 | int(payload[i+1])
		i += 2
		if naluSize <= 0 || i+naluSize > len(payload) {
			break
		}

		switch payload[i] & 0x1F {
		case 7:
			spsNALU = cloneBytes(payload[i : i+naluSize])
		case 8:
			ppsNALU = cloneBytes(payload[i : i+naluSize])
		}
		i += naluSize
	}

	return spsNALU, ppsNALU
}

func (stream *WebRTCStream) bufferVideoParameterSet(pkt *rtp.Packet) {
	if pkt == nil || len(pkt.Payload) == 0 {
		return
	}

	naluType := pkt.Payload[0] & 0x1F

	stream.mediaMu.Lock()
	defer stream.mediaMu.Unlock()

	switch naluType {
	case 7:
		stream.videoSPSNALU = cloneBytes(pkt.Payload)
		stream.videoSPSBytes = len(pkt.Payload)
	case 8:
		stream.videoPPSNALU = cloneBytes(pkt.Payload)
		stream.videoPPSBytes = len(pkt.Payload)
	case 24:
		spsNALU, ppsNALU := parseSTAPAParameterSets(pkt.Payload)
		if len(spsNALU) == 0 && len(ppsNALU) == 0 {
			return
		}
		if len(spsNALU) > 0 {
			stream.videoSPSNALU = spsNALU
			stream.videoSPSBytes = len(spsNALU)
		}
		if len(ppsNALU) > 0 {
			stream.videoPPSNALU = ppsNALU
			stream.videoPPSBytes = len(ppsNALU)
		}
	}
}

func (stream *WebRTCStream) prependVideoParameterSets(sample *media.Sample, streamID string) (*media.Sample, bool) {
	if sample == nil {
		return nil, false
	}

	stream.mediaMu.RLock()
	spsNALU := cloneBytes(stream.videoSPSNALU)
	ppsNALU := cloneBytes(stream.videoPPSNALU)
	spsBytes := stream.videoSPSBytes
	ppsBytes := stream.videoPPSBytes
	stream.mediaMu.RUnlock()

	if len(spsNALU) == 0 || len(ppsNALU) == 0 || spsBytes == 0 || ppsBytes == 0 {
		if stream.videoParamsMissed.CompareAndSwap(false, true) {
			log.Printf("[WHEP_PROXY] Missing buffered SPS/PPS before IDR for %s: sps=%d pps=%d", streamID, spsBytes, ppsBytes)
		}
		return sample, false
	}

	const annexBStartCode = "\x00\x00\x00\x01"
	prefixed := make([]byte, 0, len(sample.Data)+len(spsNALU)+len(ppsNALU)+8)
	prefixed = append(prefixed, annexBStartCode...)
	prefixed = append(prefixed, spsNALU...)
	prefixed = append(prefixed, annexBStartCode...)
	prefixed = append(prefixed, ppsNALU...)
	prefixed = append(prefixed, sample.Data...)

	sampleCopy := *sample
	sampleCopy.Data = prefixed

	if stream.videoReplayLogged.CompareAndSwap(false, true) {
		log.Printf("[WHEP_PROXY] Prepended SPS (%d bytes) + PPS (%d bytes) to first IDR sample for %s", spsBytes, ppsBytes, streamID)
	}
	stream.videoParamsMissed.Store(false)
	return &sampleCopy, true
}

func h264SampleHasIDR(data []byte) bool {
	remaining := data
	for len(remaining) > 0 {
		start := bytes.Index(remaining, []byte{0x00, 0x00, 0x01})
		startLen := 3
		if start == -1 {
			start = bytes.Index(remaining, []byte{0x00, 0x00, 0x00, 0x01})
			startLen = 4
		}
		if start == -1 {
			if len(remaining) > 0 && remaining[0]&0x1F == 5 {
				return true
			}
			return false
		}

		naluStart := start + startLen
		remaining = remaining[naluStart:]
		if len(remaining) == 0 {
			return false
		}

		nextStart := bytes.Index(remaining, []byte{0x00, 0x00, 0x01})
		nextStartLen := 3
		if nextStart == -1 {
			nextStart = bytes.Index(remaining, []byte{0x00, 0x00, 0x00, 0x01})
			nextStartLen = 4
		}

		naluEnd := len(remaining)
		if nextStart != -1 {
			naluEnd = nextStart
		}
		if naluEnd > 0 && remaining[0]&0x1F == 5 {
			return true
		}

		if nextStart == -1 {
			return false
		}
		remaining = remaining[nextStart+nextStartLen:]
	}

	return false
}

func (stream *WebRTCStream) writeVideoSample(localTrack *webrtc.TrackLocalStaticRTP, sample *media.Sample) error {
	if localTrack == nil || sample == nil {
		return fmt.Errorf("local track or sample unavailable")
	}

	stream.videoTrackMu.Lock()
	defer stream.videoTrackMu.Unlock()

	if stream.videoPacketizer == nil {
		stream.videoPacketizer = rtp.NewPacketizer(
			1200,
			0,
			0,
			&codecs.H264Payloader{},
			rtp.NewRandomSequencer(),
			90000,
		)
	}

	samples := uint32(sample.Duration.Seconds() * 90000)
	if samples == 0 && stream.videoLastInTSSet {
		samples = sample.PacketTimestamp - stream.videoLastInTS
	}
	if samples == 0 {
		samples = 90000 / 20
	}
	stream.videoLastInTS = sample.PacketTimestamp
	stream.videoLastInTSSet = true

	packets := stream.videoPacketizer.Packetize(sample.Data, samples)
	if len(packets) == 0 {
		return fmt.Errorf("video packetizer produced no packets")
	}

	pauseEvery, pause := videoPacketizerPause()
	for i, pkt := range packets {
		pkt.Timestamp = sample.PacketTimestamp
		if err := localTrack.WriteRTP(pkt); err != nil {
			return err
		}
		if pause > 0 && pauseEvery > 0 && i+1 < len(packets) && (i+1)%pauseEvery == 0 {
			time.Sleep(pause)
		}
	}

	return nil
}

func (stream *WebRTCStream) writeLocalTrack(localTrack *webrtc.TrackLocalStaticRTP, pkt *rtp.Packet) error {
	if localTrack == nil || pkt == nil {
		return fmt.Errorf("local track or packet unavailable")
	}

	stream.audioTrackMu.Lock()
	defer stream.audioTrackMu.Unlock()

	if !stream.audioSeqOffsetSet {
		if stream.audioOutSeqSet {
			stream.audioSeqOffset = stream.audioOutSeq + 1 - pkt.SequenceNumber
		} else {
			stream.audioSeqOffset = 0
		}
		stream.audioSeqOffsetSet = true
	}
	pkt.SequenceNumber += stream.audioSeqOffset
	stream.audioOutSeq = pkt.SequenceNumber
	stream.audioOutSeqSet = true

	return localTrack.WriteRTP(pkt)
}

func forwardAudioTrack(
	streamID string,
	stream *WebRTCStream,
	session *UpstreamSession,
	track *webrtc.TrackRemote,
	localTrack *webrtc.TrackLocalStaticRTP,
) {
	stream.forwardWg.Add(1)
	defer stream.forwardWg.Done()

	var readCount uint64
	var writtenCount uint64
	var droppedCount uint64

	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			log.Printf(
				"[WHEP_PROXY] Track ended for %s (%s): read=%d written=%d dropped=%d err=%v",
				streamID,
				track.Kind().String(),
				readCount,
				writtenCount,
				droppedCount,
				err,
			)
			stream.handleUpstreamDisconnect(session, fmt.Sprintf("%s track ended: %v", track.Kind().String(), err))
			return
		}

		readCount++
		if stream.videoTrack != nil && !stream.videoStarted.Load() {
			droppedCount++
			continue
		}
		if stream.whepClients.Load() == 0 {
			droppedCount++
		} else if err = stream.writeLocalTrack(localTrack, pkt); err != nil {
			droppedCount++
		} else {
			writtenCount++
		}

		if readCount%5000 == 0 {
			log.Printf(
				"[WHEP_PROXY] RTP stats for %s (%s): read=%d written=%d dropped=%d clients=%d",
				streamID,
				track.Kind().String(),
				readCount,
				writtenCount,
				droppedCount,
				stream.whepClients.Load(),
			)
		}
	}
}

func forwardVideoTrack(
	streamID string,
	stream *WebRTCStream,
	session *UpstreamSession,
	track *webrtc.TrackRemote,
	localTrack *webrtc.TrackLocalStaticRTP,
) {
	stream.forwardWg.Add(1)
	defer stream.forwardWg.Done()

	var readCount uint64
	var writtenCount uint64
	var droppedCount uint64
	waitForVideoKeyframe := true
	maxLate := videoSampleBuilderMaxLate()
	builder := samplebuilder.New(maxLate, &codecs.H264Packet{}, 90000)
	log.Printf("[WHEP_PROXY] H264 samplebuilder maxLate for %s: %d packets", streamID, maxLate)

	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			log.Printf(
				"[WHEP_PROXY] Track ended for %s (%s): read=%d written=%d dropped=%d err=%v",
				streamID,
				track.Kind().String(),
				readCount,
				writtenCount,
				droppedCount,
				err,
			)
			stream.handleUpstreamDisconnect(session, fmt.Sprintf("%s track ended: %v", track.Kind().String(), err))
			return
		}

		readCount++
		stream.bufferVideoParameterSet(pkt)
		builder.Push(cloneRTPPacket(pkt))

		for sample := builder.Pop(); sample != nil; sample = builder.Pop() {
			isIDR := h264SampleHasIDR(sample.Data)

			if sample.PrevDroppedPackets > 0 {
				droppedCount += uint64(sample.PrevDroppedPackets)
				waitForVideoKeyframe = true
				stream.videoReplayLogged.Store(false)
				stream.videoPLIRequested.Store(true)
				log.Printf(
					"[WHEP_PROXY] Video sample discontinuity for %s: dropped_packets=%d timestamp=%d",
					streamID,
					sample.PrevDroppedPackets,
					sample.PacketTimestamp,
				)
			}

			if waitForVideoKeyframe {
				if !isIDR {
					droppedCount++
					continue
				}
				waitForVideoKeyframe = false
			}

			if stream.whepClients.Load() == 0 {
				waitForVideoKeyframe = true
				stream.videoReplayLogged.Store(false)
				droppedCount++
				continue
			}

			if isIDR && !stream.videoReplayLogged.Load() {
				sample, _ = stream.prependVideoParameterSets(sample, streamID)
				if stream.videoIDRLogged.CompareAndSwap(false, true) {
					log.Printf(
						"[WHEP_PROXY] First IDR sample for %s: timestamp=%d bytes=%d dropped_before=%d",
						streamID,
						sample.PacketTimestamp,
						len(sample.Data),
						sample.PrevDroppedPackets,
					)
				}
			}

			if err = stream.writeVideoSample(localTrack, sample); err != nil {
				waitForVideoKeyframe = true
				stream.videoReplayLogged.Store(false)
				stream.videoPLIRequested.Store(true)
				droppedCount++
				continue
			}

			writtenCount++
			stream.videoStarted.Store(true)
			if stream.videoPLIRequested.CompareAndSwap(true, false) {
				if pliErr := stream.requestVideoKeyframe("first downstream write"); pliErr != nil {
					log.Printf("[WHEP_PROXY] Failed to request keyframe for %s after first write: %v", streamID, pliErr)
					stream.videoPLIRequested.Store(true)
				}
			}
		}

		if readCount%5000 == 0 {
			log.Printf(
				"[WHEP_PROXY] RTP stats for %s (%s): read=%d written=%d dropped=%d clients=%d",
				streamID,
				track.Kind().String(),
				readCount,
				writtenCount,
				droppedCount,
				stream.whepClients.Load(),
			)
		}
	}
}

func readReceiverRTCP(streamID string, track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
	for {
		pkts, _, err := receiver.ReadRTCP()
		if err != nil {
			log.Printf("[WHEP_PROXY] Receiver RTCP ended for %s (%s): %v", streamID, track.Kind().String(), err)
			return
		}

		for _, pkt := range pkts {
			switch pkt.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				log.Printf("[WHEP_PROXY] Upstream RTCP feedback for %s (%s): %T", streamID, track.Kind().String(), pkt)
			}
		}
	}
}

func cleanupUpstreamLocked(stream *WebRTCStream) {
	current := stream.upstream
	stream.upstream = nil
	stream.upstreamAlive.Store(false)
	stream.resetUpstreamMediaState()
	closeUpstreamSession(current)
}

func cleanupUpstreamIfCurrent(stream *WebRTCStream, session *UpstreamSession) bool {
	if !stream.clearUpstreamIfCurrent(session) {
		return false
	}
	stream.resetUpstreamMediaState()
	closeUpstreamSession(session)
	return true
}

func destroyStreamLocked(streamID string, stream *WebRTCStream) {
	stream.destroyed.Store(true)
	cleanupUpstreamLocked(stream)
	delete(streams, streamID)
}

func destroyStream(streamID string, stream *WebRTCStream) {
	streamsMu.Lock()
	defer streamsMu.Unlock()
	destroyStreamLocked(streamID, stream)
}

func destroyStreamIfCurrent(streamID string, stream *WebRTCStream) {
	streamsMu.Lock()
	defer streamsMu.Unlock()
	if current, ok := streams[streamID]; ok && current == stream {
		destroyStreamLocked(streamID, stream)
	}
}

func (stream *WebRTCStream) scheduleReconnect(reason string) {
	if stream.destroyed.Load() {
		return
	}
	if !stream.reconnecting.CompareAndSwap(false, true) {
		return
	}

	go func() {
		stream.forwardWg.Wait()
		for attempt := 1; ; attempt++ {
			if stream.destroyed.Load() {
				stream.reconnecting.Store(false)
				return
			}

			delay := time.Duration(attempt*2) * time.Second
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
			log.Printf(
				"[WHEP_PROXY] Reconnecting upstream for %s (%s), attempt %d in %s",
				stream.streamID,
				reason,
				attempt,
				delay,
			)
			time.Sleep(delay)

			config, err := fetchKVSConfig(stream.streamID)
			if err != nil {
				log.Printf("[WHEP_PROXY] Failed to refresh KVS config for %s: %v", stream.streamID, err)
				continue
			}
			stream.setConfig(config)

			if err := establishUpstream(stream); err != nil {
				log.Printf("[WHEP_PROXY] Reconnect attempt %d failed for %s: %v", attempt, stream.streamID, err)
				continue
			}

			log.Printf("[WHEP_PROXY] Upstream reconnected for %s on attempt %d", stream.streamID, attempt)
			stream.reconnecting.Store(false)
			return
		}
	}()
}

func (stream *WebRTCStream) handleUpstreamDisconnect(session *UpstreamSession, reason string) {
	if stream.destroyed.Load() {
		return
	}
	if !cleanupUpstreamIfCurrent(stream, session) {
		return
	}
	log.Printf("[WHEP_PROXY] Upstream session ended for %s: %s", stream.streamID, reason)
	stream.scheduleReconnect(reason)
}

func fetchKVSConfig(streamID string) (WebRTCConfig, error) {
	var config WebRTCConfig

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:5000/kvs-config/%s", streamID))
	if err != nil {
		return config, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return config, fmt.Errorf("refresh config status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	if err := json.NewDecoder(resp.Body).Decode(&config); err != nil {
		return config, err
	}
	if config.SignalingURL == "" {
		return config, fmt.Errorf("refresh config missing signaling_url")
	}

	return config, nil
}

func main() {
	r := mux.NewRouter()
	r.HandleFunc("/whep/{streamID}", whepHandler).Methods("GET", "OPTIONS", "POST")
	r.HandleFunc("/websocket/{streamID}", websocketHandler).Methods("GET", "POST")
	r.HandleFunc("/status/{streamID}", statusHandler).Methods("GET")
	r.HandleFunc("/request-keyframe/{streamID}", requestKeyframeHandler).Methods("POST")

	go func() {
		fmt.Println("[WHEP_PROXY] Listening on :8080")
		if err := http.ListenAndServe(":8080", r); err != nil {
			panic(err)
		}
	}()

	sigchan := make(chan os.Signal, 1)
	signal.Notify(sigchan, os.Interrupt)
	<-sigchan

	fmt.Println("[WHEP_PROXY] Exiting.")

	streamsMu.Lock()
	defer streamsMu.Unlock()
	for streamID, stream := range streams {
		destroyStreamLocked(streamID, stream)
	}
}

func redactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	query := parsed.Query()
	for _, key := range []string{
		"X-Amz-Security-Token",
		"X-Amz-Signature",
		"X-Amz-Credential",
		"X-Amz-Date",
		"X-Amz-Expires",
	} {
		if query.Has(key) {
			query.Set(key, "REDACTED")
		}
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func sendSignalingMessage(
	session *UpstreamSession,
	action string,
	payload interface{},
	correlationID string,
) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	envelope := map[string]interface{}{
		"action":         action,
		"messagePayload": base64.StdEncoding.EncodeToString(encoded),
	}
	if correlationID != "" {
		envelope["correlationId"] = correlationID
	}

	session.wsMu.Lock()
	defer session.wsMu.Unlock()
	if session.wsConn == nil {
		return fmt.Errorf("websocket unavailable")
	}
	return session.wsConn.WriteJSON(envelope)
}

func decodeSignalingPayload(msg map[string]interface{}) ([]byte, error) {
	payload, _ := msg["messagePayload"].(string)
	if payload == "" {
		return nil, fmt.Errorf("empty messagePayload")
	}
	return base64.StdEncoding.DecodeString(payload)
}

func generateCorrelationID(phoneID string) string {
	correlationID := fmt.Sprintf("%s.%d", phoneID, time.Now().UnixMilli())
	if phoneID == "" {
		correlationID = fmt.Sprintf("%d", time.Now().UnixMilli())
	}
	if len(correlationID) <= 256 {
		return correlationID
	}
	return correlationID[len(correlationID)-256:]
}

func rewriteSessionLine(sdp, correlationID string) string {
	if correlationID == "" {
		return sdp
	}
	rewritten := strings.Replace(sdp, "s=-\r\n", "s="+correlationID+"\r\n", 1)
	if rewritten != sdp {
		return rewritten
	}
	return strings.Replace(sdp, "s=-\n", "s="+correlationID+"\n", 1)
}

func decodeSignalingURL(rawURL string) (string, error) {
	return rawURL, nil
}

func createPeerConnection(config WebRTCConfig) (*webrtc.PeerConnection, error) {
	iceServers := []webrtc.ICEServer{}
	downstreamLocalOnly := len(config.ICEServers) == 0 && config.SignalingURL == ""

	for _, server := range config.ICEServers {
		iceServers = append(iceServers, webrtc.ICEServer{
			URLs:       []string{server.URL},
			Username:   server.Username,
			Credential: server.Credential,
		})
	}
	if len(iceServers) == 0 && !downstreamLocalOnly {
		iceServers = []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}}
	}

	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}

	interceptorRegistry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, interceptorRegistry); err != nil {
		return nil, err
	}

	settingEngine := webrtc.SettingEngine{}
	if downstreamLocalOnly {
		settingEngine.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
		settingEngine.SetIncludeLoopbackCandidate(true)
		settingEngine.SetInterfaceFilter(func(iface string) bool {
			return iface == "lo"
		})
		settingEngine.SetIPFilter(func(ip net.IP) bool {
			return ip != nil && ip.IsLoopback()
		})
		settingEngine.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	}

	return webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(interceptorRegistry),
		webrtc.WithSettingEngine(settingEngine),
	).NewPeerConnection(webrtc.Configuration{ICEServers: iceServers})
}

func handleRemoteAnswer(streamID string, session *UpstreamSession, msg map[string]interface{}) error {
	decoded, err := decodeSignalingPayload(msg)
	if err != nil {
		return err
	}

	var answer webrtc.SessionDescription
	if err := json.Unmarshal(decoded, &answer); err != nil {
		return fmt.Errorf("unmarshal SDP_ANSWER: %w", err)
	}

	fmt.Println("[WHEP_PROXY] Received SDP_ANSWER for", streamID)
	answer.SDP = strings.ReplaceAll(answer.SDP, "\\r\\n", "\r\n")
	if err := session.peerConnection.SetRemoteDescription(answer); err != nil {
		return fmt.Errorf("set remote description: %w", err)
	}
	session.remoteDescription = &answer

	for _, candidate := range session.pendingCandidates {
		if err := session.peerConnection.AddICECandidate(candidate); err != nil {
			fmt.Println("[WHEP_PROXY] Failed to add queued ICE candidate:", err)
		}
	}
	session.pendingCandidates = nil
	return nil
}

func handleRemoteCandidate(streamID string, session *UpstreamSession, msg map[string]interface{}) error {
	decoded, err := decodeSignalingPayload(msg)
	if err != nil {
		return err
	}

	var candidateMap map[string]interface{}
	if err := json.Unmarshal(decoded, &candidateMap); err != nil {
		return fmt.Errorf("unmarshal ICE_CANDIDATE: %w", err)
	}

	candidateString, ok := candidateMap["candidate"].(string)
	if !ok || candidateString == "" {
		return fmt.Errorf("candidate string missing")
	}

	candidate := webrtc.ICECandidateInit{Candidate: candidateString}
	if sdpMid, ok := candidateMap["sdpMid"].(string); ok && sdpMid != "" {
		candidate.SDPMid = &sdpMid
	}
	if mLineIndex, ok := candidateMap["sdpMLineIndex"].(float64); ok {
		uint16Val := uint16(mLineIndex)
		candidate.SDPMLineIndex = &uint16Val
	}

	if session.remoteDescription == nil {
		session.pendingCandidates = append(session.pendingCandidates, candidate)
		fmt.Println("[WHEP_PROXY] Queued ICE_CANDIDATE for", streamID)
		return nil
	}

	fmt.Println("[WHEP_PROXY] Received ICE_CANDIDATE for", streamID)
	return session.peerConnection.AddICECandidate(candidate)
}

func createAndSendOffer(streamID string, session *UpstreamSession) error {
	offer, err := session.peerConnection.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create offer: %w", err)
	}
	if err := session.peerConnection.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set local description: %w", err)
	}

	// Wait for ICE gathering to complete so the offer SDP includes local candidates.
	// Some KVS/camera implementations expect at least one candidate before responding.
	gatherComplete := webrtc.GatheringCompletePromise(session.peerConnection)
	select {
	case <-gatherComplete:
		// done
	case <-time.After(10 * time.Second):
		fmt.Println("[WHEP_PROXY] ICE gathering timeout for", streamID, "; sending offer anyway")
	}

	localDescription := session.peerConnection.LocalDescription()
	if localDescription == nil {
		return fmt.Errorf("local description unavailable after SetLocalDescription")
	}

	envelope := map[string]interface{}{
		"type": "offer",
		"sdp":  rewriteSessionLine(localDescription.SDP, session.correlationID),
	}
	if decoded, err := json.Marshal(envelope); err == nil {
		fmt.Println("[WHEP_PROXY] SDP_OFFER payload for", streamID, string(decoded))
	}
	if payload, err := json.Marshal(map[string]interface{}{
		"action":         "SDP_OFFER",
		"messagePayload": base64.StdEncoding.EncodeToString(mustJSON(envelope)),
		"correlationId":  session.correlationID,
	}); err == nil {
		fmt.Println("[WHEP_PROXY] Sending envelope for", streamID, string(payload))
	}
	return sendSignalingMessage(session, "SDP_OFFER", envelope, session.correlationID)
}

func establishUpstream(stream *WebRTCStream) error {
	config := stream.getConfig()
	decodedURL, err := decodeSignalingURL(config.SignalingURL)
	if err != nil {
		return fmt.Errorf("decode signaling URL: %w", err)
	}

	fmt.Println("[WHEP_PROXY] Connecting websocket:", redactURL(decodedURL))

	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 20 * time.Second
	dialer.EnableCompression = true
	headers := http.Header{
		"User-Agent": {"okhttp/4.12.0"},
	}
	conn, resp, err := dialer.Dial(decodedURL, headers)
	if err != nil {
		if resp != nil {
			if resp.Body != nil {
				defer resp.Body.Close()
			}
			fmt.Println("[WHEP_PROXY] Websocket handshake status:", resp.Status)
			fmt.Println("[WHEP_PROXY] Websocket handshake headers:", resp.Header)
		}
		if resp != nil && resp.Body != nil {
			bodyBytes := make([]byte, 2048)
			if n, readErr := resp.Body.Read(bodyBytes); readErr == nil || readErr == io.EOF {
				fmt.Println("[WHEP_PROXY] Websocket response:", string(bodyBytes[:n]))
			}
		}
		return fmt.Errorf("connect websocket: %w", err)
	}

	peerConnection, err := createPeerConnection(config)
	if err != nil {
		_ = conn.Close()
		return err
	}

	session := &UpstreamSession{
		peerConnection: peerConnection,
		wsConn:         conn,
		correlationID:  generateCorrelationID(config.PhoneID),
	}
	stream.setUpstream(session)

	if _, err = peerConnection.AddTransceiverFromKind(
		webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly},
	); err != nil {
		stream.handleUpstreamDisconnect(session, fmt.Sprintf("add video transceiver: %v", err))
		return err
	}

	if _, err = peerConnection.AddTransceiverFromKind(
		webrtc.RTPCodecTypeAudio,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly},
	); err != nil {
		stream.handleUpstreamDisconnect(session, fmt.Sprintf("add audio transceiver: %v", err))
		return err
	}

	peerConnection.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("[WHEP_PROXY] ICE connection state for %s: %s", stream.streamID, state.String())
	})
	peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("[WHEP_PROXY] Peer connection state for %s: %s", stream.streamID, state.String())
		switch state {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			stream.handleUpstreamDisconnect(session, fmt.Sprintf("peer connection state=%s", state.String()))
		}
	})

	peerConnection.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		candidate := c.ToJSON()
		if err := sendSignalingMessage(session, "ICE_CANDIDATE", candidate, session.correlationID); err != nil {
			fmt.Println("[WHEP_PROXY] Error sending ICE candidate:", err)
		}
	})

	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		if stream.currentUpstream() != session {
			return
		}

		log.Printf("[WHEP_PROXY] Received track for %s: codec=%s", stream.streamID, track.Codec().MimeType)
		fmt.Printf(
			"[WHEP_PROXY] Received remote track for %s: kind=%s codec=%s payloadType=%d fmtp=%q\n",
			stream.streamID,
			track.Kind().String(),
			track.Codec().MimeType,
			track.PayloadType(),
			track.Codec().SDPFmtpLine,
		)

		switch track.Kind() {
		case webrtc.RTPCodecTypeVideo:
			stream.setVideoSource(track)
			if stream.whepClients.Load() > 0 {
				stream.videoPLIRequested.Store(true)
				if err := stream.requestVideoKeyframe("upstream track available"); err != nil {
					log.Printf("[WHEP_PROXY] Failed to request keyframe for %s on upstream track: %v", stream.streamID, err)
				}
			}
			go readReceiverRTCP(stream.streamID, track, receiver)
			go forwardVideoTrack(stream.streamID, stream, session, track, stream.videoTrack)
		case webrtc.RTPCodecTypeAudio:
			stream.setAudioReady(true)
			go readReceiverRTCP(stream.streamID, track, receiver)
			go forwardAudioTrack(stream.streamID, stream, session, track, stream.audioTrack)
		default:
			return
		}

		if track.Kind() == webrtc.RTPCodecTypeVideo {
			go func() {
				interval := periodicKeyframeInterval()
				log.Printf("[WHEP_PROXY] Periodic keyframe refresh interval for %s: %v", stream.streamID, interval)
				ticker := time.NewTicker(interval)
				defer ticker.Stop()

				for range ticker.C {
					if stream.currentUpstream() != session || session.peerConnection == nil || session.peerConnection.ConnectionState() == webrtc.PeerConnectionStateClosed {
						return
					}
					if stream.whepClients.Load() == 0 {
						continue
					}
					if err := stream.requestVideoKeyframe("periodic downstream refresh"); err != nil {
						log.Printf("[WHEP_PROXY] Failed to request keyframe for %s: %v", stream.streamID, err)
						return
					}
				}
			}()
		}
	})

	go func() {
		for {
			messageType, data, err := conn.ReadMessage()
			if err != nil {
				var closeErr *websocket.CloseError
				if errors.As(err, &closeErr) {
					fmt.Printf("[WHEP_PROXY] Websocket closed by peer: code=%d reason=%q\n", closeErr.Code, closeErr.Text)
				} else if !errors.Is(err, io.EOF) {
					fmt.Println("[WHEP_PROXY] Error reading message:", err)
				} else {
					fmt.Println("[WHEP_PROXY] Websocket read EOF (connection closed)")
				}
				stream.handleUpstreamDisconnect(session, fmt.Sprintf("websocket closed: %v", err))
				return
			}

			if len(data) == 0 {
				log.Println("[WHEP_PROXY] Skipping empty keepalive frame")
				continue
			}

			const rawLogLen = 200
			msgTypeStr := "other"
			switch messageType {
			case websocket.TextMessage:
				msgTypeStr = "text"
			case websocket.BinaryMessage:
				msgTypeStr = "binary"
			}
			sampleLen := len(data)
			if sampleLen > rawLogLen {
				sampleLen = rawLogLen
			}
			if messageType == websocket.BinaryMessage && sampleLen > 0 {
				fmt.Printf("[WHEP_PROXY] raw message type=%s len=%d first%d_hex=%s\n", msgTypeStr, len(data), sampleLen, hex.EncodeToString(data[:sampleLen]))
			} else if sampleLen > 0 {
				payload := string(data[:sampleLen])
				if strings.ContainsAny(payload, "\r\n") {
					payload = strings.ReplaceAll(strings.ReplaceAll(payload, "\r", "\\r"), "\n", "\\n")
				}
				fmt.Printf("[WHEP_PROXY] raw message type=%s len=%d first%d=%s\n", msgTypeStr, len(data), sampleLen, payload)
			} else {
				fmt.Printf("[WHEP_PROXY] raw message type=%s len=0\n", msgTypeStr)
			}

			var jsonData []byte
			switch messageType {
			case websocket.TextMessage:
				jsonData = data
			case websocket.BinaryMessage:
				if jsonErr := json.Unmarshal(data, &(map[string]interface{}{})); jsonErr == nil {
					jsonData = data
				} else {
					decoded, decErr := base64.StdEncoding.DecodeString(string(data))
					if decErr != nil {
						fmt.Printf("[WHEP_PROXY] Binary message: not valid JSON (%v) and base64 decode failed: %v\n", jsonErr, decErr)
						continue
					}
					jsonData = decoded
				}
			default:
				fmt.Printf("[WHEP_PROXY] Ignoring websocket message type %d\n", messageType)
				continue
			}

			var msg map[string]interface{}
			if err := json.Unmarshal(jsonData, &msg); err != nil {
				fmt.Println("[WHEP_PROXY] Error unmarshaling signaling JSON:", err)
				continue
			}

			msgType, _ := msg["messageType"].(string)
			if msgType == "" {
				msgType, _ = msg["action"].(string)
			}
			if msgType == "" {
				fmt.Println("[WHEP_PROXY] Ignoring signaling message without type:", msg)
				continue
			}

			if correlationID, _ := msg["correlationId"].(string); correlationID != "" && correlationID != session.correlationID {
				fmt.Printf("[WHEP_PROXY] Ignoring %s for %s due to mismatched correlationId %q\n", msgType, stream.streamID, correlationID)
				continue
			}

			switch msgType {
			case "SDP_ANSWER":
				if err := handleRemoteAnswer(stream.streamID, session, msg); err != nil {
					fmt.Println("[WHEP_PROXY] Failed to handle SDP_ANSWER:", err)
				}
			case "ICE_CANDIDATE":
				if err := handleRemoteCandidate(stream.streamID, session, msg); err != nil {
					fmt.Println("[WHEP_PROXY] Failed to handle ICE_CANDIDATE:", err)
				}
			case "STATUS_RESPONSE":
				fmt.Println("[WHEP_PROXY] Received STATUS_RESPONSE for", stream.streamID, msg)
			default:
				fmt.Println("[WHEP_PROXY] Ignoring signaling message type:", msgType)
			}
		}
	}()

	if delayMs := os.Getenv("WHEP_SIGNALING_DELAY_MS"); delayMs != "" {
		if ms, err := strconv.Atoi(delayMs); err == nil && ms > 0 {
			d := time.Duration(ms) * time.Millisecond
			fmt.Printf("[WHEP_PROXY] Waiting %v before sending SDP_OFFER for %s\n", d, stream.streamID)
			time.Sleep(d)
		}
	}
	if err := createAndSendOffer(stream.streamID, session); err != nil {
		stream.handleUpstreamDisconnect(session, fmt.Sprintf("createAndSendOffer failed: %v", err))
		return err
	}

	return nil
}

func websocketHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	streamID := mux.Vars(r)["streamID"]

	var config WebRTCConfig
	var stream *WebRTCStream
	if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
		http.Error(w, "Invalid JSON configuration", http.StatusBadRequest)
		return
	}
	if config.SignalingURL == "" {
		http.Error(w, "Signaling URL is required", http.StatusBadRequest)
		return
	}
	// If we already have an active proxy for this stream (e.g. duplicate POST from setup_streams + runOnInit),
	// do not replace it — return 200 so the first session stays alive for ICE/media.
	streamsMu.Lock()
	if existing := streams[streamID]; existing != nil {
		if !existing.canReuse() {
			fmt.Println("[WHEP_PROXY] Existing stream is stale; replacing", streamID)
			destroyStreamLocked(streamID, existing)
		} else {
			existing.setConfig(config)
			streamsMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok","reused":true}`))
			return
		}
	}

	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: defaultH264PacketizationMode1Fmtp,
		},
		"video",
		"pion",
	)
	if err != nil {
		streamsMu.Unlock()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	audioTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypePCMU,
			ClockRate: 8000,
			Channels:  2,
		},
		"audio",
		"pion",
	)
	if err != nil {
		streamsMu.Unlock()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	stream = &WebRTCStream{
		streamID:   streamID,
		videoTrack: videoTrack,
		audioTrack: audioTrack,
	}
	stream.setConfig(config)
	streams[streamID] = stream
	streamsMu.Unlock()

	// Respond 200 immediately so the client (Python) does not time out and retry. The client uses
	// a 10s timeout; delay + ICE gathering + offer can exceed that and cause a second POST, which
	// would replace this stream and close the websocket before ICE/media establish.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))

	go func(stream *WebRTCStream) {
		if err := establishUpstream(stream); err != nil {
			log.Printf("[WHEP_PROXY] Initial upstream establish failed for %s: %v", stream.streamID, err)
			stream.scheduleReconnect("initial establish failed")
		}
	}(stream)
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	streamID := mux.Vars(r)["streamID"]

	streamsMu.Lock()
	stream, ok := streams[streamID]
	streamsMu.Unlock()
	if !ok {
		http.Error(w, fmt.Sprintf("Stream %s not found", streamID), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(stream.status()); err != nil {
		http.Error(w, "Error encoding status", http.StatusInternalServerError)
	}
}

func requestKeyframeHandler(w http.ResponseWriter, r *http.Request) {
	streamID := mux.Vars(r)["streamID"]

	streamsMu.Lock()
	stream, ok := streams[streamID]
	streamsMu.Unlock()
	if !ok {
		http.Error(w, fmt.Sprintf("Stream %s not found", streamID), http.StatusNotFound)
		return
	}

	if err := stream.requestVideoKeyframe("api snapshot preflight"); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func mustJSON(v interface{}) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

func sdpHasMediaLine(sdp, media string) bool {
	return strings.Contains(sdp, "\nm="+media+" ") || strings.HasPrefix(sdp, "m="+media+" ")
}

func h264SDPSummary(sdp string) string {
	lines := strings.Split(sdp, "\n")
	h264PayloadTypes := map[string]struct{}{}
	summary := make([]string, 0, 4)

	for _, rawLine := range lines {
		line := strings.TrimSpace(strings.TrimSuffix(rawLine, "\r"))
		if !strings.HasPrefix(line, "a=rtpmap:") || !strings.Contains(strings.ToUpper(line), " H264/90000") {
			continue
		}

		summary = append(summary, line)

		payloadType := strings.TrimPrefix(line, "a=rtpmap:")
		if fields := strings.Fields(payloadType); len(fields) > 0 {
			h264PayloadTypes[fields[0]] = struct{}{}
		}
	}

	for _, rawLine := range lines {
		line := strings.TrimSpace(strings.TrimSuffix(rawLine, "\r"))
		if !strings.HasPrefix(line, "a=fmtp:") {
			continue
		}

		payloadType := strings.TrimPrefix(line, "a=fmtp:")
		if idx := strings.Index(payloadType, " "); idx > 0 {
			if _, ok := h264PayloadTypes[payloadType[:idx]]; ok {
				summary = append(summary, line)
			}
		}
	}

	if len(summary) == 0 {
		return "none"
	}

	return strings.Join(summary, " | ")
}

func whepHandler(w http.ResponseWriter, r *http.Request) {
	streamID := mux.Vars(r)["streamID"]

	streamsMu.Lock()
	stream, ok := streams[streamID]
	streamsMu.Unlock()
	if !ok {
		http.Error(w, fmt.Sprintf("Stream %s not found", streamID), http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodOptions, http.MethodGet:
		w.Header().Set("Content-Type", "application/sdp")
		fmt.Fprint(w, "")
	case http.MethodPost:
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/sdp") {
			http.Error(w, "Content-Type must be application/sdp", http.StatusUnsupportedMediaType)
			return
		}

		if !stream.hasOutputReady() {
			timeout := downstreamReadyTimeout()
			log.Printf("[WHEP_PROXY] Waiting up to %v for upstream media before answering WHEP for %s", timeout, streamID)
			if !stream.waitForOutputReady(timeout) {
				log.Printf(
					"[WHEP_PROXY] Upstream media still not ready for %s after %v: upstream_alive=%t video_ready=%t audio_ready=%t",
					streamID,
					timeout,
					stream.upstreamAlive.Load(),
					stream.videoReady.Load(),
					stream.audioReady.Load(),
				)
			}
		}

		log.Printf("[WHEP_PROXY] WHEP offer received for %s from %s", streamID, r.RemoteAddr)

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Error reading request body", http.StatusBadRequest)
			return
		}
		offerSDP := string(body)
		log.Printf(
			"[WHEP_PROXY] WHEP offer for %s: video=%t audio=%t",
			streamID,
			sdpHasMediaLine(offerSDP, "video"),
			sdpHasMediaLine(offerSDP, "audio"),
		)
		log.Printf("[WHEP_PROXY] WHEP offer H264 for %s: %s", streamID, h264SDPSummary(offerSDP))

		peerConnection, err := createPeerConnection(WebRTCConfig{})
		if err != nil {
			http.Error(w, "Error creating peer connection", http.StatusInternalServerError)
			return
		}

		if stream.videoTrack == nil && stream.audioTrack == nil {
			_ = peerConnection.Close()
			http.Error(w, "Stream has no output tracks", http.StatusServiceUnavailable)
			return
		}

		var countedClient atomic.Bool
		peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
			log.Printf("[WHEP_PROXY] Downstream WHEP peer for %s: state=%s", streamID, state.String())
			log.Printf("[WHEP_PROXY] WHEP client state for %s: %s", streamID, state.String())
			switch state {
			case webrtc.PeerConnectionStateConnected:
				if countedClient.CompareAndSwap(false, true) {
					if stream.whepClients.Add(1) == 1 {
						stream.resetDownstreamStartupState()
					}
					stream.videoPLIRequested.Store(true)
				}
				if err := stream.requestVideoKeyframe("downstream connected"); err != nil {
					log.Printf("[WHEP_PROXY] Failed to request keyframe for %s on connect: %v", streamID, err)
				}
			case webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
				if countedClient.CompareAndSwap(true, false) {
					stream.whepClients.Add(-1)
					if stream.whepClients.Load() == 0 {
						stream.videoPLIRequested.Store(false)
						stream.resetDownstreamStartupState()
					}
				}
				if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
					_ = peerConnection.Close()
				}
			}
		})

		if err = peerConnection.SetRemoteDescription(webrtc.SessionDescription{
			Type: webrtc.SDPTypeOffer,
			SDP:  offerSDP,
		}); err != nil {
			_ = peerConnection.Close()
			http.Error(w, "Error setting remote description", http.StatusInternalServerError)
			return
		}

		videoAdded := false
		audioAdded := false
		addTrack := func(track webrtc.TrackLocal, mimeType string) error {
			rtpSender, addTrackErr := peerConnection.AddTrack(track)
			if addTrackErr != nil {
				return addTrackErr
			}
			switch strings.ToLower(mimeType) {
			case strings.ToLower(webrtc.MimeTypeH264):
				videoAdded = true
			case strings.ToLower(webrtc.MimeTypePCMU):
				audioAdded = true
			}

			go func(sender *webrtc.RTPSender) {
				rtcpBuf := make([]byte, 1500)
				for {
					n, _, rtcpErr := sender.Read(rtcpBuf)
					if rtcpErr != nil {
						return
					}

					packets, unmarshalErr := rtcp.Unmarshal(rtcpBuf[:n])
					if unmarshalErr != nil {
						continue
					}
					for _, pkt := range packets {
						switch pkt.(type) {
						case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
							if err := stream.requestVideoKeyframe("downstream rtcp feedback"); err != nil {
								log.Printf("[WHEP_PROXY] Failed to forward keyframe request for %s: %v", streamID, err)
							}
						}
					}
				}
			}(rtpSender)
			return nil
		}
		if stream.videoTrack != nil {
			if err := addTrack(stream.videoTrack, webrtc.MimeTypeH264); err != nil {
				_ = peerConnection.Close()
				http.Error(w, "Error adding video track", http.StatusInternalServerError)
				return
			}
		}
		if stream.audioTrack != nil {
			if err := addTrack(stream.audioTrack, webrtc.MimeTypePCMU); err != nil {
				_ = peerConnection.Close()
				http.Error(w, "Error adding audio track", http.StatusInternalServerError)
				return
			}
		}
		log.Printf("[WHEP_PROXY] WHEP tracks added for %s: video=%v audio=%v", streamID, videoAdded, audioAdded)

		gatherComplete := webrtc.GatheringCompletePromise(peerConnection)
		answer, err := peerConnection.CreateAnswer(&webrtc.AnswerOptions{})
		if err != nil {
			_ = peerConnection.Close()
			http.Error(w, "Error creating SDP answer", http.StatusInternalServerError)
			return
		}
		if err = peerConnection.SetLocalDescription(answer); err != nil {
			_ = peerConnection.Close()
			http.Error(w, "Error setting local description", http.StatusInternalServerError)
			return
		}

		etag := stream.ensureETag()
		<-gatherComplete
		localDescription := peerConnection.LocalDescription()
		if localDescription == nil {
			_ = peerConnection.Close()
			http.Error(w, "Local description unavailable", http.StatusInternalServerError)
			return
		}
		log.Printf(
			"[WHEP_PROXY] WHEP answer for %s: video=%t audio=%t",
			streamID,
			sdpHasMediaLine(localDescription.SDP, "video"),
			sdpHasMediaLine(localDescription.SDP, "audio"),
		)
		log.Printf("[WHEP_PROXY] WHEP answer H264 for %s: %s", streamID, h264SDPSummary(localDescription.SDP))

		w.Header().Set("Content-Type", "application/sdp")
		w.Header().Set("Location", fmt.Sprintf("/whep/%s", streamID))
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, localDescription.SDP)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}
