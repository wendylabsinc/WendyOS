package ipcam

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// probeTestCamera matches the fixed inputs the digest vector in
// TestDigestResponseVector was hand-derived from, so
// TestProbeCredentials_DigestAuthAccepted can assert the exact same hex
// string appears on the wire: proof that ProbeCredentials wires realm, nonce,
// method and uri into digestResponse the way the vector test proves
// digestResponse itself computes them.
func probeTestCamera() Camera {
	return Camera{MAC: "ec:71:db:2a:ae:7e", ID: 7, Address: "10.0.0.5"}
}

// withScriptedServer starts a TCP listener, accepts exactly one connection,
// and hands it to script on a goroutine. It points probeDialContext at the
// listener for the duration of the test (restored via t.Cleanup) so
// ProbeCredentials, which always dials Camera.Address:554, actually reaches
// this fake camera instead.
func withScriptedServer(t *testing.T, script func(t *testing.T, conn net.Conn)) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck

	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close() //nolint:errcheck
		script(t, conn)
	}()
	t.Cleanup(func() { <-accepted })

	original := probeDialContext
	probeDialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, ln.Addr().String())
	}
	t.Cleanup(func() { probeDialContext = original })
}

// readRequest reads one RTSP request's headers (request line + header lines,
// up to the blank line) off the server side of the fake connection and
// returns them joined by "\n" so a test can assert on their content, e.g. the
// Authorization header a digest resend must carry.
func readRequest(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	var lines []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("server: reading request: %v", err)
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			break
		}
		lines = append(lines, trimmed)
	}
	return strings.Join(lines, "\n")
}

// TestDigestResponseVector hand-derives one RFC 2617 digest chain outside
// this package's own code (via an independent Python md5 computation, not by
// calling digestResponse a second time) and checks digestResponse reproduces
// it, so a bug that broke the formula could not also fake the expected value.
//
// Inputs: username="admin", realm="IP Camera", password="hunter2",
// nonce="6629fae49393a05397450978507c4ef1", method="DESCRIBE",
// uri="rtsp://10.0.0.5:554/h264Preview_01_sub" — the same login, camera
// address and default sub-stream path TestProbeCredentials_DigestAuthAccepted
// uses, so both tests check the same computation from two directions.
//
// Derivation (python3 -c 'import hashlib; ...', reproducible by hand with any
// md5 tool):
//
//	HA1 input:      "admin:IP Camera:hunter2"
//	HA1 = md5(HA1 input) = e891128b69317a620410fe637e7ebf85
//
//	HA2 input:      "DESCRIBE:rtsp://10.0.0.5:554/h264Preview_01_sub"
//	HA2 = md5(HA2 input) = e36b666f9ea60f6e4563d1cd6312216b
//
//	response input: HA1 + ":" + nonce + ":" + HA2
//	              = "e891128b69317a620410fe637e7ebf85:6629fae49393a05397450978507c4ef1:e36b666f9ea60f6e4563d1cd6312216b"
//	response = md5(response input) = e5026857c5e65fe127d4876714c83a5b
func TestDigestResponseVector(t *testing.T) {
	const (
		username = "admin"
		realm    = "IP Camera"
		password = "hunter2"
		nonce    = "6629fae49393a05397450978507c4ef1"
		method   = "DESCRIBE"
		uri      = "rtsp://10.0.0.5:554/h264Preview_01_sub"
		want     = "e5026857c5e65fe127d4876714c83a5b"
	)
	if got := digestResponse(username, password, realm, nonce, method, uri); got != want {
		t.Fatalf("digestResponse = %q, want %q", got, want)
	}
}

