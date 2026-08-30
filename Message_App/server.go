package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type serverConfig struct {
	roomTTL           time.Duration
	ticketTTL         time.Duration
	maxRooms          int
	maxClientsPerRoom int
	maxMessagesWindow int
	messageWindow     time.Duration
}

func defaultConfig() serverConfig {
	return serverConfig{
		roomTTL: 30 * time.Minute, ticketTTL: 30 * time.Second,
		maxRooms: 500, maxClientsPerRoom: 50,
		maxMessagesWindow: 20, messageWindow: 10 * time.Second,
	}
}

type joinTicket struct {
	roomID, clientID, name string
	expires                time.Time
}

type ticketStore struct {
	mu      sync.Mutex
	tickets map[string]joinTicket
	ttl     time.Duration
}

func newTicketStore(ttl time.Duration) *ticketStore {
	return &ticketStore{tickets: make(map[string]joinTicket), ttl: ttl}
}

func (s *ticketStore) issue(roomID, clientID, name string) (string, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for token, ticket := range s.tickets {
		if now.After(ticket.expires) {
			delete(s.tickets, token)
		}
	}
	token := mustRandomToken(32)
	expires := now.Add(s.ttl)
	s.tickets[token] = joinTicket{roomID: roomID, clientID: clientID, name: name, expires: expires}
	return token, expires
}

func (s *ticketStore) consume(token string) (joinTicket, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ticket, exists := s.tickets[token]
	delete(s.tickets, token)
	if !exists || time.Now().After(ticket.expires) {
		return joinTicket{}, false
	}
	return ticket, true
}

type windowEntry struct {
	count   int
	resetAt time.Time
}
type windowLimiter struct {
	mu      sync.Mutex
	entries map[string]windowEntry
	limit   int
	window  time.Duration
}

func newWindowLimiter(limit int, window time.Duration) *windowLimiter {
	return &windowLimiter{entries: make(map[string]windowEntry), limit: limit, window: window}
}

func (l *windowLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	entry := l.entries[key]
	if entry.resetAt.IsZero() || now.After(entry.resetAt) {
		l.entries[key] = windowEntry{count: 1, resetAt: now.Add(l.window)}
		return true
	}
	if entry.count >= l.limit {
		return false
	}
	entry.count++
	l.entries[key] = entry
	return true
}

type Server struct {
	hub            *Hub
	tickets        *ticketStore
	indexPath      string
	allowedOrigins map[string]struct{}
	config         serverConfig
	stats          *Stats
	startedAt      time.Time
	createLimiter  *windowLimiter
	ticketLimiter  *windowLimiter
	trustProxy     bool
}

func newServer(indexPath, origins string) *Server {
	server := newServerWithConfig(indexPath, origins, defaultConfig())
	server.trustProxy = strings.EqualFold(os.Getenv("TRUST_PROXY"), "true")
	return server
}

func newServerWithConfig(indexPath, origins string, config serverConfig) *Server {
	allowed := make(map[string]struct{})
	for _, origin := range strings.Split(origins, ",") {
		origin = strings.TrimSpace(strings.TrimSuffix(origin, "/"))
		if origin != "" {
			allowed[origin] = struct{}{}
		}
	}
	stats := &Stats{}
	return &Server{
		hub:     newHub(config.maxRooms, config.maxClientsPerRoom, config.roomTTL, stats),
		tickets: newTicketStore(config.ticketTTL), indexPath: indexPath,
		allowedOrigins: allowed, config: config, stats: stats, startedAt: time.Now(),
		createLimiter: newWindowLimiter(12, 10*time.Minute),
		ticketLimiter: newWindowLimiter(120, time.Minute),
	}
}

func (s *Server) close() { s.hub.close() }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.serveHome)
	mux.HandleFunc("/healthz", s.serveHealth)
	mux.HandleFunc("/metrics", s.serveMetrics)
	mux.HandleFunc("/api/rooms", s.serveRooms)
	mux.HandleFunc("/api/rooms/", s.serveRoom)
	mux.HandleFunc("/api/tickets", s.serveTickets)
	mux.HandleFunc("/ws", s.serveWebSocket)
	return requestLogger(securityHeaders(mux))
}

