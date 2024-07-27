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

type OAuthUser struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	VerifiedEmail bool   `json:"verified_email"`
	Picture       string `json:"picture"`
}

type UserDTO struct {
	Name          string `json:"name"`
	Email         string `json:"email"`
	Picture       string `json:"picture"`
	GoogleOauthId string `json:"google_oauth_id"`
}

type Room struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
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
	RoomName string `json:"room_name"`
	Status   string `json:"status"`
	Message  string `json:"message"`
}

type RoomRequestData struct {
	RoomName string `json:"room_name"`
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

	// Register User struct for session
	gob.Register(OAuthUser{})

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
		user, ok := session.Values["user"].(OAuthUser)
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

	var user OAuthUser
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
	user, ok := session.Values["user"].(OAuthUser)
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
	roomName := reqData.RoomName
	err = db.QueryRow("SELECT EXISTS(SELECT 1 FROM rooms WHERE name=$1)", roomName).Scan(&exists)
	if err != nil {
		http.Error(w, "Room does not exist", http.StatusNotFound)
		return
	}

	var res RoomResponse
	if exists {
		res = RoomResponse{
			RoomName: roomName,
			Status:   "success",
			Message:  "ご指定のroomNameは既に存在します。",
		}
	} else {
		stmt, err := db.Prepare("INSERT INTO rooms(id) VALUES($1)")
		if err != nil {
			http.Error(w, "Error preparing SQL statement", http.StatusInternalServerError)
			return
		}

		_, err = stmt.Exec(roomName)
		if err != nil {
			http.Error(w, "Error executing SQL statement", http.StatusInternalServerError)
			return
		}

		res = RoomResponse{
			RoomName: roomName,
			Status:   "success",
			Message:  "新しいroomを作成しました。",
		}
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusOK)

	json.NewEncoder(w).Encode(res)
}

