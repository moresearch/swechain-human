package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/redis/go-redis/v9"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"
	"gopkg.in/yaml.v2"
)

const (
	ollamaEndpoint = "http://localhost:11434/api/generate"
	modelName      = "cogito:3b"
	configFile     = "config.yml"
	templateFile   = "templates/index.html"
	landingFile    = "templates/landing.html"
)

// Config structure for the yml file
type Config struct {
	GitHub struct {
		ClientID     string `yaml:"client_id"`
		ClientSecret string `yaml:"client_secret"`
	} `yaml:"github"`
	Redis struct {
		Host     string `yaml:"host"`
		Port     string `yaml:"port"`
		Password string `yaml:"password"`
		DB       int    `yaml:"db"`
	} `yaml:"redis"`
	Server struct {
		Port string `yaml:"port"`
	} `yaml:"server"`
}

// Global application configuration
var appConfig Config

// GitHub OAuth config
var githubOauthConfig *oauth2.Config

// Redis client
var redisClient *redis.Client

// Templates
var tmpl *template.Template
var landingTmpl *template.Template

// Custom logger
type Logger struct {
	*log.Logger
}

var logger *Logger

// WebSocket upgrader
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins in development
	},
}

// ClientManager for tracking connections
type ClientManager struct {
	clients    map[*websocket.Conn]string // Map connection to username
	register   chan *websocket.Conn
	unregister chan *websocket.Conn
	mutex      sync.Mutex
}

var manager = ClientManager{
	clients:    make(map[*websocket.Conn]string),
	register:   make(chan *websocket.Conn),
	unregister: make(chan *websocket.Conn),
}

// ChatMessage for history
type ChatMessage struct {
	Sender    string    `json:"sender"`
	Text      string    `json:"text"`
	Timestamp time.Time `json:"timestamp"`
}

type OllamaRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

type OllamaResponse struct {
	Response string `json:"response"`
	Done     bool   `json:"done"`
}

// Template data
type TemplateData struct {
	Username       string
	AccountAddress string
	TokenBalance   int
}

