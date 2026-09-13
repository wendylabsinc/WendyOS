package ipcam

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // WS-Security UsernameToken PasswordDigest mandates SHA-1; not a security boundary.
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// StreamPaths is what an ONVIF camera says its RTSP streams are called. Both
// are request paths (with any query string), never full URLs: the host, port
// and credentials are the registry's and the credential store's business, and
// a camera that reports its own address wrongly (a NAT, a factory default) must
// not be able to redirect a stream pull to a different host.
type StreamPaths struct {
	Sub  string
	Main string
}

// onvifHTTPTimeout bounds one SOAP round trip. A camera on its own cabled
// segment answers ONVIF in tens of milliseconds; three seconds is generous and
// keeps a discovery round from stalling on a camera whose ONVIF service is
// wedged while its WS-Discovery still answers.
const onvifHTTPTimeout = 3 * time.Second

// onvifHTTPClient is the seam tests replace to point ONVIF calls at an
// in-process server.
var onvifHTTPClient = &http.Client{Timeout: onvifHTTPTimeout}

// ErrNoStreamPaths is returned when a camera answered ONVIF but reported no
// profile with a usable RTSP URI.
var ErrNoStreamPaths = errors.New("camera reported no RTSP stream")

// ResolveStreamPaths asks a camera, over ONVIF, what its RTSP streams are
// called, so StreamURL can stop guessing.
//
// The registry has carried StreamSub and StreamMain since it was written and
// nothing ever filled them, so every camera that is not a Reolink got the
// Reolink default paths and answered 404. Measured on 2026-09-13 against an
// ONVIF "IPC-XD400-N": `camera test` reported "the stream path returned RTSP
// 404 and may be wrong" and `camera view` failed its pipeline, while the same
// camera answered GetProfiles and GetStreamUri without so much as a login:
//
//	profile 0001  2560x1440  rtsp://192.168.0.123:554/media/live/1/1
//	profile 0002   640x360   rtsp://192.168.0.123:554/media/live/1/2
//
// The sequence is GetCapabilities on the device service (for the Media
// service's address), GetProfiles, then GetStreamUri per profile. Each call is
// tried without credentials first, because plenty of cameras — this one
// included — serve ONVIF open; a 401 or a NotAuthorized fault is retried once
// with a WS-Security UsernameToken when a login is known. Main is the profile
// with the most pixels, Sub the one with the fewest; a camera with one
// profile gets the same path for both.
func ResolveStreamPaths(ctx context.Context, cam Camera, cred Credential) (StreamPaths, error) {
	if cam.ONVIFAddr == "" {
		return StreamPaths{}, errors.New("camera has no ONVIF address")
	}
	mediaURL, err := onvifMediaURL(ctx, cam.ONVIFAddr, cred)
	if err != nil {
		return StreamPaths{}, err
	}
	env, err := onvifCall(ctx, mediaURL, cred,
		`<GetProfiles xmlns="http://www.onvif.org/ver10/media/wsdl"/>`)
	if err != nil {
		return StreamPaths{}, fmt.Errorf("GetProfiles: %w", err)
	}
	if len(env.Body.Profiles) == 0 {
		return StreamPaths{}, ErrNoStreamPaths
	}

	type found struct {
		path   string
		pixels int
	}
	var streams []found
	for _, p := range env.Body.Profiles {
		if p.Token == "" {
			continue
		}
		uriEnv, err := onvifCall(ctx, mediaURL, cred, fmt.Sprintf(
			`<GetStreamUri xmlns="http://www.onvif.org/ver10/media/wsdl">`+
				`<StreamSetup><Stream xmlns="http://www.onvif.org/ver10/schema">RTP-Unicast</Stream>`+
				`<Transport xmlns="http://www.onvif.org/ver10/schema"><Protocol>RTSP</Protocol></Transport></StreamSetup>`+
				`<ProfileToken>%s</ProfileToken></GetStreamUri>`, xmlEscape(p.Token)))
		if err != nil {
			// One profile refusing is not the camera refusing; the others may
			// still name a stream.
			continue
		}
		path := rtspPathOf(uriEnv.Body.StreamURI)
		if path == "" {
			continue
		}
		streams = append(streams, found{path: path, pixels: p.Video.Width * p.Video.Height})
	}
	if len(streams) == 0 {
		return StreamPaths{}, ErrNoStreamPaths
	}
	sub, main := streams[0], streams[0]
	for _, s := range streams[1:] {
		if s.pixels > main.pixels {
			main = s
		}
		if s.pixels < sub.pixels {
			sub = s
		}
	}
	return StreamPaths{Sub: sub.path, Main: main.path}, nil
}

