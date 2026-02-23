package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Reminder хранит информацию о напоминании
type Reminder struct {
	ID         int
	Medicine   string
	Hour       int
	Minute     int
	CourseDays int // Количество дней курса (0 = бесконечно)
	DosesTaken int // Количество отправленных напоминаний (счётчик)
}

func (r Reminder) TimeString() string {
	return fmt.Sprintf("%02d:%02d", r.Hour, r.Minute)
}

// CourseString возвращает строку прогресса курса
func (r Reminder) CourseString() string {
	if r.CourseDays == 0 {
		return fmt.Sprintf("%d/∞", r.DosesTaken)
	}
	return fmt.Sprintf("%d/%d", r.DosesTaken, r.CourseDays)
}

// IsCompleted проверяет, завершён ли курс
func (r Reminder) IsCompleted() bool {
	return r.CourseDays > 0 && r.DosesTaken >= r.CourseDays
}

// UserState определяет текущее состояние диалога
type UserState int

const (
	StateNone UserState = iota
	StateWaitingMedicine
	StateWaitingHour
	StateWaitingMinute
	StateWaitingCourse       // Ожидание выбора длительности курса
	StateWaitingCustomCourse // Ожидание ввода своего количества дней
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
	PendingMinute   int
	PendingMsgID    int // ID сообщения для редактирования
}

// PendingReminder хранит временное состояние создания напоминания
type PendingReminder struct {
	State    UserState
	Medicine string
	Hour     int
	Minute   int
	MsgID    int
}

type Bot struct {
	api     *tgbotapi.BotAPI
	storage *Storage
	pending map[int64]*PendingReminder // временные состояния диалогов
	mu      sync.RWMutex
	adminID int64
}

func NewBot(token string, storage *Storage) (*Bot, error) {
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
		tgbotapi.BotCommand{Command: "donate", Description: "Поддержать автора"},
		tgbotapi.BotCommand{Command: "stats", Description: "Статистика бота"},
	)
	if _, err := api.Request(commands); err != nil {
		log.Printf("Failed to set bot commands: %v", err)
	}

	// Устанавливаем Menu Button
	// Если есть WEBAPP_URL - показываем кнопку Web App, иначе - меню команд
	webAppURL := os.Getenv("WEBAPP_URL")
	menuParams := tgbotapi.Params{}
	if webAppURL != "" {
		menuParams.AddNonEmpty("menu_button", fmt.Sprintf(`{"type":"web_app","text":"📊 История","web_app":{"url":"%s"}}`, webAppURL))
		log.Printf("Web App URL: %s", webAppURL)
	} else {
		menuParams.AddNonEmpty("menu_button", `{"type":"commands"}`)
	}
	if _, err := api.MakeRequest("setChatMenuButton", menuParams); err != nil {
		log.Printf("Failed to set menu button: %v", err)
	}

	var adminID int64
	if adminStr := os.Getenv("ADMIN_ID"); adminStr != "" {
		adminID, _ = strconv.ParseInt(adminStr, 10, 64)
		log.Printf("Admin ID set to: %d", adminID)
	}

	return &Bot{
		api:     api,
		storage: storage,
		pending: make(map[int64]*PendingReminder),
		adminID: adminID,
	}, nil
}

