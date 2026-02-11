package main

import (
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Reminder хранит информацию о напоминании
type Reminder struct {
	ID       int
	Medicine string
	Hour     int
	Minute   int
}

func (r Reminder) TimeString() string {
	return fmt.Sprintf("%02d:%02d", r.Hour, r.Minute)
}

// UserState определяет текущее состояние диалога
type UserState int

const (
	StateNone UserState = iota
	StateWaitingMedicine
	StateWaitingHour
	StateWaitingMinute
)

// User хранит информацию о пользователе
type User struct {
	ChatID    int64
	Active    bool
	Reminders []Reminder
	NextID    int

	// Состояние для пошагового создания напоминания
	State           UserState
	PendingMedicine string
	PendingHour     int
	PendingMsgID    int // ID сообщения для редактирования
}

type Bot struct {
	api     *tgbotapi.BotAPI
	users   map[int64]*User
	mu      sync.RWMutex
	adminID int64
}

func NewBot(token string) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("failed to create bot: %w", err)
	}

	log.Printf("Authorized on account %s", api.Self.UserName)

	descParams := tgbotapi.Params{}
	descParams.AddNonEmpty("description", "Бот для напоминаний о приёме лекарств. Добавляй свои лекарства и время — я напомню!")
	if _, err := api.MakeRequest("setMyDescription", descParams); err != nil {
		log.Printf("Failed to set bot description: %v", err)
	}

	commands := tgbotapi.NewSetMyCommands(
		tgbotapi.BotCommand{Command: "start", Description: "Начать работу"},
		tgbotapi.BotCommand{Command: "add", Description: "Добавить напоминание"},
		tgbotapi.BotCommand{Command: "list", Description: "Мои напоминания"},
		tgbotapi.BotCommand{Command: "stop", Description: "Отключить напоминания"},
		tgbotapi.BotCommand{Command: "stats", Description: "Статистика бота"},
	)
	if _, err := api.Request(commands); err != nil {
		log.Printf("Failed to set bot commands: %v", err)
	}

	var adminID int64
	if adminStr := os.Getenv("ADMIN_ID"); adminStr != "" {
		adminID, _ = strconv.ParseInt(adminStr, 10, 64)
		log.Printf("Admin ID set to: %d", adminID)
	}

	return &Bot{
		api:     api,
		users:   make(map[int64]*User),
		adminID: adminID,
	}, nil
}

func (b *Bot) HandleUpdates() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := b.api.GetUpdatesChan(u)

	for update := range updates {
		// Обработка callback-кнопок
		if update.CallbackQuery != nil {
			log.Printf("[CALLBACK] user=%s (id=%d) data=%s",
				update.CallbackQuery.From.UserName,
				update.CallbackQuery.From.ID,
				update.CallbackQuery.Data)
			b.handleCallback(update.CallbackQuery)
			continue
		}

		if update.Message == nil {
			continue
		}

		chatID := update.Message.Chat.ID
		userName := update.Message.From.UserName
		if userName == "" {
			userName = update.Message.From.FirstName
		}
		log.Printf("[MSG] user=%s (id=%d) text=%q", userName, chatID, update.Message.Text)

		// Проверяем состояние пользователя
		b.mu.RLock()
		user, exists := b.users[chatID]
		state := StateNone
		if exists {
			state = user.State
		}
		b.mu.RUnlock()

		// Если ждём ввода названия лекарства
		if state == StateWaitingMedicine && !update.Message.IsCommand() {
			b.handleMedicineInput(update.Message)
			continue
		}

		if update.Message.IsCommand() {
			// Сбрасываем состояние при любой команде
			b.mu.Lock()
			if user, exists := b.users[chatID]; exists {
				user.State = StateNone
			}
			b.mu.Unlock()

			switch update.Message.Command() {
			case "start":
				b.handleStart(update.Message)
			case "add":
				b.handleAdd(update.Message)
			case "list":
				b.handleList(update.Message)
			case "stop":
				b.handleStop(update.Message)
			case "stats":
				b.handleStats(update.Message)
			}
			continue
		}

		// Обработка нажатий reply-кнопок
		text := update.Message.Text
		switch {
		case strings.Contains(text, "Добавить"):
			b.handleAdd(update.Message)
		case strings.Contains(text, "напоминания"):
			b.handleList(update.Message)
		case strings.Contains(text, "Отключить"):
			b.handleStop(update.Message)
		case strings.Contains(text, "Включить"):
			b.handleStart(update.Message)
		case strings.ToLower(text) == "привет":
			b.sendMessage(chatID, "Привет! Я бот для напоминаний о лекарствах. Используй /start чтобы начать.")
		}
	}
}

