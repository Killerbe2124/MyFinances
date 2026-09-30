package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // встроенная база часовых поясов: LoadLocation работает и на голом сервере
	"unicode"

	"expensebot/domain"
	"expensebot/parser"
	"expensebot/store"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const helpText = "Привет! Я бот для учёта трат.\n\n" +
	"Просто пиши траты в свободной форме:\n" +
	"• 450 продукты\n" +
	"• 1200 такси вчера\n" +
	"• 85,5 кофе\n" +
	"• 450 продукты, 1200 такси (несколько трат за раз)\n\n" +
	"Если не пойму, к какой категории относится трата, спрошу — и запомню ответ.\n" +
	"Ошиблась трата? Под ответом бота есть кнопка «🗑 Удалить», " +
	"а в «Последних тратах» можно удалить любую из десяти.\n\n" +
	"В статистике стрелками ◀ ▶ листаются месяцы. Всё остальное — кнопками ниже."

var monthsRu = [...]string{
	"январь", "февраль", "март", "апрель", "май", "июнь",
	"июль", "август", "сентябрь", "октябрь", "ноябрь", "декабрь",
}

// «+10% к августу» — месяц в дательном падеже
var monthsDat = [...]string{
	"январю", "февралю", "марту", "апрелю", "маю", "июню",
	"июлю", "августу", "сентябрю", "октябрю", "ноябрю", "декабрю",
}

// Данные inline-кнопок (callback_data, лимит Telegram — 64 байта):
//
//	new:stats / new:list — показать экран НОВЫМ сообщением (кнопки под подтверждением траты и в /start)
//	stats:<period>       — перерисовать сообщение как статистику: today | week | month | m:2026-08
//	list                 — перерисовать сообщение как список последних трат
//	del:<id>             — удалить трату из сообщения-подтверждения
//	dl:<id>              — удалить трату из списка (список обновится)
//	cat:<id>:<категория> — отнести трату <id> к категории (и запомнить правило)
//	cn:<id>              — «новая категория»: следующее сообщение станет названием категории
//	cs:<id>              — пропустить вопрос о категории
//	fa:<id правила>      — забыть выученное правило
const (
	cbNewStats = "new:stats"
	cbNewList  = "new:list"
	cbStats    = "stats:"
	cbList     = "list"
	cbDel      = "del:"
	cbDelList  = "dl:"
	cbCat      = "cat:"
	cbNewCat   = "cn:"
	cbSkip     = "cs:"
	cbForget   = "fa:"

	maxCallbackData = 64
	pendingTTL      = 10 * time.Minute
	maxAsksPerMsg   = 3 // сколько вопросов о категории задаём на одно сообщение
)

// ---------- конфиг ----------

type config struct {
	token   string
	dbPath  string
	loc     *time.Location
	allowed map[int64]bool // пусто = отвечаем всем
}

func loadConfig() (config, error) {
	cfg := config{
		token:  os.Getenv("BOT_TOKEN"),
		dbPath: envOr("DB_PATH", "bot.db"),
	}
	if cfg.token == "" {
		return cfg, errors.New("не задана переменная окружения BOT_TOKEN (или строка BOT_TOKEN=... в файле .env)")
	}

	loc, err := time.LoadLocation(envOr("BOT_TZ", "UTC"))
	if err != nil {
		return cfg, fmt.Errorf("BOT_TZ: %w", err)
	}
	cfg.loc = loc

	cfg.allowed = map[int64]bool{}
	for _, part := range strings.Split(os.Getenv("ALLOWED_USER_IDS"), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return cfg, fmt.Errorf("ALLOWED_USER_IDS: %q — не число", part)
		}
		cfg.allowed[id] = true
	}
	return cfg, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// loadDotEnv читает KEY=VALUE из файла .env (удобно при локальной разработке).
// Уже заданные переменные окружения не перезаписываются.
func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		if _, exists := os.LookupEnv(k); !exists {
			os.Setenv(k, v)
		}
	}
}

// ---------- приложение ----------

// pendingCategory — пользователь нажал «➕ Новая» и следующим сообщением пришлёт название категории.
type pendingCategory struct {
	expenseID int64
	chatID    int64
	msgID     int // сообщение с вопросом — его потом обновим
	until     time.Time
}

