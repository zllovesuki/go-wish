## go-wish

A very simple go program to test WHIP/WHEP implementations

```
Usage of ./wish:
  -audio string
        Address for incoming/outgoing audio stream RTP (must be Opus) (default "127.0.0.1:11112")
  -endpoint string
        WHIP or WHEP endpoint to publish/play (default "https://customer-<placeholder>.cloudflarestream.com/<placeholder>/webRTC/publish")
  -mode string
        Publish to or play from the endpoint (default "publisher")
  -video string
        Address for incoming/outgoing video stream RTP (must be VP8) (default "127.0.0.1:11111")
```

### Fun commands

Play an webm (vp8/opus) file from ffmpeg and forward to RTP (forever):
```
ffmpeg -re -fflags +genpts -stream_loop -1 -i rick.webm -vcodec copy -an -f rtp 'rtp://127.0.0.1:11111?pkt_size=1400' -c:a libopus -b:a 128k -vn -f rtp -max_delay 0 -application lowdelay 'rtp://127.0.0.1:11112?pkt_size=1400'
```

Restream from 1 WHEP endpoint to another WHIP endpoint:
```
# player
wish -endpoint https://customer-abcd.cloudflarestream.com/xyz/webRTC/play -mode player
# publisher
wish -endpoint https://customer-1234.cloudflarestream.com/567/webRTC/publish
```

### Reference

WHIP: [https://datatracker.ietf.org/doc/draft-ietf-wish-whip/](https://datatracker.ietf.org/doc/draft-ietf-wish-whip/)

WHEP: [https://datatracker.ietf.org/doc/draft-murillo-whep/01/](https://datatracker.ietf.org/doc/draft-murillo-whep/01/)