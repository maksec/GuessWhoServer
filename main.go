package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true }, // В продакшене нужно ограничить
}

// геймплей
type Player struct {
	ID        string          `json:"id,omitempty"`
	Nickname  string          `json:"nickname,omitempty"`
	AvatarIdx int             `json:"avatarIdx,omitempty"`
	IsHost    bool            `json:"isHost,omitempty"`
	Conn      *websocket.Conn `json:"-"`
	SendChan  chan []byte     `json:"-"`
}

type Lobby struct {
	ID      string     `json:"id,omitempty"` // 6 символов
	Players []*Player  `json:"players,omitempty"`
	mu      sync.Mutex `json:"-"`
}

type Payload struct {
	Lobby  *Lobby  `json:"lobby,omitempty"`  // Используем указатель
	Player *Player `json:"player,omitempty"` // Используем указатель
}

// сервер
type Server struct {
	Lobbies map[string]*Lobby  `json:"-"`
	Players map[string]*Player `json:"-"`
	mu      sync.Mutex         `json:"-"`
}

var server = &Server{
	Lobbies: make(map[string]*Lobby),
	Players: make(map[string]*Player),
}

// вебсокет сообщения
type WsMessageType string

const (
	// общие типы
	WsMessageTypeUnknown WsMessageType = "Unknown"
	WsMessageTypeError   WsMessageType = "Error"

	// client -> server types
	WsMessageTypeCreateLobby WsMessageType = "CreateLobby"
	WsMessageTypeJoinLobby   WsMessageType = "JoinLobby"
	WsMessageTypePlayerQuit  WsMessageType = "PlayerQuit"

	// server -> client types
	WsMessageTypeConnected    WsMessageType = "Connected"
	WsMessageTypeLobbyCreated WsMessageType = "LobbyCreated"
	WsMessageTypeLobbyJoined  WsMessageType = "LobbyJoined"
)

