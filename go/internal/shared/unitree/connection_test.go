package unitree

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

const testDeviceKey = "0123456789abcdef0123456789abcdef"

func notificationFixture(t *testing.T, publicKey *rsa.PublicKey, version int, suffix string) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	data := "0123456789" + base64.StdEncoding.EncodeToString(der) + suffix
	if version == 2 || version == 3 {
		key := legacyGCMKey
		if version == 3 {
			key, _ = hex.DecodeString(testDeviceKey)
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			t.Fatal(err)
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			t.Fatal(err)
		}
		nonce := bytes.Repeat([]byte{42}, gcm.NonceSize())
		sealed := gcm.Seal(nil, nonce, []byte(data), nil)
		wire := append([]byte(nil), sealed[:len(sealed)-gcm.Overhead()]...)
		wire = append(wire, nonce...)
		wire = append(wire, sealed[len(sealed)-gcm.Overhead():]...)
		data = base64.StdEncoding.EncodeToString(wire)
	}
	payload, _ := json.Marshal(map[string]any{"data1": data, "data2": version})
	return []byte(base64.StdEncoding.EncodeToString(payload))
}

func TestDecodeNotifyEncryptionVersions(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{0, 1, 2, 3} {
		deviceKey := ""
		if version == 3 {
			deviceKey = testDeviceKey
		}
		pub, path, err := decodeNotify(notificationFixture(t, &key.PublicKey, version, "aAbBcCdDeJ"), deviceKey)
		if err != nil {
			t.Fatalf("version %d: %v", version, err)
		}
		if !pub.Equal(&key.PublicKey) || path != "01239" {
			t.Fatalf("version %d: wrong key or path %q", version, path)
		}
	}
	for _, tc := range []struct {
		name    string
		version int
		key     string
		suffix  string
	}{
		{"missing key", 3, "", "aAbBcCdDeJ"},
		{"wrong key", 3, "ffffffffffffffffffffffffffffffff", "aAbBcCdDeJ"},
		{"malformed key", 3, "not-a-key", "aAbBcCdDeJ"},
		{"unknown version", 4, testDeviceKey, "aAbBcCdDeJ"},
		{"invalid path", 1, "", "aAbBcCdDex"},
		{"keyed plaintext downgrade", 0, testDeviceKey, "aAbBcCdDeJ"},
		{"keyed public RSA downgrade", 1, testDeviceKey, "aAbBcCdDeJ"},
		{"keyed shared key downgrade", 2, testDeviceKey, "aAbBcCdDeJ"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := decodeNotify(notificationFixture(t, &key.PublicKey, tc.version, tc.suffix), tc.key)
			if err == nil {
				t.Fatal("accepted invalid connection notification")
			}
			if tc.key != "" && strings.Contains(err.Error(), tc.key) {
				t.Fatal("error disclosed encryption key")
			}
		})
	}
}

func TestDecodeNotifyBoundsRSAKeySize(t *testing.T) {
	for _, tc := range []struct {
		bits int
		ok   bool
	}{{1023, false}, {1024, true}, {2048, true}, {4096, true}, {4097, false}, {65536, false}} {
		t.Run(fmt.Sprint(tc.bits), func(t *testing.T) {
			// Encoding a public key is sufficient: never generate huge private keys.
			modulus := new(big.Int).Lsh(big.NewInt(1), uint(tc.bits-1))
			modulus.Add(modulus, big.NewInt(1))
			public := &rsa.PublicKey{N: modulus, E: 65537}
			_, _, err := decodeNotify(notificationFixture(t, public, 1, "aAbBcCdDeJ"), "")
			if (err == nil) != tc.ok {
				t.Fatalf("RSA size %d accepted=%v, want %v", tc.bits, err == nil, tc.ok)
			}
		})
	}
}

type signalingHTTPFunc func(*http.Request) (*http.Response, error)

func (f signalingHTTPFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestKeyedSignalingRefusesLegacyPortBeforeSendingCredentials(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: signalingHTTPFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("unexpected signaling request")
	})}
	var probes []string
	probe := func(_ context.Context, endpoint string) error {
		probes = append(probes, endpoint)
		return errors.New("modern port unavailable")
	}
	_, err := exchangeLocalSDPWithProbe(context.Background(), client, "192.0.2.1", testDeviceKey, []byte(`{"token":"secret"}`), probe)
	if err == nil || requests != 0 || len(probes) != 1 || probes[0] != "192.0.2.1:9991" {
		t.Fatalf("downgrade: err=%v, requests=%d, probes=%v", err, requests, probes)
	}
}

