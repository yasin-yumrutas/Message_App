package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	writeWait       = 10 * time.Second
	pongWait        = 60 * time.Second
	pingPeriod      = pongWait * 9 / 10
	maxMessageBytes = 1024
)

var safeIdentifier = regexp.MustCompile(`^[a-zA-Z0-9_-]{2,32}$`)

type Message struct {
	ID           string `json:"id"`
	Room         string `json:"room"`
	Text         string `json:"text,omitempty"`
	Type         string `json:"type"`
	ClientID     string `json:"client_id,omitempty"`
	ClientName   string `json:"client_name,omitempty"`
	SentAt       string `json:"sent_at"`
	Participants int    `json:"participants,omitempty"`
}

type Client struct {
	id   string
	name string
	room *Room
	conn *websocket.Conn
	send chan Message
}

type Room struct {
	id         string
	clients    map[*Client]struct{}
	register   chan *Client
	unregister chan *Client
	broadcast  chan Message
}

func newRoom(id string) *Room {
	room := &Room{
		id:         id,
		clients:    make(map[*Client]struct{}),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		broadcast:  make(chan Message, 64),
	}
	go room.run()
	return room
}

func (r *Room) run() {
	for {
		select {
		case client := <-r.register:
			r.clients[client] = struct{}{}
			r.publishPresence("join", client)
		case client := <-r.unregister:
			if _, exists := r.clients[client]; !exists {
				continue
			}
			delete(r.clients, client)
			close(client.send)
			r.publishPresence("leave", client)
		case message := <-r.broadcast:
			for client := range r.clients {
				select {
				case client.send <- message:
				default:
					delete(r.clients, client)
					close(client.send)
				}
			}
		}
	}
}

func (r *Room) publishPresence(event string, client *Client) {
	message := Message{
		ID:           newMessageID(),
		Room:         r.id,
		Type:         "presence." + event,
		ClientID:     client.id,
		ClientName:   client.name,
		SentAt:       time.Now().UTC().Format(time.RFC3339Nano),
		Participants: len(r.clients),
	}
	for peer := range r.clients {
		select {
		case peer.send <- message:
		default:
			delete(r.clients, peer)
			close(peer.send)
		}
	}
}

type Hub struct {
	mu    sync.Mutex
	rooms map[string]*Room
}

func newHub() *Hub {
	return &Hub{rooms: make(map[string]*Room)}
}

func (h *Hub) room(id string) *Room {
	h.mu.Lock()
	defer h.mu.Unlock()
	if room, exists := h.rooms[id]; exists {
		return room
	}
	room := newRoom(id)
	h.rooms[id] = room
	return room
}

type Server struct {
	hub            *Hub
	indexPath      string
	allowedOrigins map[string]struct{}
}

func newServer(indexPath string, origins string) *Server {
	allowed := make(map[string]struct{})
	for _, origin := range strings.Split(origins, ",") {
		origin = strings.TrimSpace(strings.TrimSuffix(origin, "/"))
		if origin != "" {
			allowed[origin] = struct{}{}
		}
	}
	return &Server{hub: newHub(), indexPath: indexPath, allowedOrigins: allowed}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.serveHome)
	mux.HandleFunc("/healthz", s.serveHealth)
	mux.HandleFunc("/ws", s.serveWebSocket)
	return securityHeaders(mux)
}

func (s *Server) serveHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	http.ServeFile(w, r, s.indexPath)
}

func (s *Server) serveHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) serveWebSocket(w http.ResponseWriter, r *http.Request) {
	roomID := r.URL.Query().Get("room")
	clientID := r.URL.Query().Get("id")
	clientName := strings.TrimSpace(r.URL.Query().Get("name"))
	if !safeIdentifier.MatchString(roomID) || !safeIdentifier.MatchString(clientID) || len([]rune(clientName)) < 2 || len([]rune(clientName)) > 40 {
		http.Error(w, "invalid room or client", http.StatusBadRequest)
		return
	}

	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     s.checkOrigin,
	}
	connection, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	client := &Client{
		id: clientID, name: clientName, room: s.hub.room(roomID),
		conn: connection, send: make(chan Message, 64),
	}
	client.room.register <- client
	go client.writePump()
	go client.readPump()
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

func (c *Client) readPump() {
	defer func() {
		c.room.unregister <- c
		_ = c.conn.Close()
	}()
	c.conn.SetReadLimit(maxMessageBytes)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	for {
		_, payload, err := c.conn.ReadMessage()
		if err != nil {
			if !errors.Is(err, websocket.ErrCloseSent) && websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("websocket read: %v", err)
			}
			return
		}
		text := strings.TrimSpace(string(payload))
		if text == "" || len([]rune(text)) > 500 {
			continue
		}
		c.room.broadcast <- Message{
			ID: newMessageID(), Room: c.room.id, Text: text, Type: "message",
			ClientID: c.id, ClientName: c.name, SentAt: time.Now().UTC().Format(time.RFC3339Nano),
		}
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
	}()
	for {
		select {
		case message, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteJSON(message); err != nil {
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

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self' ws: wss:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; img-src 'self' data:")
		next.ServeHTTP(w, r)
	})
}

func newMessageID() string {
	return fmt.Sprintf("msg_%d", time.Now().UnixNano())
}

func main() {
	defaultPort := os.Getenv("PORT")
	if defaultPort == "" {
		defaultPort = "8080"
	}
	address := flag.String("addr", ":"+defaultPort, "HTTP listen address")
	flag.Parse()

	server := newServer("index.html", os.Getenv("ALLOWED_ORIGINS"))
	log.Printf("Realtime Messaging Engine listening on %s", *address)
	if err := http.ListenAndServe(*address, server.routes()); err != nil {
		log.Fatal(err)
	}
}
