package main

import (
	"fmt"
	"log"
	"strings"
	"time"

	"expensebot/domain"
	"expensebot/parser"
	"expensebot/store"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const TelegramToken = "8875594329:AAF1TakI_SuOSGf2saYSBABHuaLX2-bl7Ek"

func main() {
	// 1. Инициализируем БД
	db, err := store.New("bot.db")
	if err != nil {
		log.Fatal("Не удалось открыть БД: ", err)
	}
	defer db.Close()

	// 2. Инициализируем бота
	bot, err := tgbotapi.NewBotAPI(TelegramToken)
	if err != nil {
		log.Fatal("Ошибка запуска бота: ", err)
	}
	bot.Debug = false
	log.Printf("Бот авторизован как @%s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	for update := range updates {
		if update.Message == nil {
			continue
		}
		msg := update.Message

		switch {
		case msg.Text == "/start":
			send(bot, msg.Chat.ID,
				"Привет! Я бот для учёта трат.\n\n"+
					"Просто пиши траты в свободной форме:\n"+
					"• 450 продукты\n"+
					"• 1200 такси вчера\n"+
					"• 85,5 кофе\n\n"+
					"Команды:\n"+
					"/stats — статистика за месяц\n"+
					"/list — последние 10 трат")

		case msg.Text == "/stats":
			handleStats(bot, db, msg)

		case msg.Text == "/list":
			handleList(bot, db, msg)

		case msg.Text != "":
			handleExpense(bot, db, msg)
		}
	}
}

func handleExpense(bot *tgbotapi.BotAPI, db *store.Store, msg *tgbotapi.Message) {
	parts := parser.Split(msg.Text)
	var replies []string

	for _, part := range parts {
		entry, err := parser.ParseLine(part, msg.Time())
		if err != nil {
			replies = append(replies, fmt.Sprintf("❌ Не понял «%s»: %v", part, err))
			continue
		}

		expense := domain.Expense{
			UserID:   msg.From.ID,
			Amount:   entry.Amount,
			Category: entry.Category,
			Comment:  entry.Comment,
			SpentAt:  entry.SpentAt,
		}
		if err := db.Save(&expense); err != nil {
			log.Printf("ошибка сохранения: %v", err)
			replies = append(replies, fmt.Sprintf("⚠️ Ошибка сохранения: %v", err))
			continue
		}

		replies = append(replies,
			fmt.Sprintf("✅ %s → %s", formatAmount(entry.Amount), entry.Category)+
				nonEmpty("\n💬 %s", entry.Comment)+
				fmt.Sprintf("\n📅 %s", entry.SpentAt.Format("02.01.2006")))
	}

	send(bot, msg.Chat.ID, strings.Join(replies, "\n\n"))
}

func handleStats(bot *tgbotapi.BotAPI, db *store.Store, msg *tgbotapi.Message) {
	now := time.Now()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	to := from.AddDate(0, 1, 0)

	stats, err := db.StatsByCategory(msg.From.ID, from, to)
	if err != nil {
		send(bot, msg.Chat.ID, "⚠️ Ошибка чтения статистики")
		return
	}
	if len(stats) == 0 {
		send(bot, msg.Chat.ID, " В этом месяце трат ещё не было.")
		return
	}

	var total int64
	for _, s := range stats {
		total += s.Total
	}

	lines := []string{fmt.Sprintf("📊 Статистика за %s:", from.Format("January 2006"))}
	for _, s := range stats {
		pct := float64(s.Total) / float64(total) * 100
		lines = append(lines,
			fmt.Sprintf("• %s: %s (%.0f%%, %d записей)",
				s.Category, formatAmount(s.Total), pct, s.Count))
	}
	lines = append(lines, fmt.Sprintf("\n💰 Итого: %s", formatAmount(total)))

	send(bot, msg.Chat.ID, strings.Join(lines, "\n"))
}

func handleList(bot *tgbotapi.BotAPI, db *store.Store, msg *tgbotapi.Message) {
	list, err := db.List(msg.From.ID, time.Time{}, time.Time{})
	if err != nil {
		send(bot, msg.Chat.ID, "️ Ошибка чтения списка")
		return
	}
	if len(list) == 0 {
		send(bot, msg.Chat.ID, " Список пуст. Напиши первую трату!")
		return
	}
	if len(list) > 10 {
		list = list[:10]
	}

	lines := []string{"📝 Последние траты:"}
	for _, e := range list {
		line := fmt.Sprintf("• %s — %s", e.SpentAt.Format("02.01"), formatAmount(e.Amount))
		if e.Category != "Другое" {
			line += " (" + e.Category + ")"
		}
		lines = append(lines, line)
	}
	send(bot, msg.Chat.ID, strings.Join(lines, "\n"))
}

// --- утилиты ---

func send(bot *tgbotapi.BotAPI, chatID int64, text string) {
	m := tgbotapi.NewMessage(chatID, text)
	m.ParseMode = "HTML"
	_, _ = bot.Send(m)
}

func formatAmount(kopecks int64) string {
	rubles := kopecks / 100
	cents := kopecks % 100
	if cents == 0 {
		return fmt.Sprintf("%d ₽", rubles)
	}
	return fmt.Sprintf("%d,%02d ₽", rubles, cents)
}

func nonEmpty(format, s string) string {
	if s == "" {
		return ""
	}
	return fmt.Sprintf(format, s)
}
