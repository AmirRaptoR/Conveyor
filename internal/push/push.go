// Package push sends Web Push notifications (RFC 8030) with aes128gcm content
// encryption (RFC 8291) and VAPID authorisation (RFC 8292), on the standard
// library alone: crypto/ecdh for the key agreement, crypto/hkdf for the key
// schedule, AES-GCM for the record, ECDSA for the token.
//
// ponytail: single-record payloads only (under 4 KB), which is every
// notification this board sends. A payload that needs more than one record
// is refused rather than mis-encoded.
package push

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var b64 = base64.RawURLEncoding

// Keys is the VAPID key pair, persisted so subscriptions survive a restart:
// a browser subscribes against the public key, and a new one would orphan
// every subscription taken with the old.
type Keys struct {
	priv *ecdsa.PrivateKey
	// Public is the uncompressed P-256 point, base64url, as the page hands
	// it to PushManager.subscribe.
	Public string
}

// LoadKeys reads the key pair at path, creating it on first use.
func LoadKeys(path string) (*Keys, error) {
	var file struct {
		D string `json:"d"`
	}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &file); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		d, err := b64.DecodeString(file.D)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return fromScalar(d)
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	d := make([]byte, 32)
	priv.D.FillBytes(d)
	file.D = b64.EncodeToString(d)
	b, _ := json.Marshal(file)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return nil, err
	}
	return fromScalar(d)
}

func fromScalar(d []byte) (*Keys, error) {
	priv := new(ecdsa.PrivateKey)
	priv.Curve = elliptic.P256()
	priv.D = new(big.Int).SetBytes(d)
	priv.X, priv.Y = priv.Curve.ScalarBaseMult(d)
	pub := elliptic.Marshal(elliptic.P256(), priv.X, priv.Y) //nolint:staticcheck // the wire format VAPID wants
	return &Keys{priv: priv, Public: b64.EncodeToString(pub)}, nil
}

// token is the VAPID JWT for one push service origin, valid twelve hours.
func (k *Keys) token(aud, sub string, now time.Time) (string, error) {
	head := b64.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, _ := json.Marshal(map[string]any{
		"aud": aud, "exp": now.Add(12 * time.Hour).Unix(), "sub": sub,
	})
	signing := head + "." + b64.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, k.priv, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + b64.EncodeToString(sig), nil
}

// Subscription is what PushManager.subscribe returned, as the page posts it.
type Subscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// encrypt produces the aes128gcm body for one subscription.
func encrypt(sub Subscription, plaintext []byte) ([]byte, error) {
	uaPub, err := b64.DecodeString(sub.Keys.P256dh)
	if err != nil {
		return nil, fmt.Errorf("p256dh: %w", err)
	}
	auth, err := b64.DecodeString(sub.Keys.Auth)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	asKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return seal(uaPub, auth, asKey, salt, plaintext)
}

// seal is encrypt with its randomness passed in, so RFC 8291's test vector
// can drive it (push_test.go).
func seal(uaPub, auth []byte, asKey *ecdh.PrivateKey, salt, plaintext []byte) ([]byte, error) {
	if len(plaintext) > 3993 {
		return nil, errors.New("push payload over one record")
	}
	uaKey, err := ecdh.P256().NewPublicKey(uaPub)
	if err != nil {
		return nil, fmt.Errorf("p256dh: %w", err)
	}
	asPub := asKey.PublicKey().Bytes()
	secret, err := asKey.ECDH(uaKey)
	if err != nil {
		return nil, err
	}
	// RFC 8291 §3.3 — §3.4.
	keyInfo := append(append([]byte("WebPush: info\x00"), uaPub...), asPub...)
	ikm, err := hkdf.Key(sha256.New, secret, auth, string(keyInfo), 32)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// One record: the plaintext, the last-record delimiter, no padding.
	record := gcm.Seal(nil, nonce, append(bytes.Clone(plaintext), 0x02), nil)

	// RFC 8188 header: salt | rs | idlen | keyid.
	var body bytes.Buffer
	body.Write(salt)
	_ = binary.Write(&body, binary.BigEndian, uint32(4096))
	body.WriteByte(byte(len(asPub)))
	body.Write(asPub)
	body.Write(record)
	return body.Bytes(), nil
}

// Gone reports a subscription the push service says no longer exists; the
// caller should forget it.
var Gone = errors.New("subscription gone")

// Send delivers one payload to one subscription. sub is the VAPID contact
// (a mailto: or https: URL) the push service may use to reach the operator.
func (k *Keys) Send(ctx context.Context, s Subscription, payload []byte, contact string) error {
	body, err := encrypt(s, payload)
	if err != nil {
		return err
	}
	u, err := url.Parse(s.Endpoint)
	if err != nil {
		return err
	}
	tok, err := k.token(u.Scheme+"://"+u.Host, contact, time.Now())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", "86400")
	req.Header.Set("Urgency", "high")
	req.Header.Set("Authorization", "vapid t="+tok+", k="+k.Public)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusGone:
		return Gone
	case res.StatusCode >= 300:
		return fmt.Errorf("push service answered %s", res.Status)
	}
	return nil
}

// Store is the set of subscriptions, one JSON file, rewritten whole on every
// change. A device subscribes once per install and there are a handful, so
// nothing more is worth having.
type Store struct {
	mu   sync.Mutex
	path string
	subs []Subscription
}

func OpenStore(path string) *Store {
	st := &Store{path: path}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &st.subs)
	}
	return st
}

// Add records a subscription and reports whether it is new; the same
// endpoint twice is one subscription.
func (st *Store) Add(s Subscription) (bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for i := range st.subs {
		if st.subs[i].Endpoint == s.Endpoint {
			st.subs[i] = s
			return false, st.save()
		}
	}
	st.subs = append(st.subs, s)
	return true, st.save()
}

func (st *Store) Remove(endpoint string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	kept := st.subs[:0]
	for _, s := range st.subs {
		if s.Endpoint != endpoint {
			kept = append(kept, s)
		}
	}
	st.subs = kept
	return st.save()
}

func (st *Store) All() []Subscription {
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]Subscription(nil), st.subs...)
}

func (st *Store) Len() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.subs)
}

func (st *Store) save() error {
	b, err := json.Marshal(st.subs)
	if err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, st.path)
}