// onvifMediaURL finds the Media service behind a device service address:
// GetCapabilities names it, and when that fails the conventional sibling path
// is tried, which is what most firmware uses.
func onvifMediaURL(ctx context.Context, deviceAddr string, cred Credential) (string, error) {
	env, err := onvifCall(ctx, deviceAddr, cred,
		`<GetCapabilities xmlns="http://www.onvif.org/ver10/device/wsdl"><Category>Media</Category></GetCapabilities>`)
	if err == nil && env.Body.Capabilities != nil && env.Body.Capabilities.Media.XAddr != "" {
		return sameHostAs(deviceAddr, env.Body.Capabilities.Media.XAddr), nil
	}
	u, perr := url.Parse(deviceAddr)
	if perr != nil {
		if err != nil {
			return "", fmt.Errorf("GetCapabilities: %w", err)
		}
		return "", perr
	}
	u.Path = strings.Replace(u.Path, "device_service", "media_service", 1)
	if !strings.Contains(u.Path, "media_service") {
		u.Path = "/onvif/media_service"
	}
	return u.String(), nil
}

// sameHostAs returns xaddr rewritten to the host and port of deviceAddr. A
// camera that reports its Media XAddr on an address it does not actually have —
// a factory default, a NAT — would otherwise send every later call somewhere
// that never answers, and only the host we already reached is known to exist.
func sameHostAs(deviceAddr, xaddr string) string {
	d, err1 := url.Parse(deviceAddr)
	x, err2 := url.Parse(xaddr)
	if err1 != nil || err2 != nil || x.Host == "" {
		return xaddr
	}
	x.Scheme, x.Host = d.Scheme, d.Host
	return x.String()
}

// rtspPathOf reduces a stream URI to its request path plus query. Anything else
// in it (scheme, host, port, embedded credentials) is discarded on purpose:
// StreamURL supplies those from the registry and the credential store.
func rtspPathOf(uri string) string {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return ""
	}
	u, err := url.Parse(uri)
	if err != nil || u.Path == "" {
		return ""
	}
	path := u.Path
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return path
}

// onvifEnvelope is the subset of the SOAP replies this file reads. Field
// names match local element names only, so trt:/tt:/tds: prefixes and
// SOAP-ENV vs s envelopes all parse the same.
type onvifEnvelope struct {
	Body struct {
		Fault *struct {
			Code struct {
				Value   string `xml:"Value"`
				Subcode struct {
					Value string `xml:"Value"`
				} `xml:"Subcode"`
			} `xml:"Code"`
			Reason struct {
				Text string `xml:"Text"`
			} `xml:"Reason"`
		} `xml:"Fault"`
		Capabilities *struct {
			Media struct {
				XAddr string `xml:"XAddr"`
			} `xml:"Media"`
		} `xml:"GetCapabilitiesResponse>Capabilities"`
		Profiles []struct {
			Token string `xml:"token,attr"`
			Name  string `xml:"Name"`
			Video struct {
				Encoding string `xml:"Encoding"`
				Width    int    `xml:"Resolution>Width"`
				Height   int    `xml:"Resolution>Height"`
			} `xml:"VideoEncoderConfiguration"`
		} `xml:"GetProfilesResponse>Profiles"`
		StreamURI string `xml:"GetStreamUriResponse>MediaUri>Uri"`
	} `xml:"Body"`
}