// GitHub User structure
type GitHubUser struct {
	Login     string `json:"login"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
}

// Token update message
type TokenUpdate struct {
	TokenBalance int `json:"token_balance"`
}

// Connection wrapper to provide thread safety for WebSocket writes
type SafeConn struct {
	conn  *websocket.Conn
	mutex sync.Mutex
}

// Thread-safe write method
func (sc *SafeConn) WriteMessage(messageType int, data []byte) error {
	sc.mutex.Lock()
	defer sc.mutex.Unlock()
	return sc.conn.WriteMessage(messageType, data)
}

func init() {
	// Setup logger
	file, err := os.OpenFile("app.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
	if err != nil {
		log.Fatalf("Failed to open log file: %v", err)
	}
	logger = &Logger{log.New(file, "", log.Ldate|log.Ltime|log.Lshortfile)}

	// Load configuration
	loadConfig()

	// Ensure templates directory exists
	if _, err := os.Stat("templates"); os.IsNotExist(err) {
		if err := os.Mkdir("templates", 0755); err != nil {
			logger.Printf("ERROR: Failed to create templates directory: %v", err)
			log.Fatalf("Failed to create templates directory: %v", err)
		}
	}

	// Load templates
	_, err = os.Stat(templateFile)
	if err == nil {
		tmpl, err = template.ParseFiles(templateFile)
		if err != nil {
			logger.Printf("ERROR: Failed to parse template file %s: %v", templateFile, err)
			log.Fatalf("Failed to parse template: %v", err)
		}
		logger.Printf("INFO: Using template file: %s", templateFile)
	} else {
		log.Fatalf("Template file not found: %s. Please create this file.", templateFile)
	}

	_, err = os.Stat(landingFile)
	if err == nil {
		landingTmpl, err = template.ParseFiles(landingFile)
		if err != nil {
			logger.Printf("ERROR: Failed to parse landing template file %s: %v", landingFile, err)
			log.Fatalf("Failed to parse landing template: %v", err)
		}
		logger.Printf("INFO: Using landing template file: %s", landingFile)
	} else {
		log.Fatalf("Landing template file not found: %s. Please create this file.", landingFile)
	}

	// Setup OAuth config
	githubOauthConfig = &oauth2.Config{
		ClientID:     appConfig.GitHub.ClientID,
		ClientSecret: appConfig.GitHub.ClientSecret,
		RedirectURL:  "http://localhost:" + appConfig.Server.Port + "/callback",
		Scopes:       []string{"user:email", "repo"},
		Endpoint:     github.Endpoint,
	}

	// Setup Redis client
	redisClient = redis.NewClient(&redis.Options{
		Addr:     appConfig.Redis.Host + ":" + appConfig.Redis.Port,
		Password: appConfig.Redis.Password,
		DB:       appConfig.Redis.DB,
	})
}

// Load config from YAML file
func loadConfig() {
	// Default values
	appConfig = Config{}
	appConfig.GitHub.ClientID = "YOUR_GITHUB_CLIENT_ID"
	appConfig.GitHub.ClientSecret = "YOUR_GITHUB_CLIENT_SECRET"
	appConfig.Redis.Host = "localhost"
	appConfig.Redis.Port = "6379"
	appConfig.Redis.DB = 0
	appConfig.Server.Port = "8080"

	// Check if config file exists and load it
	if _, err := os.Stat(configFile); err == nil {
		data, err := os.ReadFile(configFile)
		if err != nil {
			logger.Printf("WARNING: Error reading config file: %v. Using defaults.", err)
		} else {
			err = yaml.Unmarshal(data, &appConfig)
			if err != nil {
				logger.Printf("WARNING: Error parsing config file: %v. Using defaults.", err)
			} else {
				logger.Printf("INFO: Loaded configuration from %s", configFile)
			}
		}
	} else {
		// Create default config file if it doesn't exist
		defaultConfig := Config{}
		defaultConfig.GitHub.ClientID = "YOUR_GITHUB_CLIENT_ID"
		defaultConfig.GitHub.ClientSecret = "YOUR_GITHUB_CLIENT_SECRET"
		defaultConfig.Redis.Host = "localhost"
		defaultConfig.Redis.Port = "6379"
		defaultConfig.Redis.Password = ""
		defaultConfig.Redis.DB = 0
		defaultConfig.Server.Port = "8080"

		data, err := yaml.Marshal(defaultConfig)
		if err == nil {
			err = os.WriteFile(configFile, data, 0600)
			if err != nil {
				logger.Printf("WARNING: Failed to create default config file: %v", err)
			} else {
				logger.Printf("INFO: Created default config file at %s", configFile)
			}
		}
	}
}

func main() {
	ctx := context.Background()
	if err := redisClient.Ping(ctx).Err(); err != nil {
		logger.Printf("WARNING: Redis connection failed: %v. Chat history will not be persistent.", err)
	}

	// Start Echo
	e := echo.New()

	// Middleware
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())
	e.Use(middleware.CORS())

	// Routes
	e.GET("/", handleRoot)
	e.GET("/login", handleLogin)
	e.GET("/callback", handleCallback)
	e.GET("/ws", handleWebSocket)
	e.GET("/logout", handleLogout)
	e.GET("/token-updates", handleTokenUpdates)

	// API routes
	e.GET("/api/user/issues", handleUserIssues)

	// Serve static files from img directory
	e.Static("/img", "img")

	go manager.run()

	port := appConfig.Server.Port
	logger.Printf("INFO: Server starting on :%s", port)
	if err := e.Start(":" + port); err != nil && err != http.ErrServerClosed {
		logger.Fatalf("Server failed: %v", err)
	}
}

func (m *ClientManager) run() {
	for {
		select {
		case conn := <-m.register:
			m.mutex.Lock()
			m.clients[conn] = "" // Placeholder until username is set
			m.mutex.Unlock()
			logger.Printf("INFO: Client connected. Total: %d", len(m.clients))
		case conn := <-m.unregister:
			m.mutex.Lock()
			if username, ok := m.clients[conn]; ok {
				delete(m.clients, conn)
				conn.Close()
				logger.Printf("INFO: Client disconnected (%s). Total: %d", username, len(m.clients))
			}
			m.mutex.Unlock()
		}
	}
}

// Get GitHub user data from GitHub API
func getGitHubUser(token string) (*GitHubUser, error) {
	req, err := http.NewRequest("GET", "https://api.github.com/user", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create GitHub API request: %v", err)
	}

	// Add auth token
	req.Header.Add("Authorization", "token "+token)
	req.Header.Add("Accept", "application/vnd.github.v3+json")
	req.Header.Add("User-Agent", "SWEChain-App")

	// Make the request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub API request failed: %v", err)
	}
	defer resp.Body.Close()

	// Check for success
	if resp.StatusCode != http.StatusOK {
		body, _ := ioutil.ReadAll(resp.Body)
		return nil, fmt.Errorf("GitHub API returned non-200 status: %d, body: %s", resp.StatusCode, string(body))
	}

	// Read and parse the response
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read GitHub API response: %v", err)
	}

	// Parse the JSON response
	var user GitHubUser
	err = json.Unmarshal(body, &user)
	if err != nil {
		return nil, fmt.Errorf("failed to parse GitHub API response: %v", err)
	}

	return &user, nil
}

// Setup blockchain account for user and return account address and token balance
func setupBlockchainAccount(username string) (string, int, error) {
	// Check if the user already has keys
	accountAddress := ""

	// Get user's account address
	checkAddressCmd := exec.Command("swechaind", "keys", "show", username, "-a", "--keyring-backend", "test")
	addressOutput, err := checkAddressCmd.Output()

	if err == nil {
		// User already exists, get their address
		accountAddress = strings.TrimSpace(string(addressOutput))
		logger.Printf("INFO: Found existing blockchain account for %s at %s", username, accountAddress)
	} else {
		// Create command to add keys
		addKeysCmd := exec.Command("swechaind", "keys", "add", username, "--keyring-backend", "test")
		keyOutput, err := addKeysCmd.Output()
		if err != nil {
			return "", 0, fmt.Errorf("failed to add blockchain keys: %v", err)
		}

		// Parse the output to get the address
		outputLines := strings.Split(string(keyOutput), "\n")
		for _, line := range outputLines {
			if strings.Contains(line, "address:") {
				accountAddress = strings.TrimSpace(strings.Split(line, ":")[1])
				break
			}
		}

		if accountAddress == "" {
			return "", 0, fmt.Errorf("failed to extract account address from command output")
		}

		// Add tokens to user - 1000000 tokens initially
		addTokensCmd := exec.Command("swechaind", "tx", "bank", "send",
			"faucet", accountAddress, "1000000token",
			"--from", "faucet",
			"--keyring-backend", "test",
			"--chain-id", "swechain",
			"--fees", "200token",
			"--yes")

		err = addTokensCmd.Run()
		if err != nil {
			return "", 0, fmt.Errorf("failed to add tokens: %v", err)
		}

		logger.Printf("INFO: Created new blockchain account for %s at %s", username, accountAddress)
	}

	// Get token balance
	tokenBalance, err := getTokenBalance(accountAddress)
	if err != nil {
		logger.Printf("WARNING: Failed to get token balance: %v", err)
		tokenBalance = 0
	}

	// Store account address and balance in Redis for faster access later
	ctx := context.Background()
	key := fmt.Sprintf("account:%s", username)
	redisClient.HSet(ctx, key, "address", accountAddress)
	redisClient.HSet(ctx, key, "balance", tokenBalance)

	return accountAddress, tokenBalance, nil
}

// Get current token balance for a specific address
func getTokenBalance(address string) (int, error) {
	balanceCmd := exec.Command("swechaind", "query", "bank", "balances", address, "--output", "json")
	balanceOutput, err := balanceCmd.Output()

	if err != nil {
		return 0, fmt.Errorf("failed to query balance: %v", err)
	}

	// Parse JSON to get the balance
	var balanceData struct {
		Balances []struct {
			Denom  string `json:"denom"`
			Amount string `json:"amount"`
		} `json:"balances"`
	}

	err = json.Unmarshal(balanceOutput, &balanceData)
	if err != nil {
		return 0, fmt.Errorf("failed to parse balance data: %v", err)
	}

	for _, coin := range balanceData.Balances {
		if coin.Denom == "token" {
			amount, err := strconv.Atoi(coin.Amount)
			if err != nil {
				return 0, fmt.Errorf("failed to parse token amount: %v", err)
			}
			return amount, nil
		}
	}

	// No tokens found
	return 0, nil
}

// HTML handler - check auth inside
func handleRoot(c echo.Context) error {
	// Check if user is authenticated
	cookie, err := c.Cookie("github_token")
	if err != nil || cookie.Value == "" {
		// User is not authenticated, show landing page
		logger.Printf("INFO: Showing landing page to unauthenticated user")
		return landingTmpl.Execute(c.Response().Writer, nil)
	}

	// User is authenticated, get GitHub info
	githubUser, err := getGitHubUser(cookie.Value)
	username := "User"
	if err != nil {
		logger.Printf("WARNING: Failed to get GitHub user info: %v", err)
	} else if githubUser != nil {
		// Use login (username) instead of display name
		username = githubUser.Login
	}

	// Get account address and balance from Redis or blockchain
	accountAddress := ""
	tokenBalance := 0

	ctx := context.Background()
	key := fmt.Sprintf("account:%s", username)

	// Try to get from Redis first (faster)
	accountAddress, err = redisClient.HGet(ctx, key, "address").Result()
	if err != nil || accountAddress == "" {
		// If not in Redis, try to set up from blockchain
		accountAddress, tokenBalance, err = setupBlockchainAccount(username)
		if err != nil {
			logger.Printf("WARNING: Failed to get account info: %v", err)
			accountAddress = "Not available"
		}
	} else {
		// Get balance from Redis
		balanceStr, err := redisClient.HGet(ctx, key, "balance").Result()
		if err == nil {
			tokenBalance, _ = strconv.Atoi(balanceStr)
		}
	}

	// Create template data with user info
	data := TemplateData{
		Username:       username,
		AccountAddress: accountAddress,
		TokenBalance:   tokenBalance,
	}

	// Use the template to render the page
	return tmpl.Execute(c.Response().Writer, data)
}

func handleLogin(c echo.Context) error {
	url := githubOauthConfig.AuthCodeURL("state", oauth2.AccessTypeOnline)
	logger.Printf("INFO: Redirecting to GitHub OAuth: %s", url)
	return c.Redirect(http.StatusSeeOther, url)
}

func handleCallback(c echo.Context) error {
	code := c.QueryParam("code")
	if code == "" {
		logger.Printf("ERROR: No OAuth code provided in callback")
		return c.String(http.StatusBadRequest, "No code provided")
	}

	token, err := githubOauthConfig.Exchange(context.Background(), code)
	if err != nil {
		logger.Printf("ERROR: Failed to exchange OAuth token: %v", err)
		return c.String(http.StatusInternalServerError, "Failed to exchange token")
	}

	// Get the user info to set up blockchain account
	githubUser, err := getGitHubUser(token.AccessToken)
	if err == nil && githubUser != nil {
		// Try to set up blockchain account with the GitHub username
		accountAddress, tokenBalance, err := setupBlockchainAccount(githubUser.Login)
		if err != nil {
			logger.Printf("WARNING: Failed to setup blockchain account: %v", err)
		} else {
			logger.Printf("INFO: Successfully set up blockchain account for %s with address %s and %d tokens",
				githubUser.Login, accountAddress, tokenBalance)
		}
	}

	// Set cookie
	c.SetCookie(&http.Cookie{
		Name:     "github_token",
		Value:    token.AccessToken,
		Expires:  token.Expiry,
		Path:     "/",
		Secure:   false, // Set to true in production with HTTPS
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	logger.Printf("INFO: OAuth successful, redirecting to /")
	return c.Redirect(http.StatusSeeOther, "/")
}

func handleLogout(c echo.Context) error {
	// Clear the auth cookie by setting an expired cookie
	c.SetCookie(&http.Cookie{
		Name:     "github_token",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Now().Add(-24 * time.Hour), // Set expiry in the past
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	logger.Printf("INFO: User logged out, redirecting to /")
	return c.Redirect(http.StatusSeeOther, "/")
}

// Websocket endpoint for live token updates
func handleTokenUpdates(c echo.Context) error {
	// Check auth
	cookie, err := c.Cookie("github_token")
	if err != nil || cookie.Value == "" {
		logger.Printf("ERROR: Token update WebSocket connection attempt without authentication")
		return c.NoContent(http.StatusUnauthorized)
	}

	// Get username
	githubUser, err := getGitHubUser(cookie.Value)
	username := "User"
	if err == nil && githubUser != nil {
		username = githubUser.Login
	}

	// Upgrade to WebSocket
	ws, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		logger.Printf("ERROR: Token WebSocket upgrade failed: %v", err)
		return err
	}
	defer ws.Close()

	// Create a safe connection
	safeConn := &SafeConn{
		conn: ws,
	}

	// Get account address
	ctx := context.Background()
	key := fmt.Sprintf("account:%s", username)
	accountAddress, err := redisClient.HGet(ctx, key, "address").Result()
	if err != nil || accountAddress == "" {
		// Try to set up again if needed
		accountAddress, _, err = setupBlockchainAccount(username)
		if err != nil {
			logger.Printf("ERROR: Failed to get account address for token updates: %v", err)
			return nil
		}
	}

	// Send initial token balance
	if err := sendCurrentTokenBalance(safeConn, accountAddress); err != nil {
		logger.Printf("ERROR: Failed to send initial token balance: %v", err)
		return nil
	}

	// Setup periodic updates (every 30 seconds)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// Handle client disconnection
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, _, err := ws.ReadMessage()
			if err != nil {
				return
			}
		}
	}()

	// Send periodic updates
	for {
		select {
		case <-done:
			return nil
		case <-ticker.C:
			if err := sendCurrentTokenBalance(safeConn, accountAddress); err != nil {
				logger.Printf("ERROR: Failed to send token update: %v", err)
				return nil
			}
		}
	}
}

func sendCurrentTokenBalance(sc *SafeConn, accountAddress string) error {
	// Get current token balance from blockchain
	tokenBalance, err := getTokenBalance(accountAddress)
	if err != nil {
		return fmt.Errorf("failed to get token balance: %v", err)
	}

	// Send update to client
	update := TokenUpdate{
		TokenBalance: tokenBalance,
	}

	jsonMsg, err := json.Marshal(update)
	if err != nil {
		return fmt.Errorf("failed to marshal token update: %v", err)
	}

	if err := sc.WriteMessage(websocket.TextMessage, jsonMsg); err != nil {
		return fmt.Errorf("failed to write token update: %v", err)
	}

	return nil
}

func handleUserIssues(c echo.Context) error {
	// Get the GitHub access token from cookie
	cookie, err := c.Cookie("github_token")
	if err != nil {
		logger.Printf("ERROR: User not authenticated for /api/user/issues")
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "Not authenticated"})
	}

	// Create a GitHub API request
	req, err := http.NewRequest("GET", "https://api.github.com/user/issues?filter=created", nil)
	if err != nil {
		logger.Printf("ERROR: Failed to create GitHub API request: %v", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to create request"})
	}

	// Add auth token
	req.Header.Add("Authorization", "token "+cookie.Value)
	req.Header.Add("Accept", "application/vnd.github.v3+json")
	req.Header.Add("User-Agent", "SWEChain-App")

	// Make the request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		logger.Printf("ERROR: GitHub API request failed: %v", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to connect to GitHub API"})
	}
	defer resp.Body.Close()

	// Check for success
	if resp.StatusCode != http.StatusOK {
		body, _ := ioutil.ReadAll(resp.Body)
		logger.Printf("ERROR: GitHub API returned non-200 status: %d, body: %s", resp.StatusCode, string(body))
		return c.JSON(resp.StatusCode, map[string]string{"error": "GitHub API error", "details": string(body)})
	}

	// Read and parse the response
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		logger.Printf("ERROR: Failed to read GitHub API response: %v", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to read response"})
	}

	// Parse the JSON response
	var issues []map[string]interface{}
	err = json.Unmarshal(body, &issues)
	if err != nil {
		logger.Printf("ERROR: Failed to parse GitHub API response: %v", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to parse response"})
	}

	// Return the issues to the client
	return c.JSON(http.StatusOK, issues)
}

func handleWebSocket(c echo.Context) error {
	cookie, err := c.Cookie("github_token")
	if err != nil || cookie.Value == "" {
		logger.Printf("ERROR: WebSocket connection attempt without authentication")
		return c.NoContent(http.StatusUnauthorized)
	}

	ws, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		logger.Printf("ERROR: WebSocket upgrade failed: %v", err)
		return err
	}

	// Create a thread-safe WebSocket connection wrapper
	safeConn := &SafeConn{
		conn: ws,
	}

	// Get the GitHub username (login) for the user
	githubUser, err := getGitHubUser(cookie.Value)
	var username string
	if err != nil || githubUser == nil {
		username = "User"
		logger.Printf("WARNING: Failed to get GitHub username for WebSocket: %v", err)
	} else {
		username = githubUser.Login // Use login (username) instead of display name
	}

	// Register this connection
	manager.register <- ws
	manager.mutex.Lock()
	manager.clients[ws] = username
	manager.mutex.Unlock()

	// Send username to client
	userInfoMsg := struct {
		UserInfo string `json:"userinfo"`
	}{UserInfo: username}

	if userInfoBytes, err := json.Marshal(userInfoMsg); err == nil {
		safeConn.WriteMessage(websocket.TextMessage, userInfoBytes)
	}

	// Make sure to unregister on disconnect
	defer func() { manager.unregister <- ws }()

	ctx := context.Background()

	// Main WebSocket read loop
	for {
		_, message, err := ws.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				logger.Printf("ERROR: WebSocket read error for %s: %v", username, err)
			}
			return nil // Exit cleanly on close
		}

		msgStr := string(message)
		if msgStr == "HISTORY" {
			history, err := getUserHistory(ctx, username)
			if err != nil {
				logger.Printf("ERROR: Failed to get history for %s: %v", username, err)
				sendSystemMessage(safeConn, "Failed to load chat history")
				continue
			}

			logger.Printf("INFO: Sent history to %s", username)
			if err := sendWSHistoryMessage(safeConn, history); err != nil {
				logger.Printf("ERROR: Failed to send history to %s: %v", username, err)
			}
			continue
		}

		// Process the message in a goroutine to not block the WebSocket
		go processUserMessage(ctx, safeConn, username, msgStr)
	}
}

func processUserMessage(ctx context.Context, sc *SafeConn, username, msg string) {
	logger.Printf("INFO: Received message from %s: %s", username, msg)
	chatMsg := ChatMessage{Sender: "You", Text: msg, Timestamp: time.Now()}

	// Add message to history
	if err := addToUserHistory(ctx, username, chatMsg); err != nil {
		logger.Printf("ERROR: Failed to save user message for %s: %v", username, err)
	}

	// Send confirmation back to user
	if err := sendWSMessage(sc, "You", msg, nil); err != nil {
		logger.Printf("ERROR: Failed to send message to %s: %v", username, err)
		return
	}

	// Send request to Ollama with streaming enabled
	reqBody := OllamaRequest{
		Model:  modelName, // Use the defined model name (cogito:3b)
		Prompt: msg,
		Stream: true, // Enable streaming
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		logger.Printf("ERROR: Failed to marshal Ollama request: %v", err)
		sendSystemMessage(sc, "Failed to process your request")
		return
	}

	resp, err := http.Post(ollamaEndpoint, "application/json", strings.NewReader(string(jsonBody)))
	if err != nil {
		logger.Printf("ERROR: Failed to connect to Ollama: %v", err)
		sendSystemMessage(sc, "Failed to connect to Ollama service")
		return
	}
	defer resp.Body.Close()

	// Create a scanner to read line by line from the response
	scanner := bufio.NewScanner(resp.Body)

	// Send start of streaming
	sendStreamStart(sc)

	// Keep track of the full response for history
	var fullResponse strings.Builder

	// Process the streaming response
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) == 0 {
			continue
		}

		var ollamaResp OllamaResponse
		if err := json.Unmarshal([]byte(line), &ollamaResp); err != nil {
			logger.Printf("ERROR: Failed to unmarshal streaming response: %v", err)
			continue
		}

		// Send the chunk to the client
		sendStreamChunk(sc, ollamaResp.Response, ollamaResp.Done)

		// Append to full response
		fullResponse.WriteString(ollamaResp.Response)

		if ollamaResp.Done {
			// We're done streaming
			break
		}
	}

	if err := scanner.Err(); err != nil {
		logger.Printf("ERROR: Failed reading stream from Ollama: %v", err)
		sendSystemMessage(sc, "Error while reading response from Ollama")
		return
	}

	// Save the complete message to history
	ollamaMsg := ChatMessage{
		Sender:    "Ollama",
		Text:      fullResponse.String(),
		Timestamp: time.Now(),
	}

	if err := addToUserHistory(ctx, username, ollamaMsg); err != nil {
		logger.Printf("ERROR: Failed to save Ollama response for %s: %v", username, err)
	}
}

func sendStreamStart(sc *SafeConn) error {
	msg := struct {
		Stream bool   `json:"stream"`
		Start  bool   `json:"start"`
		Text   string `json:"text"`
	}{
		Stream: true,
		Start:  true,
		Text:   "",
	}

	jsonMsg, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal stream start message: %v", err)
	}

	if err := sc.WriteMessage(websocket.TextMessage, jsonMsg); err != nil {
		return fmt.Errorf("failed to write stream start message: %v", err)
	}

	return nil
}

func sendStreamChunk(sc *SafeConn, text string, done bool) error {
	msg := struct {
		Stream bool   `json:"stream"`
		Text   string `json:"text"`
		Done   bool   `json:"done"`
	}{
		Stream: true,
		Text:   text,
		Done:   done,
	}

	jsonMsg, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal stream chunk message: %v", err)
	}

	if err := sc.WriteMessage(websocket.TextMessage, jsonMsg); err != nil {
		return fmt.Errorf("failed to write stream chunk message: %v", err)
	}

	return nil
}

func sendSystemMessage(sc *SafeConn, text string) error {
	msg := struct {
		System bool   `json:"system"`
		Text   string `json:"text"`
	}{System: true, Text: text}

	jsonMsg, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal system message: %v", err)
	}

	if err := sc.WriteMessage(websocket.TextMessage, jsonMsg); err != nil {
		return fmt.Errorf("failed to write system message: %v", err)
	}

	return nil
}

func sendWSMessage(sc *SafeConn, sender, text string, history []ChatMessage) error {
	var msg interface{}

	if len(history) > 0 {
		msg = struct {
			History []ChatMessage `json:"history"`
		}{History: history}
	} else {
		msg = struct {
			Sender    string `json:"sender"`
			Text      string `json:"text"`
			Timestamp string `json:"timestamp"`
		}{Sender: sender, Text: text, Timestamp: time.Now().Format(time.RFC3339)}
	}

	jsonMsg, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal WebSocket message: %v", err)
	}

	if err := sc.WriteMessage(websocket.TextMessage, jsonMsg); err != nil {
		return fmt.Errorf("failed to write WebSocket message: %v", err)
	}

	return nil
}

func sendWSHistoryMessage(sc *SafeConn, history []ChatMessage) error {
	msg := struct {
		History []ChatMessage `json:"history"`
	}{History: history}

	jsonMsg, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal history message: %v", err)
	}

	if err := sc.WriteMessage(websocket.TextMessage, jsonMsg); err != nil {
		return fmt.Errorf("failed to write history message: %v", err)
	}

	return nil
}

func addToUserHistory(ctx context.Context, username string, msg ChatMessage) error {
	key := fmt.Sprintf("chat:%s", username)
	jsonMsg, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal chat message: %v", err)
	}

	// Try to push to Redis, but don't fail if Redis is unavailable
	if err := redisClient.RPush(ctx, key, jsonMsg).Err(); err != nil {
		return fmt.Errorf("failed to push to Redis: %v", err)
	}

	// Trim history to last 100 messages
	redisClient.LTrim(ctx, key, -100, -1)

	return nil
}

func getUserHistory(ctx context.Context, username string) ([]ChatMessage, error) {
	key := fmt.Sprintf("chat:%s", username)
	vals, err := redisClient.LRange(ctx, key, 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get history from Redis: %v", err)
	}

	history := make([]ChatMessage, 0, len(vals))
	for _, val := range vals {
		var msg ChatMessage
		if err := json.Unmarshal([]byte(val), &msg); err != nil {
			logger.Printf("ERROR: Failed to unmarshal message from Redis for %s: %v", username, err)
			continue
		}
		history = append(history, msg)
	}

	return history, nil
}
