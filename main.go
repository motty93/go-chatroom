package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"

	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"golang.org/x/oauth2"

	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
)

//go:embed templates/*
var resources embed.FS

var (
	db       *sql.DB
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}
	oauthConfig      *oauth2.Config
	oauthStateString = "pseudo-random"
)

type User struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	VerifiedEmail bool   `json:"verified_email"`
	Picture       string `json:"picture"`
}

type Room struct {
	id      string
	clients map[*websocket.Conn]bool
}

type WebSocketServer struct {
	rooms map[string]*Room
}

type RoomResponse struct {
	RoomID  string `json:"roomID"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

type RoomRequestData struct {
	RoomId string `json:"room_id"`
}

func init() {
	var err error

	err = godotenv.Load()
	if err != nil {
		fmt.Println("Error loading .env file")
	}

	dbUrl := os.Getenv("DB_URL")
	db, err = sql.Open("postgres", dbUrl)
	if err != nil {
		log.Fatal(err)
	}

	_, err = db.Exec("CREATE TABLE IF NOT EXISTS rooms (id TEXT PRIMARY KEY)")
	if err != nil {
		log.Fatal(err)
	}
}

func newWebSocketServer() *WebSocketServer {
	return &WebSocketServer{
		rooms: make(map[string]*Room),
	}
}

func allowOrigins(origins []string) func(http.Handler) http.Handler {
	return handlers.CORS(
		handlers.AllowedOrigins(origins),
		handlers.AllowedMethods([]string{"POST", "GET", "OPTIONS"}),
		handlers.AllowedHeaders([]string{"Content-Type"}),
	)
}

func healthCheck(w http.ResponseWriter, r *http.Request) {
	err := db.Ping()
	if err != nil {
		http.Error(w, "Database connection error.", http.StatusInternalServerError)
	}

	fmt.Println("healthy!!!!!!!!!!!")

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (s *WebSocketServer) findOrCreateRoom(w http.ResponseWriter, r *http.Request) {
	var reqData RoomRequestData

	err := json.NewDecoder(r.Body).Decode(&reqData)
	if err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if db == nil {
		http.Error(w, "Database not configured", http.StatusInternalServerError)
		return
	}

	var exists bool
	roomID := reqData.RoomId
	err = db.QueryRow("SELECT EXISTS(SELECT 1 FROM rooms WHERE id=$1)", roomID).Scan(&exists)
	if err != nil {
		http.Error(w, "Room does not exist", http.StatusNotFound)
		return
	}

	var res RoomResponse
	if exists {
		res = RoomResponse{
			RoomID:  roomID,
			Status:  "success",
			Message: "ご指定のroomIDは既に存在します。",
		}
	} else {
		stmt, err := db.Prepare("INSERT INTO rooms(id) VALUES($1)")
		if err != nil {
			http.Error(w, "Error preparing SQL statement", http.StatusInternalServerError)
			return
		}

		_, err = stmt.Exec(roomID)
		if err != nil {
			http.Error(w, "Error executing SQL statement", http.StatusInternalServerError)
			return
		}

		res = RoomResponse{
			RoomID:  roomID,
			Status:  "success",
			Message: "新しいroomを作成しました。",
		}
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusOK)

	json.NewEncoder(w).Encode(res)
}

func (s *WebSocketServer) findRoom(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	roomID := vars["roomID"]

	if db == nil {
		http.Error(w, "Database not configured", http.StatusInternalServerError)
		return
	}

	var exists bool
	err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM rooms WHERE id=$1)", roomID).Scan(&exists)
	if err != nil {
		http.Error(w, "Room does not exist", http.StatusNotFound)
		return
	}

	var res RoomResponse
	if exists {
		res = RoomResponse{
			RoomID:  roomID,
			Status:  "success",
			Message: "Roomが見つかりました！",
		}
	} else {
		res = RoomResponse{
			RoomID:  roomID,
			Status:  "failed",
			Message: "Roomは存在しません。",
		}
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusOK)

	json.NewEncoder(w).Encode(res)
}

func (s *WebSocketServer) closeRoom(w http.ResponseWriter, r *http.Request) {
	var reqData RoomRequestData

	err := json.NewDecoder(r.Body).Decode(&reqData)
	if err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if db == nil {
		http.Error(w, "Database not configured", http.StatusInternalServerError)
		return
	}

	stmt, err := db.Prepare("UPDATE rooms SET is_closed = TRUE WHERE id = $1")
	if err != nil {
		http.Error(w, "Error preparing SQL statement", http.StatusInternalServerError)
		return
	}
	roomID := reqData.RoomId
	_, err = stmt.Exec(roomID)
	if err != nil {
		http.Error(w, "Error executing SQL statement", http.StatusInternalServerError)
		return
	}

	fmt.Fprintf(w, "Room %s closed", roomID)
}

func (s *WebSocketServer) handleConnections(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	roomID := vars["roomID"]

	if db == nil {
		http.Error(w, "Database not configured", http.StatusInternalServerError)
		return
	}

	var exists bool
	err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM rooms WHERE id=$1)", roomID).Scan(&exists)
	if err != nil || !exists {
		http.Error(w, "Room does not exist", http.StatusNotFound)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	defer conn.Close()

	room, ok := s.rooms[roomID]
	if !ok {
		room = &Room{
			id:      roomID,
			clients: make(map[*websocket.Conn]bool),
		}
		s.rooms[roomID] = room
	}

	room.clients[conn] = true

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			log.Println(err)
			delete(room.clients, conn)
			break
		}

		fmt.Printf("message: %s\n", message)
		for client := range room.clients {
			if err := client.WriteMessage(websocket.TextMessage, message); err != nil {
				log.Println(err)
				client.Close()
				delete(room.clients, client)
			}
		}
	}
}

func (s *WebSocketServer) joinRoom(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	roomID := vars["roomID"]

	if db == nil {
		http.Error(w, "Database not configured", http.StatusInternalServerError)
		return
	}

	var isClosed bool
	err := db.QueryRow("SELECT is_closed FROM rooms WHERE id=$1", roomID).Scan(&isClosed)
	if err != nil {
		http.Error(w, "Room does not exist", http.StatusNotFound)
		return
	}

	if isClosed {
		http.Error(w, "Room is closed", http.StatusForbidden)
		return
	}

	var exists bool
	err = db.QueryRow("SELECT EXISTS(SELECT 1 FROM rooms WHERE id=$1)", roomID).Scan(&exists)
	if err != nil || !exists {
		http.Error(w, "Room does not exist", http.StatusNotFound)
		return
	}

	data := map[string]string{
		"Title":  "チャットルーム",
		"RoomID": roomID,
	}
	t := template.Must(template.ParseFS(resources, "templates/*"))
	err = t.ExecuteTemplate(w, "rooms.html.tmpl", data)
	if err != nil {
		http.Error(w, "Template parse error", http.StatusNotFound)
	}
}

func healthCheckTemplate(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id := vars["id"]
	data := map[string]string{
		"Title": "HealthCheck",
		"id":    id,
	}
	t := template.Must(template.ParseFS(resources, "templates/*"))

	err := t.ExecuteTemplate(w, "health.html.tmpl", data)
	if err != nil {
		http.Error(w, "Template parse error", http.StatusNotFound)
	}
}

func index(w http.ResponseWriter, r *http.Request) {
	t := template.Must(template.ParseFS(resources, "templates/index.html.tmpl"))

	t.Execute(w, nil)
}

func handleGoogleLogin(w http.ResponseWriter, r *http.Request) {
	url := oauthConfig.AuthCodeURL(oauthStateString)
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func handleGoogleCallBack(w http.ResponseWriter, r *http.Request) {
	content, err := getUserInfo(r.FormValue("state"), r.FormValue("code"))
	if err != nil {
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		return
	}

	fmt.Fprintf(w, "UserInfo: %s\n", content)
}

func getUserInfo(state string, code string) ([]byte, error) {
	if state != oauthStateString {
		return nil, fmt.Errorf("invalid oauth state")
	}

	ctx := context.Background()
	token, err := oauthConfig.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("code exchange failed: %s", err.Error())
	}

	res, err := http.Get("https://www.googleapis.com/oauth2/v2/userinfo?access_token=" + token.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("failed getting user info: %s", err.Error())
	}
	defer res.Body.Close()

	var user User
	if json.NewDecoder(res.Body).Decode(&user); err != nil {
		return nil, err
	}

	return json.Marshal(user)
}

func main() {
	s := newWebSocketServer()

	r := mux.NewRouter()
	r.HandleFunc("/", index)
	r.HandleFunc("/login", handleGoogleLogin)
	r.HandleFunc("/callback", handleGoogleCallBack)
	r.HandleFunc("/health", healthCheck)
	r.HandleFunc("/health/{id}", healthCheckTemplate)
	r.HandleFunc("/close-room", s.closeRoom).Methods("POST")
	r.HandleFunc("/rooms", s.findOrCreateRoom).Methods("POST")
	r.HandleFunc("/rooms/{roomID}", s.joinRoom)
	r.HandleFunc("/room/{roomID}", s.findRoom)
	r.HandleFunc("/ws/{roomID}", s.handleConnections)

	origins := []string{"*"}

	fmt.Println("Server started on :8080")
	err := http.ListenAndServe(":8080", allowOrigins(origins)(r))
	if err != nil {
		fmt.Println("Failed to start server:", err)
	}
}