// A camera that challenges with Digest must be answered with the RFC 2617
// response, on the same connection, and the second DESCRIBE must carry it —
// checked against the exact hex string TestDigestResponseVector hand-derived.
func TestProbeCredentials_DigestAuthAccepted(t *testing.T) {
	const wantResponse = "e5026857c5e65fe127d4876714c83a5b" // see TestDigestResponseVector

	var secondRequest string
	withScriptedServer(t, func(t *testing.T, conn net.Conn) {
		r := bufio.NewReader(conn)
		readRequest(t, r) // unauthenticated DESCRIBE

		// Header case and quoting deliberately vary from the second response
		// below, to exercise case-insensitive header parsing here too.
		_, err := conn.Write([]byte("RTSP/1.0 401 Unauthorized\r\n" +
			"CSeq: 1\r\n" +
			"www-authenticate: Digest realm=\"IP Camera\", nonce=\"6629fae49393a05397450978507c4ef1\"\r\n" +
			"\r\n"))
		if err != nil {
			t.Fatalf("server: writing 401: %v", err)
		}

		secondRequest = readRequest(t, r) // authenticated DESCRIBE

		_, err = conn.Write([]byte("RTSP/1.0 200 OK\r\n" +
			"CSeq: 2\r\n" +
			"Content-Type: application/sdp\r\n" +
			"Content-Length: 0\r\n" +
			"\r\n"))
		if err != nil {
			t.Fatalf("server: writing 200: %v", err)
		}
	})

	result, detail := ProbeCredentials(context.Background(), probeTestCamera(),
		Credential{Username: "admin", Password: "hunter2"})

	if result != ProbeOK {
		t.Fatalf("result = %v, want ProbeOK; detail=%q", result, detail)
	}
	if !strings.Contains(secondRequest, `response="`+wantResponse+`"`) {
		t.Fatalf("resend did not carry the RFC 2617 response: %q", secondRequest)
	}
	if !strings.Contains(secondRequest, `username="admin"`) || !strings.Contains(secondRequest, `realm="IP Camera"`) {
		t.Fatalf("resend missing digest fields: %q", secondRequest)
	}
	if strings.Contains(detail, "hunter2") {
		t.Fatalf("detail leaked the password: %q", detail)
	}
}

// Wrong credentials still get a properly-formed digest resend, but the camera
// keeps rejecting it: the outcome must be ProbeAuthFailed, not ProbeOK, and
// nothing about the failed password may reach the detail string. The
// challenge also carries a Basic offer ahead of the Digest one, as two
// separate WWW-Authenticate headers, to check that Digest is preferred
// regardless of header order.
func TestProbeCredentials_DigestAuthRejected(t *testing.T) {
	withScriptedServer(t, func(t *testing.T, conn net.Conn) {
		r := bufio.NewReader(conn)
		readRequest(t, r) // unauthenticated DESCRIBE

		_, err := conn.Write([]byte("RTSP/1.0 401 Unauthorized\r\n" +
			"CSeq: 1\r\n" +
			"WWW-Authenticate: Basic realm=\"IP Camera\"\r\n" +
			"WWW-Authenticate: Digest realm=\"IP Camera\", nonce=\"deadbeefnonce\"\r\n" +
			"\r\n"))
		if err != nil {
			t.Fatalf("server: writing 401: %v", err)
		}

		second := readRequest(t, r) // authenticated DESCRIBE, still wrong
		if !strings.HasPrefix(second, "DESCRIBE ") || !strings.Contains(second, "Digest") {
			t.Fatalf("server did not see a Digest resend: %q", second)
		}

		_, err = conn.Write([]byte("RTSP/1.0 401 Unauthorized\r\n" +
			"CSeq: 2\r\n" +
			"WWW-Authenticate: Digest realm=\"IP Camera\", nonce=\"deadbeefnonce\"\r\n" +
			"\r\n"))
		if err != nil {
			t.Fatalf("server: writing second 401: %v", err)
		}
	})

	result, detail := ProbeCredentials(context.Background(), probeTestCamera(),
		Credential{Username: "admin", Password: "wrong-password"})

	if result != ProbeAuthFailed {
		t.Fatalf("result = %v, want ProbeAuthFailed; detail=%q", result, detail)
	}
	if strings.Contains(detail, "wrong-password") {
		t.Fatalf("detail leaked the password: %q", detail)
	}
}

// A camera offering only Basic must get a base64 Authorization header, and
// accepting it must read as ProbeOK.
func TestProbeCredentials_BasicAuth(t *testing.T) {
	// base64("viewer:s3cret") — asserted against directly so the test does not
	// simply call the same encoder the implementation does.
	const wantAuth = "Basic dmlld2VyOnMzY3JldA=="

	var secondRequest string
	withScriptedServer(t, func(t *testing.T, conn net.Conn) {
		r := bufio.NewReader(conn)
		readRequest(t, r)

		_, err := conn.Write([]byte("RTSP/1.0 401 Unauthorized\r\n" +
			"CSeq: 1\r\n" +
			"WWW-Authenticate: Basic realm=\"IP Camera\"\r\n" +
			"\r\n"))
		if err != nil {
			t.Fatalf("server: writing 401: %v", err)
		}

		secondRequest = readRequest(t, r)

		_, err = conn.Write([]byte("RTSP/1.0 200 OK\r\nCSeq: 2\r\nContent-Length: 0\r\n\r\n"))
		if err != nil {
			t.Fatalf("server: writing 200: %v", err)
		}
	})

	result, detail := ProbeCredentials(context.Background(), probeTestCamera(),
		Credential{Username: "viewer", Password: "s3cret"})

	if result != ProbeOK {
		t.Fatalf("result = %v, want ProbeOK; detail=%q", result, detail)
	}
	if !strings.Contains(secondRequest, "Authorization: "+wantAuth) {
		t.Fatalf("resend did not carry the expected Basic header: %q", secondRequest)
	}
}

