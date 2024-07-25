package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/gorilla/sessions"
	"github.com/gorilla/websocket"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
)

//go:embed templates/*
var resources embed.FS

var (
	db               *sql.DB
	oauthConfig      *oauth2.Config
	oauthStateString string
	store            *sessions.CookieStore

	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}
	allowedDomains = []string{"androots.co.jp"}
)

type User struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	VerifiedEmail bool   `json:"verified_email"`
	Picture       string `json:"picture"`
}

type Room struct {
	ID       string `json:"id"`
	IsClosed bool   `json:"is_closed"`
}

type WebSocketRoom struct {
	id      string
	clients map[*websocket.Conn]bool
}

type WebSocketServer struct {
	rooms map[string]*WebSocketRoom
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

	// Register User struct for session
	gob.Register(User{})

	// OAuth2 configuration
	oauthConfig = &oauth2.Config{
		RedirectURL:  os.Getenv("API_URL") + "/callback",
		ClientID:     os.Getenv("GOOGLE_OAUTH_CLIENT_ID"),
		ClientSecret: os.Getenv("GOOGLE_OAUTH_CLIENT_SECRET"),
		Scopes:       []string{"https://www.googleapis.com/auth/userinfo.email", "https://www.googleapis.com/auth/userinfo.profile"},
		Endpoint:     google.Endpoint,
	}
	store = sessions.NewCookieStore([]byte(generateRandomKey()))
	store.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   86400 * 7,
		HttpOnly: true,
	}
}

func newWebSocketServer() *WebSocketServer {
	return &WebSocketServer{
		rooms: make(map[string]*WebSocketRoom),
	}
}

func generateRandomKey() string {
	key := make([]byte, 32) // 32 bytes = 256 bits
	if _, err := rand.Read(key); err != nil {
		log.Fatalf("Failed to generate random key: %v", err)
	}

	return base64.StdEncoding.EncodeToString(key)
}

func isAllowedDomain(domain string) bool {
	for _, d := range allowedDomains {
		if d == domain {
			return true
		}
	}
	return false
}

func allowOrigins(origins []string) func(http.Handler) http.Handler {
	return handlers.CORS(
		handlers.AllowedOrigins(origins),
		handlers.AllowedMethods([]string{"POST", "GET", "OPTIONS"}),
		handlers.AllowedHeaders([]string{"Content-Type"}),
	)
}

func isAuthenticated(next http.HandlerFunc) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, _ := store.Get(r, "user-session")
		fmt.Println("session: ", session.Values)
		user, ok := session.Values["user"].(User)
		if !ok || user.Email == "" {
			log.Printf("User not authenticated. Session values: %+v", session.Values)
			http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
			return
		}

		next.ServeHTTP(w, r)
	})
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

