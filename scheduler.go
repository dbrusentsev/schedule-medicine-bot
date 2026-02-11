package main

import (
	"fmt"
	"log"
	"time"
)

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
				text := fmt.Sprintf("⏰ Время принять: 💊 %s", r.Medicine)
				bot.sendMessage(chatID, text)
			}
		}
	}
}
