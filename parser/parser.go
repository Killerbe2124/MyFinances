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

var (
	ErrNoAmount  = errors.New("не нашёл сумму")
	ErrBadAmount = errors.New("сумма должна быть больше нуля")
)

type Entry struct {
	Amount   int64
	Category string
	Comment  string
	SpentAt  time.Time
}

var splitRe = regexp.MustCompile(`[\n;]+|,\s+`)

func Split(text string) []string {
	var out []string
	for _, p := range splitRe.Split(text, -1) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

var spaceThousandsRe = regexp.MustCompile(`(?:^|[^0-9.,])(\d{1,3}(?: \d{3})+)\b`)

func normalizeThousands(line string) string {
	return spaceThousandsRe.ReplaceAllStringFunc(line, func(m string) string {
		if len(m) > 0 && (m[0] < '0' || m[0] > '9') {
			return string(m[0]) + strings.ReplaceAll(m[1:], " ", "_")
		}
		return strings.ReplaceAll(m, " ", "_")
	})
}

func ParseLine(line string, now time.Time) (Entry, error) {
	line = normalizeThousands(line)
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

	category, comment := resolveCategory(strings.Join(words, " "))
	return Entry{Amount: amount, Category: category, Comment: comment, SpentAt: spent}, nil
}

var amountRe = regexp.MustCompile(`^((?:\d{1,3}(?:[ _]\d{3})+)|\d{1,9})(?:[.,](\d{1,2}))?([кk])?$`)

func parseAmount(tok string) (int64, bool) {
	m := amountRe.FindStringSubmatch(tok)
	if m == nil {
		return 0, false
	}

	wholeStr := strings.ReplaceAll(strings.ReplaceAll(m[1], " ", ""), "_", "")
	whole, err := strconv.ParseInt(wholeStr, 10, 64)
	if err != nil {
		return 0, false
	}

	var frac int64
	if m[2] != "" {
		f := m[2]
		if len(f) == 1 {
			f += "0"
		}
		frac, err = strconv.ParseInt(f, 10, 64)
		if err != nil {
			return 0, false
		}
	}

	total := whole*100 + frac
	if m[3] != "" {
		total *= 1000
	}
	return total, true
}

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
	if t.Day() != d || int(t.Month()) != mo {
		return time.Time{}, false
	}
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return now, true
	}
	return t, true
}

type rule struct {
	category string
	stems    []string
}

var rules = []rule{
	{"Продукты", []string{"продукт", "еда", "магазин", "супермаркет", "пятероч", "магнит", "перекрест", "ашан", "овощ", "фрукт", "хлеб", "молок", "мясо", "рынок"}},
	{"Кафе и рестораны", []string{"кафе", "кофе", "ресторан", "обед", "ужин", "завтрак", "пицц", "суши", "бургер", "макдон", "столов", "доставк"}},
	{"Транспорт", []string{"такси", "убер", "uber", "bolt", "метро", "автобус", "трамвай", "маршрутк", "проезд", "бензин", "заправк", "парковк", "электричк", "поезд", "транспорт", "ехал", "поехал", "доехал"}},
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

func norm(s string) string {
	return strings.ReplaceAll(strings.ToLower(s), "ё", "е")
}

var bareCategory = map[string]bool{
	"продукты": true,
	"такси":    true,
	"аренда":   true,
}

func resolveCategory(text string) (category, comment string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "Другое", ""
	}

	normText := norm(text)

	for _, w := range strings.Fields(normText) {
		w = strings.Trim(w, ".,!?:()\"'")
		for _, r := range rules {
			for _, s := range r.stems {
				if strings.HasPrefix(w, s) {
					if normText == norm(r.category) || bareCategory[normText] {
						return r.category, ""
					}
					return r.category, truncate(text, 200)
				}
			}
		}
	}

	if len(strings.Fields(text)) <= 2 {
		return truncate(capitalize(text), 40), ""
	}

	return "Другое", truncate(text, 200)
}

func capitalize(s string) string {
	if s == "" {
		return ""
	}
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
