// Package parser превращает текст вида "450 продукты вчера" в структуру траты.
package parser

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// OtherCategory — категория для трат, которые не удалось определить.
const OtherCategory = "Другое"

var (
	ErrNoAmount  = errors.New("не нашёл сумму")
	ErrBadAmount = errors.New("сумма должна быть больше нуля")
)

// Aliases — правила, которым бот научился у пользователя:
// нормализованный текст (см. NormalizeKey), например "барбершоп" -> "Красота".
type Aliases map[string]string

// Entry — результат разбора одной строки.
type Entry struct {
	Amount   int64 // в копейках
	Category string
	Comment  string
	SpentAt  time.Time

	// Unknown == true: категорию определить не удалось. Тогда Category == OtherCategory,
	// а Comment хранит исходный текст — по нему бот спрашивает пользователя и учится.
	Unknown bool
}

// Одно сообщение может содержать несколько трат: их разделяют перенос строки,
// точка с запятой или запятая с пробелом (запятая без пробела — это "85,5").
var splitRe = regexp.MustCompile(`[\n;]+|,\s+`)

// Split разбивает сообщение на отдельные строки-траты.
func Split(text string) []string {
	var out []string
	for _, p := range splitRe.Split(text, -1) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ParseLine разбирает одну трату по встроенному словарю категорий.
// now — время сообщения в часовом поясе пользователя.
func ParseLine(line string, now time.Time) (Entry, error) {
	return ParseLineWith(line, now, nil)
}

// ParseLineWith то же, что ParseLine, но сначала смотрит в правила пользователя (aliases):
// они приоритетнее встроенного словаря.
//
// Понимает: "450 продукты", "такси 1200", "85,5 кофе", "300р кино",
// "1.5к аренда", "500 подарок вчера", "500 подарок 25.09", "500 подарок 25.09.2026".
func ParseLineWith(line string, now time.Time, aliases Aliases) (Entry, error) {
	tokens := strings.Fields(line)

	var cands []int
	for i, t := range tokens {
		if _, ok := parseAmount(t); ok {
			cands = append(cands, i)
		}
	}
	if len(cands) == 0 {
		return Entry{}, ErrNoAmount
	}

	// Если чисел несколько, "25.09" похоже на дату — сумма это другое число.
	amountIdx := cands[0]
	if len(cands) > 1 {
		amountIdx = cands[len(cands)-1]
		for _, i := range cands {
			if _, ok := parseShortDate(tokens[i], now); !ok {
				amountIdx = i
				break
			}
		}
	}

	amount, _ := parseAmount(tokens[amountIdx])
	if amount <= 0 {
		return Entry{}, ErrBadAmount
	}

	spent := now
	dateFound := false
	var words []string
	for i, t := range tokens {
		if i == amountIdx {
			continue
		}
		if !dateFound {
			if d, ok := parseDate(t, now); ok {
				spent, dateFound = d, true
				continue
			}
		}
		words = append(words, t)
	}

	category, comment, unknown := resolveCategory(strings.Join(words, " "), aliases)
	return Entry{Amount: amount, Category: category, Comment: comment, SpentAt: spent, Unknown: unknown}, nil
}

// ---- сумма ----

// 450 | 85,5 | 12.50 | 300р | 1.5к | 100₽ | 20zł
var amountRe = regexp.MustCompile(`^(\d{1,9})(?:[.,](\d{1,2}))?([кk])?(?:р|р\.|руб|руб\.|₽|zł|pln|usd|eur|\$|€)?$`)

// parseAmount возвращает сумму в копейках.
func parseAmount(tok string) (int64, bool) {
	m := amountRe.FindStringSubmatch(strings.ToLower(tok))
	if m == nil {
		return 0, false
	}
	whole, _ := strconv.ParseInt(m[1], 10, 64)
	var frac int64
	if m[2] != "" {
		f := m[2]
		if len(f) == 1 {
			f += "0" // "85,5" = 85 руб 50 коп
		}
		frac, _ = strconv.ParseInt(f, 10, 64)
	}
	total := whole*100 + frac
	if m[3] != "" {
		total *= 1000 // "1.5к" = 1500
	}
	return total, true
}

// ---- дата ----

var (
	shortDateRe = regexp.MustCompile(`^(\d{1,2})\.(\d{1,2})$`)
	fullDateRe  = regexp.MustCompile(`^(\d{1,2})\.(\d{1,2})\.(\d{4}|\d{2})$`)
	relDays     = map[string]int{"сегодня": 0, "вчера": -1, "позавчера": -2}
)

func parseDate(tok string, now time.Time) (time.Time, bool) {
	low := strings.ToLower(tok)
	if d, ok := relDays[low]; ok {
		return now.AddDate(0, 0, d), true
	}
	if m := fullDateRe.FindStringSubmatch(low); m != nil {
		y, _ := strconv.Atoi(m[3])
		if len(m[3]) == 2 {
			y += 2000
		}
		return makeDate(y, m[1], m[2], now)
	}
	return parseShortDate(low, now)
}

// parseShortDate разбирает "25.09". Если такая дата в этом году ещё не наступила
// (больше чем завтра), считаем, что имелся в виду прошлый год.
func parseShortDate(tok string, now time.Time) (time.Time, bool) {
	m := shortDateRe.FindStringSubmatch(tok)
	if m == nil {
		return time.Time{}, false
	}
	t, ok := makeDate(now.Year(), m[1], m[2], now)
	if ok && t.After(now.AddDate(0, 0, 1)) {
		t, ok = makeDate(now.Year()-1, m[1], m[2], now)
	}
	return t, ok
}

func makeDate(year int, dd, mm string, now time.Time) (time.Time, bool) {
	d, _ := strconv.Atoi(dd)
	mo, _ := strconv.Atoi(mm)
	if mo < 1 || mo > 12 || d < 1 || d > 31 {
		return time.Time{}, false
	}
	t := time.Date(year, time.Month(mo), d, 12, 0, 0, 0, now.Location())
	if t.Day() != d || int(t.Month()) != mo { // например, 31.02
		return time.Time{}, false
	}
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return now, true
	}
	return t, true
}

