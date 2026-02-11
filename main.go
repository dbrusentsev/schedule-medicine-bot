package main

import (
	"log"
	"os"
)

func main() {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN is not set")
	}

	bot, err := NewBot(token)
	if err != nil {
		log.Fatalf("Failed to create bot: %v", err)
	}

	go StartScheduler(bot)

	log.Println("Bot is running...")
	bot.HandleUpdates()
}