type app struct {
	bot     *tgbotapi.BotAPI
	db      *store.Store
	loc     *time.Location
	token   string
	allowed map[int64]bool
	now     func() time.Time // подменяется в тестах

	mu      sync.Mutex
	pending map[int64]pendingCategory // userID -> ожидаем название категории
}

func newApp(bot *tgbotapi.BotAPI, db *store.Store, loc *time.Location, token string, allowed map[int64]bool) *app {
	return &app{
		bot: bot, db: db, loc: loc, token: token, allowed: allowed,
		now:     time.Now,
		pending: map[int64]pendingCategory{},
	}
}

func main() {
	loadDotEnv(".env")
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	// 1. Инициализируем БД
	db, err := store.New(cfg.dbPath)
	if err != nil {
		log.Fatal("Не удалось открыть БД: ", err)
	}
	defer db.Close()

	// 2. Инициализируем бота
	bot, err := tgbotapi.NewBotAPI(cfg.token)
	if err != nil {
		// в тексте сетевых ошибок бывает URL с токеном — прячем его
		log.Fatal("Ошибка запуска бота: ", redact(err.Error(), cfg.token))
	}
	bot.Debug = false
	log.Printf("Бот авторизован как @%s", bot.Self.UserName)
	if len(cfg.allowed) == 0 {
		log.Println("ВНИМАНИЕ: ALLOWED_USER_IDS не задан — бот отвечает любому пользователю")
	}

	a := newApp(bot, db, cfg.loc, cfg.token, cfg.allowed)

	// Ctrl+C / остановка сервиса: закрываем канал апдейтов и выходим штатно
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		bot.StopReceivingUpdates()
	}()

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	for update := range bot.GetUpdatesChan(u) {
		a.handleUpdate(update)
	}
	log.Println("Бот остановлен")
}

func (a *app) isAllowed(userID int64) bool {
	return len(a.allowed) == 0 || a.allowed[userID]
}

func (a *app) handleUpdate(update tgbotapi.Update) {
	// одна ошибка в обработчике не должна ронять всего бота
	defer func() {
		if r := recover(); r != nil {
			log.Printf("паника при обработке апдейта %d: %v\n%s", update.UpdateID, r, debug.Stack())
		}
	}()

	if cb := update.CallbackQuery; cb != nil {
		a.handleCallback(cb)
		return
	}

	msg := update.Message
	if msg == nil || msg.From == nil || msg.Text == "" {
		return
	}

	if !a.isAllowed(msg.From.ID) {
		log.Printf("отказано пользователю %d", msg.From.ID)
		a.send(msg.Chat.ID, "Это приватный бот.")
		return
	}

	if msg.IsCommand() {
		switch msg.Command() { // без "/" и без "@имя_бота"
		case "start", "help":
			kb := menuKeyboard()
			a.sendKB(msg.Chat.ID, helpText, &kb)
		case "stats":
			a.sendStats(msg.Chat.ID, msg.From.ID, "month")
		case "list":
			a.sendList(msg.Chat.ID, msg.From.ID)
		case "cancel":
			a.clearPending(msg.From.ID)
			a.send(msg.Chat.ID, "Ок, отменил.")
		default:
			a.send(msg.Chat.ID, "Не знаю такой команды. Нажми /start — там кнопки.")
		}
		return
	}

	// Ждём название новой категории? Тогда это сообщение — название, если только
	// пользователь не передумал и не прислал обычную трату.
	if p, ok := a.takePending(msg.From.ID); ok && !looksLikeExpense(msg.Text) {
		a.finishPending(msg, p)
		return
	}

	a.handleExpense(msg)
}

// ---------- траты ----------

