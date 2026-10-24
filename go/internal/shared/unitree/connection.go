package unitree

// The LAN wire format follows the Unitree mobile app protocol documented by
// https://github.com/legion1581/unitree_webrtc_connect/tree/e0abc5780761539eff89a382a79319e5cf6ad1f4
// in unitree_auth.py, encryption.py and msgs/validation.py.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5" // Unitree's validation protocol requires MD5.
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

const (
	maxSignalingResponse = 1 << 20
	// Bound unauthenticated RSA work before wrapping a credential-bearing offer.
	maxSignalingRSAKeyBits = 4096
)

var legacyGCMKey = []byte{232, 86, 130, 189, 22, 84, 155, 0, 142, 4, 166, 104, 43, 179, 235, 227}

// Connection is a protocol-validated Go2 WebRTC connection. Movement is sent
// only through explicitly invoked, bounded Move/Stop requests. The public
// validation challenge is not a cryptographic proof of robot identity; modern
// signaling is authenticated separately with the account's per-device key.
type Connection struct {
	peer        *webrtc.PeerConnection
	channel     *webrtc.DataChannel
	connected   atomic.Bool
	validated   atomic.Bool
	done        chan struct{}
	closeOnce   sync.Once
	closeErr    error
	motionMu    sync.Mutex
	responseMu  sync.Mutex
	pending     map[uint64]pendingSportRequest
	requestID   atomic.Uint64
	videoSSRC   atomic.Uint32
	subMu       sync.Mutex
	subscribers map[*subscriber]struct{}
	topicRefs   map[string]int
	lidarRefs   int
	videoRefs   int
}

// Connected reports whether the validated WebRTC peer is currently connected.
func (c *Connection) Connected() bool {
	if c == nil {
		return false
	}
	select {
	case <-c.done:
		return false
	default:
		return c.connected.Load()
	}
}

// Done closes when the connection ends or Close is called.
func (c *Connection) Done() <-chan struct{} { return c.done }

// Close releases the robot's WebRTC client slot and stops heartbeats.
func (c *Connection) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		c.connected.Store(false)
		close(c.done)
		c.closeErr = c.peer.Close()
	})
	return c.closeErr
}

// Connect establishes and validates a Go2 connection at a numeric IP address.
// The token argument is retained for caller compatibility but never sent to
// the LAN peer. Unitree's LocalSTA protocol accepts an empty token; account
// login is needed to fetch the modern firmware's encryptionKey, not to expose
// an account-wide bearer credential to a robot. The context bounds setup, not
// the returned connection's lifetime. The owner must call Close when it ends.
func Connect(ctx context.Context, address, _ string, encryptionKey string) (*Connection, error) {
	ip, err := netip.ParseAddr(strings.TrimSpace(address))
	if err != nil || ip.IsUnspecified() || ip.IsMulticast() {
		return nil, errors.New("Unitree address must be a unicast IP address")
	}
	if encryptionKey != "" {
		if _, err := parseDeviceKey(encryptionKey); err != nil {
			return nil, err
		}
	}
	setupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	transport := &http.Transport{
		// LAN credentials must never be forwarded to an environment HTTP proxy.
		DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("Unitree signaling redirects are not allowed")
		},
	}
	return connectWithSignaling(setupCtx, func(ctx context.Context, offer []byte) ([]byte, error) {
		return exchangeLocalSDP(ctx, client, ip.String(), encryptionKey, offer)
	})
}

func connectWithSignaling(ctx context.Context, exchange func(context.Context, []byte) ([]byte, error)) (*Connection, error) {
	peer, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, fmt.Errorf("create Unitree WebRTC peer: %w", err)
	}
	return connectWithPeer(ctx, peer, exchange)
}

