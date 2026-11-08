package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// maxStateLength is the host's OAuth state limit. The sealed blob has to fit in
// it, which is why the device is kept beside the flow rather than inside it.
const maxStateLength = 128

// loginState is the small amount of flow context CPA's opaque state carries
// between auth.login.start and auth.login.poll.
type loginState struct {
	Site    string
	Expires int64
	// Flow names the callback listener and the device sidecar of this flow.
	Flow []byte
}

var stateKeyMu sync.Mutex

func loadStateKey(authDir string) ([]byte, error) {
	if authDir == "" {
		return nil, errors.New("CPA auth directory unavailable")
	}
	stateKeyMu.Lock()
	defer stateKeyMu.Unlock()
	if err := os.MkdirAll(authDir, 0700); err != nil {
		return nil, errors.New("cannot create auth directory")
	}
	path := filepath.Join(authDir, ".trae-oauth-state.key")
	key, err := os.ReadFile(path)
	if err == nil {
		if len(key) != 32 {
			return nil, errors.New("invalid state encryption key")
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, errors.New("cannot read state encryption key")
	}
	key = make([]byte, 32)
	if _, err = io.ReadFull(rand.Reader, key); err != nil {
		return nil, errors.New("cannot create state encryption key")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		key, err = os.ReadFile(path)
		if err != nil || len(key) != 32 {
			return nil, errors.New("cannot read state encryption key")
		}
		return key, nil
	}
	if err != nil {
		return nil, errors.New("cannot persist state encryption key")
	}
	_, err = f.Write(key)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		_ = os.Remove(path)
		return nil, errors.New("cannot persist state encryption key")
	}
	return key, nil
}

func newNonce() ([]byte, error) {
	b := make([]byte, 16)
	_, err := io.ReadFull(rand.Reader, b)
	return b, err
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// uuidString formats 16 bytes as the dashed UUID Trae's client sends.
func uuidString(b []byte) string {
	if len(b) != 16 {
		return ""
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// digits returns a numeric device id of n digits whose first digit is never 0.
func digits(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return ""
	}
	out := make([]byte, n)
	for i := range b {
		d := b[i] % 10
		if i == 0 && d == 0 {
			d = 1 + b[i]%9
		}
		out[i] = '0' + d
	}
	return string(out)
}

func sealState(key []byte, s loginState) (string, error) {
	if len(s.Flow) != 16 || s.Expires <= 0 {
		return "", errors.New("unsupported login flow state")
	}
	spec, ok := siteFor(s.Site)
	if !ok {
		return "", errors.New("unsupported login site")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", errors.New("cannot generate login state")
	}
	plain := make([]byte, 21)
	if spec.ID == siteTraeCN {
		plain[0] = 1
	}
	binary.BigEndian.PutUint32(plain[1:5], uint32(s.Expires))
	copy(plain[5:21], s.Flow)
	encoded := base64.RawURLEncoding.EncodeToString(append(append([]byte{}, nonce...), gcm.Seal(nil, nonce, plain, []byte("trae-login-v1"))...))
	if len(encoded) > maxStateLength {
		return "", errors.New("login state exceeds the CPA state length limit")
	}
	return encoded, nil
}

func openState(key []byte, encoded string) (loginState, error) {
	var s loginState
	if len(encoded) > maxStateLength {
		return s, errors.New("invalid login state")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) != 49 {
		return s, errors.New("invalid login state")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return s, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return s, err
	}
	nonce := raw[:12]
	p, err := gcm.Open(nil, nonce, raw[12:], []byte("trae-login-v1"))
	if err != nil || len(p) != 21 || p[0]&^byte(1) != 0 {
		return s, errors.New("invalid login state")
	}
	s = loginState{
		Site:    siteTrae,
		Expires: int64(binary.BigEndian.Uint32(p[1:5])),
		Flow:    append([]byte(nil), p[5:21]...),
	}
	if p[0]&1 != 0 {
		s.Site = siteTraeCN
	}
	return s, nil
}