func (b *Bot) handleCallback(callback *tgbotapi.CallbackQuery) {
	chatID := callback.Message.Chat.ID
	data := callback.Data

	// Подтверждаем получение callback
	b.api.Request(tgbotapi.NewCallback(callback.ID, ""))

	switch {
	case strings.HasPrefix(data, "hour_"):
		// Выбран час
		hourStr := strings.TrimPrefix(data, "hour_")
		hour, _ := strconv.Atoi(hourStr)
		b.handleHourSelected(chatID, callback.Message.MessageID, hour)

	case strings.HasPrefix(data, "time_"):
		// Выбрано полное время (час:минута)
		timeStr := strings.TrimPrefix(data, "time_")
		parts := strings.Split(timeStr, ":")
		if len(parts) == 2 {
			hour, _ := strconv.Atoi(parts[0])
			minute, _ := strconv.Atoi(parts[1])
			b.handleTimeSelected(chatID, callback.Message.MessageID, hour, minute)
		}

	case strings.HasPrefix(data, "del_"):
		// Удаление напоминания
		idStr := strings.TrimPrefix(data, "del_")
		id, _ := strconv.Atoi(idStr)
		b.handleDeleteReminder(chatID, callback.Message.MessageID, id)

	case data == "cancel":
		b.mu.Lock()
		if user, exists := b.users[chatID]; exists {
			user.State = StateNone
		}
		b.mu.Unlock()
		b.deleteMessage(chatID, callback.Message.MessageID)
		b.sendMessage(chatID, "Отменено")
	}
}

func (b *Bot) handleStart(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID

	b.mu.Lock()
	if _, exists := b.users[chatID]; !exists {
		b.users[chatID] = &User{ChatID: chatID, Active: true, NextID: 1}
	} else {
		b.users[chatID].Active = true
	}
	b.mu.Unlock()

	text := "Привет! Я помогу тебе не забывать принимать лекарства.\n\n" +
		"Используй кнопки ниже или команды:\n" +
		"/add — добавить напоминание\n" +
		"/list — список напоминаний"

	keyboard := b.getMainKeyboard(true)

	reply := tgbotapi.NewMessage(chatID, text)
	reply.ReplyMarkup = keyboard
	if _, err := b.api.Send(reply); err != nil {
		log.Printf("Failed to send message to %d: %v", chatID, err)
	}
}

func (b *Bot) handleAdd(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID

	b.mu.Lock()
	if _, exists := b.users[chatID]; !exists {
		b.users[chatID] = &User{ChatID: chatID, Active: true, NextID: 1}
	}
	b.users[chatID].State = StateWaitingMedicine
	b.users[chatID].PendingMedicine = ""
	b.mu.Unlock()

	// Просим ввести название лекарства
	cancelKeyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("❌ Отмена", "cancel"),
		),
	)

	reply := tgbotapi.NewMessage(chatID, "Введи название лекарства:")
	reply.ReplyMarkup = cancelKeyboard
	if _, err := b.api.Send(reply); err != nil {
		log.Printf("Failed to send message: %v", err)
	}
}

func (b *Bot) handleMedicineInput(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	medicine := strings.TrimSpace(msg.Text)

	if medicine == "" {
		b.sendMessage(chatID, "Название не может быть пустым. Попробуй ещё раз:")
		return
	}

	b.mu.Lock()
	user := b.users[chatID]
	user.PendingMedicine = medicine
	user.State = StateWaitingHour
	b.mu.Unlock()

	// Показываем выбор часа
	b.showHourSelection(chatID, medicine)
}