func (b *Bot) HandleUpdates() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := b.api.GetUpdatesChan(u)

	for update := range updates {
		// Обработка pre-checkout запросов (для Telegram Stars)
		if update.PreCheckoutQuery != nil {
			b.handlePreCheckout(update.PreCheckoutQuery)
			continue
		}

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

		// Обработка успешного платежа
		if update.Message.SuccessfulPayment != nil {
			b.handleSuccessfulPayment(update.Message)
			continue
		}

		chatID := update.Message.Chat.ID
		userName := update.Message.From.UserName
		if userName == "" {
			userName = update.Message.From.FirstName
		}
		log.Printf("[MSG] user=%s (id=%d) text=%q", userName, chatID, update.Message.Text)

		// Проверяем состояние пользователя (из pending map)
		b.mu.RLock()
		pending := b.pending[chatID]
		state := StateNone
		if pending != nil {
			state = pending.State
		}
		b.mu.RUnlock()

		// Если ждём ввода названия лекарства
		if state == StateWaitingMedicine && !update.Message.IsCommand() {
			b.handleMedicineInput(update.Message)
			continue
		}

		// Если ждём ввода своего количества дней курса
		if state == StateWaitingCustomCourse && !update.Message.IsCommand() {
			b.handleCustomCourseInput(update.Message)
			continue
		}

		if update.Message.IsCommand() {
			// Сбрасываем состояние при любой команде
			b.mu.Lock()
			delete(b.pending, chatID)
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
			case "donate":
				b.handleDonate(update.Message)
			case "stats":
				b.handleStats(update.Message)
			case "notify":
				b.handleNotify(update.Message)
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

	case strings.HasPrefix(data, "course_"):
		// Выбор длительности курса
		courseStr := strings.TrimPrefix(data, "course_")
		if courseStr == "custom" {
			// Пользователь хочет ввести своё значение
			b.mu.Lock()
			if p := b.pending[chatID]; p != nil {
				p.State = StateWaitingCustomCourse
				p.MsgID = callback.Message.MessageID
			}
			b.mu.Unlock()
			b.deleteMessage(chatID, callback.Message.MessageID)
			b.sendMessage(chatID, "Введи количество дней курса (число от 1 до 365):")
		} else {
			courseDays, _ := strconv.Atoi(courseStr)
			b.handleCourseSelected(chatID, callback.Message.MessageID, courseDays)
		}

	case strings.HasPrefix(data, "taken_"):
		// Подтверждение приёма лекарства
		idStr := strings.TrimPrefix(data, "taken_")
		id, _ := strconv.Atoi(idStr)
		b.handleTakenConfirm(chatID, callback.Message.MessageID, id)

	case strings.HasPrefix(data, "stars_"):
		// Выбор суммы доната
		amountStr := strings.TrimPrefix(data, "stars_")
		amount, _ := strconv.Atoi(amountStr)
		b.deleteMessage(chatID, callback.Message.MessageID)
		b.sendStarsInvoice(chatID, amount)

	case data == "cancel":
		b.mu.Lock()
		delete(b.pending, chatID)
		b.mu.Unlock()
		b.deleteMessage(chatID, callback.Message.MessageID)
		b.sendMessage(chatID, "Отменено")
	}
}

func (b *Bot) handleStart(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID

	if _, err := b.storage.GetOrCreateUser(chatID); err != nil {
		log.Printf("Failed to create user %d: %v", chatID, err)
	}
	if err := b.storage.SetUserActive(chatID, true); err != nil {
		log.Printf("Failed to set user active %d: %v", chatID, err)
	}

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

	if _, err := b.storage.GetOrCreateUser(chatID); err != nil {
		log.Printf("Failed to create user %d: %v", chatID, err)
	}

	b.mu.Lock()
	b.pending[chatID] = &PendingReminder{State: StateWaitingMedicine}
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
	if p := b.pending[chatID]; p != nil {
		p.Medicine = medicine
		p.State = StateWaitingHour
	}
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
	p := b.pending[chatID]
	if p == nil || p.Medicine == "" {
		b.mu.Unlock()
		b.deleteMessage(chatID, messageID)
		b.sendMessage(chatID, "Ошибка. Попробуй снова: /add")
		return
	}
	medicine := p.Medicine
	p.Hour = hour
	p.State = StateWaitingMinute
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
	p := b.pending[chatID]
	if p == nil || p.Medicine == "" {
		b.mu.Unlock()
		b.deleteMessage(chatID, messageID)
		b.sendMessage(chatID, "Ошибка. Попробуй снова: /add")
		return
	}

	// Сохраняем выбранное время и переходим к выбору курса
	p.Hour = hour
	p.Minute = minute
	p.State = StateWaitingCourse
	medicine := p.Medicine
	b.mu.Unlock()

	// Показываем выбор длительности курса
	b.showCourseSelection(chatID, messageID, medicine, hour, minute)
}

