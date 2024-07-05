package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
)

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

type WebsocketServer struct {
	rooms map[string]*Room
}

func init() {
	var err error

	err = godotenv.Load()
	if err != nil {
		log.Fatalf("Error loading .env file")
	}

	// dbUser := os.Getenv("DB_USER")
	// dbName := os.Getenv("DB_NAME")
	// dbPassword := os.Getenv("DB_PASSWORD")
	// dbHost := os.Getenv("DB_HOST")
	// dbPort := os.Getenv("DB_PORT")
	// connStr := fmt.Sprintf("user=%s dbname=%s password=%s host=%s port=%s sslmode=disable", dbUser, dbName, dbPassword, dbHost, dbPort)

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

func newWebSocketServer() *WebsocketServer {
	return &WebsocketServer{
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

func (s *WebsocketServer) healthCheck(w http.ResponseWriter, r *http.Request) {
	err := db.Ping()
	if err != nil {
		http.Error(w, "Database connection error.", http.StatusInternalServerError)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func main() {
	s := newWebSocketServer()

	r := mux.NewRouter()
	r.HandleFunc("/health", s.healthCheck)
	r.HandleFunc("/", s.healthCheck)

	origins := []string{"*"}

	fmt.Println("Server started on :8080")
	log.Fatal(http.ListenAndServe(":8080", allowOrigins(origins)(r)))
}
