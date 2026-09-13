package ipcam

import (
	"bufio"
	"context"
	"crypto/md5" //nolint:gosec // RFC 2617 digest auth mandates MD5; this is not a security boundary.
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ProbeResult classifies the outcome of ProbeCredentials.
type ProbeResult int

const (
	// ProbeOK means the camera accepted the DESCRIBE, with or without
	// credentials, or rejected only the stream path (404/454) rather than the
	// login. Either way the credentials themselves are not the problem.
	ProbeOK ProbeResult = iota
	// ProbeAuthFailed means the camera rejected the credentials: a 401/403
	// survived an authenticated retry, or the challenge itself could not be
	// answered (an auth scheme this probe does not implement).
	ProbeAuthFailed
	// ProbeUnreachable means the camera's RTSP port never produced a usable
	// answer: the dial failed, or the connection stopped responding partway
	// through the exchange.
	ProbeUnreachable
)

// probeDialContext is the seam ProbeCredentials dials through. Tests replace
// it to point at an in-process net.Listener instead of a real camera.
var probeDialContext = (&net.Dialer{Timeout: 3 * time.Second}).DialContext

// probeIOTimeout bounds the whole DESCRIBE/challenge/DESCRIBE exchange, not
// just the dial. A camera that completes the TCP handshake but never answers
// — a half-open firewall, a wedged RTSP stack — must not hang `wendy device
// camera test` forever; three seconds is ample for two small round trips to a
// LAN device.
const probeIOTimeout = 3 * time.Second

// ProbeCredentials dials the camera's RTSP port and performs a DESCRIBE with
// the given login, the same request a stream pull opens with. It is plain TCP
// plus crypto/md5 digest auth per RFC 2617 — no GStreamer, no kernel module —
// so `camera test` works on every WendyOS build and works remotely over the
// tunnel, unlike a pipeline probe that needs the local video stack.
//
// The DESCRIBE targets the camera's normal stream path (via streamPath, the
// same helper StreamURL uses) purely to exercise the same request a real
// pull would send; the path itself is not what is under test. That is why a
// 404/454 after the camera has accepted (or waived) authentication still
// counts as ProbeOK: it means the login worked and the path is wrong, which
// is a different problem than the one this probe answers.
func ProbeCredentials(ctx context.Context, cam Camera, cred Credential) (ProbeResult, string) {
	if cam.Address == "" {
		return ProbeUnreachable, RedactText(FormatUnreachable(cam), cred.Username, cred.Password)
	}

	dialAddr := net.JoinHostPort(cam.Address, strconv.Itoa(RTSPPort))
	conn, err := probeDialContext(ctx, "tcp", dialAddr)
	if err != nil {
		return ProbeUnreachable, RedactText(FormatUnreachable(cam), cred.Username, cred.Password)
	}
	defer conn.Close() //nolint:errcheck

	deadline := time.Now().Add(probeIOTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)

	// The URI names the address as configured, not the (possibly seam-redirected)
	// socket dialed: it is what a real client sends and what the digest response
	// is computed over, and it must match the URL a stream pull would build.
	uri := (&url.URL{
		Scheme: "rtsp",
		Host:   net.JoinHostPort(cam.Address, strconv.Itoa(RTSPPort)),
		Path:   streamPath(cam, StreamAuto),
	}).String()

	r := bufio.NewReader(conn)

	if err := writeDescribeRequest(conn, uri, 1, ""); err != nil {
		return ProbeUnreachable, RedactText(FormatUnreachable(cam), cred.Username, cred.Password)
	}
	resp, err := readRTSPResponse(r)
	if err != nil {
		return ProbeUnreachable, RedactText(FormatUnreachable(cam), cred.Username, cred.Password)
	}

	// challenged records whether the response being classified followed an
	// authenticated retry, or is the camera's first answer. A 404/454 means
	// something different in each case: after a challenge it means the login
	// was evaluated and accepted; on the first response it means the camera
	// never evaluated the login at all, which classifyProbeResponse's detail
	// string has to say honestly rather than imply the credentials were
	// checked.
	challenged := false
	if resp.status == 401 {
		authorization, ok := buildAuthorization(resp.headers["www-authenticate"], cred, uri)
		if !ok {
			return ProbeAuthFailed, RedactText(fmt.Sprintf(
				"camera %d at %s challenged with an authentication scheme this probe cannot answer",
				cam.ID, cam.Address), cred.Username, cred.Password)
		}
		if err := writeDescribeRequest(conn, uri, 2, authorization); err != nil {
			return ProbeUnreachable, RedactText(FormatUnreachable(cam), cred.Username, cred.Password)
		}
		resp, err = readRTSPResponse(r)
		if err != nil {
			return ProbeUnreachable, RedactText(FormatUnreachable(cam), cred.Username, cred.Password)
		}
		challenged = true
	}

	return classifyProbeResponse(resp, cam, cred, challenged)
}

// classifyProbeResponse turns the final RTSP status of the exchange into a
// ProbeResult. It only ever sees the last response: either the first one, if
// it was not a 401, or the one that followed an authenticated retry —
// challenged says which, since a 404/454 means something different in each
// case.
func classifyProbeResponse(resp rtspResponse, cam Camera, cred Credential, challenged bool) (ProbeResult, string) {
	switch {
	case resp.status >= 200 && resp.status < 300:
		return ProbeOK, RedactText(fmt.Sprintf(
			"camera %d at %s accepted the credentials", cam.ID, cam.Address), cred.Username, cred.Password)
	case resp.status == 401 || resp.status == 403:
		return ProbeAuthFailed, RedactText(fmt.Sprintf(
			"camera %d at %s rejected the credentials (RTSP %d)", cam.ID, cam.Address, resp.status),
			cred.Username, cred.Password)
	case resp.status == 404 || resp.status == 454:
		if challenged {
			// The login was not what stopped this request: the camera evaluated
			// and accepted it, then objected to the path instead. Credentials are
			// the only thing this probe promises to answer for.
			return ProbeOK, RedactText(fmt.Sprintf(
				"camera %d at %s accepted the credentials; the stream path returned RTSP %d, which is a separate issue",
				cam.ID, cam.Address, resp.status), cred.Username, cred.Password)
		}
		// No challenge means the camera never evaluated the credentials at all —
		// saying it "accepted" them here would overclaim. Still ProbeOK, because
		// nothing rejected the login; the path is what needs a look.
		return ProbeOK, RedactText(fmt.Sprintf(
			"camera %d at %s did not request authentication; the stream path returned RTSP %d and may be wrong",
			cam.ID, cam.Address, resp.status), cred.Username, cred.Password)
	default:
		// Neither an accept nor a recognized auth rejection: treated as
		// AuthFailed because ProbeOK would overclaim, but the message says only
		// what happened rather than asserting the credentials were the problem.
		return ProbeAuthFailed, RedactText(fmt.Sprintf(
			"camera %d at %s returned unexpected RTSP status %d to DESCRIBE, which this probe cannot read as a credential verdict",
			cam.ID, cam.Address, resp.status), cred.Username, cred.Password)
	}
}

// writeDescribeRequest sends a DESCRIBE request. CSeq is required by RTSP
// (RFC 2326 §12.17) on every request; authorization, when non-empty, is sent
// as the Authorization header value verbatim.
//
// Accept is sent on every DESCRIBE, although RFC 2326 §12.1 makes it optional
// with application/sdp as the implied default. Some cameras enforce it: an
// ONVIF "IPC-XD400-N" (firmware V1.0.4.10) answers a DESCRIBE with no Accept
// header with "RTSP/1.0 551 Option not supported" and the same request with
// "Accept: application/sdp" with 200 and the SDP. Measured on the entrance
// pilot Jetson on 2026-09-13; the 551 reached the operator as "returned
// unexpected RTSP status 551 to DESCRIBE, which this probe cannot read as a
// credential verdict", and `camera test` refused a camera whose login was
// fine. Every RTSP client that streams (GStreamer's rtspsrc, ffmpeg, VLC)
// sends this header, which is why the same camera streams and does not probe.
func writeDescribeRequest(w io.Writer, uri string, cseq int, authorization string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "DESCRIBE %s RTSP/1.0\r\n", uri)
	fmt.Fprintf(&b, "CSeq: %d\r\n", cseq)
	b.WriteString("Accept: application/sdp\r\n")
	if authorization != "" {
		fmt.Fprintf(&b, "Authorization: %s\r\n", authorization)
	}
	b.WriteString("\r\n")
	_, err := w.Write([]byte(b.String()))
	return err
}

