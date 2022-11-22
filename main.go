package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/pion/dtls/v2/pkg/protocol/extension"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v3"
)

var (
	endpoint  = flag.String("endpoint", "https://customer-<placeholder>.cloudflarestream.com/<placeholder>/webRTC/publish", "WHIP or WHEP endpoint to publish/play")
	mode      = flag.String("mode", "publisher", "Publish to or play from the endpoint")
	videoAddr = flag.String("video", "127.0.0.1:11111", "Address for incoming/outgoing video stream RTP (can be vp8 or vp9)")
	audioAddr = flag.String("audio", "127.0.0.1:11112", "Address for incoming/outgoing audio stream RTP (must be Opus)")
	codec     = flag.String("codec", "vp9", "Codec for the publisher RTP, can be vp9 or vp8")
	noTrickle = flag.Bool("trickle", false, "Disable trickle ICE")
)

func main() {
	flag.Parse()

	iceUfrag := RandStringBytesMaskImpr(8)
	icePwd := RandStringBytesMaskImpr(24)
	s := webrtc.SettingEngine{}
	s.SetICECredentials(iceUfrag, icePwd)
	s.SetAnsweringDTLSRole(webrtc.DTLSRoleClient)
	s.SetSRTPProtectionProfiles(extension.SRTP_AEAD_AES_128_GCM, extension.SRTP_AEAD_AES_256_GCM)

	m := webrtc.MediaEngine{}
	m.RegisterDefaultCodecs()

	i := interceptor.Registry{}
	webrtc.RegisterDefaultInterceptors(&m, &i)

	api := webrtc.NewAPI(webrtc.WithSettingEngine(s), webrtc.WithMediaEngine(&m), webrtc.WithInterceptorRegistry(&i))

	peerConnection, err := api.NewPeerConnection(webrtc.Configuration{
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
		log.Fatalf("cannot create PeerConnection: %v", err)
	}
	defer func() {
		if cErr := peerConnection.Close(); cErr != nil {
			log.Printf("cannot close peerConnection: %v\n", cErr)
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
		log.Fatalf("unexpected mode: %s, expecting publisher or player", *mode)
	}

	peerConnection.OnSignalingStateChange(func(ss webrtc.SignalingState) {
		log.Printf("Signaling State has changed: %s \n", ss.String())
	})

	peerConnection.OnICEGatheringStateChange(func(is webrtc.ICEGathererState) {
		log.Printf("ICE Gathering State has changed: %s \n", is.String())
	})

	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		log.Printf("ICE Connection State has changed: %s \n", connectionState.String())
		switch connectionState {
		case webrtc.ICEConnectionStateConnected:
			iceConnectedCtxCancel()
		}
	})

	peerConnection.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Printf("Peer Connection State has changed: %s\n", s.String())
		switch s {
		case webrtc.PeerConnectionStateFailed:
			os.Exit(0)
		case webrtc.PeerConnectionStateConnected:
			pair := <-p
			videoCandidate, vErr := pair.Video.Transport().ICETransport().GetSelectedCandidatePair()
			audioCandidate, aErr := pair.Audio.Transport().ICETransport().GetSelectedCandidatePair()
			if vErr != nil || aErr != nil || videoCandidate == nil || audioCandidate == nil {
				return
			}
			log.Printf("Video stream candidate: (local %s)-(remote %s)\n", videoCandidate.Local.String(), videoCandidate.Remote.String())
			log.Printf("Audio stream candidate: (local %s)-(remote %s)\n", audioCandidate.Local.String(), audioCandidate.Remote.String())
		}
	})

	wish := &WISH{
		Endpoint: *endpoint,
		HTTPClient: &http.Client{
			Timeout: time.Second * 3,
		},
	}
	defer wish.Terminate()

	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		log.Fatalf("failed to create offer: %v\n", err)
	}

	var answer string
	if *noTrickle {
		if err := peerConnection.SetLocalDescription(offer); err != nil {
			log.Fatalf("error setting local description: %v\n", err)
		}

		<-webrtc.GatheringCompletePromise(peerConnection)

		offer = *peerConnection.LocalDescription()

		answer, err = wish.Signal(offer.SDP)
		if err != nil {
			log.Fatalf("failed to exchange offer: %v\n", err)
		}
	} else {
		answer, err = wish.Signal(offer.SDP)
		if err != nil {
			log.Fatalf("failed to exchange offer: %v\n", err)
		}
		peerConnection.OnICECandidate(func(i *webrtc.ICECandidate) {
			if i == nil {
				return
			}
			log.Printf("trickling ICE candidate: %v\n", i.String())
			if err := wish.Trickle(iceUfrag, icePwd, i.ToJSON().Candidate); err != nil {
				log.Printf("failed to trickle ICE candidate: %v\n", err)
			}
		})
		if err := peerConnection.SetLocalDescription(offer); err != nil {
			log.Fatalf("error setting local description: %v\n", err)
		}
	}

	peerConnection.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  answer,
	})

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	log.Printf("received signal to stop: %v\n", <-sigs)
}