func (a *app) handleExpense(msg *tgbotapi.Message) {
	now := msg.Time().In(a.loc) // время сообщения в твоём часовом поясе

	aliases, err := a.db.Aliases(msg.From.ID) // выученные правила категорий
	if err != nil {
		log.Printf("правила категорий: %v", err) // без правил бот всё равно работает
	}

	parts := parser.Split(msg.Text)
	var replies []string
	var rows [][]tgbotapi.InlineKeyboardButton
	var asks []domain.Expense // траты, про категорию которых нужно спросить

	for _, part := range parts {
		entry, err := parser.ParseLineWith(part, now, aliases)
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
		if err := a.db.Save(&expense); err != nil {
			log.Printf("ошибка сохранения: %v", err)
			replies = append(replies, "⚠️ Не удалось сохранить, попробуй ещё раз")
			continue
		}

		replies = append(replies,
			fmt.Sprintf("✅ %s → %s", formatAmount(entry.Amount), entry.Category)+
				nonEmpty("\n💬 %s", entry.Comment)+
				fmt.Sprintf("\n📅 %s", entry.SpentAt.In(a.loc).Format("02.01.2006")))

		if entry.Unknown && expense.ID != 0 && len(asks) < maxAsksPerMsg {
			asks = append(asks, expense)
		}

		// Save должен записать ID обратно в expense; без ID удалить трату нечем
		if expense.ID != 0 && len(rows) < 10 {
			label := fmt.Sprintf("🗑 Удалить %s · %s", formatAmount(entry.Amount), clipRunes(entry.Category, 20))
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData(label, cbDel+strconv.FormatInt(expense.ID, 10))))
		}
	}

	if len(replies) == 0 {
		return
	}
	rows = append(rows, menuRow())
	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	a.sendKB(msg.Chat.ID, strings.Join(replies, "\n\n"), &kb)

	for _, e := range asks {
		a.askCategory(msg.Chat.ID, msg.From.ID, e)
	}
}

// looksLikeExpense — в тексте есть сумма, значит это трата, а не название категории.
func looksLikeExpense(text string) bool {
	parts := parser.Split(text)
	if len(parts) == 0 {
		return false
	}
	_, err := parser.ParseLine(parts[0], time.Now())
	return err == nil
}

// ---------- обучение категориям ----------

func noKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}}
}

// categoryChoices — категории для кнопок: сначала те, что пользователь уже использовал
// (частые первыми), потом встроенные.
func (a *app) categoryChoices(userID int64) []string {
	used, err := a.db.Categories(userID, parser.OtherCategory, 8)
	if err != nil {
		log.Printf("категории пользователя: %v", err)
	}
	seen := map[string]bool{}
	var out []string
	add := func(c string) {
		if k := strings.ToLower(c); !seen[k] {
			seen[k] = true
			out = append(out, c)
		}
	}
	for _, c := range used {
		add(c)
	}
	for _, c := range parser.DefaultCategories() {
		add(c)
	}
	if len(out) > 10 {
		out = out[:10]
	}
	return out
}

func (a *app) categoryKeyboard(userID, expenseID int64) tgbotapi.InlineKeyboardMarkup {
	var rows [][]tgbotapi.InlineKeyboardButton
	var row []tgbotapi.InlineKeyboardButton
	for _, c := range a.categoryChoices(userID) {
		data := fmt.Sprintf("%s%d:%s", cbCat, expenseID, c)
		if len(data) > maxCallbackData { // слишком длинное название не поместится в callback_data
			continue
		}
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(c, data))
		if len(row) == 2 {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	id := strconv.FormatInt(expenseID, 10)
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("➕ Новая категория", cbNewCat+id),
		tgbotapi.NewInlineKeyboardButtonData("Пропустить", cbSkip+id),
	))
	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func (a *app) askCategory(chatID, userID int64, e domain.Expense) {
	text := fmt.Sprintf("❓ Не знаю, к какой категории отнести «%s» (%s).",
		clipRunes(e.Comment, 60), formatAmount(e.Amount))
	if parser.Learnable(parser.NormalizeKey(e.Comment)) {
		text += "\nВыбери — запомню и в следующий раз определю сам."
	} else {
		text += "\nВыбери категорию для этой траты."
	}
	kb := a.categoryKeyboard(userID, e.ID)
	a.sendKB(chatID, text, &kb)
}

var errBadCategory = errors.New("неподходящее название категории")

// applyCategory переносит трату в категорию и, если текст траты короткий, запоминает правило.
// Возвращает текст и кнопки для сообщения-результата.
func (a *app) applyCategory(userID, expenseID int64, category string) (string, tgbotapi.InlineKeyboardMarkup, error) {
	category = parser.CleanCategory(category)
	if category == "" {
		return "", noKeyboard(), errBadCategory
	}
	e, err := a.db.Get(expenseID, userID)
	if err != nil {
		return "", noKeyboard(), err
	}
	if err := a.db.UpdateCategory(expenseID, userID, category); err != nil {
		return "", noKeyboard(), err
	}

	key := parser.NormalizeKey(e.Comment)
	var aliasID int64
	if parser.Learnable(key) && key != parser.NormalizeKey(category) {
		aliasID, err = a.db.SetAlias(userID, key, category)
		if err != nil { // категория уже изменена, поэтому не считаем это провалом
			log.Printf("не удалось сохранить правило %q: %v", key, err)
			aliasID = 0
		}
	}
	if aliasID == 0 {
		return fmt.Sprintf("✅ Трата %s перенесена в «%s».", formatAmount(e.Amount), category), noKeyboard(), nil
	}

	text := fmt.Sprintf("🧠 Запомнил: «%s» → %s.\nТрата %s перенесена, в следующий раз определю сам.",
		key, category, formatAmount(e.Amount))
	kb := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("↩️ Забыть это правило", cbForget+strconv.FormatInt(aliasID, 10))))
	return text, kb, nil
}

