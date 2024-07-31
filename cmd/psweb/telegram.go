package main

import (
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"peerswap-web/cmd/psweb/config"
	"peerswap-web/cmd/psweb/ln"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"golang.org/x/net/proxy"
)

var (
	chatId int64
	bot    *tgbotapi.BotAPI
)

func telegramStart() {
	if config.Config.TelegramToken == "" || bot != nil {
		// disabled or already started
		return
	}

	var err error
	if config.Config.ProxyURL != "" {
		// Set up Tor proxy
		p, err := url.Parse(config.Config.ProxyURL)
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
			config.Config.TelegramToken,
			"https://api.telegram.org/bot%s/%s",
			&http.Client{Transport: &http.Transport{Dial: dialer.Dial}})
		if err != nil {
			log.Println("Error connecting to Telegram bot with proxy:", err)
			return
		}
	} else {
		// Initialize bot with token
		bot, err = tgbotapi.NewBotAPI(config.Config.TelegramToken)
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
	chatId = config.Config.TelegramChatId
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
				if config.Config.PeginTxId == "" {
					t = "No pending peg-in or BTC withdrawal"
				} else {
					cl, clean, er := ln.GetClient()
					if er != nil {
						t = "❗ Error: " + er.Error()
					} else {
						confs, _ := ln.GetTxConfirmations(cl, config.Config.PeginTxId)
						duration := time.Duration(10*(102-confs)) * time.Minute
						formattedDuration := time.Time{}.Add(duration).Format("15h 04m")
						t = "⏰ Amount: " + formatWithThousandSeparators(uint64(config.Config.PeginAmount)) + " sats, Confs: " + strconv.Itoa(int(confs))
						if config.Config.PeginClaimScript != "" {
							t += "/102, Time left: " + formattedDuration
						}
						t += ". TxId: `" + config.Config.PeginTxId + "`"
						clean()
					}
				}
				telegramSendMessage(t)
			case "/autoswaps":
				t := "🤖 Auto swap-ins are "
				if config.Config.AutoSwapEnabled {
					t += "Enabled"
					t += "\nThreshold Amount: " + formatWithThousandSeparators(config.Config.AutoSwapThresholdAmount)
					t += "\nMinimum PPM: " + formatWithThousandSeparators(config.Config.AutoSwapThresholdPPM)
					t += "\nTarget Pct: " + formatWithThousandSeparators(config.Config.AutoSwapTargetPct)

					var candidate SwapParams

					if err := findSwapInCandidate(&candidate); err == nil {
						if candidate.Amount > 0 {
							t += "\nCandidate: " + candidate.PeerAlias
							t += "\nMax Amount: " + formatWithThousandSeparators(candidate.Amount)
							t += "\nRecent PPM: " + formatWithThousandSeparators(candidate.PPM)
						} else {
							t += "No swap candidates"
						}
					}
				} else {
					t += "Disabled"
				}
				telegramSendMessage(t)
			case "/version":
				t := "Current version: " + version + "\n"
				t += "Latest version: " + latestVersion
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
				Description: "Elements wallet backup",
			},
			tgbotapi.BotCommand{
				Command:     "pegin",
				Description: "Status of peg-in or BTC withdrawal",
			},
			tgbotapi.BotCommand{
				Command:     "autoswaps",
				Description: "Status of auto swap-ins",
			},
			tgbotapi.BotCommand{
				Command:     "version",
				Description: "Check version",
			},
		)
		bot.Send(cmdCfg)
		config.Config.TelegramChatId = chatId
		config.Save()
	} else {
		chatId = 0
		bot = nil
	}
}

func telegramSendMessage(msgText string) bool {
	if chatId == 0 {
		return false
	}
	msg := tgbotapi.NewMessage(chatId, EscapeMarkdownV2(msgText))
	msg.ParseMode = "MarkdownV2"

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

// EscapeMarkdownV2 escapes special characters for MarkdownV2
func EscapeMarkdownV2(text string) string {
	replacer := strings.NewReplacer(
		"_", "\\_",
		"*", "\\*",
		"[", "\\[",
		"]", "\\]",
		"(", "\\(",
		")", "\\)",
		"~", "\\~",
		"`", "\\`",
		">", "\\>",
		"#", "\\#",
		"+", "\\+",
		"-", "\\-",
		"=", "\\=",
		"|", "\\|",
		"{", "\\{",
		"}", "\\}",
		".", "\\.",
		"!", "\\!",
	)
	return replacer.Replace(text)
}