// rtspResponse is the subset of an RTSP response this probe needs: the status
// code and the headers, keyed lower-case with every value kept (a camera may
// repeat WWW-Authenticate once per scheme it offers).
type rtspResponse struct {
	status  int
	headers map[string][]string
}

// readRTSPResponse parses one RTSP response from r. It tolerates the variance
// a real camera exhibits: CRLF or bare LF line endings, and a response line
// with no reason phrase — only the numeric status in the second
// whitespace-separated field is required.
func readRTSPResponse(r *bufio.Reader) (rtspResponse, error) {
	statusLine, err := r.ReadString('\n')
	if err != nil {
		return rtspResponse{}, fmt.Errorf("reading RTSP status line: %w", err)
	}
	fields := strings.Fields(statusLine)
	if len(fields) < 2 {
		return rtspResponse{}, fmt.Errorf("malformed RTSP status line: %q", strings.TrimSpace(statusLine))
	}
	status, err := strconv.Atoi(fields[1])
	if err != nil {
		return rtspResponse{}, fmt.Errorf("malformed RTSP status code: %q", fields[1])
	}

	headers := make(map[string][]string)
	contentLength := 0
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return rtspResponse{}, fmt.Errorf("reading RTSP headers: %w", err)
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			break
		}
		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue // not a well-formed header line; skip rather than abort the probe
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		headers[key] = append(headers[key], value)
		if key == "content-length" {
			if n, err := strconv.Atoi(value); err == nil {
				contentLength = n
			}
		}
	}

	// A DESCRIBE reply can carry an SDP body (Content-Length > 0). This probe
	// never reads it, but it must still be drained: a resend on the same
	// connection would otherwise read stale body bytes as the next status line.
	if contentLength > 0 {
		if _, err := r.Discard(contentLength); err != nil {
			return rtspResponse{}, fmt.Errorf("reading RTSP body: %w", err)
		}
	}

	return rtspResponse{status: status, headers: headers}, nil
}