// onvifAuthRequired reports whether a reply is the camera asking for a login:
// an HTTP 401, or a SOAP fault whose subcode or reason says so (ONVIF cameras
// answer an unauthenticated call with 400 + ter:NotAuthorized as often as with
// 401).
func onvifAuthRequired(status int, env *onvifEnvelope) bool {
	if status == http.StatusUnauthorized {
		return true
	}
	if env == nil || env.Body.Fault == nil {
		return false
	}
	text := strings.ToLower(env.Body.Fault.Code.Subcode.Value + " " + env.Body.Fault.Reason.Text)
	return strings.Contains(text, "notauthorized") || strings.Contains(text, "not authorized") ||
		strings.Contains(text, "unauthorized")
}

// onvifCall performs one SOAP request, retrying once with a UsernameToken when
// the camera asks for a login and one is known.
func onvifCall(ctx context.Context, endpoint string, cred Credential, body string) (*onvifEnvelope, error) {
	status, env, err := onvifPost(ctx, endpoint, "", body)
	if err != nil {
		return nil, err
	}
	if onvifAuthRequired(status, env) {
		if cred.Username == "" {
			return nil, fmt.Errorf("camera requires a login for ONVIF (HTTP %d)", status)
		}
		status, env, err = onvifPost(ctx, endpoint, usernameToken(cred, time.Now()), body)
		if err != nil {
			return nil, err
		}
		if onvifAuthRequired(status, env) {
			return nil, fmt.Errorf("camera rejected the ONVIF login (HTTP %d)", status)
		}
	}
	if env.Body.Fault != nil {
		return nil, fmt.Errorf("camera returned a SOAP fault: %s %s",
			strings.TrimSpace(env.Body.Fault.Code.Subcode.Value), strings.TrimSpace(env.Body.Fault.Reason.Text))
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("camera answered HTTP %d", status)
	}
	return env, nil
}

// maxONVIFReply caps a SOAP reply. GetProfiles from a camera with many profiles
// runs to tens of kilobytes; a megabyte is far past any real answer.
const maxONVIFReply = 1 << 20

func onvifPost(ctx context.Context, endpoint, header, body string) (int, *onvifEnvelope, error) {
	var b strings.Builder
	b.WriteString(`<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">`)
	if header != "" {
		b.WriteString(`<s:Header>` + header + `</s:Header>`)
	}
	b.WriteString(`<s:Body>` + body + `</s:Body></s:Envelope>`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBufferString(b.String()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/soap+xml; charset=utf-8")
	resp, err := onvifHTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxONVIFReply))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	env := &onvifEnvelope{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := xml.Unmarshal(raw, env); err != nil {
			// A 401 often comes with an HTML body or nothing at all; the status
			// is still the answer.
			if resp.StatusCode == http.StatusUnauthorized {
				return resp.StatusCode, env, nil
			}
			return resp.StatusCode, nil, fmt.Errorf("camera answered HTTP %d with a reply that is not SOAP", resp.StatusCode)
		}
	}
	return resp.StatusCode, env, nil
}

// usernameToken builds a WS-Security UsernameToken header with a
// PasswordDigest, the login every ONVIF camera accepts:
//
//	digest = Base64( SHA1( nonce + created + password ) )
//
// where nonce is the raw bytes and created is the UTC timestamp, both of which
// also travel in the header.
func usernameToken(cred Credential, now time.Time) string {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		// crypto/rand failing is a broken host; a fixed nonce still yields a
		// valid, if replayable, token, and the call is over a private cable.
		copy(nonce, []byte("wendy-agent-nonce"))
	}
	created := now.UTC().Format("2006-01-02T15:04:05.000Z")
	h := sha1.New() //nolint:gosec // mandated by the UsernameToken profile
	h.Write(nonce)
	h.Write([]byte(created))
	h.Write([]byte(cred.Password))
	digest := base64.StdEncoding.EncodeToString(h.Sum(nil))
	return `<Security s:mustUnderstand="1" xmlns="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd">` +
		`<UsernameToken><Username>` + xmlEscape(cred.Username) + `</Username>` +
		`<Password Type="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest">` + digest + `</Password>` +
		`<Nonce EncodingType="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-soap-message-security-1.0#Base64Binary">` +
		base64.StdEncoding.EncodeToString(nonce) + `</Nonce>` +
		`<Created xmlns="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd">` + created + `</Created>` +
		`</UsernameToken></Security>`
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