func (b *Bot) showHourSelection(chatID int64, medicine string) {
	var rows [][]tgbotapi.InlineKeyboardButton

	// Утро: 6-11
	row1 := []tgbotapi.InlineKeyboardButton{}
	for h := 6; h <= 11; h++ {
		row1 = append(row1, tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("%02d", h), fmt.Sprintf("hour_%d", h)))
	}
	rows = append(rows, row1)

	// День: 12-17
	row2 := []tgbotapi.InlineKeyboardButton{}
	for h := 12; h <= 17; h++ {
		row2 = append(row2, tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("%02d", h), fmt.Sprintf("hour_%d", h)))
	}
	rows = append(rows, row2)

	// Вечер: 18-23
	row3 := []tgbotapi.InlineKeyboardButton{}
	for h := 18; h <= 23; h++ {
		row3 = append(row3, tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("%02d", h), fmt.Sprintf("hour_%d", h)))
	}
	rows = append(rows, row3)

	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("❌ Отмена", "cancel"),
	})

	keyboard := tgbotapi.NewInlineKeyboardMarkup(rows...)

	reply := tgbotapi.NewMessage(chatID, fmt.Sprintf("💊 %s\n\nВыбери час (Часовой пояс: Екатеринбург):", medicine))
	reply.ReplyMarkup = keyboard
	if _, err := b.api.Send(reply); err != nil {
		log.Printf("Failed to send message: %v", err)
	}
}

func (b *Bot) handleHourSelected(chatID int64, messageID int, hour int) {
	b.mu.Lock()
	user, exists := b.users[chatID]
	if !exists || user.PendingMedicine == "" {
		b.mu.Unlock()
		b.deleteMessage(chatID, messageID)
		b.sendMessage(chatID, "Ошибка. Попробуй снова: /add")
		return
	}
	medicine := user.PendingMedicine
	user.PendingHour = hour
	user.State = StateWaitingMinute
	b.mu.Unlock()

	// Показываем выбор минут
	minutes := []int{0, 15, 30, 45}
	var row []tgbotapi.InlineKeyboardButton
	for _, m := range minutes {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(
			fmt.Sprintf("%02d:%02d", hour, m),
			fmt.Sprintf("time_%d:%d", hour, m),
		))
	}

	rows := [][]tgbotapi.InlineKeyboardButton{
		row,
		{tgbotapi.NewInlineKeyboardButtonData("❌ Отмена", "cancel")},
	}

	keyboard := tgbotapi.NewInlineKeyboardMarkup(rows...)

	edit := tgbotapi.NewEditMessageText(chatID, messageID, fmt.Sprintf("💊 %s\n\nВыбери точное время (Часовой пояс: Екатеринбург):", medicine))
	edit.ReplyMarkup = &keyboard
	if _, err := b.api.Send(edit); err != nil {
		log.Printf("Failed to edit message: %v", err)
	}
}

func (b *Bot) handleTimeSelected(chatID int64, messageID int, hour, minute int) {
	b.mu.Lock()
	user, exists := b.users[chatID]
	if !exists || user.PendingMedicine == "" {
		b.mu.Unlock()
		b.deleteMessage(chatID, messageID)
		b.sendMessage(chatID, "Ошибка. Попробуй снова: /add")
		return
	}

	reminder := Reminder{
		ID:       user.NextID,
		Medicine: user.PendingMedicine,
		Hour:     hour,
		Minute:   minute,
	}
	user.NextID++
	user.Reminders = append(user.Reminders, reminder)
	user.PendingMedicine = ""
	user.State = StateNone
	user.Active = true
	b.mu.Unlock()

	b.deleteMessage(chatID, messageID)

	text := fmt.Sprintf("✅ Напоминание добавлено!\n\n💊 %s\n⏰ %s\n\nИспользуй /list чтобы увидеть все напоминания",
		reminder.Medicine, reminder.TimeString())
	b.sendMessage(chatID, text)
}

func (b *Bot) handleList(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID

	b.mu.RLock()
	user, exists := b.users[chatID]
	var reminders []Reminder
	if exists {
		reminders = make([]Reminder, len(user.Reminders))
		copy(reminders, user.Reminders)
	}
	b.mu.RUnlock()

	if !exists || len(reminders) == 0 {
		b.sendMessage(chatID, "У тебя пока нет напоминаний.\n\nИспользуй /add чтобы добавить")
		return
	}

	// Сортируем по времени
	sort.Slice(reminders, func(i, j int) bool {
		if reminders[i].Hour != reminders[j].Hour {
			return reminders[i].Hour < reminders[j].Hour
		}
		return reminders[i].Minute < reminders[j].Minute
	})

	var text strings.Builder
	text.WriteString("📋 Твои напоминания (часовой пояс Екатеринбург):\n\n")

	for _, r := range reminders {
		text.WriteString(fmt.Sprintf("⏰ %s — 💊 %s\n", r.TimeString(), r.Medicine))
	}

	// Кнопки удаления
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, r := range reminders {
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(
				fmt.Sprintf("🗑 Удалить: %s %s", r.TimeString(), r.Medicine),
				fmt.Sprintf("del_%d", r.ID),
			),
		})
	}

	keyboard := tgbotapi.NewInlineKeyboardMarkup(rows...)

	reply := tgbotapi.NewMessage(chatID, text.String())
	reply.ReplyMarkup = keyboard
	if _, err := b.api.Send(reply); err != nil {
		log.Printf("Failed to send message: %v", err)
	}
}