func (b *Bot) showCourseSelection(chatID int64, messageID int, medicine string, hour, minute int) {
	rows := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("7 дней", "course_7"),
			tgbotapi.NewInlineKeyboardButtonData("14 дней", "course_14"),
			tgbotapi.NewInlineKeyboardButtonData("21 день", "course_21"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("30 дней", "course_30"),
			tgbotapi.NewInlineKeyboardButtonData("60 дней", "course_60"),
			tgbotapi.NewInlineKeyboardButtonData("90 дней", "course_90"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("♾ Бесконечно", "course_0"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("✏️ Ввести своё", "course_custom"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("❌ Отмена", "cancel"),
		},
	}

	keyboard := tgbotapi.NewInlineKeyboardMarkup(rows...)

	text := fmt.Sprintf("💊 %s\n⏰ %02d:%02d\n\nВыбери длительность курса:", medicine, hour, minute)
	edit := tgbotapi.NewEditMessageText(chatID, messageID, text)
	edit.ReplyMarkup = &keyboard
	if _, err := b.api.Send(edit); err != nil {
		log.Printf("Failed to edit message: %v", err)
	}
}

func (b *Bot) handleCourseSelected(chatID int64, messageID int, courseDays int) {
	b.mu.Lock()
	p := b.pending[chatID]
	if p == nil || p.Medicine == "" {
		b.mu.Unlock()
		b.deleteMessage(chatID, messageID)
		b.sendMessage(chatID, "Ошибка. Попробуй снова: /add")
		return
	}

	medicine := p.Medicine
	hour := p.Hour
	minute := p.Minute
	delete(b.pending, chatID)
	b.mu.Unlock()

	// Сохраняем в БД
	_, err := b.storage.AddReminder(chatID, medicine, hour, minute, courseDays)
	if err != nil {
		log.Printf("Failed to add reminder: %v", err)
		b.sendMessage(chatID, "Ошибка сохранения. Попробуй снова: /add")
		return
	}

	b.storage.SetUserActive(chatID, true)
	b.deleteMessage(chatID, messageID)

	courseStr := "♾ Бесконечно"
	if courseDays > 0 {
		courseStr = fmt.Sprintf("%d дней", courseDays)
	}

	text := fmt.Sprintf("✅ Напоминание добавлено!\n\n💊 %s\n⏰ %02d:%02d\n📅 Курс: %s\n\nИспользуй /list чтобы увидеть все напоминания",
		medicine, hour, minute, courseStr)
	b.sendMessage(chatID, text)
}

func (b *Bot) handleCustomCourseInput(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	text := strings.TrimSpace(msg.Text)

	courseDays, err := strconv.Atoi(text)
	if err != nil || courseDays < 1 || courseDays > 365 {
		b.sendMessage(chatID, "Пожалуйста, введи число от 1 до 365:")
		return
	}

	b.mu.Lock()
	p := b.pending[chatID]
	if p == nil || p.Medicine == "" {
		b.mu.Unlock()
		b.sendMessage(chatID, "Ошибка. Попробуй снова: /add")
		return
	}

	medicine := p.Medicine
	hour := p.Hour
	minute := p.Minute
	delete(b.pending, chatID)
	b.mu.Unlock()

	// Сохраняем в БД
	_, err = b.storage.AddReminder(chatID, medicine, hour, minute, courseDays)
	if err != nil {
		log.Printf("Failed to add reminder: %v", err)
		b.sendMessage(chatID, "Ошибка сохранения. Попробуй снова: /add")
		return
	}

	b.storage.SetUserActive(chatID, true)

	resultText := fmt.Sprintf("✅ Напоминание добавлено!\n\n💊 %s\n⏰ %02d:%02d\n📅 Курс: %d дней\n\nИспользуй /list чтобы увидеть все напоминания",
		medicine, hour, minute, courseDays)
	b.sendMessage(chatID, resultText)
}

func (b *Bot) handleList(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID

	reminders, err := b.storage.GetReminders(chatID)
	if err != nil {
		log.Printf("Failed to get reminders: %v", err)
		b.sendMessage(chatID, "Ошибка загрузки напоминаний")
		return
	}

	if len(reminders) == 0 {
		b.sendMessage(chatID, "У тебя пока нет напоминаний.\n\nИспользуй /add чтобы добавить")
		return
	}

	// Уже отсортированы в storage.GetReminders

	var text strings.Builder
	text.WriteString("📋 Твои напоминания (часовой пояс Екатеринбург):\n\n")

	for _, r := range reminders {
		text.WriteString(fmt.Sprintf("⏰ %s — 💊 %s — 📊 %s\n", r.TimeString(), r.Medicine, r.CourseString()))
	}

	// Кнопки удаления
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, r := range reminders {
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(
				fmt.Sprintf("🗑 %s %s [%s]", r.TimeString(), r.Medicine, r.CourseString()),
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
	if err := b.storage.DeleteReminder(chatID, reminderID); err != nil {
		log.Printf("Failed to delete reminder: %v", err)
	}

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

	totalUsers, activeUsers, totalReminders, finiteCourses, infiniteCourses, totalDosesTaken, totalDosesPlanned, err := b.storage.GetStats()
	if err != nil {
		log.Printf("Failed to get stats: %v", err)
		b.sendMessage(chatID, "Ошибка загрузки статистики")
		return
	}

	text := fmt.Sprintf("📊 Статистика бота:\n\n"+
		"👥 Всего пользователей: %d\n"+
		"✅ Активных: %d\n\n"+
		"💊 Всего напоминаний: %d\n"+
		"   📅 Курсов с датой окончания: %d\n"+
		"   ♾ Бесконечных курсов: %d\n\n"+
		"📈 Принято доз: %d\n"+
		"📋 Запланировано доз: %d",
		totalUsers, activeUsers, totalReminders, finiteCourses, infiniteCourses, totalDosesTaken, totalDosesPlanned)

	b.sendMessage(chatID, text)
}

func (b *Bot) handleStop(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID

	if err := b.storage.SetUserActive(chatID, false); err != nil {
		log.Printf("Failed to deactivate user %d: %v", chatID, err)
	}

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

// sendReminderWithButton отправляет напоминание с кнопкой "Принял"
func (b *Bot) sendReminderWithButton(chatID int64, text string, reminderID int) {
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Принял", fmt.Sprintf("taken_%d", reminderID)),
		),
	)

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = keyboard
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("Failed to send reminder to %d: %v", chatID, err)
	}
}

// handleTakenConfirm обрабатывает подтверждение приёма лекарства
func (b *Bot) handleTakenConfirm(chatID int64, messageID int, reminderID int) {
	// Инкрементируем счётчик
	medicineName, newCount, total, completed := b.IncrementDoseTaken(chatID, reminderID)

	if medicineName == "" {
		// Напоминание не найдено (возможно уже удалено)
		b.deleteMessage(chatID, messageID)
		return
	}

	// Формируем строку прогресса
	var progressStr string
	if total == 0 {
		progressStr = fmt.Sprintf("%d/∞", newCount)
	} else {
		progressStr = fmt.Sprintf("%d/%d", newCount, total)
	}

	// Обновляем сообщение — убираем кнопку, показываем подтверждение
	text := fmt.Sprintf("✅ Принято: 💊 %s\n📊 Приём: %s", medicineName, progressStr)
	edit := tgbotapi.NewEditMessageText(chatID, messageID, text)
	if _, err := b.api.Send(edit); err != nil {
		log.Printf("Failed to edit message: %v", err)
	}

	// Если курс завершён, отправляем поздравление
	if completed {
		b.sendMessage(chatID, fmt.Sprintf("🎉 Курс \"%s\" завершён! Ты молодец!", medicineName))
	}
}

// ReminderJSON структура для JSON ответа
type ReminderJSON struct {
	ID         int    `json:"id"`
	Medicine   string `json:"medicine"`
	Time       string `json:"time"`
	CourseDays int    `json:"course_days"`
	DosesTaken int    `json:"doses_taken"`
}

// GetUserReminders возвращает напоминания пользователя для API
func (b *Bot) GetUserReminders(chatID int64) []ReminderJSON {
	reminders, err := b.storage.GetReminders(chatID)
	if err != nil {
		log.Printf("Failed to get reminders for API: %v", err)
		return []ReminderJSON{}
	}

	result := make([]ReminderJSON, len(reminders))
	for i, r := range reminders {
		result[i] = ReminderJSON{
			ID:         r.ID,
			Medicine:   r.Medicine,
			Time:       r.TimeString(),
			CourseDays: r.CourseDays,
			DosesTaken: r.DosesTaken,
		}
	}
	return result
}

// parseUserFromInitData извлекает user_id из Telegram initData
func (b *Bot) parseUserFromInitData(initData string) int64 {
	// Упрощённый парсинг - в продакшене нужна полная валидация HMAC!
	// initData формат: query_id=...&user={"id":123,...}&auth_date=...&hash=...

	// Декодируем URL-encoded строку
	decoded, err := url.QueryUnescape(initData)
	if err != nil {
		return 0
	}

	// Ищем user= параметр
	params, err := url.ParseQuery(decoded)
	if err != nil {
		return 0
	}

	userJSON := params.Get("user")
	if userJSON == "" {
		return 0
	}

	var userData struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(userJSON), &userData); err != nil {
		return 0
	}

	return userData.ID
}

// GetRemindersForTime возвращает список напоминаний для указанного времени
func (b *Bot) GetRemindersForTime(hour, minute int) map[int64][]Reminder {
	result, err := b.storage.GetRemindersForTime(hour, minute)
	if err != nil {
		log.Printf("Failed to get reminders for time: %v", err)
		return make(map[int64][]Reminder)
	}
	return result
}

// IncrementDoseTaken увеличивает счётчик принятых доз и удаляет завершённые курсы
func (b *Bot) IncrementDoseTaken(chatID int64, reminderID int) (medicineName string, newCount int, total int, completed bool) {
	medicineName, newCount, total, completed, err := b.storage.IncrementDoseTaken(chatID, reminderID)
	if err != nil {
		log.Printf("Failed to increment dose: %v", err)
		return "", 0, 0, false
	}
	return medicineName, newCount, total, completed
}

// handleDonate отправляет меню выбора суммы доната
func (b *Bot) handleDonate(message *tgbotapi.Message) {
	chatID := message.Chat.ID

	// Показываем выбор суммы доната
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⭐ 1", "stars_1"),
			tgbotapi.NewInlineKeyboardButtonData("⭐ 5", "stars_5"),
			tgbotapi.NewInlineKeyboardButtonData("⭐ 10", "stars_10"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⭐ 50", "stars_50"),
			tgbotapi.NewInlineKeyboardButtonData("⭐ 100", "stars_100"),
		),
	)

	msg := tgbotapi.NewMessage(chatID, "Выбери сумму доната:\n\nТвоя поддержка помогает развивать бота! 💊")
	msg.ReplyMarkup = keyboard
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("Failed to send donate message: %v", err)
	}
}