func (a *app) onPickCategory(cb *tgbotapi.CallbackQuery, rest string) {
	idStr, name, ok := strings.Cut(rest, ":")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if !ok || err != nil {
		a.answer(cb.ID, "Некорректная кнопка")
		return
	}
	a.clearPendingFor(cb.From.ID, id)

	chatID, msgID := cb.Message.Chat.ID, cb.Message.MessageID
	text, kb, err := a.applyCategory(cb.From.ID, id, name)
	switch {
	case err == nil:
		a.answer(cb.ID, "Готово")
		a.edit(chatID, msgID, text, kb)
	case errors.Is(err, errBadCategory):
		a.answer(cb.ID, "Эту категорию выбрать нельзя")
	case errors.Is(err, store.ErrNotFound):
		a.answer(cb.ID, "Этой траты уже нет")
		a.edit(chatID, msgID, "Этой траты уже нет.", noKeyboard())
	default:
		log.Printf("смена категории у траты %d: %v", id, err)
		a.answer(cb.ID, "⚠️ Не удалось обновить")
	}
}

func (a *app) onNewCategory(cb *tgbotapi.CallbackQuery, idStr string) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		a.answer(cb.ID, "Некорректная кнопка")
		return
	}
	chatID, msgID := cb.Message.Chat.ID, cb.Message.MessageID

	e, err := a.db.Get(id, cb.From.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			a.answer(cb.ID, "Этой траты уже нет")
			a.edit(chatID, msgID, "Этой траты уже нет.", noKeyboard())
		} else {
			log.Printf("получение траты %d: %v", id, err)
			a.answer(cb.ID, "⚠️ Ошибка, попробуй ещё раз")
		}
		return
	}

	a.setPending(cb.From.ID, pendingCategory{
		expenseID: id, chatID: chatID, msgID: msgID, until: a.now().Add(pendingTTL),
	})
	a.answer(cb.ID, "")
	kb := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("Отмена", cbSkip+idStr)))
	a.edit(chatID, msgID,
		fmt.Sprintf("✍️ Напиши название категории для «%s» одним сообщением.", clipRunes(e.Comment, 60)), kb)
}

func (a *app) onSkipCategory(cb *tgbotapi.CallbackQuery, idStr string) {
	if id, err := strconv.ParseInt(idStr, 10, 64); err == nil {
		a.clearPendingFor(cb.From.ID, id)
	}
	a.answer(cb.ID, "")
	a.edit(cb.Message.Chat.ID, cb.Message.MessageID, "👌 Оставил в категории «Другое».", noKeyboard())
}

func (a *app) onForget(cb *tgbotapi.CallbackQuery, idStr string) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		a.answer(cb.ID, "Некорректная кнопка")
		return
	}
	text := cb.Message.Text
	switch err := a.db.DeleteAlias(id, cb.From.ID); {
	case err == nil:
		a.answer(cb.ID, "Правило забыто")
		text += "\n\n↩️ Правило забыто: такие траты снова буду спрашивать."
	case errors.Is(err, store.ErrNotFound):
		a.answer(cb.ID, "Правило уже удалено")
	default:
		log.Printf("удаление правила %d: %v", id, err)
		a.answer(cb.ID, "⚠️ Не удалось забыть")
		return
	}
	a.edit(cb.Message.Chat.ID, cb.Message.MessageID, text, noKeyboard())
}

