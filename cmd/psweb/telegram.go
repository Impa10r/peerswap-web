package main

import (
	"log"
	"os"
	"path/filepath"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

var (
	chatId int64
	bot    *tgbotapi.BotAPI
)

func telegramStart() {
	if config.TelegramToken == "" {
		return
	}

	var err error
	// Initialize bot with token and wait for chat Id
	bot, err = tgbotapi.NewBotAPI(config.TelegramToken)
	if err != nil {
		log.Println("Error connecting to Telegram bot:", err)
		return
	}

	// Set bot to debug mode
	bot.Debug = false

	log.Printf("Authorized on account %s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	// Try saved chatId
	chatId = config.TelegramChatId
	if chatId > 0 && !telegramSendMessage("PeerSwap Web UI connected") {
		chatId = 0
	}

	updates := bot.GetUpdatesChan(u)

	// Process updates
	for update := range updates {
		if update.Message != nil {
			if chatId != update.Message.Chat.ID {
				chatId = update.Message.Chat.ID
				if telegramSendMessage("PeerSwap Web UI connected") {
					config.TelegramChatId = chatId
					saveConfig()
				} else {
					chatId = 0
				}
			}
		}
	}
}

func telegramSendMessage(msgText string) bool {
	msg := tgbotapi.NewMessage(chatId, msgText)
	_, err := bot.Send(msg)
	if err != nil {
		log.Println(err)
		return false
	}
	return true
}

func telegramSendFile(folder, fileName, satAmount string) error {
	// Open file
	file, err := os.Open(filepath.Join(folder, fileName))
	if err != nil {
		return err
	}
	defer file.Close()

	// Create file config
	fileConfig := tgbotapi.FileReader{Name: fileName, Reader: file}

	// Create message config
	msg := tgbotapi.NewDocument(chatId, fileConfig)

	msg.Caption = "🌊 L-BTC Balance: " + satAmount

	// Send file
	_, err = bot.Send(msg)
	if err != nil {
		return err
	}

	return nil
}
