package imbridge

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// GenerateNonce returns a cryptographically random 16-byte hex string.
func GenerateNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type ActionTokenPayload struct {
	WorkspaceID          string `json:"ws"`
	ProjectID            string `json:"prj"`
	TaskID               string `json:"tsk"`
	StepID               string `json:"stp"`
	Action               string `json:"act"` // approve | edit | review_approve | reject
	ChannelID            string `json:"cid,omitempty"`
	ConnectionID         string `json:"conn_id,omitempty"`
	PostID               string `json:"post_id,omitempty"`
	ExpectedStateVersion int64  `json:"ver"`
	ReviewSnapshotHash   string `json:"hsh"`
	Nonce                string `json:"nonce"`
	ExpiresAt            int64  `json:"exp"`
}

type DialogTokenPayload struct {
	WorkspaceID          string `json:"ws"`
	ProjectID            string `json:"prj"`
	TaskID               string `json:"tsk"`
	StepID               string `json:"stp"`
	Action               string `json:"act"`
	ChannelID            string `json:"cid,omitempty"`
	ConnectionID         string `json:"conn_id,omitempty"`
	ExpectedStateVersion int64  `json:"ver"`
	ReviewSnapshotHash   string `json:"hsh"`
	ActorMMUserID        string `json:"mm_uid"`
	SessionID            string `json:"sid"`
	PostID               string `json:"post_id,omitempty"`
	Nonce                string `json:"nonce"`
	ExpiresAt            int64  `json:"exp"`
}

func SignActionToken(secret string, payload ActionTokenPayload) (string, error) {
	if secret == "" {
		return "", errors.New("signing secret cannot be empty")
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal action payload: %w", err)
	}
	bodyB64 := base64.RawURLEncoding.EncodeToString(bodyBytes)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(bodyB64))
	sig := hex.EncodeToString(mac.Sum(nil))
	return bodyB64 + "." + sig, nil
}

func VerifyActionToken(secret, token string) (*ActionTokenPayload, error) {
	if secret == "" {
		return nil, errors.New("signing secret cannot be empty")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return nil, errors.New("invalid action token format")
	}
	bodyB64 := parts[0]
	sig := parts[1]

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(bodyB64))
	expectedSig := hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(sig), []byte(expectedSig)) {
		return nil, errors.New("invalid action token signature")
	}

	bodyBytes, err := base64.RawURLEncoding.DecodeString(bodyB64)
	if err != nil {
		return nil, fmt.Errorf("decode action payload: %w", err)
	}

	var payload ActionTokenPayload
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		return nil, fmt.Errorf("unmarshal action payload: %w", err)
	}

	if time.Now().UTC().Unix() > payload.ExpiresAt {
		return nil, errors.New("action token has expired")
	}

	return &payload, nil
}

func SignDialogToken(secret string, payload DialogTokenPayload) (string, error) {
	if secret == "" {
		return "", errors.New("signing secret cannot be empty")
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal dialog payload: %w", err)
	}
	bodyB64 := base64.RawURLEncoding.EncodeToString(bodyBytes)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(bodyB64))
	sig := hex.EncodeToString(mac.Sum(nil))
	return bodyB64 + "." + sig, nil
}

func VerifyDialogToken(secret, token string) (*DialogTokenPayload, error) {
	if secret == "" {
		return nil, errors.New("signing secret cannot be empty")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return nil, errors.New("invalid dialog token format")
	}
	bodyB64 := parts[0]
	sig := parts[1]

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(bodyB64))
	expectedSig := hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(sig), []byte(expectedSig)) {
		return nil, errors.New("invalid dialog token signature")
	}

	bodyBytes, err := base64.RawURLEncoding.DecodeString(bodyB64)
	if err != nil {
		return nil, fmt.Errorf("decode dialog payload: %w", err)
	}

	var payload DialogTokenPayload
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		return nil, fmt.Errorf("unmarshal dialog payload: %w", err)
	}

	if time.Now().UTC().Unix() > payload.ExpiresAt {
		return nil, errors.New("dialog token has expired")
	}

	return &payload, nil
}

// ComputeTokenHash computes SHA256 of token to store in database for uniqueness & replay defense.
func ComputeTokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
