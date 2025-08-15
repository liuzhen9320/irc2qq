package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/log"
	"github.com/gorilla/websocket"
	"gopkg.in/irc.v4"
	"gopkg.in/yaml.v3"
)

//go:embed sensitive_words.txt
var sensitiveWordsData string

// Config represents the bridge configuration
type Config struct {
	QQ struct {
		WebSocketURL string `yaml:"websocket_url"`
		BotQQ        string `yaml:"bot_qq"`
	} `yaml:"qq"`
	IRC struct {
		Server   string `yaml:"server"`
		Port     int    `yaml:"port"`
		Nick     string `yaml:"nick"`
		User     string `yaml:"user"`
		RealName string `yaml:"realname"`
		Password string `yaml:"password,omitempty"`
		UseTLS   bool   `yaml:"use_tls"`
	} `yaml:"irc"`
	Bridges []Bridge `yaml:"bridges"`
	Debug   bool     `yaml:"debug"`
}

// Bridge represents a mapping between QQ and IRC
type Bridge struct {
	QQGroup   string `yaml:"qq_group,omitempty"`
	QQUser    string `yaml:"qq_user,omitempty"`
	IRCChannel string `yaml:"irc_channel,omitempty"`
	IRCUser   string `yaml:"irc_user,omitempty"`
	Direction string `yaml:"direction"` // "both", "qq_to_irc", "irc_to_qq"
}

// OnebotMessage represents Onebot V11 message format
type OnebotMessage struct {
	PostType    string `json:"post_type"`
	MessageType string `json:"message_type"`
	SubType     string `json:"sub_type"`
	MessageID   int64  `json:"message_id"`
	UserID      int64  `json:"user_id"`
	GroupID     int64  `json:"group_id,omitempty"`
	Message     string `json:"message"`
	RawMessage  string `json:"raw_message"`
	Sender      struct {
		UserID   int64  `json:"user_id"`
		Nickname string `json:"nickname"`
	} `json:"sender"`
}

// OnebotAPI represents Onebot API request
type OnebotAPI struct {
	Action string      `json:"action"`
	Params interface{} `json:"params"`
	Echo   string      `json:"echo,omitempty"`
}

// SendGroupMsgParams for sending group messages
type SendGroupMsgParams struct {
	GroupID int64  `json:"group_id"`
	Message string `json:"message"`
}

// SendPrivateMsgParams for sending private messages
type SendPrivateMsgParams struct {
	UserID  int64  `json:"user_id"`
	Message string `json:"message"`
}

// MessageBridge handles message bridging between QQ and IRC
type MessageBridge struct {
	config         *Config
	qqConn         *websocket.Conn
	ircClient      *irc.Client
	logger         *log.Logger
	sensitiveWords []string
	messageCache   map[string]struct{}
	cacheMutex     sync.RWMutex
	cacheSize      int
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
}

const (
	MaxIRCMessageLength = 400
	MaxQQMessageLength  = 4500
	MaxCacheSize       = 50
)

func loadConfig(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	return &config, nil
}

func loadSensitiveWords() []string {
	lines := strings.Split(strings.TrimSpace(sensitiveWordsData), "\n")
	var words []string
	for _, line := range lines {
		word := strings.TrimSpace(line)
		if word != "" && !strings.HasPrefix(word, "#") {
			words = append(words, word)
		}
	}
	return words
}

func (mb *MessageBridge) Start() error {
	// Connect to QQ
	if err := mb.connectQQ(); err != nil {
		return fmt.Errorf("failed to connect to QQ: %w", err)
	}

	// Connect to IRC
	if err := mb.connectIRC(); err != nil {
		return fmt.Errorf("failed to connect to IRC: %w", err)
	}

	// Start message handlers
	mb.wg.Add(2)
	go mb.handleQQMessages()
	go mb.handleIRCMessages()

	mb.logger.Info("Bridge started successfully")
	return nil
}

func (mb *MessageBridge) Stop() {
	mb.cancel()
	
	if mb.qqConn != nil {
		mb.qqConn.Close()
	}
	
	if mb.ircClient != nil {
		mb.ircClient.Quit("Bridge shutdown")
		mb.ircClient.Close()
	}
	
	mb.wg.Wait()
	mb.logger.Info("Bridge stopped")
}

func (mb *MessageBridge) connectQQ() error {
	var err error
	mb.qqConn, _, err = websocket.DefaultDialer.Dial(mb.config.QQ.WebSocketURL, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to QQ WebSocket: %w", err)
	}
	
	mb.logger.Info("Connected to QQ", "url", mb.config.QQ.WebSocketURL)
	return nil
}

