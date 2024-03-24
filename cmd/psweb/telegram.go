package main

import (
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"golang.org/x/net/proxy"
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
	if config.ProxyURL != "" {
		// Set up Tor proxy
		p, err := url.Parse(config.ProxyURL)
		if err != nil {
			log.Println("Error connecting to Telegram bot with proxy:", err)
			return
		}
		dialer, err := proxy.SOCKS5("tcp", p.Host, nil, proxy.Direct)
		if err != nil {
			log.Println("Error connecting to Telegram bot with proxy:", err)
			return
		}

		// Create a bot instance
		bot, err = tgbotapi.NewBotAPIWithClient(
			config.TelegramToken,
			"https://api.telegram.org/bot%s/%s",
			&http.Client{Transport: &http.Transport{Dial: dialer.Dial}})
		if err != nil {
			log.Println("Error connecting to Telegram bot with proxy:", err)
			return
		}
	} else {
		// Initialize bot with token
		bot, err = tgbotapi.NewBotAPI(config.TelegramToken)
		if err != nil {
			log.Println("Error connecting to Telegram bot:", err)
			return
		}
	}

	// Set bot to debug mode
	bot.Debug = false // os.Getenv("DEBUG") == "1"

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
					tx, err := lndGetTransaction(config.PeginTxId)
					confs := int32(0)
					if err == nil {
						confs = tx.NumConfirmations
					}
					duration := time.Duration(10*(102-confs)) * time.Minute
					formattedDuration := time.Time{}.Add(duration).Format("15h 04m")
					t = "⏰ Amount: " + formatWithThousandSeparators(uint64(config.PeginAmount)) + " sats, Confs: " + strconv.Itoa(int(confs)) + "/102, Time left: " + formattedDuration
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
