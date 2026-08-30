package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	writeWait       = 10 * time.Second
	pongWait        = 60 * time.Second
	pingPeriod      = pongWait * 9 / 10
	maxFrameBytes   = 4096
	maxMessageRunes = 1000
)

var roomIDPattern = regexp.MustCompile(`^rm_[a-zA-Z0-9_-]{10,40}$`)

type Member struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Event struct {
	ID           string   `json:"id"`
	Type         string   `json:"type"`
	RoomID       string   `json:"room_id"`
	Sequence     uint64   `json:"sequence"`
	Text         string   `json:"text,omitempty"`
	Sender       *Member  `json:"sender,omitempty"`
	SentAt       string   `json:"sent_at"`
	Participants int      `json:"participants,omitempty"`
	Members      []Member `json:"members,omitempty"`
	Code         string   `json:"code,omitempty"`
}

type ClientCommand struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func randomToken(bytes int) (string, error) {
	buffer := make([]byte, bytes)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func mustRandomToken(bytes int) string {
	token, err := randomToken(bytes)
	if err != nil {
		panic(err)
	}
	return token
}

func hashSecret(secret string) [32]byte { return sha256.Sum256([]byte(secret)) }

func secretsMatch(expected [32]byte, supplied string) bool {
	actual := hashSecret(supplied)
	return subtle.ConstantTimeCompare(expected[:], actual[:]) == 1
}

func validDisplayName(value string) bool {
	value = strings.TrimSpace(value)
	length := len([]rune(value))
	return length >= 2 && length <= 40 && !strings.ContainsAny(value, "\r\n\t")
}

func validRoomName(value string) bool {
	value = strings.TrimSpace(value)
	length := len([]rune(value))
	return length >= 3 && length <= 60 && !strings.ContainsAny(value, "\r\n\t")
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339Nano) }