func (b *Bot) handleDeleteReminder(chatID int64, messageID int, reminderID int) {
	b.mu.Lock()
	user, exists := b.users[chatID]
	if exists {
		for i, r := range user.Reminders {
			if r.ID == reminderID {
				user.Reminders = append(user.Reminders[:i], user.Reminders[i+1:]...)
				break
			}
		}
	}
	b.mu.Unlock()

	b.deleteMessage(chatID, messageID)
	b.sendMessage(chatID, "🗑 Напоминание удалено")
}

func (b *Bot) handleStats(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID

	// Проверка прав администратора
	if b.adminID != 0 && chatID != b.adminID {
		b.sendMessage(chatID, "⛔ Эта команда доступна только администратору")
		return
	}

	b.mu.RLock()
	totalUsers := len(b.users)
	activeUsers := 0
	totalReminders := 0
	for _, user := range b.users {
		if user.Active {
			activeUsers++
		}
		totalReminders += len(user.Reminders)
	}
	b.mu.RUnlock()

	text := fmt.Sprintf("📊 Статистика бота:\n\n"+
		"👥 Всего пользователей: %d\n"+
		"✅ Активных: %d\n"+
		"💊 Всего напоминаний: %d",
		totalUsers, activeUsers, totalReminders)

	b.sendMessage(chatID, text)
}

func (b *Bot) handleStop(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID

	b.mu.Lock()
	if user, ok := b.users[chatID]; ok {
		user.Active = false
	}
	b.mu.Unlock()

	keyboard := b.getMainKeyboard(false)

	reply := tgbotapi.NewMessage(chatID, "⏸ Напоминания отключены.\n\nТвои настройки сохранены.")
	reply.ReplyMarkup = keyboard
	if _, err := b.api.Send(reply); err != nil {
		log.Printf("Failed to send message to %d: %v", chatID, err)
	}
}

func (b *Bot) getMainKeyboard(active bool) tgbotapi.ReplyKeyboardMarkup {
	var keyboard tgbotapi.ReplyKeyboardMarkup
	if active {
		keyboard = tgbotapi.NewReplyKeyboard(
			tgbotapi.NewKeyboardButtonRow(
				tgbotapi.NewKeyboardButton("➕ Добавить"),
				tgbotapi.NewKeyboardButton("📋 Мои напоминания"),
			),
			tgbotapi.NewKeyboardButtonRow(
				tgbotapi.NewKeyboardButton("⏸ Отключить"),
			),
		)
	} else {
		keyboard = tgbotapi.NewReplyKeyboard(
			tgbotapi.NewKeyboardButtonRow(
				tgbotapi.NewKeyboardButton("▶️ Включить"),
			),
		)
	}
	keyboard.ResizeKeyboard = true
	return keyboard
}

func (b *Bot) sendMessage(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("Failed to send message to %d: %v", chatID, err)
	}
}

func (b *Bot) deleteMessage(chatID int64, messageID int) {
	del := tgbotapi.NewDeleteMessage(chatID, messageID)
	if _, err := b.api.Request(del); err != nil {
		log.Printf("Failed to delete message: %v", err)
	}
}

// GetRemindersForTime возвращает список напоминаний для указанного времени
func (b *Bot) GetRemindersForTime(hour, minute int) map[int64][]Reminder {
	b.mu.RLock()
	defer b.mu.RUnlock()

	result := make(map[int64][]Reminder)
	for chatID, user := range b.users {
		if !user.Active {
			continue
		}
		for _, r := range user.Reminders {
			if r.Hour == hour && r.Minute == minute {
				result[chatID] = append(result[chatID], r)
			}
		}
	}
	return result
}