// A camera with no auth at all must be accepted on the first DESCRIBE, with
// no resend — and the LF-only line endings here (no \r) check that the
// response parser does not require CRLF.
func TestProbeCredentials_NoAuthRequired200(t *testing.T) {
	var requests int
	withScriptedServer(t, func(t *testing.T, conn net.Conn) {
		r := bufio.NewReader(conn)
		readRequest(t, r)
		requests++

		if _, err := conn.Write([]byte("RTSP/1.0 200 OK\nCSeq: 1\nContent-Length: 0\n\n")); err != nil {
			t.Fatalf("server: writing 200: %v", err)
		}
	})

	result, detail := ProbeCredentials(context.Background(), probeTestCamera(),
		Credential{Username: "admin", Password: "hunter2"})

	if result != ProbeOK {
		t.Fatalf("result = %v, want ProbeOK; detail=%q", result, detail)
	}
	if requests != 1 {
		t.Fatalf("server saw %d requests, want exactly 1 (no resend without a challenge)", requests)
	}
}

// A camera whose port never answers a connection attempt must report
// ProbeUnreachable with FormatUnreachable's message, not hang or panic.
func TestProbeCredentials_Unreachable(t *testing.T) {
	original := probeDialContext
	t.Cleanup(func() { probeDialContext = original })
	sentinel := errors.New("connection refused")
	probeDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return nil, sentinel
	}

	cam := probeTestCamera()
	result, detail := ProbeCredentials(context.Background(), cam, Credential{Username: "admin", Password: "hunter2"})

	if result != ProbeUnreachable {
		t.Fatalf("result = %v, want ProbeUnreachable; detail=%q", result, detail)
	}
	if !strings.Contains(detail, "10.0.0.5") {
		t.Fatalf("detail = %q, want it to name the camera FormatUnreachable would describe", detail)
	}
}

// A camera that accepts the login but has no content at the probed path
// (404) or no active session (454) is not a credential failure: the probe
// exists to test the login, and the login worked. The second response here
// also uses a non-standard reason phrase, checked defensively since RTSP
// implementations vary in wording.
func TestProbeCredentials_404AfterAuthIsOK(t *testing.T) {
	withScriptedServer(t, func(t *testing.T, conn net.Conn) {
		r := bufio.NewReader(conn)
		readRequest(t, r)

		_, err := conn.Write([]byte("RTSP/1.0 401 Unauthorized\r\n" +
			"CSeq: 1\r\n" +
			"WWW-Authenticate: Digest realm=\"IP Camera\", nonce=\"anoncevalue\"\r\n" +
			"\r\n"))
		if err != nil {
			t.Fatalf("server: writing 401: %v", err)
		}

		readRequest(t, r)

		_, err = conn.Write([]byte("RTSP/1.0 404 Stream Path Not Configured\r\nCSeq: 2\r\n\r\n"))
		if err != nil {
			t.Fatalf("server: writing 404: %v", err)
		}
	})

	result, detail := ProbeCredentials(context.Background(), probeTestCamera(),
		Credential{Username: "admin", Password: "hunter2"})

	if result != ProbeOK {
		t.Fatalf("result = %v, want ProbeOK (path issue, not a credential failure); detail=%q", result, detail)
	}
	if strings.Contains(detail, "hunter2") {
		t.Fatalf("detail leaked the password: %q", detail)
	}
}

