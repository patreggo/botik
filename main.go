package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Remnawave API types
type CreateUserRequest struct {
	Username          string   `json:"username"`
	TrafficLimitBytes int64    `json:"trafficLimitBytes,omitempty"`
	ExpireAt          string   `json:"expireAt,omitempty"`
	TelegramID        int64    `json:"telegramId,omitempty"`
	Description       string   `json:"description,omitempty"`
	Tag               string   `json:"tag,omitempty"`
	HwidDeviceLimit   int      `json:"hwidDeviceLimit,omitempty"`
	ActiveUserInbounds  []InboundTag `json:"activeUserInbounds,omitempty"`
	ActiveInternalSquads []string    `json:"activeInternalSquads,omitempty"`
}

type InboundTag struct {
	Tag string `json:"tag"`
}

type RemnawaveUser struct {
	UUID             string `json:"uuid"`
	Username         string `json:"username"`
	ShortUUID        string `json:"shortUuid"`
	SubscriptionUUID string `json:"subscriptionUuid"`
	Status           string `json:"status"`
	ShortURL         string `json:"subscriptionUrl"`
}

type RemnawaveResponse struct {
	Response RemnawaveUser `json:"response"`
}

type InboundsResponse struct {
	Response []Inbound `json:"response"`
}

type Inbound struct {
	UUID string `json:"uuid"`
	Tag  string `json:"tag"`
	Type string `json:"type"`
}

type InternalSquad struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

type InternalSquadsResponse struct {
	Response []InternalSquad `json:"response"`
}

// Bot state management
type UserState struct {
	Step       string
	TrafficGB  int
	DaysExpire int
	ClientName string
}

var (
	userStates = make(map[int64]*UserState)
	statesMu   sync.Mutex

	remnawaveAPI   string
	remnawaveToken string
	subDomain      string
	botToken       string
	adminIDs       map[int64]bool
)

func init() {
	botToken = os.Getenv("BOT_TOKEN")
	if botToken == "" {
		log.Fatal("BOT_TOKEN is required")
	}

	remnawaveAPI = os.Getenv("REMNAWAVE_API")
	if remnawaveAPI == "" {
		remnawaveAPI = "http://127.0.0.1:3000"
	}
	remnawaveAPI = strings.TrimRight(remnawaveAPI, "/")

	remnawaveToken = os.Getenv("REMNAWAVE_TOKEN")
	if remnawaveToken == "" {
		log.Fatal("REMNAWAVE_TOKEN is required")
	}

	subDomain = os.Getenv("SUB_DOMAIN")
	if subDomain == "" {
		subDomain = remnawaveAPI
	}
	subDomain = strings.TrimRight(subDomain, "/")

	// Parse admin IDs (comma-separated)
	adminIDs = make(map[int64]bool)
	if ids := os.Getenv("ADMIN_IDS"); ids != "" {
		for _, idStr := range strings.Split(ids, ",") {
			id, err := strconv.ParseInt(strings.TrimSpace(idStr), 10, 64)
			if err == nil {
				adminIDs[id] = true
			}
		}
	}
}

func isAdmin(userID int64) bool {
	if len(adminIDs) == 0 {
		return true // no restriction if no admins configured
	}
	return adminIDs[userID]
}

func main() {
	bot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		log.Fatalf("Failed to create bot: %v", err)
	}

	log.Printf("Bot started: @%s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	for update := range updates {
		if update.CallbackQuery != nil {
			handleCallback(bot, update.CallbackQuery)
			continue
		}
		if update.Message == nil {
			continue
		}
		if update.Message.IsCommand() {
			handleCommand(bot, update.Message)
			continue
		}
		handleText(bot, update.Message)
	}
}

func handleCommand(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	switch msg.Command() {
	case "start":
		sendMainMenu(bot, msg.Chat.ID)
	}
}

func sendMainMenu(bot *tgbotapi.BotAPI, chatID int64) {
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("➕ Создать клиента", "create_client"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📋 Мои подписки", "my_subs"),
		),
	)

	text := "🔐 *Панель управления VPN*\n\nВыберите действие:"
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = keyboard
	bot.Send(msg)
}

func handleCallback(bot *tgbotapi.BotAPI, cb *tgbotapi.CallbackQuery) {
	bot.Send(tgbotapi.NewCallback(cb.ID, ""))

	userID := cb.From.ID
	chatID := cb.Message.Chat.ID

	if !isAdmin(userID) {
		bot.Send(tgbotapi.NewMessage(chatID, "⛔ У вас нет доступа."))
		return
	}

	switch {
	case cb.Data == "create_client":
		handleCreateClient(bot, chatID, userID)

	case cb.Data == "my_subs":
		handleMySubs(bot, chatID, userID)

	case cb.Data == "main_menu":
		sendMainMenu(bot, chatID)

	case strings.HasPrefix(cb.Data, "traffic_"):
		handleTrafficChoice(bot, chatID, userID, cb.Data)

	case strings.HasPrefix(cb.Data, "expire_"):
		handleExpireChoice(bot, chatID, userID, cb.Data)
	}
}