func (mb *MessageBridge) connectIRC() error {
	ircConfig := irc.ClientConfig{
		Nick:     mb.config.IRC.Nick,
		User:     mb.config.IRC.User,
		Name:     mb.config.IRC.RealName,
		Password: mb.config.IRC.Password,
	}

	mb.ircClient = irc.NewClient(ircConfig)
	
	// Add handlers
	mb.ircClient.HandleFunc(irc.CONNECTED, func(c *irc.Client, m *irc.Message) {
		mb.logger.Info("Connected to IRC", "server", mb.config.IRC.Server)
		// Join channels
		for _, bridge := range mb.config.Bridges {
			if bridge.IRCChannel != "" {
				c.Write("JOIN " + bridge.IRCChannel)
			}
		}
	})

	mb.ircClient.HandleFunc(irc.DISCONNECTED, func(c *irc.Client, m *irc.Message) {
		mb.logger.Warn("Disconnected from IRC, attempting to reconnect...")
		mb.reconnectIRC()
	})

	// Connect with retry logic
	return mb.dialIRC()
}

func (mb *MessageBridge) dialIRC() error {
	server := fmt.Sprintf("%s:%d", mb.config.IRC.Server, mb.config.IRC.Port)
	
	var err error
	if mb.config.IRC.UseTLS {
		err = mb.ircClient.DialTLS(server, nil)
	} else {
		err = mb.ircClient.Dial(server)
	}
	
	return err
}

func (mb *MessageBridge) reconnectIRC() {
	backoff := time.Second
	maxBackoff := 30 * time.Second
	
	for {
		select {
		case <-mb.ctx.Done():
			return
		case <-time.After(backoff):
			if err := mb.dialIRC(); err != nil {
				mb.logger.Error("IRC reconnection failed", "error", err, "backoff", backoff)
				if backoff < maxBackoff {
					backoff *= 2
					if backoff > maxBackoff {
						backoff = maxBackoff
					}
				}
				continue
			}
			mb.logger.Info("IRC reconnection successful")
			return
		}
	}
}

func (mb *MessageBridge) handleQQMessages() {
	defer mb.wg.Done()
	
	for {
		select {
		case <-mb.ctx.Done():
			return
		default:
			var msg OnebotMessage
			if err := mb.qqConn.ReadJSON(&msg); err != nil {
				mb.logger.Error("Failed to read QQ message", "error", err)
				continue
			}
			
			if msg.PostType != "message" {
				continue
			}
			
			// Skip messages from bot itself
			if fmt.Sprintf("%d", msg.UserID) == mb.config.QQ.BotQQ {
				continue
			}
			
			mb.processQQMessage(&msg)
		}
	}
}

func (mb *MessageBridge) handleIRCMessages() {
	defer mb.wg.Done()
	
	for {
		select {
		case <-mb.ctx.Done():
			return
		default:
			msg, err := mb.ircClient.ReadMessage()
			if err != nil {
				mb.logger.Error("Failed to read IRC message", "error", err)
				continue
			}
			
			if msg.Command == "PRIVMSG" {
				// Skip messages from bot itself
				if msg.Nick == mb.config.IRC.Nick {
					continue
				}
				
				mb.processIRCMessage(msg)
			}
		}
	}
}