func (s *Server) serveHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}
	http.ServeFile(w, r, s.indexPath)
}

func (s *Server) serveHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "rooms": s.stats.rooms.Load(), "connections": s.stats.connections.Load(),
		"uptime_seconds": int64(time.Since(s.startedAt).Seconds()),
	})
}

func (s *Server) serveMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "relay_rooms %d\nrelay_connections %d\nrelay_messages_total %d\nrelay_rejected_total %d\nrelay_slow_consumers_total %d\nrelay_tickets_issued_total %d\n",
		s.stats.rooms.Load(), s.stats.connections.Load(), s.stats.messages.Load(), s.stats.rejected.Load(), s.stats.slowConsumers.Load(), s.stats.ticketsIssued.Load())
}

func (s *Server) serveRooms(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}
	if !s.createLimiter.allow(s.remoteIP(r)) {
		s.reject(w, http.StatusTooManyRequests, "rate_limited", "Too many rooms created; try again later")
		return
	}
	var request struct {
		Name       string `json:"name"`
		Visibility string `json:"visibility"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		s.reject(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if !validRoomName(request.Name) || (request.Visibility != "public" && request.Visibility != "private") {
		s.reject(w, http.StatusBadRequest, "invalid_room", "Room name or visibility is invalid")
		return
	}
	room, accessKey, err := s.hub.create(request.Name, request.Visibility == "private")
	if errors.Is(err, errRoomLimit) {
		s.reject(w, http.StatusServiceUnavailable, "capacity_reached", "Room capacity is temporarily full")
		return
	}
	if err != nil {
		s.reject(w, http.StatusInternalServerError, "internal_error", "Room could not be created")
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		Room      RoomInfo `json:"room"`
		AccessKey string   `json:"access_key,omitempty"`
	}{Room: room.info(), AccessKey: accessKey})
}

func (s *Server) serveRoom(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/rooms/")
	if !roomIDPattern.MatchString(id) {
		writeAPIError(w, http.StatusBadRequest, "invalid_room_id", "Room ID is invalid")
		return
	}
	room, exists := s.hub.get(id)
	if !exists {
		writeAPIError(w, http.StatusNotFound, "room_not_found", "Room was not found or expired")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"room": room.info()})
}

func (s *Server) serveTickets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}
	if !s.ticketLimiter.allow(s.remoteIP(r)) {
		s.reject(w, http.StatusTooManyRequests, "rate_limited", "Too many join attempts; try again later")
		return
	}
	var request struct {
		RoomID    string `json:"room_id"`
		Name      string `json:"name"`
		AccessKey string `json:"access_key"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		s.reject(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if !roomIDPattern.MatchString(request.RoomID) || !validDisplayName(request.Name) {
		s.reject(w, http.StatusBadRequest, "invalid_join", "Room ID or display name is invalid")
		return
	}
	room, exists := s.hub.get(request.RoomID)
	if !exists {
		s.reject(w, http.StatusNotFound, "room_not_found", "Room was not found or expired")
		return
	}
	if !room.checkAccess(request.AccessKey) {
		s.reject(w, http.StatusForbidden, "access_denied", "Private room access key is invalid")
		return
	}
	if room.participants.Load() >= int64(room.maxClients) {
		s.reject(w, http.StatusConflict, "room_full", "Room is full")
		return
	}
	clientID := "cl_" + mustRandomToken(9)
	token, expires := s.tickets.issue(room.id, clientID, request.Name)
	s.stats.ticketsIssued.Add(1)
	writeJSON(w, http.StatusCreated, map[string]any{"ticket": token, "expires_at": expires.UTC().Format(time.RFC3339), "client_id": clientID})
}

func (s *Server) serveWebSocket(w http.ResponseWriter, r *http.Request) {
	if !s.checkOrigin(r) {
		s.reject(w, http.StatusForbidden, "origin_denied", "Origin is not allowed")
		return
	}
	ticket, valid := s.tickets.consume(r.URL.Query().Get("ticket"))
	if !valid {
		s.reject(w, http.StatusUnauthorized, "invalid_ticket", "Join ticket is invalid, expired, or already used")
		return
	}
	room, exists := s.hub.get(ticket.roomID)
	if !exists {
		s.reject(w, http.StatusNotFound, "room_not_found", "Room was not found or expired")
		return
	}
	upgrader := websocket.Upgrader{ReadBufferSize: 1024, WriteBufferSize: 1024, CheckOrigin: s.checkOrigin}
	connection, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	client := &Client{id: ticket.clientID, name: ticket.name, room: room, conn: connection, send: make(chan Event, 64), maxMessage: s.config.maxMessagesWindow, stats: s.stats}
	if err := room.registerClient(client); err != nil {
		_ = connection.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "room unavailable"), time.Now().Add(writeWait))
		_ = connection.Close()
		return
	}
	go client.writePump()
	go client.readPump(s.config.messageWindow)
}