func TestKeyedSignalingRefusesNotificationDowngradeBeforeSendingOffer(t *testing.T) {
	public := &rsa.PublicKey{N: new(big.Int).Lsh(big.NewInt(1), 2047), E: 65537}
	for _, version := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.URL.Path != "/con_notify" {
					t.Error("credential-bearing offer reached an unauthenticated peer")
				}
				_, _ = w.Write(notificationFixture(t, public, version, "aAbBcCdDeJ"))
			}))
			defer server.Close()
			_, err := exchangeEncryptedSDP(context.Background(), server.Client(), server.URL, testDeviceKey, []byte(`{"token":"secret"}`))
			if err == nil || requests != 1 {
				t.Fatalf("downgrade: err=%v, requests=%d", err, requests)
			}
		})
	}
}

func TestEncryptedSignalingRoundTrip(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	offer := []byte(`{"id":"STA_localNetwork","type":"offer","sdp":"test offer","token":"secret-access-token"}`)
	answer := []byte(`{"type":"answer","sdp":"test answer"}`)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		switch r.URL.Path {
		case "/con_notify":
			_, _ = w.Write(notificationFixture(t, &privateKey.PublicKey, 3, "aAbBcCdDeJ"))
		case "/con_ing_01239":
			if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
				t.Errorf("content type = %q", got)
			}
			var body struct{ Data1, Data2 string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			wrappedKey, err := base64.StdEncoding.DecodeString(body.Data2)
			if err != nil {
				t.Error(err)
				return
			}
			sessionKey, err := rsa.DecryptPKCS1v15(rand.Reader, privateKey, wrappedKey)
			if err != nil {
				t.Error(err)
				return
			}
			if len(sessionKey) != 32 {
				t.Errorf("session key length = %d", len(sessionKey))
			}
			decoded, err := decryptECB(body.Data1, sessionKey)
			if err != nil || !bytes.Equal(decoded, offer) {
				t.Errorf("SDP offer mismatch: %v", err)
			}
			encrypted, err := encryptECB(answer, sessionKey)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = io.WriteString(w, encrypted)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	got, err := exchangeEncryptedSDP(context.Background(), server.Client(), server.URL, testDeviceKey, offer)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, answer) || requests != 2 {
		t.Fatalf("answer = %s, requests = %d", got, requests)
	}
}

func TestDecryptECBRejectsBadPadding(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	block, _ := aes.NewCipher(key)
	for _, plaintext := range [][]byte{
		bytes.Repeat([]byte{0}, 16),
		bytes.Repeat([]byte{17}, 16),
		append(bytes.Repeat([]byte{1}, 15), 2),
	} {
		encrypted := make([]byte, len(plaintext))
		block.Encrypt(encrypted, plaintext)
		if _, err := decryptECB(base64.StdEncoding.EncodeToString(encrypted), key); err == nil {
			t.Fatal("accepted invalid PKCS7 padding")
		}
	}
	for _, malformed := range []string{"", "!!!!", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := decryptECB(malformed, key); err == nil {
			t.Fatal("accepted invalid ciphertext")
		}
	}
}

func TestECBMatchesOpenSSL(t *testing.T) {
	// openssl enc -aes-256-ecb with this 32-byte ASCII key and PKCS7 padding.
	const ciphertext = "yUDXO5k5hmedMzDHBSRF4w=="
	key := []byte("0123456789abcdef0123456789abcdef")
	plaintext := []byte("Unitree SDP \u2603")
	got, err := encryptECB(plaintext, key)
	if err != nil || got != ciphertext {
		t.Fatalf("OpenSSL encryption vector mismatch: %q, %v", got, err)
	}
	decoded, err := decryptECB(ciphertext, key)
	if err != nil || !bytes.Equal(decoded, plaintext) {
		t.Fatalf("OpenSSL decryption vector mismatch: %q, %v", decoded, err)
	}
}

func TestConnectInputValidation(t *testing.T) {
	for _, address := range []string{"", "robot.local", "192.168.1.2:9991", "http://192.168.1.2", "0.0.0.0", "224.0.0.1", "::"} {
		if _, err := Connect(context.Background(), address, "", testDeviceKey); err == nil {
			t.Errorf("accepted address %q", address)
		}
	}
	if _, err := Connect(context.Background(), "127.0.0.1", "", "invalid-key"); err == nil || strings.Contains(err.Error(), "invalid-key") {
		t.Fatalf("key error = %v", err)
	}
}

func TestSignalingBoundsAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"http failure", http.StatusForbidden, "secret-token"},
		{"oversized", http.StatusOK, strings.Repeat("x", maxSignalingResponse+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			_, err := signalingPOST(context.Background(), server.Client(), server.URL, "", nil)
			if err == nil || strings.Contains(err.Error(), "secret-token") {
				t.Fatalf("error = %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := signalingPOST(ctx, http.DefaultClient, "http://127.0.0.1:1", "", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled request = %v", err)
	}
}

func TestConnectWebRTCValidationAndHeartbeat(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	robot, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer robot.Close()
	validated := make(chan struct{}, 1)
	heartbeat := make(chan struct{}, 1)
	robot.OnDataChannel(func(channel *webrtc.DataChannel) {
		if channel.Label() != "data" {
			t.Errorf("channel label = %s", channel.Label())
		}
		channel.OnOpen(func() {
			_ = channel.SendText(`{"type":"validation","data":"test-challenge"}`)
		})
		channel.OnMessage(func(message webrtc.DataChannelMessage) {
			var body struct {
				Type  string          `json:"type"`
				Topic string          `json:"topic"`
				Data  json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(message.Data, &body); err != nil {
				t.Error(err)
				return
			}
			switch body.Type {
			case "validation":
				var response string
				_ = json.Unmarshal(body.Data, &response)
				if response != "/wEEZblz/4G06Q2Xixam3A==" {
					t.Errorf("incorrect challenge response: %s", response)
					return
				}
				// Publish the verified challenge before the acknowledgement can
				// reach Connect on another goroutine. Publishing afterwards races
				// the caller despite a correctly validated data channel.
				validated <- struct{}{}
				if err := channel.SendText(`{"type":"validation","data":"Validation Ok."}`); err != nil {
					t.Error(err)
				}
			case "heartbeat":
				var data struct {
					TimeInNum int64  `json:"timeInNum"`
					TimeInStr string `json:"timeInStr"`
				}
				if json.Unmarshal(body.Data, &data) != nil || data.TimeInNum <= 0 || data.TimeInStr == "" || body.Topic != "" {
					t.Error("invalid heartbeat")
				}
				select {
				case heartbeat <- struct{}{}:
				default:
				}
			default:
				t.Errorf("unexpected message type %q; pairing must not move the robot", body.Type)
			}
		})
	})
	conn, err := connectWithSignaling(ctx, func(ctx context.Context, offer []byte) ([]byte, error) {
		return answerOffer(t, ctx, robot, offer)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if !conn.Connected() {
		t.Fatal("connection was not validated")
	}
	select {
	case <-validated:
	default:
		t.Fatal("Connect returned before validation")
	}
	select {
	case <-heartbeat:
	case <-ctx.Done():
		t.Fatal("no heartbeat received")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if conn.Connected() {
		t.Fatal("closed connection reports connected")
	}
	select {
	case <-conn.Done():
	default:
		t.Fatal("Close did not signal Done")
	}
}

func answerOffer(t *testing.T, ctx context.Context, robot *webrtc.PeerConnection, offer []byte) ([]byte, error) {
	t.Helper()
	var payload struct {
		ID    string `json:"id"`
		Type  string `json:"type"`
		SDP   string `json:"sdp"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(offer, &payload); err != nil {
		return nil, err
	}
	if payload.ID != "STA_localNetwork" || payload.Type != "offer" || payload.Token != "" {
		t.Error("incorrect local signaling payload")
	}
	if err := robot.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: payload.SDP}); err != nil {
		return nil, err
	}
	answer, err := robot.CreateAnswer(nil)
	if err != nil {
		return nil, err
	}
	gathered := webrtc.GatheringCompletePromise(robot)
	if err := robot.SetLocalDescription(answer); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-gathered:
	}
	return json.Marshal(robot.LocalDescription())
}

func TestConnectRejectsUnvalidatedChannel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	setupCtx, cancelSetup := context.WithCancel(ctx)
	defer cancelSetup()
	robot, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer robot.Close()
	opened := make(chan struct{}, 1)
	robot.OnDataChannel(func(channel *webrtc.DataChannel) {
		channel.OnOpen(func() {
			opened <- struct{}{}
			cancelSetup()
		})
	})
	conn, err := connectWithSignaling(setupCtx, func(ctx context.Context, offer []byte) ([]byte, error) {
		return answerOffer(t, ctx, robot, offer)
	})
	if conn != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("unvalidated connection = %v, %v", conn, err)
	}
	select {
	case <-opened:
	default:
		t.Fatal("fixture never opened its channel")
	}
}

func TestConnectRejectsBusyAndMalformedAnswers(t *testing.T) {
	for _, answer := range []string{`{"type":"answer","sdp":"reject"}`, `{"type":"offer","sdp":"test"}`, `{"type":"answer","sdp":""}`, `not json`} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		conn, err := connectWithSignaling(ctx, func(context.Context, []byte) ([]byte, error) { return []byte(answer), nil })
		cancel()
		if conn != nil || err == nil {
			t.Fatalf("answer %q accepted", answer)
		}
	}
}