// A blocked or dead-air TCP connection (accepted, then silent) must time out
// rather than hang the caller indefinitely. The fake server here must not
// read or close the connection while the probe is in flight: either would
// unblock the client early and defeat the point of the test, which is that
// probeIOTimeout — not the peer — is what ends the wait.
func TestProbeCredentials_UnreachableAfterConnectHangs(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- conn
	}()

	original := probeDialContext
	t.Cleanup(func() { probeDialContext = original })
	probeDialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, ln.Addr().String())
	}

	start := time.Now()
	result, _ := ProbeCredentials(context.Background(), probeTestCamera(),
		Credential{Username: "admin", Password: "hunter2"})
	elapsed := time.Since(start)

	// Only now, after the probe has given up, retire the server-side
	// connection so the accept goroutine does not outlive the test.
	select {
	case conn := <-accepted:
		conn.Close() //nolint:errcheck
	case <-time.After(time.Second):
		t.Fatal("server never accepted the connection")
	}

	if result != ProbeUnreachable {
		t.Fatalf("result = %v, want ProbeUnreachable", result)
	}
	if elapsed > probeIOTimeout+time.Second {
		t.Fatalf("ProbeCredentials took %v, want it bounded by probeIOTimeout (%v)", elapsed, probeIOTimeout)
	}
}

// parseAuthParams must handle both quoting styles a camera might send, and
// must not let a comma inside a quoted value (legal in a realm string) end
// the parameter early.
func TestParseAuthParams(t *testing.T) {
	cases := []struct {
		name      string
		challenge string
		want      map[string]string
	}{
		{
			name:      "quoted values, comma inside a quoted realm",
			challenge: `Digest realm="Login, Please", nonce="abc123"`,
			want:      map[string]string{"realm": "Login, Please", "nonce": "abc123"},
		},
		{
			name:      "quoted nonce containing a comma",
			challenge: `Digest realm="IP Camera", nonce="a,b,c-nonce"`,
			want:      map[string]string{"realm": "IP Camera", "nonce": "a,b,c-nonce"},
		},
		{
			name:      "unquoted values",
			challenge: `Digest realm=IPCam, nonce=abc123`,
			want:      map[string]string{"realm": "IPCam", "nonce": "abc123"},
		},
		{
			name:      "mixed quoted and unquoted",
			challenge: `Digest realm="IP Camera", nonce=abc123`,
			want:      map[string]string{"realm": "IP Camera", "nonce": "abc123"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAuthParams(tc.challenge)
			for key, want := range tc.want {
				if got[key] != want {
					t.Errorf("params[%q] = %q, want %q (all params: %v)", key, got[key], want, got)
				}
			}
		})
	}
}

// selectChallenge must recognize Digest and Basic regardless of case, and
// must prefer Digest over Basic no matter which order the headers arrived in.
func TestSelectChallenge(t *testing.T) {
	cases := []struct {
		name       string
		challenges []string
		wantScheme string
		wantOK     bool
	}{
		{"lowercase digest", []string{`digest realm="x", nonce="y"`}, "digest", true},
		{"mixed-case Digest", []string{`DiGeSt realm="x", nonce="y"`}, "digest", true},
		{"uppercase BASIC", []string{`BASIC realm="x"`}, "basic", true},
		{"lowercase basic only", []string{`basic realm="x"`}, "basic", true},
		{"basic then digest, digest still wins", []string{`Basic realm="x"`, `Digest realm="x", nonce="y"`}, "digest", true},
		{"digest then basic, digest still wins", []string{`Digest realm="x", nonce="y"`, `Basic realm="x"`}, "digest", true},
		{"unrecognized scheme only", []string{`NTLM realm="x"`}, "", false},
		{"empty list", nil, "", false},
		{"blank entries ignored", []string{"", "   ", `Basic realm="x"`}, "basic", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scheme, _, ok := selectChallenge(tc.challenges)
			if scheme != tc.wantScheme || ok != tc.wantOK {
				t.Fatalf("selectChallenge(%v) = (%q, %v), want (%q, %v)",
					tc.challenges, scheme, ok, tc.wantScheme, tc.wantOK)
			}
		})
	}
}

// A Digest challenge with no nonce cannot be answered — there is nothing to
// compute the response over — so this must resolve as ProbeAuthFailed rather
// than sending a malformed Authorization header or panicking.
func TestProbeCredentials_DigestChallengeMissingNonceIsAuthFailed(t *testing.T) {
	withScriptedServer(t, func(t *testing.T, conn net.Conn) {
		r := bufio.NewReader(conn)
		readRequest(t, r) // unauthenticated DESCRIBE

		// realm with no nonce: a Digest challenge this probe cannot answer.
		_, err := conn.Write([]byte("RTSP/1.0 401 Unauthorized\r\n" +
			"CSeq: 1\r\n" +
			"WWW-Authenticate: Digest realm=\"IP Camera\"\r\n" +
			"\r\n"))
		if err != nil {
			t.Fatalf("server: writing 401: %v", err)
		}
		// No second request: ProbeCredentials must give up rather than resend.
	})

	result, detail := ProbeCredentials(context.Background(), probeTestCamera(),
		Credential{Username: "admin", Password: "hunter2"})

	if result != ProbeAuthFailed {
		t.Fatalf("result = %v, want ProbeAuthFailed; detail=%q", result, detail)
	}
	if strings.Contains(detail, "hunter2") {
		t.Fatalf("detail leaked the password: %q", detail)
	}
}

