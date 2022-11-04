package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v3"
)

var (
	endpoint  = flag.String("endpoint", "https://customer-<placeholder>.cloudflarestream.com/<placeholder>/webRTC/publish", "WHIP or WHEP endpoint to publish/play")
	mode      = flag.String("mode", "publisher", "Publish to or play from the endpoint")
	videoAddr = flag.String("video", "127.0.0.1:11111", "Address for incoming/outgoing video stream RTP (must be VP8)")
	audioAddr = flag.String("audio", "127.0.0.1:11112", "Address for incoming/outgoing audio stream RTP (must be Opus)")
)

func main() {
	flag.Parse()

	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.cloudflare.com:3478"},
			},
		},
		BundlePolicy:  webrtc.BundlePolicyMaxBundle,
		RTCPMuxPolicy: webrtc.RTCPMuxPolicyRequire,
		SDPSemantics:  webrtc.SDPSemanticsUnifiedPlan,
	})
	if err != nil {
		panic(err)
	}
	defer func() {
		if cErr := peerConnection.Close(); cErr != nil {
			fmt.Printf("cannot close peerConnection: %v\n", cErr)
		}
	}()

	iceConnectedCtx, iceConnectedCtxCancel := context.WithCancel(context.Background())

	var p <-chan Pair
	switch *mode {
	case "publisher":
		p = setupPublisher(iceConnectedCtx, peerConnection)
	case "player":
		p = setupPlayer(iceConnectedCtx, peerConnection)
	default:
		panic(fmt.Sprintf("unexpected mode: %s, expecting publisher or player", *mode))
	}

	peerConnection.OnSignalingStateChange(func(ss webrtc.SignalingState) {
		fmt.Printf("Signaling State has changed: %s \n", ss.String())
	})

	peerConnection.OnICEGatheringStateChange(func(is webrtc.ICEGathererState) {
		fmt.Printf("ICE Gathering State has changed: %s \n", is.String())
	})

	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("ICE Connection State has changed: %s \n", connectionState.String())
		switch connectionState {
		case webrtc.ICEConnectionStateConnected:
			iceConnectedCtxCancel()
		}
	})

	peerConnection.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		fmt.Printf("Peer Connection State has changed: %s\n", s.String())
		switch s {
		case webrtc.PeerConnectionStateFailed:
			os.Exit(0)
		case webrtc.PeerConnectionStateConnected:
			pair := <-p
			videoCandidate, err := pair.Video.Transport().ICETransport().GetSelectedCandidatePair()
			if err != nil {
				panic(err)
			}
			audioCandidate, err := pair.Audio.Transport().ICETransport().GetSelectedCandidatePair()
			if err != nil {
				panic(err)
			}
			fmt.Printf("Video stream candidate: (local %s)-(remote %s)\n", videoCandidate.Local.String(), videoCandidate.Remote.String())
			fmt.Printf("Audio stream candidate: (local %s)-(remote %s)\n", audioCandidate.Local.String(), audioCandidate.Remote.String())
		}
	})

	wish := &WISH{
		Endpoint: *endpoint,
	}
	defer wish.Terminate()

	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		panic(err)
	}

	if err := peerConnection.SetLocalDescription(offer); err != nil {
		panic(err)
	}

	<-webrtc.GatheringCompletePromise(peerConnection)

	offer = *peerConnection.LocalDescription()

	answer, err := wish.Signal(offer.SDP)
	if err != nil {
		panic(err)
	}

	peerConnection.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  answer,
	})

	select {}

}

type transporter interface {
	Transport() *webrtc.DTLSTransport
}

type WISH struct {
	Endpoint string
	Resource string
	etag     string
}

func (w *WISH) Signal(offer string) (answer string, err error) {
	req, err := http.NewRequest("POST", w.Endpoint, bytes.NewBufferString(offer))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/sdp")

	c := &http.Client{
		Timeout: time.Second * 3,
	}

	resp, err := c.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	responseText, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}

	if resp.StatusCode != 201 {
		return "", fmt.Errorf("unexpected response (%d): %s", resp.StatusCode, responseText)
	}

	r := resp.Header.Get("Location")
	if strings.HasPrefix(r, "http") {
		// absolute
		w.Resource = r
	} else {
		// relative
		parsed, err := url.Parse(w.Endpoint)
		if err != nil {
			return "", fmt.Errorf("parsing resource url from header: %w", err)
		}
		parsed.Path = r
		w.Resource = parsed.String()
	}

	fmt.Printf("wish: got resource url %s\n", w.Resource)

	w.etag = resp.Header.Get("ETag")

	return string(responseText), nil
}

func (w *WISH) Trickle(c *webrtc.ICECandidate) error {
	// TODO: batch candidates
	panic("not implemented yet")
}

func (w *WISH) Terminate() {
	if w.Resource == "" {
		return
	}
	req, err := http.NewRequest("DELETE", w.Resource, nil)
	if err != nil {
		return
	}

	c := &http.Client{
		Timeout: time.Second * 3,
	}

	resp, err := c.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
}

func drainRTCP(x *webrtc.RTPSender) {
	rtcpBuf := make([]byte, 1500)
	for {
		if _, _, rtcpErr := x.Read(rtcpBuf); rtcpErr != nil {
			return
		}
	}
}