func (s *WebSocketServer) findRoom(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	roomName := vars["roomName"]

	if db == nil {
		http.Error(w, "Database not configured", http.StatusInternalServerError)
		return
	}

	var exists bool
	err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM rooms WHERE name=$1)", roomName).Scan(&exists)
	if err != nil {
		http.Error(w, "Room does not exist", http.StatusNotFound)
		return
	}

	var res RoomResponse
	if exists {
		res = RoomResponse{
			RoomName: roomName,
			Status:   "success",
			Message:  "Roomが見つかりました！",
		}
	} else {
		res = RoomResponse{
			RoomName: roomName,
			Status:   "failed",
			Message:  "Roomは存在しません。",
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

	stmt, err := db.Prepare("UPDATE rooms SET is_closed = TRUE WHERE name=$1")
	if err != nil {
		http.Error(w, "Error preparing SQL statement", http.StatusInternalServerError)
		return
	}
	roomName := reqData.RoomName
	_, err = stmt.Exec(roomName)
	if err != nil {
		http.Error(w, "Error executing SQL statement", http.StatusInternalServerError)
		return
	}

	fmt.Fprintf(w, "Room %s closed", roomName)
}

func (s *WebSocketServer) handleConnections(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	roomName := vars["roomName"]

	if db == nil {
		http.Error(w, "Database not configured", http.StatusInternalServerError)
		return
	}

	// var exists bool
	// err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM rooms WHERE id=$1)", roomName).Scan(&exists)
	// if err != nil || !exists {
	// 	http.Error(w, "Room does not exist", http.StatusNotFound)
	// 	return
	// }

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	defer conn.Close()

	room, ok := s.rooms[roomName]
	if !ok {
		room = &WebSocketRoom{
			id:      roomName,
			clients: make(map[*websocket.Conn]bool),
		}
		s.rooms[roomName] = room
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

		// TODO: roomNameではなくroomIDをmuxで取れるようにしたほうがいい
		var roomId string
		if err := db.QueryRow("SELECT id FROM rooms WHERE name=$1", roomName).Scan(&roomId); err != nil {
			http.Error(w, "Error querying room", http.StatusInternalServerError)
			return
		}

		var userId string
		session, _ := store.Get(r, "user-session")
		user, _ := session.Values["user"].(OAuthUser)
		if err := db.QueryRow("SELECT id FROM users WHERE google_oauth_id=$1", user.ID).Scan(&userId); err != nil {
			http.Error(w, "Error querying user", http.StatusInternalServerError)
			return
		}

		stmt, err := db.Prepare("INSERT INTO messages(room_id, user_id, content) VALUES($1, $2, $3)")
		if err != nil {
			http.Error(w, "Error preparing SQL statement", http.StatusInternalServerError)
			return
		}

		_, err = stmt.Exec(roomId, userId, string(message))
		if err != nil {
			http.Error(w, "Error executing SQL statement", http.StatusInternalServerError)
			return
		}

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
	roomName := vars["roomName"]

	if db == nil {
		http.Error(w, "Database not configured", http.StatusInternalServerError)
		return
	}

	var isClosed bool
	err := db.QueryRow("SELECT is_closed FROM rooms WHERE name=$1", roomName).Scan(&isClosed)
	if err != nil {
		http.Error(w, "Room does not exist", http.StatusNotFound)
		return
	}

	if isClosed {
		http.Error(w, "Room is closed", http.StatusForbidden)
		return
	}

	var exists bool
	err = db.QueryRow("SELECT EXISTS(SELECT 1 FROM rooms WHERE name=$1)", roomName).Scan(&exists)
	if err != nil || !exists {
		http.Error(w, "Room does not exist", http.StatusNotFound)
		return
	}

	session, _ := store.Get(r, "user-session")
	user, _ := session.Values["user"].(OAuthUser)
	userName := strings.Split(strings.Split(user.Email, "@")[0], ".")[1]
	data := map[string]string{
		"Title":    "チャットルーム",
		"RoomName": roomName,
		"Email":    user.Email,
		"Picture":  user.Picture,
		"Name":     userName,
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
		err := rows.Scan(&room.ID, &room.Name, &room.IsClosed)
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
	if user, ok := session.Values["user"].(OAuthUser); ok && user.Email != "" {
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
	fmt.Println("login! callback url: ", url)
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func handleGoogleCallBack(w http.ResponseWriter, r *http.Request) {
	fmt.Println("callback!")
	session, _ := store.Get(r, "user-session")
	// ログイン情報があればcallbackと同じHTMLのroomsを表示
	if user, ok := session.Values["user"].(OAuthUser); ok && user.Email != "" {
		fmt.Println("User already authenticated: ", user)
		t := template.Must(template.ParseFS(resources, "templates/*"))
		if err := t.ExecuteTemplate(w, "rooms.html.tmpl", user); err != nil {
			http.Error(w, "Template parse error", http.StatusNotFound)
		}

		return
	}

	fmt.Println("getUserInfo")
	content, err := getUserInfo(r.FormValue("state"), r.FormValue("code"))
	if err != nil {
		log.Printf("Failed to get user info: %v", err)
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		return
	}

	fmt.Println("callback content: ", string(content))
	// Parse user info
	var user OAuthUser
	if err := json.Unmarshal(content, &user); err != nil {
		log.Printf("Failed to parse user info: %v", err)
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		return
	}
	fmt.Println("callback getUser: ", user)

	// check androots domain
	emailDomain := strings.Split(user.Email, "@")[1]
	if !isAllowedDomain(emailDomain) {
		log.Printf("Invalid domain: %s", emailDomain)
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

	if db == nil {
		http.Error(w, "Database not configured", http.StatusInternalServerError)
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		return
	}

	userName := strings.Split(strings.Split(user.Email, "@")[0], ".")[1]
	// NOTE: ORM使ってないのでDTO作る必要ナッシング
	userDTO := UserDTO{
		Name:          userName,
		Email:         user.Email,
		Picture:       user.Picture,
		GoogleOauthId: user.ID,
	}
	query := `INSERT INTO users (name, email, picture, google_oauth_id) VALUES ($1, $2, $3, $4) ON CONFLICT (google_oauth_id) DO NOTHING`
	err = db.QueryRow(query, userDTO.Name, userDTO.Email, userDTO.Picture, userDTO.GoogleOauthId).Scan()
	if err != nil {
		// google_oauth_idで重複している場合はエラーになるが、それは正常なのでログだけ出してリダイレクト
		log.Printf("Failed to insert user: %v", err)
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

	user := session.Values["user"].(OAuthUser)
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
	r.HandleFunc("/rooms/{roomName}", isAuthenticated(s.joinRoom))
	r.HandleFunc("/room/{roomName}", isAuthenticated(s.findRoom))
	r.HandleFunc("/ws/{roomName}", isAuthenticated(s.handleConnections))

	origins := []string{"*"}

	fmt.Println("Server started on :8080")
	err := http.ListenAndServe(":8080", allowOrigins(origins)(r))
	if err != nil {
		fmt.Println("Failed to start server:", err)
	}
}
