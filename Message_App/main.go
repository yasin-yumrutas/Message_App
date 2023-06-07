package main

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

// Hub maintains the set of active clients and broadcasts messages to the
// clients.
type Room struct {
	id int32

	// Registered clients in current room
	clients map[*Client]bool

	// Inbound messages from the clients in room
	broadcast chan *Message

	// Register requests from the clients.
	register chan *Client

	// Unregister requests from clients.
	unregister chan *Client
}

// sample json message to be sent over the wire
type Message struct {
	Message  string `json:"message,omitempty"`
	Type     string `json:"type,omitempty"`
	ClientID string `json:"client_id,omitempty"`
}

// type ChatServer struct {
// 	rooms []*Room
// 	// Register requests from the clients.
// 	register chan *Room

// 	// Unregister requests from clients.
// 	unregister chan *Room
// }

func newRoom() *Room {

	// send the rand to each call to create a new rome creates a new unique ID

	rand.Seed(time.Now().UnixNano())
	room := &Room{
		id:         rand.Int31(),
		broadcast:  make(chan *Message),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		clients:    make(map[*Client]bool),
	}

	go room.run()
	return room
}

// this function runs an active room on the server
func (r *Room) run() {
	for {
		select {
		// registers a new client to a room
		case client := <-r.register:
			fmt.Println("client registered... room id -", client.room.id)

			r.clients[client] = true

			fmt.Println("clients", len(r.clients))
		case client := <-r.unregister:
			if _, ok := r.clients[client]; ok {
				delete(r.clients, client)
				close(client.send)
			}
			fmt.Println("clients unregistered", len(r.clients))
		case message := <-r.broadcast:
			fmt.Println(message)
			for client := range r.clients {

				client.send <- message

			}
		}
	}
}

const (
	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the peer.
	pongWait = 60 * time.Second

	// Send pings to peer with this period. Must be less than pongWait.
	pingPeriod = (pongWait * 9) / 10

	// Maximum message size allowed from peer.
	maxMessageSize = 512
)

var (
	newline = []byte{'\n'}
	space   = []byte{' '}
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// Client is a middleman between the websocket connection and the hub.
type Client struct {
	id string

	room *Room

	// The websocket connection.
	conn *websocket.Conn

	// Buffered channel of outbound messages.
	send chan *Message
}

// readPump pumps messages from the websocket connection to the hub.
//
// The application runs readPump in a per-connection goroutine. The application
// ensures that there is at most one reader on a connection by executing all
// reads from this goroutine.
func (c *Client) recieveMessages() {
	defer func() {
		c.room.unregister <- c
		c.conn.Close()
	}()
	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))

	c.conn.SetPongHandler(func(gg string) error {
		fmt.Println("pong hit", gg)
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("error: %v", err)
			}
			break
		}
		// this replaces space with new lines
		message = bytes.TrimSpace(bytes.Replace(message, newline, space, -1))
		c.room.broadcast <- &Message{
			Message:  string(message),
			ClientID: c.id,
			Type:     "text",
		}
	}
}

// writePump pumps messages from the hub to the websocket connection.
//
// A goroutine running writePump is started for each connection. The
// application ensures that there is at most one writer to a connection by
// executing all writes from this goroutine.
func (c *Client) sendMessages() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case message, ok := <-c.send:
			// sets a write timeout of 10 seconds
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// The hub closed the channel.
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			err := c.conn.WriteJSON(message)
			if err != nil {
				return
			}

		// this sends a ping to the connect very 54 seconds
		case <-ticker.C:
			fmt.Println("ticker hit")
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// creates a new client websocket client connection
func newClient(id string, room *Room, w http.ResponseWriter, r *http.Request) *Client {

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
	}

	client := &Client{id: id, room: room, conn: conn, send: make(chan *Message, 256)}

	go client.sendMessages()
	go client.recieveMessages()

	return client
}

var addr = flag.String("addr", ":8080", "192.168.1.105")

func serveHome(w http.ResponseWriter, r *http.Request) {
	log.Println(r.URL)
	if r.URL.Path != "/" {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	http.ServeFile(w, r, "index.html")
}

func main() {

	flag.Parse()

	// check which room the client wants to connect too
	room := newRoom()

	http.HandleFunc("/", serveHome)
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {

		// here you would validate the client and set the client ID
		id := r.URL.Query().Get("id")
		// create the client
		client := newClient(id, room, w, r)

		client.room.register <- client

		// room.register <- client

		fmt.Println(client.room)
	})
	err := http.ListenAndServe(*addr, nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}

}