func (mb *MessageBridge) processQQMessage(msg *OnebotMessage) {
	// Clean message content
	cleanMsg := mb.cleanQQMessage(msg.RawMessage)
	if cleanMsg == "" {
		return
	}
	
	// Check for sensitive words
	if mb.containsSensitiveWords(cleanMsg) {
		mb.logger.Warn("Message blocked due to sensitive words", "from", fmt.Sprintf("qq/%d", msg.UserID))
		return
	}
	
	// Check message length
	if len(cleanMsg) > MaxQQMessageLength {
		mb.logger.Warn("QQ message too long, rejecting", "length", len(cleanMsg), "max", MaxQQMessageLength)
		return
	}
	
	// Check for duplicates
	msgHash := mb.generateMessageHash(fmt.Sprintf("qq:%d:%s", msg.UserID, cleanMsg))
	if mb.isDuplicateMessage(msgHash) {
		mb.logger.Debug("Duplicate QQ message ignored", "hash", msgHash[:8])
		return
	}
	mb.cacheMessage(msgHash)
	
	// Find matching bridge and forward
	for _, bridge := range mb.config.Bridges {
		if bridge.Direction == "irc_to_qq" {
			continue
		}
		
		var shouldForward bool
		var target string
		
		if msg.MessageType == "group" && bridge.QQGroup == fmt.Sprintf("%d", msg.GroupID) {
			shouldForward = true
			target = bridge.IRCChannel
		} else if msg.MessageType == "private" && bridge.QQUser == fmt.Sprintf("%d", msg.UserID) {
			shouldForward = true
			if bridge.IRCChannel != "" {
				target = bridge.IRCChannel
			} else {
				target = bridge.IRCUser
			}
		}
		
		if shouldForward && target != "" {
			formattedMsg := fmt.Sprintf("[%s] %s", msg.Sender.Nickname, cleanMsg)
			mb.sendToIRC(target, formattedMsg)
			
			from := fmt.Sprintf("qq/%d", msg.UserID)
			if msg.MessageType == "group" {
				from = fmt.Sprintf("qq/%d", msg.GroupID)
			}
			to := fmt.Sprintf("irc/%s", target)
			
			mb.logger.Info("Message forwarded", "from", from, "to", to, "message", cleanMsg)
		}
	}
}

func (mb *MessageBridge) processIRCMessage(msg *irc.Message) {
	cleanMsg := mb.cleanIRCMessage(msg.Trailing)
	if cleanMsg == "" {
		return
	}
	
	// Check for sensitive words
	if mb.containsSensitiveWords(cleanMsg) {
		mb.logger.Warn("Message blocked due to sensitive words", "from", fmt.Sprintf("irc/%s", msg.Nick))
		return
	}
	
	// Check for duplicates
	msgHash := mb.generateMessageHash(fmt.Sprintf("irc:%s:%s", msg.Nick, cleanMsg))
	if mb.isDuplicateMessage(msgHash) {
		mb.logger.Debug("Duplicate IRC message ignored", "hash", msgHash[:8])
		return
	}
	mb.cacheMessage(msgHash)
	
	// Find matching bridge and forward
	source := msg.Params[0] // Channel or user
	
	for _, bridge := range mb.config.Bridges {
		if bridge.Direction == "qq_to_irc" {
			continue
		}
		
		var shouldForward bool
		var qqGroup, qqUser string
		
		if bridge.IRCChannel == source {
			shouldForward = true
			qqGroup = bridge.QQGroup
			qqUser = bridge.QQUser
		} else if bridge.IRCUser == msg.Nick {
			shouldForward = true
			qqGroup = bridge.QQGroup
			qqUser = bridge.QQUser
		}
		
		if shouldForward {
			formattedMsg := fmt.Sprintf("[%s] %s", msg.Nick, cleanMsg)
			
			if qqGroup != "" {
				mb.sendToQQGroup(qqGroup, formattedMsg)
				mb.logger.Info("Message forwarded", "from", fmt.Sprintf("irc/%s", source), "to", fmt.Sprintf("qq/%s", qqGroup), "message", cleanMsg)
			} else if qqUser != "" {
				mb.sendToQQUser(qqUser, formattedMsg)
				mb.logger.Info("Message forwarded", "from", fmt.Sprintf("irc/%s", msg.Nick), "to", fmt.Sprintf("qq/%s", qqUser), "message", cleanMsg)
			}
		}
	}
}

func (mb *MessageBridge) sendToIRC(target, message string) {
	// Split long messages
	if len(message) > MaxIRCMessageLength {
		parts := mb.splitMessage(message, MaxIRCMessageLength)
		for i, part := range parts {
			if i > 0 {
				time.Sleep(100 * time.Millisecond) // Small delay between parts
			}
			mb.ircClient.WriteMessage(&irc.Message{
				Command:  "PRIVMSG",
				Params:   []string{target},
				Trailing: part,
			})
		}
	} else {
		mb.ircClient.WriteMessage(&irc.Message{
			Command:  "PRIVMSG",
			Params:   []string{target},
			Trailing: message,
		})
	}
}

func (mb *MessageBridge) sendToQQGroup(groupID, message string) {
	api := OnebotAPI{
		Action: "send_group_msg",
		Params: SendGroupMsgParams{
			GroupID: parseInt64(groupID),
			Message: message,
		},
	}
	
	if err := mb.qqConn.WriteJSON(api); err != nil {
		mb.logger.Error("Failed to send QQ group message", "error", err)
	}
}

