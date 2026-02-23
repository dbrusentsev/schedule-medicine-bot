package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN is not set")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is not set")
	}

	storage, err := NewStorage(databaseURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer storage.Close()

	bot, err := NewBot(token, storage)
	if err != nil {
		log.Fatalf("Failed to create bot: %v", err)
	}

	// Запускаем HTTP сервер для Web App
	go startWebServer(bot)

	go StartScheduler(bot)
	bot.HandleUpdates()
}

func startWebServer(bot *Bot) {
	port := os.Getenv("WEB_PORT")
	if port == "" {
		port = "8080"
	}

	// Статические файлы
	http.Handle("/", http.FileServer(http.Dir("web")))

	// API для получения напоминаний
	http.HandleFunc("/api/reminders", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		// Получаем chatID из Telegram Web App initData
		// В продакшене нужно валидировать initData!
		initData := r.Header.Get("X-Telegram-Init-Data")
		if initData == "" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}

		// Парсим user_id из initData (упрощённо)
		chatID := bot.parseUserFromInitData(initData)
		if chatID == 0 {
			http.Error(w, `{"error":"invalid user"}`, http.StatusBadRequest)
			return
		}

		reminders := bot.GetUserReminders(chatID)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"reminders": reminders,
		})
	})

	log.Printf("Starting web server on :%s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Printf("Web server error: %v", err)
	}
}

func StartScheduler(bot *Bot) {
	loc, err := time.LoadLocation("Asia/Yekaterinburg")
	if err != nil {
		log.Fatalf("Failed to load timezone: %v", err)
	}

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	var lastSentTime string

	for range ticker.C {
		now := time.Now().In(loc)
		hour := now.Hour()
		minute := now.Minute()

		// Проверяем только в нужные минуты (0, 15, 30, 45)
		if minute != 0 && minute != 15 && minute != 30 && minute != 45 {
			lastSentTime = ""
			continue
		}

		currentTime := fmt.Sprintf("%02d:%02d", hour, minute)
		if currentTime == lastSentTime {
			continue
		}

		// Получаем напоминания для текущего времени
		reminders := bot.GetRemindersForTime(hour, minute)
		if len(reminders) == 0 {
			continue
		}

		lastSentTime = currentTime

		log.Printf("Sending reminders at %s to %d users", currentTime, len(reminders))

		for chatID, userReminders := range reminders {
			for _, r := range userReminders {
				text := fmt.Sprintf("⏰ Время принять: 💊 %s\n📊 Приём: %s", r.Medicine, r.CourseString())
				bot.sendReminderWithButton(chatID, text, r.ID)
			}
		}
	}
}
