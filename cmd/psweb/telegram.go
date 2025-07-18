package main

import (
	"fmt"
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
	chatId      int64
	bot         *tgbotapi.BotAPI
	peginInvite string
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
		if !telegramConnect() {
			return
		}
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
					if config.Config.PeginTxId != "external" {
						confs, _ := peginConfirmations(config.Config.PeginTxId)
						if config.Config.PeginClaimJoin {
							bh := ln.GetBlockHeight()
							duration := time.Duration(10*(ln.ClaimBlockHeight-bh)) * time.Minute
							if ln.ClaimBlockHeight == 0 {
								duration = time.Duration(10*(int32(peginBlocks)-confs)) * time.Minute
							}
							eta := time.Now().Add(duration).Format("3:04 PM")
							if duration < 0 {
								eta = "Past due"
							}
							t = "🧬 " + ln.ClaimStatus
							if ln.MyRole == "none" && ln.ClaimJoinHandler != "" {
								t += ". Time limit to apply: " + eta
							} else if confs > 0 {
								t += ". ETA: " + eta
							}
						} else {
							// solo peg-in
							duration := time.Duration(10*(int32(peginBlocks)-confs)) * time.Minute
							eta := time.Now().Add(duration).Format("3:04 PM")
							t = "⏰ Peg-in pending:: "
							if config.Config.PeginClaimScript == "" {
								t = "⛓️ BTC withdrawal pending: "
							}
							t += formatWithThousandSeparators(uint64(config.Config.PeginAmount)) + " sats, Confs: " + strconv.Itoa(int(confs))
							t += fmt.Sprintf(", sat/vb: %0.2f", config.Config.PeginFeeRate)
							if config.Config.PeginClaimScript != "" {
								t += "/102, ETA: " + eta
							}
							t += ". TxId: `" + config.Config.PeginTxId + "`"
						}
					} else {
						t = "Awaiting external funding to a peg-in address"
					}
				}
				telegramSendMessage(t)
			case "/autoswaps":
				t := "🤖 Liquid auto swaps are "
				if config.Config.AutoSwapEnabled {
					t += "Enabled"
					t += "\nThreshold Amount: " + formatWithThousandSeparators(config.Config.AutoSwapThresholdAmount)
					t += "\nMinimum PPM: " + formatWithThousandSeparators(config.Config.AutoSwapThresholdPPM)
					t += "\nTarget Pct: " + formatWithThousandSeparators(config.Config.AutoSwapTargetPct)

					var candidate AutoSwapParams

					if err := findSwapInCandidate(&candidate); err == nil {
						if candidate.Amount > 0 {
							t += "\nCandidate: " + candidate.PeerAlias
							t += "\nMax Amount: " + formatWithThousandSeparators(candidate.Amount)
							t += "\nRecent PPM: " + formatWithThousandSeparators(candidate.RoutingPpm)
						} else {
							t += "No swap candidates"
						}
					}
				} else {
					t += "Disabled"
				}
				telegramSendMessage(t)
			case "/version":
				t := "Current version: " + VERSION + "\n"
				t += "Latest version: " + latestVersion
				telegramSendMessage(t)
			default:
				telegramSendMessage("I don't understand that command")
			}
		}
	}
}

func telegramConnect() bool {
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
				Description: "Status of Liquid auto swaps",
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
		if chatId > 0 {
			chatId = 0
			config.Config.TelegramChatId = chatId
			config.Save()
			log.Println("Chat Id was reset. Use /start in Telegram to start the bot.")
		}
		bot = nil
		return false
	}
	return true
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

	msg.Caption = "🌊 " + satAmount

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
