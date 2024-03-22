package main

import (
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

var (
	chatId int64
	bot    *tgbotapi.BotAPI
)

func telegramStart() {
	if config.TelegramToken == "" || bot != nil {
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
	if chatId > 0 {
		telegramConnect()
	}

	updates := bot.GetUpdatesChan(u)

	// Process updates
	for update := range updates {
		if update.Message != nil {
			switch update.Message.Text {
			case "/start":
				chatId = update.Message.Chat.ID
				telegramConnect()
			case "/backup":
				liquidBackup(true)
			case "/pegin":
				t := ""
				if config.PeginTxId == "" {
					t = "No pending peg-in"
				} else {
					confs := lndNumConfirmations(config.PeginTxId)
					futureTime := time.Now().Add(time.Duration(10*(102-confs)) * time.Minute)
					eta := futureTime.Format("2006-01-02 15:04")
					t = "⏰ " + strconv.Itoa(int(confs)) + "/102 confs, ETA: " + eta
				}
				telegramSendMessage(t)
			case "/version":
				t := "Current version: " + version + "\n"
				t += "Latest version: " + getLatestTag()
				telegramSendMessage(t)
			default:
				telegramSendMessage("I don't understand that command")
			}
		}
	}
}

func telegramConnect() {
	if telegramSendMessage("📟 PeerSwap connected") {
		// successfully connected
		cmdCfg := tgbotapi.NewSetMyCommands(
			tgbotapi.BotCommand{
				Command:     "start",
				Description: "Start the bot",
			},
			tgbotapi.BotCommand{
				Command:     "backup",
				Description: "Get Liquid wallet backup",
			},
			tgbotapi.BotCommand{
				Command:     "pegin",
				Description: "Get status of peg-in",
			},
			tgbotapi.BotCommand{
				Command:     "version",
				Description: "Check version",
			},
		)
		bot.Send(cmdCfg)
		config.TelegramChatId = chatId
		saveConfig()
	} else {
		chatId = 0
	}
}

func telegramSendMessage(msgText string) bool {
	if chatId == 0 {
		return false
	}
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

	msg.Caption = "🌊 Liquid Balance: " + satAmount

	// Send file
	_, err = bot.Send(msg)
	if err != nil {
		return err
	}

	return nil
}
