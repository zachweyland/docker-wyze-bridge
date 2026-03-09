package main

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

type WebRTCStream struct {
	peerConnection    *webrtc.PeerConnection
	wsConn            *websocket.Conn
	wsMu              sync.Mutex
	mediaMu           sync.RWMutex
	pendingCandidates []webrtc.ICECandidateInit
	remoteDescription *webrtc.SessionDescription
	correlationID     string
	etag              string
	videoTrack        *webrtc.TrackLocalStaticRTP
	audioTrack        *webrtc.TrackLocalStaticRTP
	videoSource       *webrtc.TrackRemote
	whepClients       atomic.Int32
	videoPLIRequested atomic.Bool
	videoParamPacket  *rtp.Packet
	videoSPSPacket    *rtp.Packet
	videoPPSPacket    *rtp.Packet
	videoSPSBytes     int
	videoPPSBytes     int
	videoReady        atomic.Bool
	audioReady        atomic.Bool
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

func (stream *WebRTCStream) outputTracks() []*webrtc.TrackLocalStaticRTP {
	stream.mediaMu.RLock()
	defer stream.mediaMu.RUnlock()

	tracks := make([]*webrtc.TrackLocalStaticRTP, 0, 2)
	if stream.videoTrack != nil {
		tracks = append(tracks, stream.videoTrack)
	}
	if stream.audioTrack != nil {
		tracks = append(tracks, stream.audioTrack)
	}
	return tracks
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
	if stream == nil || stream.wsConn == nil || stream.peerConnection == nil {
		return false
	}

	switch stream.peerConnection.ConnectionState() {
	case webrtc.PeerConnectionStateClosed, webrtc.PeerConnectionStateFailed:
		return false
	default:
		return true
	}
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

func (stream *WebRTCStream) status() map[string]interface{} {
	upstreamState := ""
	if stream.peerConnection != nil {
		upstreamState = stream.peerConnection.ConnectionState().String()
	}

	return map[string]interface{}{
		"upstream_state": upstreamState,
		"can_reuse":      stream.canReuse(),
		"video_ready":    stream.videoReady.Load(),
		"audio_ready":    stream.audioReady.Load(),
		"whep_clients":   stream.whepClients.Load(),
	}
}

func (stream *WebRTCStream) requestVideoKeyframe(reason string) error {
	stream.mediaMu.RLock()
	videoSource := stream.videoSource
	peerConnection := stream.peerConnection
	stream.mediaMu.RUnlock()

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

	log.Printf("[WHEP_PROXY] Requested keyframe (%s) for SSRC=%d", reason, videoSource.SSRC())
	return nil
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

func parseSTAPAParameterSets(payload []byte) (int, int) {
	if len(payload) < 3 {
		return 0, 0
	}

	var spsBytes int
	var ppsBytes int
	for i := 1; i+2 <= len(payload); {
		naluSize := int(payload[i])<<8 | int(payload[i+1])
		i += 2
		if naluSize <= 0 || i+naluSize > len(payload) {
			break
		}

		switch payload[i] & 0x1F {
		case 7:
			spsBytes = naluSize
		case 8:
			ppsBytes = naluSize
		}
		i += naluSize
	}

	return spsBytes, ppsBytes
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
		stream.videoParamPacket = nil
		stream.videoSPSPacket = cloneRTPPacket(pkt)
		stream.videoSPSBytes = len(pkt.Payload)
	case 8:
		stream.videoParamPacket = nil
		stream.videoPPSPacket = cloneRTPPacket(pkt)
		stream.videoPPSBytes = len(pkt.Payload)
	case 24:
		spsBytes, ppsBytes := parseSTAPAParameterSets(pkt.Payload)
		if spsBytes == 0 && ppsBytes == 0 {
			return
		}
		if spsBytes > 0 && ppsBytes > 0 {
			stream.videoParamPacket = cloneRTPPacket(pkt)
			stream.videoSPSPacket = nil
			stream.videoPPSPacket = nil
			stream.videoSPSBytes = spsBytes
			stream.videoPPSBytes = ppsBytes
			return
		}
		if spsBytes > 0 {
			stream.videoSPSPacket = cloneRTPPacket(pkt)
			stream.videoSPSBytes = spsBytes
		}
		if ppsBytes > 0 {
			stream.videoPPSPacket = cloneRTPPacket(pkt)
			stream.videoPPSBytes = ppsBytes
		}
	}
}

func (stream *WebRTCStream) replayVideoParameterSets(
	localTrack *webrtc.TrackLocalStaticRTP,
	streamID string,
	timestamp uint32,
) bool {
	stream.mediaMu.RLock()
	paramPacket := cloneRTPPacket(stream.videoParamPacket)
	spsPacket := cloneRTPPacket(stream.videoSPSPacket)
	ppsPacket := cloneRTPPacket(stream.videoPPSPacket)
	spsBytes := stream.videoSPSBytes
	ppsBytes := stream.videoPPSBytes
	stream.mediaMu.RUnlock()

	if paramPacket != nil && spsBytes > 0 && ppsBytes > 0 {
		paramPacket.Timestamp = timestamp
		if err := localTrack.WriteRTP(paramPacket); err != nil {
			log.Printf("[WHEP_PROXY] Failed replaying STAP-A SPS/PPS before IDR for %s: %v", streamID, err)
			return false
		}
		log.Printf("[WHEP_PROXY] Replayed SPS (%d bytes) + PPS (%d bytes) before IDR for %s", spsBytes, ppsBytes, streamID)
		return true
	}

	if spsPacket == nil || ppsPacket == nil || spsBytes == 0 || ppsBytes == 0 {
		log.Printf("[WHEP_PROXY] Missing buffered SPS/PPS before IDR for %s: sps=%d pps=%d", streamID, spsBytes, ppsBytes)
		return false
	}

	spsPacket.Timestamp = timestamp
	ppsPacket.Timestamp = timestamp
	if err := localTrack.WriteRTP(spsPacket); err != nil {
		log.Printf("[WHEP_PROXY] Failed replaying SPS before IDR for %s: %v", streamID, err)
		return false
	}
	if err := localTrack.WriteRTP(ppsPacket); err != nil {
		log.Printf("[WHEP_PROXY] Failed replaying PPS before IDR for %s: %v", streamID, err)
		return false
	}

	log.Printf("[WHEP_PROXY] Replayed SPS (%d bytes) + PPS (%d bytes) before IDR for %s", spsBytes, ppsBytes, streamID)
	return true
}

func forwardTrack(streamID string, stream *WebRTCStream, track *webrtc.TrackRemote, localTrack *webrtc.TrackLocalStaticRTP) {
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
			return
		}

		readCount++
		if track.Kind() == webrtc.RTPCodecTypeVideo {
			stream.bufferVideoParameterSet(pkt)
			isIDR, packetDesc := h264PacketInfo(pkt.Payload)
			if isIDR {
				if !stream.replayVideoParameterSets(localTrack, streamID, pkt.Timestamp) {
					droppedCount++
					continue
				}
				log.Printf(
					"[WHEP_PROXY] IDR for %s: seq=%d marker=%t bytes=%d desc=%s",
					streamID,
					pkt.SequenceNumber,
					pkt.Marker,
					len(pkt.Payload),
					packetDesc,
				)
			}
		}

		if stream.whepClients.Load() == 0 {
			droppedCount++
		} else if err = localTrack.WriteRTP(pkt); err != nil {
			droppedCount++
		} else {
			writtenCount++
			if track.Kind() == webrtc.RTPCodecTypeVideo && stream.videoPLIRequested.CompareAndSwap(true, false) {
				if pliErr := stream.requestVideoKeyframe("first downstream write"); pliErr != nil {
					log.Printf("[WHEP_PROXY] Failed to request keyframe for %s after first write: %v", streamID, pliErr)
					stream.videoPLIRequested.Store(true)
				}
			}
		}

		if readCount%1000 == 0 {
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

func main() {
	r := mux.NewRouter()
	r.HandleFunc("/whep/{streamID}", whepHandler).Methods("GET", "OPTIONS", "POST")
	r.HandleFunc("/websocket/{streamID}", websocketHandler).Methods("GET", "POST")
	r.HandleFunc("/status/{streamID}", statusHandler).Methods("GET")

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
		cleanupStreamLocked(streamID, stream)
	}
}

func cleanupStreamLocked(streamID string, stream *WebRTCStream) {
	if stream.wsConn != nil {
		stream.wsMu.Lock()
		_ = stream.wsConn.Close()
		stream.wsConn = nil
		stream.wsMu.Unlock()
	}
	if stream.peerConnection != nil {
		_ = stream.peerConnection.Close()
		stream.peerConnection = nil
	}
	delete(streams, streamID)
}

func cleanupStream(streamID string, stream *WebRTCStream) {
	streamsMu.Lock()
	defer streamsMu.Unlock()
	cleanupStreamLocked(streamID, stream)
}

func cleanupStreamIfCurrent(streamID string, stream *WebRTCStream) {
	streamsMu.Lock()
	defer streamsMu.Unlock()
	if current, ok := streams[streamID]; ok && current == stream {
		cleanupStreamLocked(streamID, stream)
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
	stream *WebRTCStream,
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

	stream.wsMu.Lock()
	defer stream.wsMu.Unlock()
	return stream.wsConn.WriteJSON(envelope)
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
	for _, server := range config.ICEServers {
		iceServers = append(iceServers, webrtc.ICEServer{
			URLs:       []string{server.URL},
			Username:   server.Username,
			Credential: server.Credential,
		})
	}
	if len(iceServers) == 0 {
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

	return webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(interceptorRegistry),
	).NewPeerConnection(webrtc.Configuration{ICEServers: iceServers})
}

func handleRemoteAnswer(streamID string, stream *WebRTCStream, msg map[string]interface{}) error {
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
	if err := stream.peerConnection.SetRemoteDescription(answer); err != nil {
		return fmt.Errorf("set remote description: %w", err)
	}
	stream.remoteDescription = &answer

	for _, candidate := range stream.pendingCandidates {
		if err := stream.peerConnection.AddICECandidate(candidate); err != nil {
			fmt.Println("[WHEP_PROXY] Failed to add queued ICE candidate:", err)
		}
	}
	stream.pendingCandidates = nil
	return nil
}

func handleRemoteCandidate(streamID string, stream *WebRTCStream, msg map[string]interface{}) error {
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

	if stream.remoteDescription == nil {
		stream.pendingCandidates = append(stream.pendingCandidates, candidate)
		fmt.Println("[WHEP_PROXY] Queued ICE_CANDIDATE for", streamID)
		return nil
	}

	fmt.Println("[WHEP_PROXY] Received ICE_CANDIDATE for", streamID)
	return stream.peerConnection.AddICECandidate(candidate)
}

func createAndSendOffer(streamID string, stream *WebRTCStream) error {
	offer, err := stream.peerConnection.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create offer: %w", err)
	}
	if err := stream.peerConnection.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set local description: %w", err)
	}

	// Wait for ICE gathering to complete so the offer SDP includes local candidates.
	// Some KVS/camera implementations expect at least one candidate before responding.
	gatherComplete := webrtc.GatheringCompletePromise(stream.peerConnection)
	select {
	case <-gatherComplete:
		// done
	case <-time.After(10 * time.Second):
		fmt.Println("[WHEP_PROXY] ICE gathering timeout for", streamID, "; sending offer anyway")
	}

	localDescription := stream.peerConnection.LocalDescription()
	if localDescription == nil {
		return fmt.Errorf("local description unavailable after SetLocalDescription")
	}

	envelope := map[string]interface{}{
		"type": "offer",
		"sdp":  rewriteSessionLine(localDescription.SDP, stream.correlationID),
	}
	if decoded, err := json.Marshal(envelope); err == nil {
		fmt.Println("[WHEP_PROXY] SDP_OFFER payload for", streamID, string(decoded))
	}
	if payload, err := json.Marshal(map[string]interface{}{
		"action":         "SDP_OFFER",
		"messagePayload": base64.StdEncoding.EncodeToString(mustJSON(envelope)),
		"correlationId":  stream.correlationID,
	}); err == nil {
		fmt.Println("[WHEP_PROXY] Sending envelope for", streamID, string(payload))
	}
	return sendSignalingMessage(stream, "SDP_OFFER", envelope, stream.correlationID)
}

func websocketHandler(w http.ResponseWriter, r *http.Request) {
	streamID := mux.Vars(r)["streamID"]

	var config WebRTCConfig
	if r.Method == "POST" {
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
				cleanupStreamLocked(streamID, existing)
			} else {
				streamsMu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"status":"ok","reused":true}`))
				return
			}
		}
		streamsMu.Unlock()
	}

	decodedURL, err := decodeSignalingURL(config.SignalingURL)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to decode WebSocket URL: %v", err), http.StatusInternalServerError)
		return
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
			fmt.Println("[WHEP_PROXY] Websocket handshake status:", resp.Status)
			fmt.Println("[WHEP_PROXY] Websocket handshake headers:", resp.Header)
		}
		if resp != nil && resp.Body != nil {
			bodyBytes := make([]byte, 2048)
			if n, readErr := resp.Body.Read(bodyBytes); readErr == nil || readErr == io.EOF {
				fmt.Println("[WHEP_PROXY] Websocket response:", string(bodyBytes[:n]))
			}
		}
		http.Error(w, fmt.Sprintf("Failed to connect to WebSocket: %v", err), http.StatusInternalServerError)
		return
	}

	streamsMu.Lock()
	defer streamsMu.Unlock()

	if existing, ok := streams[streamID]; ok {
		fmt.Println("[WHEP_PROXY] Replacing existing stream for", streamID)
		cleanupStreamLocked(streamID, existing)
	}

	peerConnection, err := createPeerConnection(config)
	if err != nil {
		_ = conn.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video",
		"pion",
	)
	if err != nil {
		_ = conn.Close()
		_ = peerConnection.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	audioTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU},
		"audio",
		"pion",
	)
	if err != nil {
		_ = conn.Close()
		_ = peerConnection.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	stream := &WebRTCStream{
		peerConnection: peerConnection,
		wsConn:         conn,
		correlationID:  generateCorrelationID(config.PhoneID),
		videoTrack:     videoTrack,
		audioTrack:     audioTrack,
	}
	streams[streamID] = stream

	if _, err = peerConnection.AddTransceiverFromKind(
		webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly},
	); err != nil {
		cleanupStreamLocked(streamID, stream)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if _, err = peerConnection.AddTransceiverFromKind(
		webrtc.RTPCodecTypeAudio,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly},
	); err != nil {
		cleanupStreamLocked(streamID, stream)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	peerConnection.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("[WHEP_PROXY] ICE connection state for %s: %s", streamID, state.String())
	})
	peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("[WHEP_PROXY] Peer connection state for %s: %s", streamID, state.String())
	})

	peerConnection.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		candidate := c.ToJSON()
		if err := sendSignalingMessage(stream, "ICE_CANDIDATE", candidate, stream.correlationID); err != nil {
			fmt.Println("[WHEP_PROXY] Error sending ICE candidate:", err)
		}
	})

	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Printf("[WHEP_PROXY] Received track for %s: codec=%s", streamID, track.Codec().MimeType)
		fmt.Printf(
			"[WHEP_PROXY] Received remote track for %s: kind=%s codec=%s payloadType=%d\n",
			streamID,
			track.Kind().String(),
			track.Codec().MimeType,
			track.PayloadType(),
		)
		var localTrack *webrtc.TrackLocalStaticRTP
		switch track.Kind() {
		case webrtc.RTPCodecTypeVideo:
			stream.setVideoSource(track)
			localTrack = stream.videoTrack
		case webrtc.RTPCodecTypeAudio:
			stream.setAudioReady(true)
			localTrack = stream.audioTrack
		default:
			return
		}

		go readReceiverRTCP(streamID, track, receiver)
		go forwardTrack(streamID, stream, track, localTrack)

		if track.Kind() == webrtc.RTPCodecTypeVideo {
			go func() {
				ticker := time.NewTicker(5 * time.Second)
				defer ticker.Stop()

				for range ticker.C {
					if stream.peerConnection == nil || stream.peerConnection.ConnectionState() == webrtc.PeerConnectionStateClosed {
						return
					}
					if stream.whepClients.Load() == 0 {
						continue
					}
					if err := stream.requestVideoKeyframe("periodic downstream refresh"); err != nil {
						log.Printf("[WHEP_PROXY] Failed to request keyframe for %s: %v", streamID, err)
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
				cleanupStreamIfCurrent(streamID, stream)
				return
			}

			if len(data) == 0 {
				log.Println("[WHEP_PROXY] Skipping empty keepalive frame")
				continue
			}

			// Raw message logging: type and first 200 bytes (hex for binary, plain for text).
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
				// Try raw JSON first (KVS often sends JSON in binary frames); fall back to base64-decode then JSON.
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

			if correlationID, _ := msg["correlationId"].(string); correlationID != "" && correlationID != stream.correlationID {
				fmt.Printf("[WHEP_PROXY] Ignoring %s for %s due to mismatched correlationId %q\n", msgType, streamID, correlationID)
				continue
			}

			switch msgType {
			case "SDP_ANSWER":
				if err := handleRemoteAnswer(streamID, stream, msg); err != nil {
					fmt.Println("[WHEP_PROXY] Failed to handle SDP_ANSWER:", err)
				}
			case "ICE_CANDIDATE":
				if err := handleRemoteCandidate(streamID, stream, msg); err != nil {
					fmt.Println("[WHEP_PROXY] Failed to handle ICE_CANDIDATE:", err)
				}
			case "STATUS_RESPONSE":
				fmt.Println("[WHEP_PROXY] Received STATUS_RESPONSE for", streamID, msg)
			default:
				fmt.Println("[WHEP_PROXY] Ignoring signaling message type:", msgType)
			}
		}
	}()

	// Respond 200 immediately so the client (Python) does not time out and retry. The client uses
	// a 10s timeout; delay + ICE gathering + offer can exceed that and cause a second POST, which
	// would replace this stream and close the websocket before ICE/media establish.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))

	// Run delay and SDP_OFFER in a goroutine so the handler can return and keep the websocket alive.
	go func() {
		if delayMs := os.Getenv("WHEP_SIGNALING_DELAY_MS"); delayMs != "" {
			if ms, err := strconv.Atoi(delayMs); err == nil && ms > 0 {
				d := time.Duration(ms) * time.Millisecond
				fmt.Printf("[WHEP_PROXY] Waiting %v before sending SDP_OFFER for %s\n", d, streamID)
				time.Sleep(d)
			}
		}
		if err := createAndSendOffer(streamID, stream); err != nil {
			fmt.Println("[WHEP_PROXY] createAndSendOffer failed:", err)
			cleanupStream(streamID, stream)
		}
	}()
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
		if r.Header.Get("Content-Type") != "application/sdp" {
			http.Error(w, "Content-Type must be application/sdp", http.StatusUnsupportedMediaType)
			return
		}

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

		peerConnection, err := createPeerConnection(WebRTCConfig{})
		if err != nil {
			http.Error(w, "Error creating peer connection", http.StatusInternalServerError)
			return
		}

		tracks := stream.outputTracks()
		if len(tracks) == 0 {
			_ = peerConnection.Close()
			http.Error(w, "Stream has no output tracks", http.StatusServiceUnavailable)
			return
		}

		var countedClient atomic.Bool
		peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
			log.Printf("[WHEP_PROXY] WHEP client state for %s: %s", streamID, state.String())
			switch state {
			case webrtc.PeerConnectionStateConnected:
				if countedClient.CompareAndSwap(false, true) {
					stream.whepClients.Add(1)
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
		for _, track := range tracks {
			rtpSender, addTrackErr := peerConnection.AddTrack(track)
			if addTrackErr != nil {
				_ = peerConnection.Close()
				http.Error(w, "Error adding track", http.StatusInternalServerError)
				return
			}
			switch strings.ToLower(track.Codec().MimeType) {
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

		w.Header().Set("Content-Type", "application/sdp")
		w.Header().Set("Location", fmt.Sprintf("/whep/%s", streamID))
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, localDescription.SDP)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}