func connectWithPeer(ctx context.Context, peer *webrtc.PeerConnection, exchange func(context.Context, []byte) ([]byte, error)) (*Connection, error) {
	c := &Connection{peer: peer, done: make(chan struct{})}
	success := false
	defer func() {
		if !success {
			_ = c.Close()
		}
	}()
	result := make(chan error, 1)
	var resultOnce sync.Once
	finish := func(err error) { resultOnce.Do(func() { result <- err }) }
	peer.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		c.connected.Store(state == webrtc.PeerConnectionStateConnected && c.validated.Load())
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			finish(errors.New("Unitree WebRTC connection closed before validation"))
			go func() { _ = c.Close() }()
		}
	})
	channel, err := peer.CreateDataChannel("data", nil)
	if err != nil {
		return nil, fmt.Errorf("create Unitree data channel: %w", err)
	}
	c.channel = channel
	// Same media sections as the Unitree app: the robot sends camera video
	// (and microphone audio) only after an explicit "vid"/"aud" on request.
	for _, kind := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeAudio, webrtc.RTPCodecTypeVideo} {
		if _, err := peer.AddTransceiverFromKind(kind, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			return nil, fmt.Errorf("add Unitree media transceiver: %w", err)
		}
	}
	peer.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) { go c.onTrack(track) })
	channel.OnClose(func() {
		finish(errors.New("Unitree data channel closed before validation"))
		go func() { _ = c.Close() }()
	})
	var challenge string // Pion serializes message callbacks for this channel.
	channel.OnMessage(func(message webrtc.DataChannelMessage) {
		if !message.IsString {
			if c.validated.Load() && len(message.Data) <= maxBinaryMessage {
				if topic, ok := binaryTopic(message.Data); ok {
					c.dispatchTopic(topic, message.Data, true)
				}
			}
			return
		}
		if len(message.Data) > maxSignalingResponse {
			return
		}
		var msg struct {
			Type  string          `json:"type"`
			Topic string          `json:"topic"`
			Data  json.RawMessage `json:"data"`
			Info  string          `json:"info"`
		}
		if json.Unmarshal(message.Data, &msg) != nil {
			return
		}
		if msg.Type == "res" {
			c.motionResponse(message.Data)
			return
		}
		if msg.Type == "msg" {
			if c.validated.Load() && ValidTopic(msg.Topic) {
				c.dispatchTopic(msg.Topic, message.Data, false)
			}
			return
		}
		switch msg.Type {
		case "validation":
			var value string
			if json.Unmarshal(msg.Data, &value) != nil || value == "" {
				finish(errors.New("Unitree returned an invalid validation challenge"))
				return
			}
			if value == "Validation Ok." {
				if challenge == "" {
					finish(errors.New("Unitree accepted validation without a challenge"))
					return
				}
				c.validated.Store(true)
				c.connected.Store(peer.ConnectionState() == webrtc.PeerConnectionStateConnected)
				finish(nil)
				return
			}
			challenge = value
		case "err":
			if msg.Info != "Validation Needed." || challenge == "" {
				return
			}
		default:
			return
		}
		if err := c.send("validation", validationResponse(challenge)); err != nil {
			finish(fmt.Errorf("send Unitree validation: %w", err))
		}
	})
	offer, err := peer.CreateOffer(nil)
	if err != nil {
		return nil, fmt.Errorf("create Unitree offer: %w", err)
	}
	gathered := webrtc.GatheringCompletePromise(peer)
	if err := peer.SetLocalDescription(offer); err != nil {
		return nil, fmt.Errorf("set Unitree offer: %w", err)
	}
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("gather Unitree connection candidates: %w", ctx.Err())
	case <-gathered:
	}
	payload, err := json.Marshal(struct {
		ID    string `json:"id"`
		SDP   string `json:"sdp"`
		Type  string `json:"type"`
		Token string `json:"token"`
	}{"STA_localNetwork", peer.LocalDescription().SDP, "offer", ""})
	if err != nil {
		return nil, err
	}
	answerJSON, err := exchange(ctx, payload)
	if err != nil {
		return nil, err
	}
	var answer struct {
		SDP  string `json:"sdp"`
		Type string `json:"type"`
	}
	if json.Unmarshal(answerJSON, &answer) != nil || answer.SDP == "" {
		return nil, errors.New("Unitree returned an invalid SDP answer")
	}
	if answer.SDP == "reject" {
		return nil, errors.New("Unitree is busy; arrange an explicit handoff with the current robot operator before retrying")
	}
	if answer.Type != "answer" {
		return nil, errors.New("Unitree returned an unexpected SDP message type")
	}
	if err := peer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		// Peer-supplied SDP may contain sensitive or untrusted text.
		return nil, errors.New("Unitree returned an unsupported SDP answer")
	}
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("validate Unitree connection: %w", ctx.Err())
	case err := <-result:
		if err != nil {
			return nil, err
		}
	}
	if !c.Connected() {
		return nil, errors.New("Unitree disconnected during validation")
	}
	success = true
	go c.heartbeat()
	return c, nil
}

func validationResponse(challenge string) string {
	digest := md5.Sum([]byte("UnitreeGo2_" + challenge))
	return base64.StdEncoding.EncodeToString(digest[:])
}

