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
	"encoding/binary"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testSubscription(t *testing.T, endpoint string) Subscription {
	t.Helper()
	ua, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	rand.Read(auth)
	var sub Subscription
	sub.Endpoint = endpoint
	sub.Keys.P256dh = b64.EncodeToString(ua.PublicKey().Bytes())
	sub.Keys.Auth = b64.EncodeToString(auth)
	return sub
}

// A real destination — every push service is one — is unaffected: Send
// reaches an httptest server on 127.0.0.1 only because these tests opt into
// allowPrivateDials, which nothing outside a test ever sets.
func TestSendRefusesAPrivateDestinationByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	k, err := LoadKeys(filepath.Join(t.TempDir(), "vapid.json"))
	if err != nil {
		t.Fatal(err)
	}
	sub := testSubscription(t, srv.URL+"/abc")

	if err := k.Send(context.Background(), sub, []byte("{}"), "mailto:a@b"); err == nil {
		t.Fatal("Send to a loopback destination succeeded; want it refused")
	}

	allowPrivateDials.Store(true)
	defer allowPrivateDials.Store(false)
	if err := k.Send(context.Background(), sub, []byte("{}"), "mailto:a@b"); err != nil {
		t.Fatalf("Send with allowPrivateDials set: %v", err)
	}
}

// A push service answering with a redirect must not be followed — the whole
// point of validating the destination is worthless if a 3xx can retarget the
// request to somewhere else entirely after the check.
func TestSendDoesNotFollowRedirects(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the redirect target must never be reached")
	}))
	defer final.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer redirecting.Close()

	allowPrivateDials.Store(true)
	defer allowPrivateDials.Store(false)
	k, err := LoadKeys(filepath.Join(t.TempDir(), "vapid.json"))
	if err != nil {
		t.Fatal(err)
	}
	sub := testSubscription(t, redirecting.URL+"/abc")
	if err := k.Send(context.Background(), sub, []byte("{}"), "mailto:a@b"); err == nil {
		t.Fatal("Send followed a redirect and reported success; want it reported as a failure")
	}
}

func TestNonPublicIP(t *testing.T) {
	for _, tc := range []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"10.0.0.5", true},
		{"172.16.0.5", true},
		{"192.168.1.1", true},
		{"169.254.1.1", true},
		{"::1", true},
		{"fc00::1", true}, // unique-local
		{"8.8.8.8", false},
		{"1.1.1.1", false},
	} {
		got := NonPublicIP(net.ParseIP(tc.ip))
		if got != tc.want {
			t.Errorf("NonPublicIP(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

// The receiver's half of RFC 8291, written independently of encrypt so the
// two have to agree on the key schedule rather than share it.
func decrypt(t *testing.T, ua *ecdh.PrivateKey, auth, body []byte) []byte {
	t.Helper()
	salt := body[:16]
	rs := binary.BigEndian.Uint32(body[16:20])
	if rs != 4096 {
		t.Fatalf("rs = %d", rs)
	}
	idlen := int(body[20])
	asPub := body[21 : 21+idlen]
	record := body[21+idlen:]

	asKey, err := ecdh.P256().NewPublicKey(asPub)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := ua.ECDH(asKey)
	if err != nil {
		t.Fatal(err)
	}
	info := "WebPush: info\x00" + string(ua.PublicKey().Bytes()) + string(asPub)
	ikm, _ := hkdf.Key(sha256.New, secret, auth, info, 32)
	cek, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, record, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if plain[len(plain)-1] != 0x02 {
		t.Fatalf("last byte %x, want the last-record delimiter", plain[len(plain)-1])
	}
	return plain[:len(plain)-1]
}

func TestEncryptRoundTrips(t *testing.T) {
	ua, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	rand.Read(auth)
	var sub Subscription
	sub.Endpoint = "https://push.example/abc"
	sub.Keys.P256dh = b64.EncodeToString(ua.PublicKey().Bytes())
	sub.Keys.Auth = b64.EncodeToString(auth)

	msg := []byte(`{"title":"Needs you","body":"Which auth scheme?"}`)
	body, err := encrypt(sub, msg)
	if err != nil {
		t.Fatal(err)
	}
	if got := decrypt(t, ua, auth, body); !bytes.Equal(got, msg) {
		t.Fatalf("got %q", got)
	}
	if _, err := encrypt(sub, make([]byte, 5000)); err == nil {
		t.Fatal("a payload over one record must be refused")
	}
}

func TestKeysPersistAndSign(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vapid.json")
	k1, err := LoadKeys(path)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := LoadKeys(path)
	if err != nil {
		t.Fatal(err)
	}
	if k1.Public != k2.Public {
		t.Fatal("the key changed between loads")
	}
	tok, err := k1.token("https://push.example", "mailto:a@b", time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	claims, _ := b64.DecodeString(parts[1])
	var c map[string]any
	if err := json.Unmarshal(claims, &c); err != nil {
		t.Fatal(err)
	}
	if c["aud"] != "https://push.example" || c["sub"] != "mailto:a@b" || c["exp"] != float64(1_700_000_000+43200) {
		t.Fatalf("claims %v", c)
	}
	// Verify with the public key exactly as a push service would.
	pub, _ := b64.DecodeString(k1.Public)
	x, y := elliptic.Unmarshal(elliptic.P256(), pub) //nolint:staticcheck
	sig, _ := b64.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, sum[:], r, s) {
		t.Fatal("the token does not verify against the public key")
	}
}

func TestStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "push.json")
	st := OpenStore(path)
	var a, b Subscription
	a.Endpoint, b.Endpoint = "https://p/1", "https://p/2"
	if fresh, err := st.Add(a); err != nil || !fresh {
		t.Fatalf("first add: fresh=%v err=%v", fresh, err)
	}
	_, _ = st.Add(b)
	if fresh, _ := st.Add(a); fresh { // once
		t.Fatal("the same endpoint again is not a new subscription")
	}
	if OpenStore(path).Len() != 2 {
		t.Fatalf("len %d", OpenStore(path).Len())
	}
	_ = st.Remove("https://p/1")
	if got := OpenStore(path).All(); len(got) != 1 || got[0].Endpoint != "https://p/2" {
		t.Fatalf("got %v", got)
	}
}

// RFC 8291 Appendix A, verbatim: the one input that settles whether this
// key schedule is the one every push service expects.
func TestRFC8291Vector(t *testing.T) {
	dec := func(s string) []byte {
		b, err := b64.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	uaPub := dec("BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4")
	auth := dec("BTBZMqHH6r4Tts7J_aSIgg")
	asKey, err := ecdh.P256().NewPrivateKey(dec("yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	salt := dec("DGv6ra1nlYgDCS1FRnbzlw")
	want := "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	got, err := seal(uaPub, auth, asKey, salt, []byte("When I grow up, I want to be a watermelon"))
	if err != nil {
		t.Fatal(err)
	}
	if b64.EncodeToString(got) != want {
		t.Fatalf("\ngot  %s\nwant %s", b64.EncodeToString(got), want)
	}
}