// buildAuthorization turns a set of WWW-Authenticate challenge values into an
// Authorization header value for uri, preferring Digest over Basic when a
// camera offers both — Digest never puts the password on the wire. It
// reports ok=false when none of the offered schemes can be answered (no
// scheme recognized, or a Digest challenge missing the nonce it needs).
func buildAuthorization(challenges []string, cred Credential, uri string) (string, bool) {
	scheme, challenge, ok := selectChallenge(challenges)
	if !ok {
		return "", false
	}
	switch scheme {
	case "digest":
		params := parseAuthParams(challenge)
		nonce := params["nonce"]
		if nonce == "" {
			return "", false
		}
		realm := params["realm"]
		response := digestResponse(cred.Username, cred.Password, realm, nonce, "DESCRIBE", uri)
		return fmt.Sprintf(
			`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s"`,
			cred.Username, realm, nonce, uri, response), true
	case "basic":
		token := base64.StdEncoding.EncodeToString([]byte(cred.Username + ":" + cred.Password))
		return "Basic " + token, true
	default:
		return "", false
	}
}

// selectChallenge picks the challenge to answer from every WWW-Authenticate
// value a response carried, favoring Digest whenever it appears among them,
// regardless of header order.
func selectChallenge(challenges []string) (scheme, raw string, ok bool) {
	var basic string
	for _, c := range challenges {
		c = strings.TrimSpace(c)
		switch {
		case c == "":
			continue
		case strings.HasPrefix(strings.ToLower(c), "digest"):
			return "digest", c, true
		case strings.HasPrefix(strings.ToLower(c), "basic"):
			basic = c
		}
	}
	if basic != "" {
		return "basic", basic, true
	}
	return "", "", false
}

// parseAuthParams parses the comma-separated key=value parameters that
// follow a challenge's scheme token, accepting both quoted ("realm=\"x\"")
// and unquoted (realm=x) values. Splitting respects quotes, so a comma inside
// a quoted value (legal in a realm string) does not end the parameter early.
func parseAuthParams(challenge string) map[string]string {
	params := make(map[string]string)
	_, rest, ok := strings.Cut(challenge, " ")
	if !ok {
		return params
	}
	for _, part := range splitAuthParams(rest) {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"`)
		params[key] = value
	}
	return params
}

// splitAuthParams splits a challenge's parameter list on commas, treating
// text inside double quotes as opaque so a quoted value containing a comma is
// not mistaken for a parameter boundary.
func splitAuthParams(s string) []string {
	var parts []string
	var cur strings.Builder
	inQuotes := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuotes = !inQuotes
			cur.WriteRune(r)
		case r == ',' && !inQuotes:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	parts = append(parts, cur.String())
	return parts
}

// digestResponse computes the RFC 2617 request-digest for a request with no
// qop (the classic, pre-RFC 2617-qop form every RTSP camera this probe
// targets still accepts):
//
//	response = MD5( MD5(username:realm:password) ":" nonce ":" MD5(method:uri) )
func digestResponse(username, password, realm, nonce, method, uri string) string {
	ha1 := md5Hex(username + ":" + realm + ":" + password)
	ha2 := md5Hex(method + ":" + uri)
	return md5Hex(ha1 + ":" + nonce + ":" + ha2)
}

// md5Hex returns the lower-case hex encoding of s's MD5 sum, the form RFC
// 2617 digest values take on the wire.
func md5Hex(s string) string {
	sum := md5.Sum([]byte(s)) //nolint:gosec // RFC 2617 digest auth mandates MD5.
	return hex.EncodeToString(sum[:])
}