func (c *Connection) send(kind string, data any) error {
	message, err := json.Marshal(struct {
		Type  string `json:"type"`
		Topic string `json:"topic"`
		Data  any    `json:"data"`
	}{kind, "", data})
	if err != nil {
		return err
	}
	return c.channel.SendText(string(message))
}

func (c *Connection) heartbeat() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case now := <-ticker.C:
			if err := c.send("heartbeat", map[string]any{"timeInStr": now.Format("2006-01-02 15:04:05"), "timeInNum": now.Unix()}); err != nil {
				_ = c.Close()
				return
			}
		}
	}
}

func exchangeLocalSDP(ctx context.Context, client *http.Client, address, key string, offer []byte) ([]byte, error) {
	return exchangeLocalSDPWithProbe(ctx, client, address, key, offer, func(ctx context.Context, endpoint string) error {
		conn, err := (&net.Dialer{Timeout: 1500 * time.Millisecond}).DialContext(ctx, "tcp", endpoint)
		if err != nil {
			return err
		}
		return conn.Close()
	})
}

func exchangeLocalSDPWithProbe(ctx context.Context, client *http.Client, address, key string, offer []byte, probe func(context.Context, string) error) ([]byte, error) {
	for _, port := range []string{"9991", "8081"} {
		// The existence of a legacy listener does not authorize a downgrade.
		if port == "8081" && key != "" {
			return nil, errors.New("keyed Unitree robots require authenticated encrypted signaling; legacy fallback is refused")
		}
		if err := probe(ctx, net.JoinHostPort(address, port)); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		base := (&url.URL{Scheme: "http", Host: net.JoinHostPort(address, port)}).String()
		if port == "8081" {
			return signalingPOST(ctx, client, base+"/offer", "application/json", offer)
		}
		// A modern authentication failure must not downgrade to the legacy port.
		return exchangeEncryptedSDP(ctx, client, base, key, offer)
	}
	return nil, errors.New("Unitree signaling is unreachable on ports 9991 and 8081; check the IP address and LAN connection")
}

func exchangeEncryptedSDP(ctx context.Context, client *http.Client, base, key string, offer []byte) ([]byte, error) {
	response, err := signalingPOST(ctx, client, base+"/con_notify", "", nil)
	if err != nil {
		return nil, err
	}
	pub, path, err := decodeNotify(response, key)
	if err != nil {
		return nil, err
	}
	var randomKey [16]byte
	if _, err := rand.Read(randomKey[:]); err != nil {
		return nil, fmt.Errorf("create Unitree session key: %w", err)
	}
	// The vendor wire protocol requires AES-ECB using 32 ASCII hex characters
	// as an AES-256 key, with RSA PKCS#1 v1.5 wrapping. This is interoperability,
	// not authenticated encryption. Keyed robots must first authenticate the
	// RSA notification with their per-device GCM key; no downgrade is allowed.
	sessionKey := []byte(hex.EncodeToString(randomKey[:]))
	encryptedOffer, err := encryptECB(offer, sessionKey)
	if err != nil {
		return nil, err
	}
	wrappedKey, err := rsa.EncryptPKCS1v15(rand.Reader, pub, sessionKey)
	if err != nil {
		return nil, errors.New("Unitree returned an unusable RSA public key")
	}
	body, err := json.Marshal(struct {
		Data1 string `json:"data1"`
		Data2 string `json:"data2"`
	}{encryptedOffer, base64.StdEncoding.EncodeToString(wrappedKey)})
	if err != nil {
		return nil, err
	}
	// Unitree expects JSON despite this form content type.
	response, err = signalingPOST(ctx, client, base+"/con_ing_"+path, "application/x-www-form-urlencoded", body)
	if err != nil {
		return nil, err
	}
	return decryptECB(string(bytes.TrimSpace(response)), sessionKey)
}

func signalingPOST(ctx context.Context, client *http.Client, endpoint, contentType string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("invalid Unitree signaling address")
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("Unitree signaling request failed; check the robot's LAN connection")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Unitree signaling returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSignalingResponse+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("read Unitree signaling response")
	}
	if len(data) > maxSignalingResponse {
		return nil, errors.New("Unitree signaling response is too large")
	}
	return data, nil
}

func parseDeviceKey(value string) ([]byte, error) {
	key, err := hex.DecodeString(strings.TrimSpace(value))
	if err != nil || len(key) != 16 {
		return nil, errors.New("Unitree encryption key must contain 32 hexadecimal characters")
	}
	return key, nil
}