func handleCreateClient(bot *tgbotapi.BotAPI, chatID, userID int64) {
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("50 GB", "traffic_50"),
			tgbotapi.NewInlineKeyboardButtonData("100 GB", "traffic_100"),
			tgbotapi.NewInlineKeyboardButtonData("200 GB", "traffic_200"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("500 GB", "traffic_500"),
			tgbotapi.NewInlineKeyboardButtonData("♾ Безлимит", "traffic_0"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Назад", "main_menu"),
		),
	)

	msg := tgbotapi.NewMessage(chatID, "📊 *Выберите лимит трафика:*")
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = keyboard
	bot.Send(msg)

	statesMu.Lock()
	userStates[userID] = &UserState{Step: "choosing_traffic"}
	statesMu.Unlock()
}

func handleTrafficChoice(bot *tgbotapi.BotAPI, chatID, userID int64, data string) {
	gb, _ := strconv.Atoi(strings.TrimPrefix(data, "traffic_"))

	statesMu.Lock()
	state, ok := userStates[userID]
	if !ok {
		state = &UserState{}
		userStates[userID] = state
	}
	state.TrafficGB = gb
	state.Step = "choosing_expire"
	statesMu.Unlock()

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("7 дней", "expire_7"),
			tgbotapi.NewInlineKeyboardButtonData("30 дней", "expire_30"),
			tgbotapi.NewInlineKeyboardButtonData("90 дней", "expire_90"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("180 дней", "expire_180"),
			tgbotapi.NewInlineKeyboardButtonData("365 дней", "expire_365"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Назад", "create_client"),
		),
	)

	trafficText := "♾ Безлимит"
	if gb > 0 {
		trafficText = fmt.Sprintf("%d GB", gb)
	}

	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("📊 Трафик: *%s*\n\n⏳ *Выберите срок действия:*", trafficText))
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = keyboard
	bot.Send(msg)
}

func handleExpireChoice(bot *tgbotapi.BotAPI, chatID, userID int64, data string) {
	days, _ := strconv.Atoi(strings.TrimPrefix(data, "expire_"))

	statesMu.Lock()
	state, ok := userStates[userID]
	if !ok {
		statesMu.Unlock()
		sendMainMenu(bot, chatID)
		return
	}
	state.DaysExpire = days
	state.Step = "entering_name"
	statesMu.Unlock()

	msg := tgbotapi.NewMessage(chatID, "✏️ *Введите имя для клиента:*\n\nТолько латиница, цифры, дефис и подчёркивание.\nНапример: `Ivan` или `iPhone-Petya`")
	msg.ParseMode = "Markdown"
	bot.Send(msg)
}

func finishClientCreation(bot *tgbotapi.BotAPI, chatID, userID int64, clientName string, trafficGB, days int) {
	// Send "creating..." message
	waitMsg := tgbotapi.NewMessage(chatID, "⏳ Создаю клиента...")
	bot.Send(waitMsg)

	// Use client name directly as username
	username := clientName

	// Calculate expiry
	expireAt := time.Now().AddDate(0, 0, days).UTC().Format(time.RFC3339)

	// Get available inbounds
	inbounds, err := getInbounds()
	if err != nil {
		log.Printf("Failed to get inbounds: %v", err)
	}

	var inboundTags []InboundTag
	for _, inb := range inbounds {
		inboundTags = append(inboundTags, InboundTag{Tag: inb.Tag})
	}

	// Get default internal squad
	var squadUUIDs []string
	squads, err := getInternalSquads()
	if err != nil {
		log.Printf("Failed to get internal squads: %v", err)
	} else {
		for _, sq := range squads {
			if strings.EqualFold(sq.Name, "default") || strings.EqualFold(sq.Name, "Default") {
				squadUUIDs = append(squadUUIDs, sq.UUID)
				break
			}
		}
		// If no "Default" found, use the first squad
		if len(squadUUIDs) == 0 && len(squads) > 0 {
			squadUUIDs = append(squadUUIDs, squads[0].UUID)
		}
	}

	// Create user request
	req := CreateUserRequest{
		Username:             username,
		ExpireAt:             expireAt,
		TelegramID:           userID,
		Description:          fmt.Sprintf("Created by bot for TG user %d", userID),
		ActiveUserInbounds:   inboundTags,
		ActiveInternalSquads: squadUUIDs,
	}

	if trafficGB > 0 {
		req.TrafficLimitBytes = int64(trafficGB) * 1024 * 1024 * 1024
	}

	// Create user in Remnawave
	user, err := createRemnawaveUser(req)
	if err != nil {
		errMsg := tgbotapi.NewMessage(chatID, fmt.Sprintf("❌ Ошибка создания клиента:\n`%s`", err.Error()))
		errMsg.ParseMode = "Markdown"
		bot.Send(errMsg)
		return
	}

	// Build subscription link
	subLink := fmt.Sprintf("%s/api/sub/%s", subDomain, user.ShortUUID)

	trafficText := "♾ Безлимит"
	if trafficGB > 0 {
		trafficText = fmt.Sprintf("%d GB", trafficGB)
	}

	resultText := fmt.Sprintf(
		"✅ *Клиент создан!*\n\n"+
			"👤 Имя: `%s`\n"+
			"📊 Трафик: *%s*\n"+
			"⏳ Срок: *%d дней*\n"+
			"📅 Истекает: *%s*\n\n"+
			"🔗 *Ссылка на подписку:*\n`%s`\n\n"+
			"Скопируйте ссылку и вставьте в ваш VPN-клиент.",
		user.Username,
		trafficText,
		days,
		time.Now().AddDate(0, 0, days).Format("02.01.2006"),
		subLink,
	)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Главное меню", "main_menu"),
		),
	)

	msg := tgbotapi.NewMessage(chatID, resultText)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = keyboard
	bot.Send(msg)
}