func rtp2webrtc(conn net.PacketConn, sender *webrtc.TrackLocalStaticRTP) {
	buf := make([]byte, 1500)

	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			panic(err)
		}
		if _, err := sender.Write(buf[:n]); err != nil {
			panic(err)
		}
	}
}

func webrtc2rtp(receiver *webrtc.TrackRemote, conn net.PacketConn, dst net.Addr) {
	buf := make([]byte, 1500)
	for {
		n, _, err := receiver.Read(buf)
		if err != nil {
			panic(err)
		}
		if _, err := conn.WriteTo(buf[:n], dst); err != nil {
			panic(err)
		}
	}
}

type Pair struct {
	Video transporter
	Audio transporter
}

func setupPublisher(ctx context.Context, ps *webrtc.PeerConnection) <-chan Pair {
	videoConn, err := net.ListenPacket("udp", *videoAddr)
	if err != nil {
		panic(err)
	}
	audioConn, err := net.ListenPacket("udp", *audioAddr)
	if err != nil {
		panic(err)
	}

	videoTrack, videoTrackErr := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}, "video", RandStringBytesMaskImpr(8))
	if videoTrackErr != nil {
		panic(videoTrackErr)
	}

	videoTransceiver, videoTrackErr := ps.AddTransceiverFromTrack(videoTrack, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
	if videoTrackErr != nil {
		panic(videoTrackErr)
	}
	videoRtpSender := videoTransceiver.Sender()

	audioTrack, audioTrackErr := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", RandStringBytesMaskImpr(8))
	if audioTrackErr != nil {
		panic(audioTrackErr)
	}

	audioTransceiver, audioTrackErr := ps.AddTransceiverFromTrack(audioTrack, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
	if audioTrackErr != nil {
		panic(audioTrackErr)
	}
	audioRtpSender := audioTransceiver.Sender()

	go func() {
		<-ctx.Done()
		fmt.Printf("Forwarding RTP video to WebRTC\n")
		go drainRTCP(videoRtpSender)
		rtp2webrtc(videoConn, videoTrack)
	}()

	go func() {
		<-ctx.Done()
		fmt.Printf("Forwarding RTP audio to WebRTC\n")
		go drainRTCP(audioRtpSender)
		rtp2webrtc(audioConn, audioTrack)
	}()

	r := make(chan Pair, 1)

	r <- Pair{
		Video: videoRtpSender,
		Audio: audioRtpSender,
	}

	return r
}

func setupPlayer(ctx context.Context, ps *webrtc.PeerConnection) <-chan Pair {
	videoConn, err := net.ListenPacket("udp", ":0")
	if err != nil {
		panic(err)
	}
	audioConn, err := net.ListenPacket("udp", ":0")
	if err != nil {
		panic(err)
	}

	videoDst, err := net.ResolveUDPAddr("udp", *videoAddr)
	if err != nil {
		panic(err)
	}
	audioDst, err := net.ResolveUDPAddr("udp", *audioAddr)
	if err != nil {
		panic(err)
	}

	wg := sync.WaitGroup{}
	wg.Add(2)

	var pMu sync.Mutex
	p := Pair{}

	_, err = ps.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	if err != nil {
		panic(err)
	}
	_, err = ps.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	if err != nil {
		panic(err)
	}

	ps.OnTrack(func(tr *webrtc.TrackRemote, r *webrtc.RTPReceiver) {
		switch tr.Codec().MimeType {
		case webrtc.MimeTypeVP8:
			go func() {
				<-ctx.Done()
				fmt.Printf("Forwarding WebRTC video to RTP\n")
				webrtc2rtp(tr, videoConn, videoDst)
			}()
			pMu.Lock()
			p.Video = r
			pMu.Unlock()

			wg.Done()
		case webrtc.MimeTypeOpus:
			go func() {
				<-ctx.Done()
				fmt.Printf("Forwarding WebRTC audio to RTP\n")
				webrtc2rtp(tr, audioConn, audioDst)
			}()
			pMu.Lock()
			p.Audio = r
			pMu.Unlock()

			wg.Done()
		default:
			panic(fmt.Sprintf("unexpected track mime: %s", tr.Codec().MimeType))
		}
	})

	r := make(chan Pair, 1)

	go func() {
		wg.Wait()
		r <- p
	}()

	return r
}

// https://stackoverflow.com/a/31832326
const letterBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
const (
	letterIdxBits = 6                    // 6 bits to represent a letter index
	letterIdxMask = 1<<letterIdxBits - 1 // All 1-bits, as many as letterIdxBits
	letterIdxMax  = 63 / letterIdxBits   // # of letter indices fitting in 63 bits
)

func RandStringBytesMaskImpr(n int) string {
	b := make([]byte, n)
	// A rand.Int63() generates 63 random bits, enough for letterIdxMax letters!
	for i, cache, remain := n-1, rand.Int63(), letterIdxMax; i >= 0; {
		if remain == 0 {
			cache, remain = rand.Int63(), letterIdxMax
		}
		if idx := int(cache & letterIdxMask); idx < len(letterBytes) {
			b[i] = letterBytes[idx]
			i--
		}
		cache >>= letterIdxBits
		remain--
	}

	return string(b)
}