// finishPending: пользователь прислал название новой категории.
func (a *app) finishPending(msg *tgbotapi.Message, p pendingCategory) {
	text, kb, err := a.applyCategory(msg.From.ID, p.expenseID, msg.Text)
	switch {
	case err == nil:
		a.sendKB(msg.Chat.ID, text, &kb)
		a.edit(p.chatID, p.msgID, "✅ Категория выбрана.", noKeyboard())
	case errors.Is(err, errBadCategory):
		a.send(msg.Chat.ID, "Так не получится: название не должно быть пустым или «Другое». "+
			"Нажми «➕ Новая категория» под вопросом ещё раз.")
	case errors.Is(err, store.ErrNotFound):
		a.send(msg.Chat.ID, "Этой траты уже нет.")
	default:
		log.Printf("новая категория для траты %d: %v", p.expenseID, err)
		a.send(msg.Chat.ID, "⚠️ Не удалось обновить категорию, попробуй ещё раз.")
	}
}

func (a *app) setPending(userID int64, p pendingCategory) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pending[userID] = p
}

// takePending забирает ожидание (и удаляет его). ok == false, если его нет или оно устарело.
func (a *app) takePending(userID int64) (pendingCategory, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.pending[userID]
	delete(a.pending, userID)
	return p, ok && a.now().Before(p.until)
}

func (a *app) clearPending(userID int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.pending, userID)
}

func (a *app) clearPendingFor(userID, expenseID int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if p, ok := a.pending[userID]; ok && p.expenseID == expenseID {
		delete(a.pending, userID)
	}
}

// ---------- экраны ----------

func menuRow() []tgbotapi.InlineKeyboardButton {
	return tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("📊 Статистика", cbNewStats),
		tgbotapi.NewInlineKeyboardButtonData("📝 Последние траты", cbNewList),
	)
}

func menuKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(menuRow())
}

// period — период статистики: сегодня, 7 дней или конкретный месяц.
type period struct {
	kind  string    // "today" | "week" | "month"
	month time.Time // первое число месяца (для kind == "month")
}

func monthStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
}

// parsePeriod разбирает данные кнопки: today | week | month | m:2026-08.
// Неизвестное значение и будущие месяцы превращаются в текущий месяц.
func parsePeriod(s string, now time.Time) period {
	cur := monthStart(now)
	switch {
	case s == "today" || s == "week":
		return period{kind: s}
	case strings.HasPrefix(s, "m:"):
		if t, err := time.ParseInLocation("2006-01", strings.TrimPrefix(s, "m:"), now.Location()); err == nil && !t.After(cur) {
			return period{kind: "month", month: t}
		}
	}
	return period{kind: "month", month: cur}
}

// bounds возвращает [from, to) и название периода для заголовка («за …»).
func (p period) bounds(now time.Time) (from, to time.Time, title string) {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch p.kind {
	case "today":
		return today, today.AddDate(0, 0, 1), "сегодня"
	case "week":
		return today.AddDate(0, 0, -6), today.AddDate(0, 0, 1), "последние 7 дней"
	default:
		return p.month, p.month.AddDate(0, 1, 0), fmt.Sprintf("%s %d", monthsRu[p.month.Month()-1], p.month.Year())
	}
}

func ucFirst(s string) string {
	r := []rune(s)
	if len(r) > 0 {
		r[0] = unicode.ToUpper(r[0])
	}
	return string(r)
}

// monthLabel — подпись кнопки-стрелки; год добавляем, только если он отличается от показанного.
func monthLabel(t, shown time.Time) string {
	s := ucFirst(monthsRu[t.Month()-1])
	if t.Year() != shown.Year() {
		s += fmt.Sprintf(" %d", t.Year())
	}
	return s
}

func statsKeyboard(p period, now time.Time) tgbotapi.InlineKeyboardMarkup {
	btn := func(label, data string, active bool) tgbotapi.InlineKeyboardButton {
		if active {
			label = "• " + label
		}
		return tgbotapi.NewInlineKeyboardButtonData(label, cbStats+data)
	}
	rows := [][]tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardRow(
			btn("Сегодня", "today", p.kind == "today"),
			btn("7 дней", "week", p.kind == "week"),
			btn("Месяц", "month", p.kind == "month"),
		),
	}

	if p.kind == "month" { // стрелки: месяц назад и (если это не текущий месяц) месяц вперёд
		prev := p.month.AddDate(0, -1, 0)
		nav := []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("◀ "+monthLabel(prev, p.month), cbStats+"m:"+prev.Format("2006-01")),
		}
		if p.month.Before(monthStart(now)) {
			next := p.month.AddDate(0, 1, 0)
			nav = append(nav,
				tgbotapi.NewInlineKeyboardButtonData(monthLabel(next, p.month)+" ▶", cbStats+"m:"+next.Format("2006-01")))
		}
		rows = append(rows, nav)
	}

	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("📝 Последние траты", cbList)))
	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func (a *app) totalFor(userID int64, from, to time.Time) (int64, error) {
	stats, err := a.db.StatsByCategory(userID, from, to)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, s := range stats {
		total += s.Total
	}
	return total, nil
}