// sendStarsInvoice отправляет инвойс для Telegram Stars
func (b *Bot) sendStarsInvoice(chatID int64, amount int) {
	invoice := tgbotapi.InvoiceConfig{
		BaseChat: tgbotapi.BaseChat{
			ChatID: chatID,
		},
		Title:               "Поддержать автора",
		Description:         fmt.Sprintf("Донат %d ⭐ — спасибо за поддержку!", amount),
		Payload:             fmt.Sprintf("donate_%d", amount),
		ProviderToken:       "", // Пустой для Telegram Stars
		Currency:            "XTR",
		Prices:              []tgbotapi.LabeledPrice{{Label: "Донат", Amount: amount}},
		SuggestedTipAmounts: []int{}, // Явно пустой массив
	}

	if _, err := b.api.Send(invoice); err != nil {
		log.Printf("Failed to send invoice: %v", err)
		b.sendMessage(chatID, "Не удалось создать платёж. Попробуй позже.")
	}
}

// handlePreCheckout подтверждает pre-checkout запрос
func (b *Bot) handlePreCheckout(query *tgbotapi.PreCheckoutQuery) {
	log.Printf("[PRECHECKOUT] user=%s amount=%d %s",
		query.From.UserName, query.TotalAmount, query.Currency)

	// Подтверждаем платёж
	callback := tgbotapi.PreCheckoutConfig{
		PreCheckoutQueryID: query.ID,
		OK:                 true,
	}

	if _, err := b.api.Request(callback); err != nil {
		log.Printf("Failed to answer pre-checkout: %v", err)
	}
}