func (s *Server) checkOrigin(r *http.Request) bool {
	origin := strings.TrimSuffix(r.Header.Get("Origin"), "/")
	if origin == "" {
		return false
	}
	parsed, err := url.Parse(origin)
	if err == nil && strings.EqualFold(parsed.Host, r.Host) {
		return true
	}
	_, allowed := s.allowedOrigins[origin]
	return allowed
}

func (s *Server) reject(w http.ResponseWriter, status int, code, message string) {
	s.stats.rejected.Add(1)
	writeAPIError(w, status, code, message)
}

func (c *Client) readPump(window time.Duration) {
	defer func() { c.room.unregisterClient(c); _ = c.conn.Close() }()
	c.conn.SetReadLimit(maxFrameBytes)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error { return c.conn.SetReadDeadline(time.Now().Add(pongWait)) })
	windowStarted, sent, violations := time.Now(), 0, 0
	for {
		var command ClientCommand
		if err := c.conn.ReadJSON(&command); err != nil {
			return
		}
		now := time.Now()
		if now.Sub(windowStarted) >= window {
			windowStarted, sent = now, 0
		}
		sent++
		if sent > c.maxMessage {
			violations++
			c.stats.rejected.Add(1)
			select {
			case c.send <- Event{ID: "evt_" + mustRandomToken(12), Type: "error", RoomID: c.room.id, SentAt: nowRFC3339(), Code: "rate_limited", Text: "Message rate limit exceeded"}:
			default:
			}
			if violations >= 3 {
				return
			}
			continue
		}
		text := strings.TrimSpace(command.Text)
		if command.Type != "message.send" || text == "" || len([]rune(text)) > maxMessageRunes {
			c.stats.rejected.Add(1)
			select {
			case c.send <- Event{ID: "evt_" + mustRandomToken(12), Type: "error", RoomID: c.room.id, SentAt: nowRFC3339(), Code: "invalid_message", Text: "Message is empty, too long, or unsupported"}:
			default:
			}
			continue
		}
		if !c.room.sendMessage(c, text) {
			return
		}
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() { ticker.Stop(); _ = c.conn.Close() }()
	for {
		select {
		case event, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteJSON(event); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		return errors.New("Content-Type must be application/json")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("Malformed JSON request")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("Request body must contain one JSON object")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func (s *Server) remoteIP(r *http.Request) string {
	if s.trustProxy {
		forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0])
		if parsed := net.ParseIP(forwarded); parsed != nil {
			return parsed.String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(body []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	written, err := r.ResponseWriter.Write(body)
	r.bytes += written
	return written, err
}

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("websocket hijacking is not supported")
	}
	r.status = http.StatusSwitchingProtocols
	return hijacker.Hijack()
}

func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		requestID := "req_" + mustRandomToken(9)
		w.Header().Set("X-Request-ID", requestID)
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		log.Printf(`request_id=%q method=%q path=%q status=%d bytes=%d duration_ms=%d`, requestID, r.Method, r.URL.Path, status, recorder.bytes, time.Since(started).Milliseconds())
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self' ws: wss:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; img-src 'self' data:; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}