type transporter interface {
	Transport() *webrtc.DTLSTransport
}

type WISH struct {
	HTTPClient *http.Client
	Endpoint   string
	Resource   string
	etag       string
}

func (w *WISH) Signal(offer string) (answer string, err error) {
	req, err := http.NewRequest("POST", w.Endpoint, bytes.NewBufferString(offer))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/sdp")

	resp, err := w.HTTPClient.Do(req)
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

	log.Printf("wish: got resource url %s\n", w.Resource)

	w.etag = resp.Header.Get("ETag")

	return string(responseText), nil
}

func (w *WISH) Trickle(ufrag, pwd, candidate string) error {
	if w.Resource == "" {
		return fmt.Errorf("no Resource URL")
	}

	buf := bytes.Buffer{}
	sdpFrag.Execute(&buf, struct {
		Username  string
		Password  string
		Candidate string
	}{
		Username:  ufrag,
		Password:  pwd,
		Candidate: candidate,
	})
	req, err := http.NewRequest("PATCH", w.Resource, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/trickle-ice-sdpfrag")

	resp, err := w.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	responseText, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode != 204 {
		return fmt.Errorf("unexpected response (%d): %s", resp.StatusCode, responseText)
	}

	return nil
}

func (w *WISH) Terminate() {
	if w.Resource == "" {
		return
	}

	log.Printf("Disconnecting stream vie DELETE")

	req, err := http.NewRequest("DELETE", w.Resource, nil)
	if err != nil {
		return
	}

	resp, err := w.HTTPClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
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
			log.Fatalf("failed to read from network: %v", err)
		}
		if _, err := sender.Write(buf[:n]); err != nil {
			log.Fatalf("failed to write rtp: %v", err)
		}
	}
}

func webrtc2rtp(receiver *webrtc.TrackRemote, conn net.PacketConn, dst net.Addr) {
	buf := make([]byte, 1500)
	for {
		n, _, err := receiver.Read(buf)
		if err != nil {
			log.Fatalf("failed to read from rtp: %v", err)
		}
		if _, err := conn.WriteTo(buf[:n], dst); err != nil {
			log.Fatalf("failed to write to network: %v", err)
		}
	}
}

type Pair struct {
	Video transporter
	Audio transporter
}

func setupPublisher(ctx context.Context, ps *webrtc.PeerConnection) <-chan Pair {
	var trackMime string
	switch strings.ToLower(*codec) {
	case "vp8":
		trackMime = webrtc.MimeTypeVP8
	case "vp9":
		trackMime = webrtc.MimeTypeVP9
	default:
		log.Fatalf("unknown codec: %s", *codec)
	}
	videoConn, err := net.ListenPacket("udp", *videoAddr)
	if err != nil {
		panic(err)
	}
	audioConn, err := net.ListenPacket("udp", *audioAddr)
	if err != nil {
		panic(err)
	}

	videoTrack, videoTrackErr := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: trackMime, ClockRate: 90000}, "video", RandStringBytesMaskImpr(8))
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
		log.Printf("Forwarding RTP video to WebRTC (%s)\n", videoTrack.Codec().MimeType)
		go drainRTCP(videoRtpSender)
		rtp2webrtc(videoConn, videoTrack)
	}()

	go func() {
		<-ctx.Done()
		log.Printf("Forwarding RTP audio to WebRTC (%s)\n", audioTrack.Codec().MimeType)
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
		codec := tr.Codec()
		switch tr.Kind() {
		case webrtc.RTPCodecTypeVideo:
			go func() {
				<-ctx.Done()
				log.Printf("Forwarding WebRTC video to RTP (%s)\n", codec.MimeType)
				webrtc2rtp(tr, videoConn, videoDst)
			}()
			pMu.Lock()
			p.Video = r
			pMu.Unlock()

			wg.Done()
		case webrtc.RTPCodecTypeAudio:
			go func() {
				<-ctx.Done()
				log.Printf("Forwarding WebRTC audio to RTP (%s)\n", codec.MimeType)
				webrtc2rtp(tr, audioConn, audioDst)
			}()
			pMu.Lock()
			p.Audio = r
			pMu.Unlock()

			wg.Done()
		default:
			log.Fatalf("unexpected track mime: %s", tr.Codec().MimeType)
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

const sdpFragTmpl = `a=ice-ufrag:{{.Username}}
a=ice-pwd:{{.Password}}
m=audio 9 RTP/AVP 0
a=mid:0
a={{.Candidate}}
`

var sdpFrag = template.Must(template.New("sdpfrag").Parse(sdpFragTmpl))