// handleSuccessfulPayment обрабатывает успешный платёж
func (b *Bot) handleSuccessfulPayment(msg *tgbotapi.Message) {
	payment := msg.SuccessfulPayment
	log.Printf("[PAYMENT] user=%d amount=%d %s",
		msg.Chat.ID, payment.TotalAmount, payment.Currency)

	text := fmt.Sprintf("🎉 Спасибо за поддержку!\n\n"+
		"Получено: %d ⭐\n\n"+
		"Твоя поддержка очень важна для развития бота!",
		payment.TotalAmount)

	b.sendMessage(msg.Chat.ID, text)

	// Уведомляем админа о донате
	if b.adminID != 0 && msg.Chat.ID != b.adminID {
		adminText := fmt.Sprintf("💰 Новый донат!\n\nОт: @%s (ID: %d)\nСумма: %d ⭐",
			msg.From.UserName, msg.Chat.ID, payment.TotalAmount)
		b.sendMessage(b.adminID, adminText)
	}
}

// handleNotify отправляет уведомление всем пользователям (только для админа)
func (b *Bot) handleNotify(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID

	// Проверка прав администратора
	if b.adminID == 0 || chatID != b.adminID {
		b.sendMessage(chatID, "Эта команда доступна только администратору")
		return
	}

	// Получаем текст после команды
	text := strings.TrimSpace(strings.TrimPrefix(msg.Text, "/notify"))
	if text == "" {
		text = "Важное уведомление от бота!"
	}

	chatIDs, err := b.storage.GetAllUsers()
	if err != nil {
		log.Printf("Failed to get users for notify: %v", err)
		b.sendMessage(chatID, "Ошибка получения списка пользователей")
		return
	}

	sentCount := 0
	for _, id := range chatIDs {
		if err := b.sendMessageWithError(id, text); err == nil {
			sentCount++
		}
	}

	b.sendMessage(chatID, fmt.Sprintf("Уведомление отправлено %d из %d пользователей", sentCount, len(chatIDs)))
}

// sendMessageWithError отправляет сообщение и возвращает ошибку
func (b *Bot) sendMessageWithError(chatID int64, text string) error {
	msg := tgbotapi.NewMessage(chatID, text)
	_, err := b.api.Send(msg)
	if err != nil {
		log.Printf("Failed to send message to %d: %v", chatID, err)
	}
	return err
}