func healthCheck(w http.ResponseWriter, r *http.Request) {
	err := db.Ping()
	if err != nil {
		http.Error(w, "Database connection error.", http.StatusInternalServerError)
	}

	fmt.Println("healthy!!!!!!!!!!!")

	session, _ := store.Get(r, "user-session")
	user, ok := session.Values["user"].(User)
	w.WriteHeader(http.StatusOK)
	if ok {
		fmt.Fprintf(w, "welcome %s！", user.Email)
	} else {
		w.Write([]byte("ok"))
	}
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
		room = &WebSocketRoom{
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
	err = t.ExecuteTemplate(w, "room.html.tmpl", data)
	if err != nil {
		http.Error(w, "Template parse error", http.StatusNotFound)
	}
}

func (s *WebSocketServer) findAllRooms(w http.ResponseWriter, r *http.Request) {
	if db == nil {
		http.Error(w, "Database not configured", http.StatusInternalServerError)
		return
	}

	query := "SELECT * FROM rooms WHERE is_closed = FALSE LIMIT 10"
	rows, err := db.Query(query)
	if err != nil {
		http.Error(w, "Error querying rooms", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var rooms []Room
	for rows.Next() {
		var room Room
		err := rows.Scan(&room.ID, &room.IsClosed)
		if err != nil {
			http.Error(w, "Error scanning rooms", http.StatusInternalServerError)
			return
		}
		rooms = append(rooms, room)
	}

	if err := rows.Err(); err != nil {
		http.Error(w, "Error iterating rooms", http.StatusInternalServerError)
		return
	}

	t := template.Must(template.ParseFS(resources, "templates/*"))
	err = t.ExecuteTemplate(w, "rooms.html.tmpl", rooms)

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

func handleIndex(w http.ResponseWriter, r *http.Request) {
	session, _ := store.Get(r, "user-session")
	// ログイン情報があればcallbackと同じHTMLのroomsを表示
	if user, ok := session.Values["user"].(User); ok && user.Email != "" {
		fmt.Println("User already authenticated: ", user)
		t := template.Must(template.ParseFS(resources, "templates/*"))
		if err := t.ExecuteTemplate(w, "rooms.html.tmpl", user); err != nil {
			http.Error(w, "Template parse error", http.StatusNotFound)
		}
		return
	}

	t := template.Must(template.ParseFS(resources, "templates/index.html.tmpl"))
	t.Execute(w, nil)
}

func handleGoogleLogin(w http.ResponseWriter, r *http.Request) {
	// generate a new state string
	oauthStateString = uuid.New().String()

	// Store the state string in the session or a temporary storage
	session, _ := store.Get(r, "user-session")
	session.Values["oauthState"] = oauthStateString
	session.Save(r, w)

	// User the state string in the AuthCodeURL
	url := oauthConfig.AuthCodeURL(oauthStateString)
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func handleGoogleCallBack(w http.ResponseWriter, r *http.Request) {
	session, _ := store.Get(r, "user-session")
	// ログイン情報があればcallbackと同じHTMLのroomsを表示
	if user, ok := session.Values["user"].(User); ok && user.Email != "" {
		fmt.Println("User already authenticated: ", user)
		t := template.Must(template.ParseFS(resources, "templates/*"))
		if err := t.ExecuteTemplate(w, "rooms.html.tmpl", user); err != nil {
			http.Error(w, "Template parse error", http.StatusNotFound)
		}

		return
	}

	content, err := getUserInfo(r.FormValue("state"), r.FormValue("code"))
	if err != nil {
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		return
	}

	// Parse user info
	var user User
	if err := json.Unmarshal(content, &user); err != nil {
		log.Printf("Failed to parse user info: %v", err)
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		return
	}
	fmt.Println("callback getUser: ", user)

	// check androots domain
	emailDomain := strings.Split(user.Email, "@")[1]
	if !isAllowedDomain(emailDomain) {
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		return
	}

	// Store user info in session
	session.Values["user"] = user
	if err := session.Save(r, w); err != nil {
		log.Printf("Failed to save session: %v", err)
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		return
	}

	t := template.Must(template.ParseFS(resources, "templates/*"))
	if err := t.ExecuteTemplate(w, "callback.html.tmpl", user); err != nil {
		http.Error(w, "Template parse error", http.StatusNotFound)
	}
}

func handleProtected(w http.ResponseWriter, r *http.Request) {
	session, err := store.Get(r, "user-session")
	if err != nil {
		http.Error(w, "Session was deprecated.", http.StatusForbidden)
		return
	}

	user := session.Values["user"].(User)
	fmt.Fprintf(w, "welcome %s！", user.Email)
}

func main() {
	s := newWebSocketServer()

	r := mux.NewRouter()
	r.HandleFunc("/", handleIndex)
	r.HandleFunc("/api/oauth/login", handleGoogleLogin)
	r.HandleFunc("/callback", handleGoogleCallBack)
	// health check
	r.HandleFunc("/health", healthCheck)
	r.HandleFunc("/health/{id}", healthCheckTemplate)
	// api
	r.HandleFunc("/api/close-room", s.closeRoom).Methods("POST")
	r.HandleFunc("/api/open-room", s.findOrCreateRoom).Methods("POST")
	// Protected routes
	r.HandleFunc("/protected", isAuthenticated(handleProtected))
	r.HandleFunc("/rooms", isAuthenticated(s.findAllRooms))
	r.HandleFunc("/rooms/{roomID}", isAuthenticated(s.joinRoom))
	r.HandleFunc("/room/{roomID}", isAuthenticated(s.findRoom))
	r.HandleFunc("/ws/{roomID}", isAuthenticated(s.handleConnections))

	origins := []string{"*"}

	fmt.Println("Server started on :8080")
	err := http.ListenAndServe(":8080", allowOrigins(origins)(r))
	if err != nil {
		fmt.Println("Failed to start server:", err)
	}
}