// A camera that returns 404/454 on the very FIRST request — no 401 challenge
// at all — never evaluated the credentials, unlike the post-auth 404 case in
// TestProbeCredentials_404AfterAuthIsOK. The result is still ProbeOK (nothing
// rejected the login), but the detail must say the camera never asked for
// authentication rather than claiming it "accepted the credentials", which
// would overclaim what actually happened.
func TestProbeCredentials_404OnFirstRequestIsOKWithPathNote(t *testing.T) {
	var requests int
	withScriptedServer(t, func(t *testing.T, conn net.Conn) {
		r := bufio.NewReader(conn)
		readRequest(t, r)
		requests++
		if _, err := conn.Write([]byte("RTSP/1.0 404 Not Found\r\nCSeq: 1\r\n\r\n")); err != nil {
			t.Fatalf("server: writing 404: %v", err)
		}
	})

	result, detail := ProbeCredentials(context.Background(), probeTestCamera(),
		Credential{Username: "admin", Password: "hunter2"})

	if result != ProbeOK {
		t.Fatalf("result = %v, want ProbeOK (nothing rejected the login); detail=%q", result, detail)
	}
	if requests != 1 {
		t.Fatalf("server saw %d requests, want exactly 1 (a bare 404 is not a 401 challenge)", requests)
	}
	if !strings.Contains(detail, "did not request authentication") {
		t.Fatalf("detail = %q, want it to note the camera never challenged for credentials", detail)
	}
	if strings.Contains(detail, "accepted the credentials") {
		t.Fatalf("detail = %q, overclaims credentials were evaluated when no challenge occurred", detail)
	}
}

// A camera that enforces the Accept header, which RFC 2326 makes optional,
// answers a bare DESCRIBE with 551 Option not supported. Measured on an ONVIF
// IPC-XD400-N on 2026-09-13: without Accept the probe reported "unexpected RTSP
// status 551" and refused credentials that were fine; with
// "Accept: application/sdp" the same camera answered 200. The fake here does
// exactly what that camera did, on the first request and on the
// authenticated resend, so the header cannot be dropped from either.
func TestProbeCredentials_DescribeCarriesAcceptForStrictCameras(t *testing.T) {
	var seen []string
	withScriptedServer(t, func(t *testing.T, conn net.Conn) {
		r := bufio.NewReader(conn)
		for cseq := 1; cseq <= 2; cseq++ {
			req := readRequest(t, r)
			seen = append(seen, req)
			if !strings.Contains(strings.ToLower(req), "accept: application/sdp") {
				_, _ = conn.Write([]byte("RTSP/1.0 551 Option not supported\r\nCSeq: " +
					strconv.Itoa(cseq) + "\r\n\r\n"))
				return
			}
			if cseq == 1 {
				if _, err := conn.Write([]byte("RTSP/1.0 401 Unauthorized\r\nCSeq: 1\r\n" +
					"WWW-Authenticate: Basic realm=\"cam\"\r\n\r\n")); err != nil {
					t.Fatalf("server: writing 401: %v", err)
				}
				continue
			}
			if _, err := conn.Write([]byte("RTSP/1.0 200 OK\r\nCSeq: 2\r\nContent-Length: 0\r\n\r\n")); err != nil {
				t.Fatalf("server: writing 200: %v", err)
			}
		}
	})

	result, detail := ProbeCredentials(context.Background(), probeTestCamera(),
		Credential{Username: "admin", Password: "hunter2"})

	if result != ProbeOK {
		t.Fatalf("result = %v, want ProbeOK; detail=%q", result, detail)
	}
	if len(seen) != 2 {
		t.Fatalf("server saw %d requests, want 2 (first DESCRIBE, then the authenticated resend)", len(seen))
	}
	for i, req := range seen {
		if !strings.Contains(strings.ToLower(req), "accept: application/sdp") {
			t.Fatalf("request %d lacks Accept: application/sdp:\n%s", i+1, req)
		}
	}
}