// compareLine сравнивает месяц с предыдущим. Для текущего (ещё не закончившегося) месяца
// берём тот же срок в прошлом: если сегодня 12-е, сравниваем с 1–12 числами прошлого месяца.
func (a *app) compareLine(userID int64, month time.Time, total int64, now time.Time) string {
	prevFrom := month.AddDate(0, -1, 0)
	prevTo := month
	suffix := ""
	if month.Equal(monthStart(now)) {
		if t := prevFrom.AddDate(0, 0, now.Day()); t.Before(prevTo) {
			prevTo = t
		}
		suffix = " за тот же срок"
	}

	prevTotal, err := a.totalFor(userID, prevFrom, prevTo)
	if err != nil {
		log.Printf("сравнение с прошлым месяцем: %v", err)
		return ""
	}
	if prevTotal == 0 {
		return ""
	}

	diff := float64(total-prevTotal) / float64(prevTotal) * 100
	name := monthsDat[prevFrom.Month()-1]
	switch {
	case diff >= 0.5:
		return fmt.Sprintf("📈 %+.0f%% к %s%s (%s)", diff, name, suffix, formatAmount(prevTotal))
	case diff <= -0.5:
		return fmt.Sprintf("📉 %+.0f%% к %s%s (%s)", diff, name, suffix, formatAmount(prevTotal))
	default:
		return fmt.Sprintf("➖ Без изменений к %s%s (%s)", name, suffix, formatAmount(prevTotal))
	}
}

func (a *app) statsView(userID int64, periodStr string) (string, tgbotapi.InlineKeyboardMarkup, error) {
	now := a.now().In(a.loc)
	p := parsePeriod(periodStr, now)
	kb := statsKeyboard(p, now)

	from, to, title := p.bounds(now)
	stats, err := a.db.StatsByCategory(userID, from, to)
	if err != nil {
		return "", kb, err
	}
	if len(stats) == 0 {
		return fmt.Sprintf("📭 За %s трат нет.", title), kb, nil
	}

	// самые большие категории сверху
	sort.Slice(stats, func(i, j int) bool { return stats[i].Total > stats[j].Total })

	var total int64
	for _, s := range stats {
		total += s.Total
	}

	lines := []string{fmt.Sprintf("📊 Статистика за %s:", title)}
	for _, s := range stats {
		pct := float64(s.Total) / float64(total) * 100
		lines = append(lines,
			fmt.Sprintf("• %s: %s (%.0f%%, %d %s)",
				s.Category, formatAmount(s.Total), pct, int(s.Count),
				plural(int(s.Count), "запись", "записи", "записей")))
	}
	lines = append(lines, fmt.Sprintf("\n💰 Итого: %s", formatAmount(total)))
	if p.kind == "month" {
		if cmp := a.compareLine(userID, p.month, total, now); cmp != "" {
			lines = append(lines, cmp)
		}
	}
	return strings.Join(lines, "\n"), kb, nil
}