type WsMessage struct {
	Type    WsMessageType   `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

func (s *Server) createLobby(player *Player) (*Lobby, error) {
	lobbyID := uuid.New().String()[:6]

	lobby := &Lobby{
		ID:      lobbyID,
		Players: []*Player{player},
	}

	s.mu.Lock()
	s.Lobbies[lobbyID] = lobby
	s.mu.Unlock()

	return lobby, nil
}

func (s *Server) joinLobby(player *Player, lobbyID string) (*Lobby, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	lobby, exists := s.Lobbies[lobbyID]
	if !exists {
		return nil, fmt.Errorf("ERROR: lobby with id %s not found", lobbyID)
	}

	if len(lobby.Players) >= 2 {
		return nil, fmt.Errorf("ERROR: lobby with id %s is already full", lobbyID)
	}

	lobby.mu.Lock()
	lobby.Players = append(lobby.Players, player)
	lobby.mu.Unlock()

	return lobby, nil
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ERROR: can't connect with websocket connection (can't upgrade HTTP), error: %v", err)
		return
	}

	defer conn.Close()

	player := &Player{
		ID:       uuid.New().String(),
		IsHost:   false,
		Conn:     conn,
		SendChan: make(chan []byte, 256),
	}

	server.Players[player.ID] = player

	player.SendChan <- generateConnectedMsg(player)

	go writer(player)

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			log.Printf("ERROR: can't read message (conn.ReadMessage()), error: %v", err)
			break
		}

		var msg WsMessage
		if err := json.Unmarshal(message, &msg); err != nil {
			log.Printf("ERROR: can't parse JSON (json.Unmarshal), error: %v", err)
			continue
		}

		log.Printf("INFO: got message: %v", msg)

		switch msg.Type {
		case WsMessageTypeCreateLobby:
			handleCreateLobby(player, msg.Payload)
		case WsMessageTypeJoinLobby:
			handleJoinLobby(player, msg.Payload)
		case WsMessageTypePlayerQuit:
			handlerPlayerQuit(player, msg.Payload)
		default:
			log.Printf("WARNING: unknown websocket message type: %s", msg.Type)
		}
	}
}

func handleCreateLobby(player *Player, payloadJson json.RawMessage) {
	var payload Payload

	if err := json.Unmarshal(payloadJson, &payload); err != nil {
		log.Println("ERROR: can't unmarshal create lobby msg", err)
		return
	}

	payloadPlayer := payload.Player

	player.IsHost = true
	player.AvatarIdx = payloadPlayer.AvatarIdx
	player.Nickname = payloadPlayer.Nickname

	lobby, err := server.createLobby(player)
	if err != nil {
		log.Printf("ERROR: can't createLobby(), error: %v", err)
	}

	player.SendChan <- generateLobbyCreatedMsg(lobby)
}

func handleJoinLobby(player *Player, payloadJson json.RawMessage) {
	var payload Payload

	if err := json.Unmarshal(payloadJson, &payload); err != nil {
		log.Println("ERROR: can't unmarshal join lobby msg", err)
		return
	}

	payloadPlayer := payload.Player

	player.IsHost = false
	player.AvatarIdx = payloadPlayer.AvatarIdx
	player.Nickname = payloadPlayer.Nickname

	lobby, err := server.joinLobby(player, payload.Lobby.ID)
	if err != nil {
		player.SendChan <- errorResponse(err.Error())
		return
	}

	msg := generateLobbyJoinedMsg(lobby)
	for _, lobbyPlayer := range lobby.Players {
		lobbyPlayer.SendChan <- msg
	}
}

func handlerPlayerQuit(player *Player, _ json.RawMessage) {
	delete(server.Players, player.ID)
}

func writer(player *Player) {
	for message := range player.SendChan {
		err := player.Conn.WriteMessage(websocket.TextMessage, message)
		if err != nil {
			log.Println("Ошибка отправки сообщения:", err)
			break
		}
	}
}

func generateConnectedMsg(player *Player) []byte {
	payload := Payload{
		Player: player,
	}
	payloadJson, err := json.Marshal(payload)
	if err != nil {
		log.Printf("ERROR: failed marshal JSON: generateConnectedMsg, error: %v", err)
	}

	message := WsMessage{
		Type:    WsMessageTypeConnected,
		Payload: payloadJson,
	}

	bytes, err := json.Marshal(message)
	if err != nil {
		log.Printf("ERROR: failed marshal JSON: WsMessage: %v, error: %v", message, err)
	}

	log.Printf("INFO: generated connected msg: %s", bytes)
	return bytes
}

func generateLobbyCreatedMsg(lobby *Lobby) []byte {
	payload := Payload{
		Lobby: lobby,
	}
	payloadJson, err := json.Marshal(payload)
	if err != nil {
		log.Printf("ERROR: failed marshal JSON: payload: %v, error: %v", payload, err)
	}

	message := WsMessage{
		Type:    WsMessageTypeLobbyCreated,
		Payload: payloadJson,
	}

	bytes, err := json.Marshal(message)
	if err != nil {
		log.Printf("ERROR: failed marshal JSON: WsMessage: %v, error: %v", message, err)
	}
	log.Printf("INFO: generated lobby created msg: %s", bytes)
	return bytes
}

func generateLobbyJoinedMsg(lobby *Lobby) []byte {
	payload := Payload{
		Lobby: lobby,
	}
	payloadJson, err := json.Marshal(payload)
	if err != nil {
		log.Printf("ERROR: failed marshal JSON: payload: %v, error: %v", payload, err)
	}

	message := WsMessage{
		Type:    WsMessageTypeLobbyJoined,
		Payload: payloadJson,
	}

	bytes, err := json.Marshal(message)
	if err != nil {
		log.Printf("ERROR: failed marshal JSON: WsMessage: %v, error: %v", message, err)
	}
	log.Printf("INFO: generated lobby joined msg: %s", bytes)
	return bytes
}

func errorResponse(message string) []byte {
	response := struct {
		Type    WsMessageType `json:"type"`
		Message string        `json:"message"`
	}{
		Type:    WsMessageTypeError,
		Message: message,
	}

	bytes, _ := json.Marshal(response)
	return bytes
}

func handlePing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		w.Write([]byte(`{"error": "Метод не поддерживается"}`))
		return
	}

	response := struct {
		OnlinePlayersCount int    `json:"onlinePlayersCount"`
		LobbiesCount       int    `json:"lobbiesCount"`
		Status             string `json:"status"`
	}{
		OnlinePlayersCount: len(server.Players),
		LobbiesCount:       len(server.Lobbies),
		Status:             "alive",
	}

	json.NewEncoder(w).Encode(response)
}

func main() {
	http.HandleFunc("/ping", handlePing)
	http.HandleFunc("/ws", handleWebSocket)

	log.Println("Сервер запущен на :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
