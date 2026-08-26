package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestSafeIdentifiers(t *testing.T) {
	for _, value := range []string{"room-01", "team_alpha", "AB"} {
		if !safeIdentifier.MatchString(value) {
			t.Fatalf("expected %q to be valid", value)
		}
	}
	for _, value := range []string{"a", "room name", "../admin", strings.Repeat("x", 33)} {
		if safeIdentifier.MatchString(value) {
			t.Fatalf("expected %q to be rejected", value)
		}
	}
}

func TestOriginPolicy(t *testing.T) {
	server := newServer("index.html", "https://portfolio.example")
	for _, test := range []struct {
		origin string
		host   string
		want   bool
	}{
		{"https://relay.example", "relay.example", true},
		{"https://portfolio.example", "relay.example", true},
		{"https://attacker.example", "relay.example", false},
		{"", "relay.example", false},
	} {
		request := httptest.NewRequest(http.MethodGet, "http://"+test.host+"/ws", nil)
		request.Header.Set("Origin", test.origin)
		if got := server.checkOrigin(request); got != test.want {
			t.Fatalf("origin %q host %q: got %v want %v", test.origin, test.host, got, test.want)
		}
	}
}

func TestBroadcastAndRoomIsolation(t *testing.T) {
	server := httptest.NewServer(newServer("index.html", "").routes())
	defer server.Close()

	connect := func(room, id, name string) *websocket.Conn {
		t.Helper()
		endpoint, _ := url.Parse(server.URL)
		endpoint.Scheme = "ws"
		endpoint.Path = "/ws"
		query := endpoint.Query()
		query.Set("room", room)
		query.Set("id", id)
		query.Set("name", name)
		endpoint.RawQuery = query.Encode()
		header := http.Header{"Origin": []string{server.URL}}
		connection, response, err := websocket.DefaultDialer.Dial(endpoint.String(), header)
		if err != nil {
			t.Fatalf("dial websocket: %v (response=%v)", err, response)
		}
		return connection
	}

	readUntil := func(connection *websocket.Conn, messageType string) Message {
		t.Helper()
		_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
		for {
			var message Message
			if err := connection.ReadJSON(&message); err != nil {
				t.Fatalf("read websocket: %v", err)
			}
			if message.Type == messageType {
				return message
			}
		}
	}

	atlas := connect("product-room", "client_atlas", "Ayse")
	defer atlas.Close()
	readUntil(atlas, "presence.join")
	boreal := connect("product-room", "client_boreal", "Mehmet")
	defer boreal.Close()
	readUntil(boreal, "presence.join")
	isolated := connect("other-room", "client_other", "Zeynep")
	defer isolated.Close()
	readUntil(isolated, "presence.join")

	if err := atlas.WriteMessage(websocket.TextMessage, []byte("release hazır")); err != nil {
		t.Fatalf("send message: %v", err)
	}
	received := readUntil(boreal, "message")
	if received.Text != "release hazır" || received.Room != "product-room" || received.ClientID != "client_atlas" {
		t.Fatalf("unexpected message: %+v", received)
	}

	_ = isolated.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
	var leaked Message
	if err := isolated.ReadJSON(&leaked); err == nil {
		t.Fatalf("message leaked into isolated room: %+v", leaked)
	}
}
