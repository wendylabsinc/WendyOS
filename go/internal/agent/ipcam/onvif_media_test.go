package ipcam

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeONVIF is an ONVIF camera shaped like the IPC-XD400-N measured on
// 2026-09-13: two profiles, main 2560x1440 and sub 640x360, whose RTSP paths
// are nothing like the Reolink defaults. requireAuth makes it behave like a
// camera that protects ONVIF: any call without a UsernameToken gets 401.
type fakeONVIF struct {
	srv         *httptest.Server
	requireAuth bool
	calls       atomic.Int32
	// mediaOnCapabilities reports the Media XAddr through GetCapabilities; when
	// false, GetCapabilities faults and the caller must fall back to the
	// conventional sibling path.
	mediaOnCapabilities bool
	// mediaHost is what the camera CLAIMS its Media service address is; a
	// wrong one must be corrected to the host we already reached.
	mediaHost string
}

func newFakeONVIF(t *testing.T) *fakeONVIF {
	t.Helper()
	f := &fakeONVIF{mediaOnCapabilities: true}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeONVIF) deviceAddr() string { return f.srv.URL + "/onvif/device_service" }

func (f *fakeONVIF) handle(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	body, _ := io.ReadAll(r.Body)
	req := string(body)
	if f.requireAuth && !strings.Contains(req, "UsernameToken") {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	env := func(inner string) {
		w.Header().Set("Content-Type", "application/soap+xml")
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://www.w3.org/2003/05/soap-envelope" xmlns:tt="http://www.onvif.org/ver10/schema" xmlns:trt="http://www.onvif.org/ver10/media/wsdl" xmlns:tds="http://www.onvif.org/ver10/device/wsdl">
<SOAP-ENV:Body>%s</SOAP-ENV:Body></SOAP-ENV:Envelope>`, inner)
	}
	switch {
	case strings.Contains(req, "GetCapabilities"):
		if !f.mediaOnCapabilities {
			w.WriteHeader(http.StatusInternalServerError)
			env(`<SOAP-ENV:Fault><SOAP-ENV:Code><SOAP-ENV:Value>SOAP-ENV:Receiver</SOAP-ENV:Value></SOAP-ENV:Code><SOAP-ENV:Reason><SOAP-ENV:Text>Action Not Implemented</SOAP-ENV:Text></SOAP-ENV:Reason></SOAP-ENV:Fault>`)
			return
		}
		host := f.mediaHost
		if host == "" {
			host = strings.TrimPrefix(f.srv.URL, "http://")
		}
		env(fmt.Sprintf(`<tds:GetCapabilitiesResponse><tds:Capabilities><tt:Media><tt:XAddr>http://%s/onvif/Media</tt:XAddr></tt:Media></tds:Capabilities></tds:GetCapabilitiesResponse>`, host))
	case strings.Contains(req, "GetProfiles"):
		if !strings.HasSuffix(r.URL.Path, "/onvif/Media") && !strings.HasSuffix(r.URL.Path, "/onvif/media_service") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		env(`<trt:GetProfilesResponse>
<trt:Profiles token="0001" fixed="true"><tt:Name>0001</tt:Name>
 <tt:VideoEncoderConfiguration token="0001"><tt:Name>0001</tt:Name><tt:Encoding>H264</tt:Encoding><tt:Resolution><tt:Width>2560</tt:Width><tt:Height>1440</tt:Height></tt:Resolution></tt:VideoEncoderConfiguration>
</trt:Profiles>
<trt:Profiles token="0002" fixed="true"><tt:Name>0002</tt:Name>
 <tt:VideoEncoderConfiguration token="0002"><tt:Name>0002</tt:Name><tt:Encoding>H264</tt:Encoding><tt:Resolution><tt:Width>640</tt:Width><tt:Height>360</tt:Height></tt:Resolution></tt:VideoEncoderConfiguration>
</trt:Profiles>
</trt:GetProfilesResponse>`)
	case strings.Contains(req, "GetStreamUri"):
		token := "0001"
		if strings.Contains(req, "<ProfileToken>0002</ProfileToken>") {
			token = "0002"
		}
		// The camera reports a host and port of its own choosing, and a
		// query string on the sub stream, both of which the resolver must
		// reduce to a path.
		uri := map[string]string{
			"0001": "rtsp://192.168.0.123:554/media/live/1/1",
			"0002": "rtsp://admin:secret@192.168.0.123:554/media/live/1/2?tcp=1",
		}[token]
		env(fmt.Sprintf(`<trt:GetStreamUriResponse><trt:MediaUri><tt:Uri>%s</tt:Uri><tt:InvalidAfterConnect>false</tt:InvalidAfterConnect><tt:InvalidAfterReboot>false</tt:InvalidAfterReboot><tt:Timeout>PT0S</tt:Timeout></trt:MediaUri></trt:GetStreamUriResponse>`, uri))
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func TestResolveStreamPaths_OpenCamera(t *testing.T) {
	f := newFakeONVIF(t)
	got, err := ResolveStreamPaths(context.Background(), Camera{MAC: "f4:00:00:06:aa:0b", ONVIFAddr: f.deviceAddr()}, Credential{})
	if err != nil {
		t.Fatalf("ResolveStreamPaths: %v", err)
	}
	if got.Main != "/media/live/1/1" {
		t.Fatalf("Main = %q, want the 2560x1440 profile's path", got.Main)
	}
	if got.Sub != "/media/live/1/2?tcp=1" {
		t.Fatalf("Sub = %q: the path must keep its query and drop the host and the embedded login", got.Sub)
	}
}

// A camera that protects ONVIF answers the first, unauthenticated call with
// 401. With a stored login the resolver retries with a UsernameToken; without
// one it reports that a login is needed rather than pretending the camera has
// no streams.
func TestResolveStreamPaths_AuthenticatedCamera(t *testing.T) {
	f := newFakeONVIF(t)
	f.requireAuth = true
	cam := Camera{MAC: "f4:00:00:06:aa:0b", ONVIFAddr: f.deviceAddr()}

	if _, err := ResolveStreamPaths(context.Background(), cam, Credential{}); err == nil {
		t.Fatal("expected an error without a login")
	}
	got, err := ResolveStreamPaths(context.Background(), cam, Credential{Username: "admin", Password: "hunter2"})
	if err != nil {
		t.Fatalf("with login: %v", err)
	}
	if got.Main != "/media/live/1/1" || got.Sub != "/media/live/1/2?tcp=1" {
		t.Fatalf("paths = %+v", got)
	}
}

// The Media XAddr a camera reports is trusted for its path and not for its
// host: a factory-default or NAT'd address must be replaced by the one that
// already answered.
func TestResolveStreamPaths_MediaHostIsRewrittenToTheReachedHost(t *testing.T) {
	f := newFakeONVIF(t)
	f.mediaHost = "192.168.1.64"
	got, err := ResolveStreamPaths(context.Background(), Camera{MAC: "m", ONVIFAddr: f.deviceAddr()}, Credential{})
	if err != nil {
		t.Fatalf("ResolveStreamPaths: %v", err)
	}
	if got.Main == "" {
		t.Fatal("no paths: the Media calls went to the camera's claimed host instead of the reached one")
	}
}

// A camera whose GetCapabilities faults still has a Media service at the
// conventional sibling path.
func TestResolveStreamPaths_FallsBackToSiblingMediaPath(t *testing.T) {
	f := newFakeONVIF(t)
	f.mediaOnCapabilities = false
	got, err := ResolveStreamPaths(context.Background(), Camera{MAC: "m", ONVIFAddr: f.deviceAddr()}, Credential{})
	if err != nil {
		t.Fatalf("ResolveStreamPaths: %v", err)
	}
	if got.Main != "/media/live/1/1" {
		t.Fatalf("Main = %q", got.Main)
	}
}

func TestRTSPPathOf(t *testing.T) {
	cases := map[string]string{
		"rtsp://192.168.0.123:554/media/live/1/1":                 "/media/live/1/1",
		"rtsp://u:p@10.0.0.5/h264Preview_01_sub":                  "/h264Preview_01_sub",
		"rtsp://10.0.0.5:554/cam/realmonitor?channel=1&subtype=0": "/cam/realmonitor?channel=1&subtype=0",
		"":            "",
		"rtsp://host": "",
	}
	for in, want := range cases {
		if got := rtspPathOf(in); got != want {
			t.Errorf("rtspPathOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUsernameTokenCarriesDigestNonceAndCreated(t *testing.T) {
	tok := usernameToken(Credential{Username: "admin", Password: "hunter2"}, testTime())
	for _, want := range []string{"<Username>admin</Username>", "PasswordDigest", "<Nonce", "<Created", "UsernameToken"} {
		if !strings.Contains(tok, want) {
			t.Errorf("token lacks %q:\n%s", want, tok)
		}
	}
	if strings.Contains(tok, "hunter2") {
		t.Fatal("the password must never travel in clear")
	}
}

func testTime() time.Time { return time.Date(2026, 9, 13, 17, 0, 0, 0, time.UTC) }
