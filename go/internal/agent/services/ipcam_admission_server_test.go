//go:build regression_camera_fixture

package services

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"image/color"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

// cameraAdmissionServer speaks ONVIF and RTSP/RTP over real local sockets.
// Every accepted connection is counted, including a liveness dial that sends
// no request. Its pixels are generated in memory; there are no image assets.
type cameraAdmissionServer struct {
	http                *httptest.Server
	rtsp                net.Listener
	httpConns           atomic.Int32
	httpRequests        atomic.Int32
	rtspConns           atomic.Int32
	rtspPlays           atomic.Int32
	authenticated       atomic.Int32
	mu                  sync.Mutex
	connections         []net.Conn
	wg                  sync.WaitGroup
	closing             bool
	accessUnit          []byte
	alternateAccessUnit atomic.Pointer[[]byte] // set before starting a client; synthetic freshness fixture
	phase               atomic.Int32
	sps, pps            []byte
}

func newCameraAdmissionServer(t *testing.T, ctx context.Context, launch string) *cameraAdmissionServer {
	t.Helper()
	f := &cameraAdmissionServer{}
	f.accessUnit = encodeAdmissionAccessUnit(t, ctx, launch, color.RGBA{R: 240, A: 255})
	// Read SPS/PPS from the real encoder for the RTSP session description.
	for _, nal := range bytes.Split(f.accessUnit, []byte{0, 0, 1})[1:] {
		nal = bytes.TrimRight(nal, "\x00")
		if len(nal) == 0 {
			continue
		}
		switch nal[0] & 0x1f {
		case 7:
			f.sps = nal
		case 8:
			f.pps = nal
		}
	}
	if len(f.sps) == 0 || len(f.pps) == 0 {
		t.Fatal("RTSP fixture has no SPS/PPS")
	}
	f.http = httptest.NewUnstartedServer(http.HandlerFunc(f.serveONVIF))
	f.http.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			f.httpConns.Add(1)
		}
	}
	f.http.Start()
	var err error
	f.rtsp, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		f.http.Close()
		t.Fatal(err)
	}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			conn, err := f.rtsp.Accept()
			if err != nil {
				return
			}
			f.rtspConns.Add(1)
			f.mu.Lock()
			if f.closing {
				f.mu.Unlock()
				_ = conn.Close()
				return
			}
			f.connections = append(f.connections, conn)
			f.mu.Unlock()
			f.serveRTSP(conn)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() {
		_ = f.rtsp.Close()
		f.mu.Lock()
		f.closing = true
		for _, conn := range f.connections {
			_ = conn.Close()
		}
		f.mu.Unlock()
		f.wg.Wait()
		f.http.Close()
	})
	return f
}

func encodeAdmissionAccessUnit(t *testing.T, ctx context.Context, launch string, left color.RGBA) []byte {
	t.Helper()
	// JPEG bytes carry no cadence. Declare the same 25fps used by RTP below,
	// so decoded caps and the kernel loopback time-per-frame are finite.
	jpegFrame := sensorLinkColorFrame(t, left).Data
	encoder := exec.CommandContext(ctx, launch, "-q", "fdsrc", "fd=0", "!", "image/jpeg,framerate=25/1", "!", "jpegparse", "!", "jpegdec", "!", "videoconvert", "!", "video/x-raw,format=I420", "!", "x264enc", "tune=zerolatency", "key-int-max=1", "byte-stream=true", "!", "video/x-h264,stream-format=byte-stream,alignment=au", "!", "fdsink", "fd=1")
	encoder.Stdin = bytes.NewReader(bytes.Repeat(jpegFrame, 3))
	var diagnostics bytes.Buffer
	encoder.Stderr = &diagnostics
	encoded, err := encoder.Output()
	if err != nil {
		t.Fatalf("RTSP fixture encode: %v: %s", err, diagnostics.String())
	}
	starts := annexBKeyframeStarts(encoded)
	if len(starts) < 2 {
		t.Fatal("RTSP fixture did not encode complete H.264 keyframes")
	}
	return encoded[starts[0]:starts[1]]
}