func (a *app) listView(userID int64) (string, tgbotapi.InlineKeyboardMarkup, error) {
	statsRow := tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("📊 Статистика", cbStats+"month"))

	list, err := a.db.List(userID, time.Time{}, time.Time{})
	if err != nil {
		return "", tgbotapi.NewInlineKeyboardMarkup(statsRow), err
	}
	if len(list) == 0 {
		return "📭 Список пуст. Напиши первую трату!", tgbotapi.NewInlineKeyboardMarkup(statsRow), nil
	}

	// новые сверху, независимо от порядка, в котором их отдаёт store
	sort.SliceStable(list, func(i, j int) bool { return list[i].SpentAt.After(list[j].SpentAt) })
	if len(list) > 10 {
		list = list[:10]
	}

	lines := []string{"📝 Последние траты (кнопка 🗑 удаляет трату с этим номером):", ""}
	var rows [][]tgbotapi.InlineKeyboardButton
	var row []tgbotapi.InlineKeyboardButton
	for i, e := range list {
		line := fmt.Sprintf("%d. %s — %s", i+1, e.SpentAt.In(a.loc).Format("02.01"), formatAmount(e.Amount))
		if e.Category != parser.OtherCategory {
			line += " (" + e.Category + ")"
		}
		if e.Comment != "" {
			line += " · " + e.Comment
		}
		lines = append(lines, line)

		row = append(row, tgbotapi.NewInlineKeyboardButtonData(
			fmt.Sprintf("🗑 %d", i+1), cbDelList+strconv.FormatInt(e.ID, 10)))
		if len(row) == 5 {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	rows = append(rows, statsRow)
	return strings.Join(lines, "\n"), tgbotapi.NewInlineKeyboardMarkup(rows...), nil
}

// новое сообщение

func (a *app) sendStats(chatID, userID int64, period string) {
	text, kb, err := a.statsView(userID, period)
	if err != nil {
		log.Printf("статистика: %v", err)
		a.send(chatID, "⚠️ Ошибка чтения статистики")
		return
	}
	a.sendKB(chatID, text, &kb)
}

func (a *app) sendList(chatID, userID int64) {
	text, kb, err := a.listView(userID)
	if err != nil {
		log.Printf("список: %v", err)
		a.send(chatID, "⚠️ Ошибка чтения списка")
		return
	}
	a.sendKB(chatID, text, &kb)
}

// перерисовка существующего сообщения

func (a *app) showStats(chatID int64, msgID int, userID int64, period string) {
	text, kb, err := a.statsView(userID, period)
	if err != nil {
		log.Printf("статистика: %v", err)
		return
	}
	a.edit(chatID, msgID, text, kb)
}

func (a *app) showList(chatID int64, msgID int, userID int64) {
	text, kb, err := a.listView(userID)
	if err != nil {
		log.Printf("список: %v", err)
		return
	}
	a.edit(chatID, msgID, text, kb)
}

// ---------- нажатия на кнопки ----------

func (a *app) handleCallback(cb *tgbotapi.CallbackQuery) {
	if cb.From == nil || !a.isAllowed(cb.From.ID) {
		a.answer(cb.ID, "Это приватный бот")
		return
	}
	if cb.Message == nil { // сообщение недоступно — редактировать нечего
		a.answer(cb.ID, "")
		return
	}
	chatID, msgID, userID := cb.Message.Chat.ID, cb.Message.MessageID, cb.From.ID

	switch data := cb.Data; {
	case data == cbNewStats:
		a.answer(cb.ID, "")
		a.sendStats(chatID, userID, "month")

	case data == cbNewList:
		a.answer(cb.ID, "")
		a.sendList(chatID, userID)

	case strings.HasPrefix(data, cbStats):
		a.answer(cb.ID, "")
		a.showStats(chatID, msgID, userID, strings.TrimPrefix(data, cbStats))

	case data == cbList:
		a.answer(cb.ID, "")
		a.showList(chatID, msgID, userID)

	case strings.HasPrefix(data, cbDelList):
		if a.deleteExpense(cb, strings.TrimPrefix(data, cbDelList)) {
			a.showList(chatID, msgID, userID)
		}

	case strings.HasPrefix(data, cbDel):
		if a.deleteExpense(cb, strings.TrimPrefix(data, cbDel)) {
			a.markDeleted(cb)
		}

	case strings.HasPrefix(data, cbCat):
		a.onPickCategory(cb, strings.TrimPrefix(data, cbCat))

	case strings.HasPrefix(data, cbNewCat):
		a.onNewCategory(cb, strings.TrimPrefix(data, cbNewCat))

	case strings.HasPrefix(data, cbSkip):
		a.onSkipCategory(cb, strings.TrimPrefix(data, cbSkip))

	case strings.HasPrefix(data, cbForget):
		a.onForget(cb, strings.TrimPrefix(data, cbForget))

	default:
		a.answer(cb.ID, "")
	}
}

// deleteExpense удаляет трату текущего пользователя и отвечает на нажатие всплывающей подсказкой.
// Возвращает true, если можно обновлять сообщение (трата удалена или её уже нет).
func (a *app) deleteExpense(cb *tgbotapi.CallbackQuery, idStr string) bool {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		a.answer(cb.ID, "Некорректная кнопка")
		return false
	}
	// store.Delete проверяет и id, и user_id: чужую трату удалить нельзя
	switch err := a.db.Delete(id, cb.From.ID); {
	case err == nil:
		a.answer(cb.ID, "🗑 Удалено")
	case errors.Is(err, store.ErrNotFound):
		a.answer(cb.ID, "Этой траты уже нет") // например, нажали кнопку в устаревшем списке
	default:
		log.Printf("удаление траты %d: %v", id, err)
		a.answer(cb.ID, "⚠️ Не удалось удалить")
		return false
	}
	return true
}

