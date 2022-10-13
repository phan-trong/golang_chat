package main

import (
	"encoding/json"
	"fmt"
	"go_chat_2/config"
	"go_chat_2/models"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	// Max wait time when writing message to peer
	writeWait = 10 * time.Second

	// Max time till next pong from peer
	pongWait = 60 * time.Second

	// Send ping interval, must be less then pong wait time
	pingPeriod = (pongWait * 9) / 10

	// Maximum message size allowed from peer.
	maxMessageSize = 10000
)

var (
	newline = []byte{'*'}
	space   = []byte{' '}
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
}

// Client represents the websocket client at the server
type Client struct {
	// The actual websocket connection
	conn     *websocket.Conn
	wsServer *WsServer
	send     chan []byte
	rooms    map[*Room]bool
	Name     string    `json:"name"`
	ID       uuid.UUID `json:"id"`
}

func newClient(conn *websocket.Conn, wsServer *WsServer, name string) *Client {
	return &Client{
		ID:       uuid.New(),
		Name:     name,
		conn:     conn,
		wsServer: wsServer,
		send:     make(chan []byte, 256),
		rooms:    make(map[*Room]bool),
	}
}

func (client *Client) handleNewMessage(jsonMessage []byte) {
	var message Message
	if err := json.Unmarshal(jsonMessage, &message); err != nil {
		log.Printf("Error on unmarshal JSON message %s", err)
	}
	fmt.Printf("Message Receving: %v \n", message)
	// Attach the client object as the sender of the message
	message.Sender = client

	switch message.Action {
	case SendMessageAction:
		// The send-message action, this will send message to a specific room now.
		// Which room will depend on the message Target
		roomId := message.Target.GetId()
		// Use the ChatServer method to find the room, and if found, broadcast!
		if room := client.wsServer.findRoomById(roomId); room != nil {
			room.broadcast <- &message
		}
	// We delegate the join and leave actions.
	case JoinRoomAction:
		client.handleJoinRoomMessage(message)
	case LeaveRoomAction:
		client.handleLeaveRoomMessage(message)
	case JoinRoomPrivateAction:
		client.handleJoinRoomPrivateMessage(message)
	}
}

func (client *Client) handleJoinRoomMessage(message Message) {
	roomName := message.Message

	client.joinRoom(roomName, nil)
}

func (client *Client) handleLeaveRoomMessage(message Message) {
	room := client.wsServer.findRoomById(message.Message)
	if room == nil {
		return
	}
	if _, ok := client.rooms[room]; ok {
		delete(client.rooms, room)
	}

	room.unregister <- client
}

func (client *Client) handleJoinRoomPrivateMessage(message Message) {

	target := client.wsServer.findUserByID(message.Message)

	if target == nil {
		return
	}

	// create unique room name combined to the two IDs
	fmt.Println(message.Message)
	roomName := message.Message + client.ID.String()

	// Join room
	joinedRoom := client.joinRoom(roomName, target)

	// Instead of instantaneously joining the target client.
	// Let the target client join with a invite request over pub/sub
	if joinedRoom != nil {
		client.inviteTargetUser(target, joinedRoom)
	}
}

// JoinRoom now returns a room or nil
func (client *Client) joinRoom(roomName string, sender models.User) *Room {

	room := client.wsServer.findRoomByName(roomName)
	if room == nil {
		room = client.wsServer.createRoom(roomName, sender != nil)
	}

	// Don't allow to join private rooms through public room message
	if sender == nil && room.Private {
		return nil
	}

	if !client.isInRoom(room) {
		client.rooms[room] = true
		room.register <- client
		client.notifyRoomJoined(room, sender)
	}
	return room
}

// Send out invite message over pub/sub in the general channel.
func (client *Client) inviteTargetUser(target models.User, room *Room) {
	inviteMessage := &Message{
		Action:  JoinRoomPrivateAction,
		Message: target.GetId(),
		Target:  room,
		Sender:  client,
	}

	if err := config.Redis.Publish(ctx, PubSubGeneralChannel, inviteMessage.encode()).Err(); err != nil {
		log.Println(err)
	}
}

// New Method
// Check if the client is not yet in the room
func (client *Client) isInRoom(room *Room) bool {
	if _, ok := client.rooms[room]; ok {
		return true
	}
	return false
}

// New Method
// Notify the client of the new room he/she joined
func (client *Client) notifyRoomJoined(room *Room, sender models.User) {
	message := &Message{
		Action: RoomJoinedAction,
		Target: room,
		Sender: sender,
	}

	client.send <- message.encode()
}

func (client *Client) readPump() {
	defer func() {
		client.disconnect()
	}()

	client.conn.SetReadLimit(maxMessageSize)
	client.conn.SetReadDeadline(time.Now().Add(pongWait))
	client.conn.SetPongHandler(func(string) error {
		client.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	// Start endless read loop, waiting for message from client
	for {
		_, jsonMessage, err := client.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("unexpected close error: %v", err)
			}
			break
		}
		// client.wsServer.broadcast <- jsonMessage
		client.handleNewMessage(jsonMessage)
	}

}

func (client *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		client.conn.Close()
	}()
	for {
		select {
		case message, ok := <-client.send:
			client.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				client.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := client.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)

			// Attach queued chat messages to the current websocket message.
			n := len(client.send)
			for i := 0; i < n; i++ {
				w.Write(newline)
				w.Write(<-client.send)
			}

			if err := w.Close(); err != nil {
				return
			}
		case <-ticker.C:
			client.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := client.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (client *Client) disconnect() {
	client.wsServer.unregister <- client
	for room := range client.rooms {
		room.unregister <- client
	}
	close(client.send)
	client.conn.Close()
}

// ServerWs handles websocket request from clients requests.
func ServerWs(wsServer *WsServer, w http.ResponseWriter, r *http.Request) {

	name, ok := r.URL.Query()["name"]

	if !ok || len(name[0]) < 1 {
		log.Println("Url Param 'name' is missing")
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}

	client := newClient(conn, wsServer, name[0])

	go client.writePump()
	go client.readPump()

	wsServer.register <- client
}

func (client *Client) GetId() string {
	return client.ID.String()
}

func (client *Client) GetName() string {
	return client.Name
}