func (f *cameraAdmissionServer) serveONVIF(w http.ResponseWriter, req *http.Request) {
	f.httpRequests.Add(1)
	body, _ := io.ReadAll(req.Body)
	if !bytes.Contains(body, []byte("UsernameToken")) || !bytes.Contains(body, []byte(">camera-user<")) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var response string
	switch {
	case bytes.Contains(body, []byte("GetCapabilities")):
		response = fmt.Sprintf(`<tds:GetCapabilitiesResponse><tds:Capabilities><tt:Media><tt:XAddr>%s/onvif/media_service</tt:XAddr></tt:Media></tds:Capabilities></tds:GetCapabilitiesResponse>`, f.http.URL)
	case bytes.Contains(body, []byte("GetProfiles")):
		response = `<trt:GetProfilesResponse><trt:Profiles token="fixture"><tt:VideoEncoderConfiguration><tt:Encoding>H264</tt:Encoding><tt:Resolution><tt:Width>64</tt:Width><tt:Height>48</tt:Height></tt:Resolution></tt:VideoEncoderConfiguration></trt:Profiles></trt:GetProfilesResponse>`
	case bytes.Contains(body, []byte("GetStreamUri")):
		response = `<trt:GetStreamUriResponse><trt:MediaUri><tt:Uri>rtsp://127.0.0.1:554/measurement</tt:Uri></trt:MediaUri></trt:GetStreamUriResponse>`
	default:
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/soap+xml")
	fmt.Fprintf(w, `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:tt="http://www.onvif.org/ver10/schema" xmlns:trt="http://www.onvif.org/ver10/media/wsdl" xmlns:tds="http://www.onvif.org/ver10/device/wsdl"><s:Body>%s</s:Body></s:Envelope>`, response)
}

func (f *cameraAdmissionServer) serveRTSP(conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	reader := textproto.NewReader(bufio.NewReader(conn))
	for {
		line, err := reader.ReadLine()
		parts := strings.Fields(line)
		if err != nil || len(parts) != 3 {
			return
		}
		headers, err := reader.ReadMIMEHeader()
		if err != nil {
			return
		}
		uri, err := url.Parse(parts[1])
		if err != nil {
			return
		}
		method, seq := parts[0], headers.Get("CSeq")
		respond := func(code, headers, body string) bool {
			_, err := fmt.Fprintf(conn, "RTSP/1.0 %s\r\nCSeq: %s\r\n%sContent-Length: %d\r\n\r\n%s", code, seq, headers, len(body), body)
			return err == nil
		}
		if method != "OPTIONS" && headers.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("camera-user:camera-password")) {
			if !respond("401 Unauthorized", "WWW-Authenticate: Basic realm=\"fixture\"\r\n", "") {
				return
			}
			continue
		}
		switch method {
		case "OPTIONS":
			if !respond("200 OK", "Public: OPTIONS, DESCRIBE, SETUP, PLAY, TEARDOWN\r\n", "") {
				return
			}
		case "DESCRIBE":
			if uri.Path != "/measurement" {
				respond("404 Not Found", "", "")
				return
			}
			f.authenticated.Add(1)
			sdp := fmt.Sprintf("v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=fixture\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\na=control:*\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\na=fmtp:96 packetization-mode=1;sprop-parameter-sets=%s,%s\r\na=control:trackID=0\r\n", base64.StdEncoding.EncodeToString(f.sps), base64.StdEncoding.EncodeToString(f.pps))
			if !respond("200 OK", "Content-Type: application/sdp\r\nContent-Base: "+uri.String()+"/\r\n", sdp) {
				return
			}
		case "SETUP":
			if !strings.Contains(headers.Get("Transport"), "RTP/AVP/TCP") {
				respond("461 Unsupported Transport", "", "")
				return
			}
			if !respond("200 OK", "Session: fixture\r\nTransport: RTP/AVP/TCP;unicast;interleaved=0-1\r\n", "") {
				return
			}
		case "PLAY":
			if !respond("200 OK", "Session: fixture\r\nRange: npt=0.000-\r\n", "") {
				return
			}
			// The 20-second deadline bounds only the RTSP handshake. PLAY must
			// remain alive across the kernel test's idle-grace observations.
			if err := conn.SetDeadline(time.Time{}); err != nil {
				return
			}
			f.rtspPlays.Add(1)
			f.sendRTP(conn)
			return
		default:
			respond("200 OK", "Session: fixture\r\n", "")
		}
	}
}

func (f *cameraAdmissionServer) sendRTP(conn net.Conn) {
	payloader := &codecs.H264Payloader{}
	ticker := time.NewTicker(40 * time.Millisecond)
	defer ticker.Stop()
	var seq uint16
	var timestamp uint32
	for range ticker.C {
		// Bound a stalled peer independently of the session's total lifetime.
		// Fixture cleanup closes this owned connection and joins the sender.
		if err := conn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
			return
		}
		accessUnit := f.accessUnit
		if alternate := f.alternateAccessUnit.Load(); f.phase.Load() == 1 && alternate != nil {
			accessUnit = *alternate
		}
		payloads := payloader.Payload(1200, accessUnit)
		for i, payload := range payloads {
			packet, _ := (&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: seq, Timestamp: timestamp, SSRC: 2650, Marker: i == len(payloads)-1}, Payload: payload}).Marshal()
			header := []byte{'$', 0, 0, 0}
			binary.BigEndian.PutUint16(header[2:], uint16(len(packet)))
			if _, err := conn.Write(append(header, packet...)); err != nil {
				return
			}
			seq++
		}
		timestamp += 3600
	}
}