func (mb *MessageBridge) sendToQQUser(userID, message string) {
	api := OnebotAPI{
		Action: "send_private_msg",
		Params: SendPrivateMsgParams{
			UserID:  parseInt64(userID),
			Message: message,
		},
	}
	
	if err := mb.qqConn.WriteJSON(api); err != nil {
		mb.logger.Error("Failed to send QQ private message", "error", err)
	}
}

func (mb *MessageBridge) cleanQQMessage(msg string) string {
	// Remove CQ codes
	cqRegex := regexp.MustCompile(`\[CQ:[^\]]+\]`)
	cleaned := cqRegex.ReplaceAllString(msg, "")
	
	return strings.TrimSpace(cleaned)
}

func (mb *MessageBridge) cleanIRCMessage(msg string) string {
	// Remove IRC color codes and formatting
	colorRegex := regexp.MustCompile(`\x03\d{0,2}(,\d{0,2})?`)
	cleaned := colorRegex.ReplaceAllString(msg, "")
	
	// Remove other formatting codes
	formatRegex := regexp.MustCompile(`[\x02\x1D\x1F\x16\x0F]`)
	cleaned = formatRegex.ReplaceAllString(cleaned, "")
	
	return strings.TrimSpace(cleaned)
}

func (mb *MessageBridge) containsSensitiveWords(msg string) bool {
	msgLower := strings.ToLower(msg)
	for _, word := range mb.sensitiveWords {
		if strings.Contains(msgLower, strings.ToLower(word)) {
			return true
		}
	}
	return false
}

func (mb *MessageBridge) generateMessageHash(content string) string {
	hash := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", hash)
}

func (mb *MessageBridge) isDuplicateMessage(hash string) bool {
	mb.cacheMutex.RLock()
	_, exists := mb.messageCache[hash]
	mb.cacheMutex.RUnlock()
	return exists
}

func (mb *MessageBridge) cacheMessage(hash string) {
	mb.cacheMutex.Lock()
	defer mb.cacheMutex.Unlock()
	
	// Add to cache
	mb.messageCache[hash] = struct{}{}
	mb.cacheSize++
	
	// Clean up if cache is too large
	if mb.cacheSize > MaxCacheSize {
		// Remove random entries to maintain size
		count := 0
		for k := range mb.messageCache {
			if count >= mb.cacheSize-MaxCacheSize {
				break
			}
			delete(mb.messageCache, k)
			count++
		}
		mb.cacheSize = len(mb.messageCache)
	}
}

func (mb *MessageBridge) splitMessage(msg string, maxLen int) []string {
	if len(msg) <= maxLen {
		return []string{msg}
	}
	
	var parts []string
	for len(msg) > maxLen {
		// Try to split at word boundary
		splitPos := maxLen
		for i := maxLen - 1; i > maxLen/2 && i < len(msg); i-- {
			if msg[i] == ' ' {
				splitPos = i
				break
			}
		}
		
		parts = append(parts, msg[:splitPos])
		msg = strings.TrimSpace(msg[splitPos:])
	}
	
	if len(msg) > 0 {
		parts = append(parts, msg)
	}
	
	return parts
}

func parseInt64(s string) int64 {
	// Simple string to int64 conversion, ignoring errors for this example
	var result int64
	fmt.Sscanf(s, "%d", &result)
	return result
}

func main() {
	// Load configuration
	config, err := loadConfig("config.yaml")
	if err != nil {
		log.Fatal("Failed to load config", "error", err)
	}

	// Setup logger
	logger := log.New(os.Stdout)
	logger.SetTimeFormat(time.RFC3339)
	if config.Debug {
		logger.SetLevel(log.DebugLevel)
		logger.SetReportCaller(true)
	} else {
		logger.SetLevel(log.InfoLevel)
	}

	// Create bridge instance
	ctx, cancel := context.WithCancel(context.Background())
	bridge := &MessageBridge{
		config:         config,
		logger:         logger,
		sensitiveWords: loadSensitiveWords(),
		messageCache:   make(map[string]struct{}),
		cacheSize:      0,
		ctx:            ctx,
		cancel:         cancel,
	}

	// Setup signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start bridge
	if err := bridge.Start(); err != nil {
		logger.Fatal("Failed to start bridge", "error", err)
	}

	// Wait for shutdown signal
	<-sigChan
	logger.Info("Shutdown signal received, gracefully closing connections...")
	bridge.Stop()
}
