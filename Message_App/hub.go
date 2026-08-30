package main

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

var (
	errRoomFull   = errors.New("room is full")
	errRoomClosed = errors.New("room is closed")
	errRoomLimit  = errors.New("room limit reached")
)

type RoomInfo struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Visibility   string `json:"visibility"`
	Participants int    `json:"participants"`
	Capacity     int    `json:"capacity"`
	CreatedAt    string `json:"created_at"`
}

type Client struct {
	id         string
	name       string
	room       *Room
	conn       *websocket.Conn
	send       chan Event
	maxMessage int
	stats      *Stats
}

func (c *Client) member() Member { return Member{ID: c.id, Name: c.name} }

type registerRequest struct {
	client *Client
	result chan error
}
type roomMessage struct {
	client *Client
	text   string
}

type Room struct {
	id           string
	name         string
	private      bool
	accessHash   [32]byte
	createdAt    time.Time
	maxClients   int
	clients      map[*Client]struct{}
	register     chan registerRequest
	unregister   chan *Client
	broadcast    chan roomMessage
	stop         chan struct{}
	done         chan struct{}
	participants atomic.Int64
	lastActive   atomic.Int64
	sequence     uint64
	stats        *Stats
}

func newRoom(id, name string, private bool, accessHash [32]byte, maxClients int, stats *Stats) *Room {
	room := &Room{
		id: id, name: name, private: private, accessHash: accessHash,
		createdAt: time.Now().UTC(), maxClients: maxClients,
		clients: make(map[*Client]struct{}), register: make(chan registerRequest),
		unregister: make(chan *Client), broadcast: make(chan roomMessage, 128),
		stop: make(chan struct{}), done: make(chan struct{}), stats: stats,
	}
	room.touch()
	go room.run()
	return room
}

func (r *Room) run() {
	defer close(r.done)
	for {
		select {
		case request := <-r.register:
			if len(r.clients) >= r.maxClients {
				request.result <- errRoomFull
				continue
			}
			r.clients[request.client] = struct{}{}
			r.participants.Store(int64(len(r.clients)))
			r.stats.connections.Add(1)
			r.touch()
			r.publish("presence.join", request.client.member(), "")
			request.result <- nil
		case client := <-r.unregister:
			if _, exists := r.clients[client]; !exists {
				continue
			}
			delete(r.clients, client)
			close(client.send)
			r.participants.Store(int64(len(r.clients)))
			r.stats.connections.Add(-1)
			r.touch()
			r.publish("presence.leave", client.member(), "")
		case message := <-r.broadcast:
			if _, exists := r.clients[message.client]; !exists {
				continue
			}
			r.stats.messages.Add(1)
			r.touch()
			r.publish("message.created", message.client.member(), message.text)
		case <-r.stop:
			for client := range r.clients {
				close(client.send)
				_ = client.conn.Close()
				r.stats.connections.Add(-1)
			}
			return
		}
	}
}

func (r *Room) publish(eventType string, member Member, text string) {
	r.sequence++
	event := Event{
		ID: "evt_" + mustRandomToken(12), Type: eventType, RoomID: r.id,
		Sequence: r.sequence, Text: text, Sender: &member, SentAt: nowRFC3339(),
		Participants: len(r.clients), Members: r.members(),
	}
	for client := range r.clients {
		select {
		case client.send <- event:
		default:
			delete(r.clients, client)
			close(client.send)
			r.stats.connections.Add(-1)
			r.stats.slowConsumers.Add(1)
		}
	}
	r.participants.Store(int64(len(r.clients)))
}

func (r *Room) members() []Member {
	members := make([]Member, 0, len(r.clients))
	for client := range r.clients {
		members = append(members, client.member())
	}
	return members
}

func (r *Room) registerClient(client *Client) error {
	result := make(chan error, 1)
	select {
	case r.register <- registerRequest{client: client, result: result}:
	case <-r.done:
		return errRoomClosed
	}
	return <-result
}

func (r *Room) unregisterClient(client *Client) {
	select {
	case r.unregister <- client:
	case <-r.done:
	}
}

func (r *Room) sendMessage(client *Client, text string) bool {
	select {
	case r.broadcast <- roomMessage{client: client, text: text}:
		return true
	case <-r.done:
		return false
	}
}

func (r *Room) checkAccess(accessKey string) bool {
	return !r.private || secretsMatch(r.accessHash, accessKey)
}

func (r *Room) info() RoomInfo {
	visibility := "public"
	if r.private {
		visibility = "private"
	}
	return RoomInfo{ID: r.id, Name: r.name, Visibility: visibility, Participants: int(r.participants.Load()), Capacity: r.maxClients, CreatedAt: r.createdAt.Format(time.RFC3339)}
}

func (r *Room) touch() { r.lastActive.Store(time.Now().UnixNano()) }
func (r *Room) idleFor(now time.Time) time.Duration {
	return now.Sub(time.Unix(0, r.lastActive.Load()))
}

type Hub struct {
	mu         sync.RWMutex
	rooms      map[string]*Room
	maxRooms   int
	roomTTL    time.Duration
	maxClients int
	stats      *Stats
	stop       chan struct{}
	done       chan struct{}
}

func newHub(maxRooms, maxClients int, roomTTL time.Duration, stats *Stats) *Hub {
	hub := &Hub{rooms: make(map[string]*Room), maxRooms: maxRooms, maxClients: maxClients, roomTTL: roomTTL, stats: stats, stop: make(chan struct{}), done: make(chan struct{})}
	go hub.janitor()
	return hub
}

func (h *Hub) create(name string, private bool) (*Room, string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.rooms) >= h.maxRooms {
		return nil, "", errRoomLimit
	}
	var id string
	for {
		id = "rm_" + mustRandomToken(9)
		if _, exists := h.rooms[id]; !exists {
			break
		}
	}
	var accessKey string
	var accessHash [32]byte
	if private {
		accessKey = mustRandomToken(24)
		accessHash = hashSecret(accessKey)
	}
	room := newRoom(id, name, private, accessHash, h.maxClients, h.stats)
	h.rooms[id] = room
	h.stats.rooms.Store(int64(len(h.rooms)))
	return room, accessKey, nil
}

func (h *Hub) get(id string) (*Room, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	room, exists := h.rooms[id]
	return room, exists
}

func (h *Hub) janitor() {
	interval := h.roomTTL / 4
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer func() { ticker.Stop(); close(h.done) }()
	for {
		select {
		case now := <-ticker.C:
			h.removeExpired(now)
		case <-h.stop:
			h.mu.Lock()
			for id, room := range h.rooms {
				close(room.stop)
				delete(h.rooms, id)
			}
			h.stats.rooms.Store(0)
			h.mu.Unlock()
			return
		}
	}
}

func (h *Hub) removeExpired(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, room := range h.rooms {
		if room.participants.Load() == 0 && room.idleFor(now) >= h.roomTTL {
			close(room.stop)
			delete(h.rooms, id)
		}
	}
	h.stats.rooms.Store(int64(len(h.rooms)))
}

func (h *Hub) close() { close(h.stop); <-h.done }

type Stats struct {
	rooms         atomic.Int64
	connections   atomic.Int64
	messages      atomic.Uint64
	rejected      atomic.Uint64
	slowConsumers atomic.Uint64
	ticketsIssued atomic.Uint64
}