func decodeNotify(response []byte, deviceKey string) (*rsa.PublicKey, string, error) {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(response)))
	if err != nil {
		return nil, "", errors.New("Unitree returned an invalid connection notification")
	}
	var notification struct {
		Data1 string `json:"data1"`
		Data2 int    `json:"data2"`
	}
	if json.Unmarshal(decoded, &notification) != nil {
		return nil, "", errors.New("Unitree returned an invalid connection notification")
	}
	// A supplied device key is the account's firmware-specific trust anchor.
	// The public validation challenge cannot authenticate the signaling peer:
	// never let a LAN impostor substitute a public/legacy RSA notification.
	if deviceKey != "" && notification.Data2 != 3 {
		return nil, "", errors.New("keyed Unitree robots require device-key-authenticated signaling")
	}
	data := notification.Data1
	switch notification.Data2 {
	case 0, 1:
	case 2, 3:
		key := legacyGCMKey
		if notification.Data2 == 3 {
			if deviceKey == "" {
				return nil, "", errors.New("this Unitree firmware requires its encryption key; sign in to the robot's Unitree account")
			}
			key, err = parseDeviceKey(deviceKey)
			if err != nil {
				return nil, "", err
			}
		}
		raw, err := base64.StdEncoding.DecodeString(data)
		if err != nil || len(raw) < 28 {
			return nil, "", errors.New("Unitree returned an invalid encrypted notification")
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, "", err
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return nil, "", err
		}
		// Unitree's layout is ciphertext || 12-byte nonce || 16-byte tag.
		ciphertext := append([]byte(nil), raw[:len(raw)-28]...)
		ciphertext = append(ciphertext, raw[len(raw)-16:]...)
		plaintext, err := gcm.Open(nil, raw[len(raw)-28:len(raw)-16], ciphertext, nil)
		if err != nil {
			return nil, "", errors.New("Unitree rejected the encryption key; select the account robot that matches this IP address")
		}
		data = string(plaintext)
	default:
		return nil, "", errors.New("Unitree uses an unsupported signaling encryption version")
	}
	if len(data) <= 20 {
		return nil, "", errors.New("Unitree returned an invalid public key notification")
	}
	ending := data[len(data)-10:]
	path := make([]byte, 5)
	for i := range path {
		letter := ending[2*i+1]
		if letter < 'A' || letter > 'J' {
			return nil, "", errors.New("Unitree returned an invalid signaling path")
		}
		path[i] = '0' + letter - 'A'
	}
	encoded := data[10 : len(data)-10]
	der, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, "", errors.New("Unitree returned an invalid RSA public key")
	}
	if block, _ := pem.Decode(der); block != nil {
		der = block.Bytes
	}
	var publicKey *rsa.PublicKey
	if parsed, err := x509.ParsePKIXPublicKey(der); err == nil {
		publicKey, _ = parsed.(*rsa.PublicKey)
	} else {
		publicKey, _ = x509.ParsePKCS1PublicKey(der)
	}
	if publicKey == nil || publicKey.N.BitLen() < 1024 || publicKey.N.BitLen() > maxSignalingRSAKeyBits {
		return nil, "", errors.New("Unitree returned an invalid RSA public key")
	}
	return publicKey, string(path), nil
}

func encryptECB(plaintext, key []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	padding := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := append(append([]byte(nil), plaintext...), bytes.Repeat([]byte{byte(padding)}, padding)...)
	for offset := 0; offset < len(padded); offset += aes.BlockSize {
		block.Encrypt(padded[offset:offset+aes.BlockSize], padded[offset:offset+aes.BlockSize])
	}
	return base64.StdEncoding.EncodeToString(padded), nil
}

func decryptECB(encoded string, key []byte) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("Unitree returned an invalid encrypted SDP answer")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	for offset := 0; offset < len(data); offset += aes.BlockSize {
		block.Decrypt(data[offset:offset+aes.BlockSize], data[offset:offset+aes.BlockSize])
	}
	padding := int(data[len(data)-1])
	if padding < 1 || padding > aes.BlockSize || !bytes.Equal(data[len(data)-padding:], bytes.Repeat([]byte{byte(padding)}, padding)) {
		return nil, errors.New("Unitree returned an invalid encrypted SDP answer")
	}
	return data[:len(data)-padding], nil
}
