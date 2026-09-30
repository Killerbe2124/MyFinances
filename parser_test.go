package parser

import (
	"errors"
	"testing"
	"time"
)

// 29 сентября 2026, 15:30
var now = time.Date(2026, 9, 29, 15, 30, 0, 0, time.FixedZone("test", 2*3600))

func TestParseLine(t *testing.T) {
	tests := []struct {
		in       string
		amount   int64
		category string
		comment  string
		unknown  bool
		y, m, d  int
	}{
		{"450 продукты", 45000, "Продукты", "", false, 2026, 9, 29},
		{"85,5 кофе", 8550, "Кафе и рестораны", "кофе", false, 2026, 9, 29},
		{"такси 1200", 120000, "Транспорт", "такси", false, 2026, 9, 29},
		{"1200 такси вчера", 120000, "Транспорт", "такси", false, 2026, 9, 28},
		{"1200 такси Позавчера", 120000, "Транспорт", "такси", false, 2026, 9, 27},
		{"300р кино", 30000, "Развлечения", "кино", false, 2026, 9, 29},
		{"1.5к аренда", 150000, "Жильё", "аренда", false, 2026, 9, 29},
		{"12.50 хлеб", 1250, "Продукты", "хлеб", false, 2026, 9, 29},
		{"500 подарок 25.09", 50000, "Подарки", "подарок", false, 2026, 9, 25},
		{"25.09 500 подарок", 50000, "Подарки", "подарок", false, 2026, 9, 25},
		{"500 подарок 25.09.2025", 50000, "Подарки", "подарок", false, 2025, 9, 25},
		{"500 подарок 25.09.25", 50000, "Подарки", "подарок", false, 2025, 9, 25},
		{"500 подарок 05.10", 50000, "Подарки", "подарок", false, 2025, 10, 5}, // в будущем -> прошлый год
		{"250 Пятёрочка молоко", 25000, "Продукты", "Пятёрочка молоко", false, 2026, 9, 29},
		{"450", 45000, "Другое", "", false, 2026, 9, 29},

		// неизвестный текст: не выдумываем категорию, а помечаем Unknown — бот спросит пользователя
		{"200 ремонт машины", 20000, "Другое", "ремонт машины", true, 2026, 9, 29},
		{"100 купил вафли для кота", 10000, "Другое", "купил вафли для кота", true, 2026, 9, 29},
		{"90 барбершоп вчера", 9000, "Другое", "барбершоп", true, 2026, 9, 28},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			e, err := ParseLine(tc.in, now)
			if err != nil {
				t.Fatalf("неожиданная ошибка: %v", err)
			}
			if e.Amount != tc.amount {
				t.Errorf("сумма: got %d, want %d", e.Amount, tc.amount)
			}
			if e.Category != tc.category {
				t.Errorf("категория: got %q, want %q", e.Category, tc.category)
			}
			if e.Comment != tc.comment {
				t.Errorf("комментарий: got %q, want %q", e.Comment, tc.comment)
			}
			if e.Unknown != tc.unknown {
				t.Errorf("Unknown: got %v, want %v", e.Unknown, tc.unknown)
			}
			if y, m, d := e.SpentAt.Date(); y != tc.y || int(m) != tc.m || d != tc.d {
				t.Errorf("дата: got %v, want %d-%02d-%02d", e.SpentAt.Format("2006-01-02"), tc.y, tc.m, tc.d)
			}
		})
	}
}

