package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type roomResponse struct {
	Room      RoomInfo `json:"room"`
	AccessKey string   `json:"access_key"`
}

type ticketResponse struct {
	Ticket   string `json:"ticket"`
	ClientID string `json:"client_id"`
}

func startTestServer(t *testing.T, config serverConfig) (*Server, *httptest.Server) {
	t.Helper()
	server := newServerWithConfig("index.html", "", config)
	httpServer := httptest.NewServer(server.routes())
	t.Cleanup(func() { httpServer.Close(); server.close() })
	return server, httpServer
}

func postJSON(t *testing.T, baseURL, path string, payload any) (*http.Response, []byte) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Post(baseURL+path, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer response.Body.Close()
	var decoded bytes.Buffer
	_, _ = decoded.ReadFrom(response.Body)
	return response, decoded.Bytes()
}

func createRoom(t *testing.T, baseURL, name, visibility string) roomResponse {
	t.Helper()
	response, body := postJSON(t, baseURL, "/api/rooms", map[string]string{"name": name, "visibility": visibility})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create room: status=%d body=%s", response.StatusCode, body)
	}
	var result roomResponse
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func issueTicket(t *testing.T, baseURL, roomID, name, accessKey string, wantStatus int) ticketResponse {
	t.Helper()
	response, body := postJSON(t, baseURL, "/api/tickets", map[string]string{"room_id": roomID, "name": name, "access_key": accessKey})
	if response.StatusCode != wantStatus {
		t.Fatalf("issue ticket: status=%d want=%d body=%s", response.StatusCode, wantStatus, body)
	}
	var result ticketResponse
	if wantStatus == http.StatusCreated {
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func dialTicket(t *testing.T, baseURL, ticket string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	endpoint, _ := url.Parse(baseURL)
	endpoint.Scheme = "ws"
	endpoint.Path = "/ws"
	query := endpoint.Query()
	query.Set("ticket", ticket)
	endpoint.RawQuery = query.Encode()
	header := http.Header{"Origin": []string{baseURL}}
	return websocket.DefaultDialer.Dial(endpoint.String(), header)
}

func readUntil(t *testing.T, connection *websocket.Conn, eventType string) Event {
	t.Helper()
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		var event Event
		if err := connection.ReadJSON(&event); err != nil {
			t.Fatalf("read websocket: %v", err)
		}
		if event.Type == eventType {
			return event
		}
	}
}

func TestValidationAndSecretComparison(t *testing.T) {
	for _, value := range []string{"Ürün Ekibi", "Backend room", "abc"} {
		if !validRoomName(value) {
			t.Fatalf("expected room name %q to be valid", value)
		}
	}
	for _, value := range []string{"a", "bad\nroom", strings.Repeat("x", 61)} {
		if validRoomName(value) {
			t.Fatalf("expected room name %q to be rejected", value)
		}
	}
	hash := hashSecret("correct-horse-battery-staple")
	if !secretsMatch(hash, "correct-horse-battery-staple") || secretsMatch(hash, "wrong") {
		t.Fatal("constant-time secret comparison failed")
	}
}

func TestOriginPolicy(t *testing.T) {
	server := newServer("index.html", "https://portfolio.example")
	defer server.close()
	for _, test := range []struct {
		origin, host string
		want         bool
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

func TestPrivateRoomRequiresAccessKeyAndTicketIsOneTime(t *testing.T) {
	_, httpServer := startTestServer(t, defaultConfig())
	created := createRoom(t, httpServer.URL, "Private Backend Review", "private")
	if created.AccessKey == "" || created.Room.Visibility != "private" || !roomIDPattern.MatchString(created.Room.ID) {
		t.Fatalf("unexpected private room: %+v", created)
	}
	issueTicket(t, httpServer.URL, created.Room.ID, "Attacker", "wrong-key", http.StatusForbidden)
	ticket := issueTicket(t, httpServer.URL, created.Room.ID, "Yasin", created.AccessKey, http.StatusCreated)
	connection, _, err := dialTicket(t, httpServer.URL, ticket.Ticket)
	if err != nil {
		t.Fatalf("dial valid ticket: %v", err)
	}
	defer connection.Close()
	joined := readUntil(t, connection, "presence.join")
	if joined.Sender == nil || joined.Sender.ID != ticket.ClientID || joined.Participants != 1 {
		t.Fatalf("unexpected join event: %+v", joined)
	}
	second, response, err := dialTicket(t, httpServer.URL, ticket.Ticket)
	if second != nil {
		second.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("ticket reuse was not rejected: err=%v response=%v", err, response)
	}
}

func TestBroadcastAndRoomIsolation(t *testing.T) {
	_, httpServer := startTestServer(t, defaultConfig())
	alpha := createRoom(t, httpServer.URL, "Alpha Room", "public")
	beta := createRoom(t, httpServer.URL, "Beta Room", "public")

	connect := func(roomID, name string) *websocket.Conn {
		ticket := issueTicket(t, httpServer.URL, roomID, name, "", http.StatusCreated)
		connection, _, err := dialTicket(t, httpServer.URL, ticket.Ticket)
		if err != nil {
			t.Fatalf("dial websocket: %v", err)
		}
		readUntil(t, connection, "presence.join")
		return connection
	}

	atlas := connect(alpha.Room.ID, "Ayşe")
	defer atlas.Close()
	boreal := connect(alpha.Room.ID, "Mehmet")
	defer boreal.Close()
	isolated := connect(beta.Room.ID, "Zeynep")
	defer isolated.Close()

	command := ClientCommand{Type: "message.send", Text: "release hazır"}
	if err := atlas.WriteJSON(command); err != nil {
		t.Fatalf("send message: %v", err)
	}
	received := readUntil(t, boreal, "message.created")
	if received.Text != command.Text || received.RoomID != alpha.Room.ID || received.Sender == nil || received.Sender.Name != "Ayşe" {
		t.Fatalf("unexpected event: %+v", received)
	}
	if received.Sequence == 0 {
		t.Fatal("expected monotonic room sequence")
	}

	_ = isolated.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
	var leaked Event
	if err := isolated.ReadJSON(&leaked); err == nil {
		t.Fatalf("message leaked into isolated room: %+v", leaked)
	}
}

func TestRoomCapacityAndExpiry(t *testing.T) {
	config := defaultConfig()
	config.maxClientsPerRoom = 1
	server, httpServer := startTestServer(t, config)
	created := createRoom(t, httpServer.URL, "Capacity Test", "public")
	firstTicket := issueTicket(t, httpServer.URL, created.Room.ID, "First", "", http.StatusCreated)
	first, _, err := dialTicket(t, httpServer.URL, firstTicket.Ticket)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	readUntil(t, first, "presence.join")
	issueTicket(t, httpServer.URL, created.Room.ID, "Second", "", http.StatusConflict)
	first.Close()
	deadline := time.Now().Add(time.Second)
	room, _ := server.hub.get(created.Room.ID)
	for room.participants.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	room.lastActive.Store(time.Now().Add(-time.Hour).UnixNano())
	server.hub.removeExpired(time.Now())
	if _, exists := server.hub.get(created.Room.ID); exists {
		t.Fatal("idle empty room was not expired")
	}
}

func TestMetricsExposeOperationalCounters(t *testing.T) {
	_, httpServer := startTestServer(t, defaultConfig())
	createRoom(t, httpServer.URL, "Metrics Room", "public")
	response, err := http.Get(httpServer.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body bytes.Buffer
	_, _ = body.ReadFrom(response.Body)
	for _, metric := range []string{"relay_rooms 1", "relay_connections", "relay_messages_total", "relay_rejected_total", "relay_tickets_issued_total"} {
		if !strings.Contains(body.String(), metric) {
			t.Fatalf("metrics missing %q: %s", metric, body.String())
		}
	}
}

func TestTrustedProxyIPAndRequestID(t *testing.T) {
	server := newServerWithConfig("index.html", "", defaultConfig())
	defer server.close()
	server.trustProxy = true
	request := httptest.NewRequest(http.MethodGet, "http://relay.example/healthz", nil)
	request.RemoteAddr = "10.0.0.4:1234"
	request.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.4")
	if got := server.remoteIP(request); got != "203.0.113.7" {
		t.Fatalf("trusted proxy IP: got %q", got)
	}
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.HasPrefix(recorder.Header().Get("X-Request-ID"), "req_") {
		t.Fatalf("missing request correlation: status=%d headers=%v", recorder.Code, recorder.Header())
	}
}