// markDeleted убирает нажатую кнопку из сообщения-подтверждения
// и дописывает в его текст, что именно удалено.
func (a *app) markDeleted(cb *tgbotapi.CallbackQuery) {
	rows := [][]tgbotapi.InlineKeyboardButton{}
	label := ""
	if cb.Message.ReplyMarkup != nil {
		for _, row := range cb.Message.ReplyMarkup.InlineKeyboard {
			hit := false
			for _, b := range row {
				if b.CallbackData != nil && *b.CallbackData == cb.Data {
					hit = true
					label = strings.TrimPrefix(b.Text, "🗑 Удалить ")
				}
			}
			if !hit {
				rows = append(rows, row)
			}
		}
	}
	text := cb.Message.Text
	if label != "" {
		text += "\n\n🗑 Удалено: " + label
	}
	a.edit(cb.Message.Chat.ID, cb.Message.MessageID, text, tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows})
}

// --- утилиты отправки ---

func (a *app) send(chatID int64, text string) {
	a.sendKB(chatID, text, nil)
}

// sendKB отправляет обычный текст (без ParseMode: пользовательский текст с "<" или "&"
// не ломает сообщение) и логирует ошибки отправки.
func (a *app) sendKB(chatID int64, text string, kb *tgbotapi.InlineKeyboardMarkup) {
	m := tgbotapi.NewMessage(chatID, clipMessage(text))
	if kb != nil && len(kb.InlineKeyboard) > 0 {
		m.ReplyMarkup = *kb
	}
	if _, err := a.bot.Send(m); err != nil {
		log.Printf("ошибка отправки в чат %d: %s", chatID, redact(err.Error(), a.token))
	}
}

// edit заменяет текст и кнопки существующего сообщения.
func (a *app) edit(chatID int64, msgID int, text string, kb tgbotapi.InlineKeyboardMarkup) {
	text = clipMessage(text)
	var c tgbotapi.Chattable
	if len(kb.InlineKeyboard) == 0 {
		c = tgbotapi.NewEditMessageText(chatID, msgID, text) // без markup кнопки исчезают
	} else {
		c = tgbotapi.NewEditMessageTextAndMarkup(chatID, msgID, text, kb)
	}
	if _, err := a.bot.Request(c); err != nil && !strings.Contains(err.Error(), "message is not modified") {
		log.Printf("ошибка редактирования сообщения %d: %s", msgID, redact(err.Error(), a.token))
	}
}

// answer обязательно нужно вызывать на каждое нажатие, иначе у кнопки крутится "часики".
func (a *app) answer(callbackID, text string) {
	if _, err := a.bot.Request(tgbotapi.NewCallback(callbackID, text)); err != nil {
		log.Printf("ошибка ответа на нажатие: %s", redact(err.Error(), a.token))
	}
}

func redact(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "***")
}

// --- форматирование ---

func clipMessage(text string) string { // лимит Telegram — 4096 символов
	return clipRunes(text, 4000)
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func formatAmount(kopecks int64) string {
	rubles, cents := kopecks/100, kopecks%100
	s := groupThousands(rubles)
	if cents != 0 {
		s += fmt.Sprintf(",%02d", cents)
	}
	return s + "\u00a0₽"
}

// 1234567 -> "1 234 567" (с неразрывным пробелом)
func groupThousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteString("\u00a0")
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func plural(n int, one, few, many string) string {
	n10, n100 := n%10, n%100
	switch {
	case n10 == 1 && n100 != 11:
		return one
	case n10 >= 2 && n10 <= 4 && (n100 < 12 || n100 > 14):
		return few
	default:
		return many
	}
}

func nonEmpty(format, s string) string {
	if s == "" {
		return ""
	}
	return fmt.Sprintf(format, s)
}