// ---- категория ----

type rule struct {
	category string
	stems    []string // начала слов, в нижнем регистре, ё заменена на е
}

var rules = []rule{
	{"Продукты", []string{"продукт", "еда", "магазин", "супермаркет", "пятероч", "магнит", "перекрест", "ашан", "овощ", "фрукт", "хлеб", "молок", "мясо", "рынок"}},
	{"Кафе и рестораны", []string{"кафе", "кофе", "ресторан", "обед", "ужин", "завтрак", "пицц", "суши", "бургер", "макдон", "столов", "доставк"}},
	{"Транспорт", []string{"такси", "убер", "uber", "bolt", "метро", "автобус", "трамвай", "маршрутк", "проезд", "бензин", "заправк", "парковк", "электричк", "поезд", "транспорт"}},
	{"Жильё", []string{"аренд", "квартир", "коммунал", "жкх", "ипотек"}},
	{"Связь и интернет", []string{"телефон", "связь", "интернет", "мобильн", "симк", "тариф"}},
	{"Здоровье", []string{"аптек", "врач", "лекарств", "таблетк", "больниц", "клиник", "стоматолог", "анализ", "здоровь"}},
	{"Развлечения", []string{"кино", "театр", "концерт", "игра", "игры", "steam", "стим", "бильярд", "боулинг", "клуб", "развлеч"}},
	{"Одежда", []string{"одежд", "куртк", "футболк", "джинс", "обув", "кроссовк", "ботинк"}},
	{"Подписки", []string{"подписк", "netflix", "spotify", "youtube", "ютуб", "кинопоиск"}},
	{"Подарки", []string{"подар"}},
	{"Образование", []string{"курс", "книг", "учеб", "обучен"}},
	{"Дом и быт", []string{"быт", "хозтовар", "ikea", "икеа", "уборк", "мебел", "посуд"}},
}

// DefaultCategories возвращает названия встроенных категорий (для кнопок выбора).
func DefaultCategories() []string {
	out := make([]string, len(rules))
	for i, r := range rules {
		out[i] = r.category
	}
	return out
}

func norm(s string) string {
	return strings.ReplaceAll(strings.ToLower(s), "ё", "е")
}

// NormalizeKey приводит текст к виду, в котором хранятся и ищутся правила:
// нижний регистр, "ё" -> "е", лишние пробелы и знаки по краям слов убраны.
func NormalizeKey(text string) string {
	var words []string
	for _, w := range strings.Fields(norm(text)) {
		if w = strings.Trim(w, ".,!?:;()\"'«»"); w != "" {
			words = append(words, w)
		}
	}
	return strings.Join(words, " ")
}

// Learnable говорит, есть ли смысл запоминать такой текст как правило:
// одно-три слова. Длинные фразы ("купил вафли для кота") больше не повторятся.
func Learnable(key string) bool {
	n := len(strings.Fields(key))
	return n >= 1 && n <= 3
}

// CleanCategory приводит введённое пользователем название категории к аккуратному виду.
// Возвращает "", если название пустое или это "Другое" (в неё правила не создаются).
func CleanCategory(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return ""
	}
	s = truncate(capitalize(s), 40)
	if norm(s) == norm(OtherCategory) {
		return ""
	}
	return s
}

// resolveCategory определяет категорию: сначала правила пользователя, потом встроенный словарь.
// Если ничего не подошло — "Другое" с исходным текстом в комментарии и unknown == true.
func resolveCategory(text string, aliases Aliases) (category, comment string, unknown bool) {
	key := NormalizeKey(text)
	if key == "" {
		return OtherCategory, "", false
	}

	if cat, ok := lookupAlias(key, aliases); ok {
		return cat, commentFor(text, key, cat), false
	}
	for _, w := range strings.Fields(key) {
		for _, r := range rules {
			for _, s := range r.stems {
				if strings.HasPrefix(w, s) {
					return r.category, commentFor(text, key, r.category), false
				}
			}
		}
	}
	return OtherCategory, truncate(strings.TrimSpace(text), 200), true
}

// lookupAlias: точное совпадение всей фразы, иначе совпадение с любым отдельным словом.
func lookupAlias(key string, aliases Aliases) (string, bool) {
	if len(aliases) == 0 {
		return "", false
	}
	if c, ok := aliases[key]; ok {
		return c, true
	}
	for _, w := range strings.Fields(key) {
		if c, ok := aliases[w]; ok {
			return c, true
		}
	}
	return "", false
}

// commentFor: если текст просто повторяет название категории, комментарий не нужен.
func commentFor(text, key, category string) string {
	if key == NormalizeKey(category) {
		return ""
	}
	return truncate(strings.TrimSpace(text), 200)
}

func capitalize(s string) string {
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