func TestParseLineWithAliases(t *testing.T) {
	aliases := Aliases{
		"барбершоп":     "Красота",
		"ремонт машины": "Авто",
		"кофе":          "Работа", // правило пользователя важнее встроенного словаря
	}
	tests := []struct {
		in       string
		category string
		comment  string
		unknown  bool
	}{
		{"90 барбершоп", "Красота", "барбершоп", false},
		{"90 Барбершоп!", "Красота", "Барбершоп!", false},             // регистр и знаки не мешают
		{"90 барбершоп у дома", "Красота", "барбершоп у дома", false}, // однословное правило ловит слово внутри фразы
		{"200 ремонт машины", "Авто", "ремонт машины", false},         // фраза целиком
		{"200 ремонт", "Другое", "ремонт", true},                      // многословное правило не срабатывает на одно слово
		{"85,5 кофе", "Работа", "кофе", false},                        // переопределили встроенную категорию
		{"450 продукты", "Продукты", "", false},                       // остальное работает как раньше
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			e, err := ParseLineWith(tc.in, now, aliases)
			if err != nil {
				t.Fatalf("неожиданная ошибка: %v", err)
			}
			if e.Category != tc.category || e.Comment != tc.comment || e.Unknown != tc.unknown {
				t.Errorf("got {%q, %q, unknown=%v}, want {%q, %q, unknown=%v}",
					e.Category, e.Comment, e.Unknown, tc.category, tc.comment, tc.unknown)
			}
		})
	}
}

func TestNormalizeKey(t *testing.T) {
	tests := map[string]string{
		"Барбершоп":           "барбершоп",
		"  Ремонт   МАШИНЫ! ": "ремонт машины",
		"Пятёрочка":           "пятерочка",
		"«Кофейня» (у дома)":  "кофейня у дома",
		"...":                 "",
	}
	for in, want := range tests {
		if got := NormalizeKey(in); got != want {
			t.Errorf("NormalizeKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLearnable(t *testing.T) {
	for key, want := range map[string]bool{
		"":                     false,
		"барбершоп":            true,
		"ремонт машины":        true,
		"кофе с собой у метро": false,
		"купил вафли для кота": false,
		"один два три":         true,
	} {
		if got := Learnable(key); got != want {
			t.Errorf("Learnable(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestCleanCategory(t *testing.T) {
	tests := map[string]string{
		"красота":          "Красота",
		"  уличная   еда ": "Уличная еда",
		"Другое":           "", // нельзя учить в "Другое"
		"другое":           "",
		"   ":              "",
		"":                 "",
	}
	for in, want := range tests {
		if got := CleanCategory(in); got != want {
			t.Errorf("CleanCategory(%q) = %q, want %q", in, got, want)
		}
	}
	long := CleanCategory("очень длинное название категории которое не должно влезать в кнопку телеграма")
	if n := len([]rune(long)); n != 40 {
		t.Errorf("длинное название должно обрезаться до 40 символов, got %d", n)
	}
}

func TestDefaultCategories(t *testing.T) {
	cats := DefaultCategories()
	if len(cats) == 0 || cats[0] != "Продукты" {
		t.Fatalf("неожиданный список категорий: %v", cats)
	}
	cats[0] = "испорчено" // возвращается копия, встроенные правила не должны меняться
	if DefaultCategories()[0] != "Продукты" {
		t.Error("DefaultCategories отдаёт внутренний слайс, а не копию")
	}
}

func TestParseLineErrors(t *testing.T) {
	if _, err := ParseLine("просто текст", now); !errors.Is(err, ErrNoAmount) {
		t.Errorf("want ErrNoAmount, got %v", err)
	}
	if _, err := ParseLine("", now); !errors.Is(err, ErrNoAmount) {
		t.Errorf("пустая строка: want ErrNoAmount, got %v", err)
	}
	if _, err := ParseLine("0 хлеб", now); !errors.Is(err, ErrBadAmount) {
		t.Errorf("want ErrBadAmount, got %v", err)
	}
	if _, err := ParseLine("99999999999999 хлеб", now); !errors.Is(err, ErrNoAmount) {
		t.Errorf("слишком большое число не должно считаться суммой, got %v", err)
	}
}

func TestSplit(t *testing.T) {
	got := Split("450 продукты, 1200 такси; 85,5 кофе\n\n300 кино ")
	want := []string{"450 продукты", "1200 такси", "85,5 кофе", "300 кино"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q, want %q", i, got[i], want[i])
		}
	}
}