func handleMySubs(bot *tgbotapi.BotAPI, chatID, userID int64) {
	user, err := getUserByTelegramID(userID)
	if err != nil {
		keyboard := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("➕ Создать клиента", "create_client"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("⬅️ Назад", "main_menu"),
			),
		)
		msg := tgbotapi.NewMessage(chatID, "📋 У вас пока нет подписок.")
		msg.ReplyMarkup = keyboard
		bot.Send(msg)
		return
	}

	subLink := fmt.Sprintf("%s/api/sub/%s", subDomain, user.ShortUUID)

	text := fmt.Sprintf(
		"📋 *Ваша подписка:*\n\n"+
			"👤 Имя: `%s`\n"+
			"📊 Статус: *%s*\n\n"+
			"🔗 *Ссылка:*\n`%s`",
		user.Username,
		user.Status,
		subLink,
	)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⬅️ Назад", "main_menu"),
		),
	)

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = keyboard
	bot.Send(msg)
}

func handleText(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	userID := msg.From.ID
	chatID := msg.Chat.ID

	statesMu.Lock()
	state, ok := userStates[userID]
	if !ok || state.Step != "entering_name" {
		statesMu.Unlock()
		sendMainMenu(bot, chatID)
		return
	}
	trafficGB := state.TrafficGB
	days := state.DaysExpire
	clientName := strings.TrimSpace(msg.Text)
	delete(userStates, userID)
	statesMu.Unlock()

	validName := regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	if clientName == "" || !validName.MatchString(clientName) {
		m := tgbotapi.NewMessage(chatID, "❌ Имя должно содержать только латиницу, цифры, дефис или подчёркивание.\nПопробуйте ещё раз:")
		bot.Send(m)
		statesMu.Lock()
		userStates[userID] = &UserState{Step: "entering_name", TrafficGB: trafficGB, DaysExpire: days}
		statesMu.Unlock()
		return
	}

	finishClientCreation(bot, chatID, userID, clientName, trafficGB, days)
}

// Remnawave API calls

func remnawaveRequest(method, path string, body interface{}) ([]byte, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, remnawaveAPI+path, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+remnawaveToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

func createRemnawaveUser(user CreateUserRequest) (*RemnawaveUser, error) {
	data, err := remnawaveRequest("POST", "/api/users", user)
	if err != nil {
		return nil, err
	}

	var resp RemnawaveResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &resp.Response, nil
}

func getUserByTelegramID(telegramID int64) (*RemnawaveUser, error) {
	data, err := remnawaveRequest("GET", fmt.Sprintf("/api/users/by-telegram-id/%d", telegramID), nil)
	if err != nil {
		return nil, err
	}

	var resp RemnawaveResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &resp.Response, nil
}

func getInternalSquads() ([]InternalSquad, error) {
	data, err := remnawaveRequest("GET", "/api/internal-squads", nil)
	if err != nil {
		return nil, err
	}

	var resp InternalSquadsResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return resp.Response, nil
}

func getInbounds() ([]Inbound, error) {
	data, err := remnawaveRequest("GET", "/api/inbounds", nil)
	if err != nil {
		return nil, err
	}

	var resp InboundsResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return resp.Response, nil
}
