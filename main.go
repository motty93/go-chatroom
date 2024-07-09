package main

import (
	"database/sql"
	"embed"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"

	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"

	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
)

//go:embed templates/*
var resources embed.FS

var (
	db       *sql.DB
	upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}
)

type Room struct {
	id      string
	clients map[*websocket.Conn]bool
}

type WebSocketServer struct {
	rooms map[string]*Room
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

func (s *WebSocketServer) healthCheck(w http.ResponseWriter, r *http.Request) {
	err := db.Ping()
	if err != nil {
		http.Error(w, "Database connection error.", http.StatusInternalServerError)
	}

	fmt.Println("healthy!!!!!!!!!!!")

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (s *WebSocketServer) createRoom(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	roomID := vars["roomID"]

	if db == nil {
		http.Error(w, "Database not configured", http.StatusInternalServerError)
		return
	}

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

	fmt.Fprintf(w, "Room %s created", roomID)
}

func (s *WebSocketServer) closeRoom(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	roomID := vars["roomID"]

	if db == nil {
		http.Error(w, "Database not configured", http.StatusInternalServerError)
		return
	}

	stmt, err := db.Prepare("UPDATE rooms SET is_closed = TRUE WHERE id = $1")
	if err != nil {
		http.Error(w, "Error preparing SQL statement", http.StatusInternalServerError)
		return
	}
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
	err = t.ExecuteTemplate(w, "index.html.tmpl", data)
	if err != nil {
		http.Error(w, "Template parse error", http.StatusNotFound)
	}
}

func (s *WebSocketServer) healthCheckTemplate(w http.ResponseWriter, r *http.Request) {
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

func main() {
	s := newWebSocketServer()

	r := mux.NewRouter()
	r.HandleFunc("/health", s.healthCheck)
	r.HandleFunc("/health/{id}", s.healthCheckTemplate)
	r.HandleFunc("/", s.healthCheck)
	r.HandleFunc("/close-room/{roomID}", s.closeRoom).Methods("POST")
	r.HandleFunc("/create-room/{roomID}", s.closeRoom).Methods("POST")
	r.HandleFunc("/rooms/{roomID}", s.joinRoom)
	r.HandleFunc("/ws/{roomID}", s.handleConnections)

	origins := []string{"*"}

	fmt.Println("Server started on :8080")
	err := http.ListenAndServe(":8080", allowOrigins(origins)(r))
	if err != nil {
		fmt.Println("Failed to start server:", err)
	}
}
