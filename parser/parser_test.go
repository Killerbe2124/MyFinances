package parser

import (
	"errors"
	"testing"
	"time"
)

var now = time.Date(2026, 9, 29, 15, 30, 0, 0, time.FixedZone("test", 2*3600))

func TestParseLine(t *testing.T) {
	tests := []struct {
		in       string
		amount   int64
		category string
		comment  string
		y, m, d  int
	}{
		{"450 продукты", 45000, "Продукты", "", 2026, 9, 29},
		{"85,5 кофе", 8550, "Кафе и рестораны", "кофе", 2026, 9, 29},
		{"такси 1200", 120000, "Транспорт", "", 2026, 9, 29},
		{"1200 такси вчера", 120000, "Транспорт", "", 2026, 9, 28},
		{"1200 такси Позавчера", 120000, "Транспорт", "", 2026, 9, 27},
		{"300 кино", 30000, "Развлечения", "кино", 2026, 9, 29},
		{"1.5к аренда", 150000, "Жильё", "", 2026, 9, 29},
		{"12.50 хлеб", 1250, "Продукты", "хлеб", 2026, 9, 29},
		{"500 подарок 25.09", 50000, "Подарки", "подарок", 2026, 9, 25},
		{"25.09 500 подарок", 50000, "Подарки", "подарок", 2026, 9, 25},
		{"500 подарок 25.09.2025", 50000, "Подарки", "подарок", 2025, 9, 25},
		{"500 подарок 25.09.25", 50000, "Подарки", "подарок", 2025, 9, 25},
		{"500 подарок 05.10", 50000, "Подарки", "подарок", 2025, 10, 5},
		{"200 ремонт машины", 20000, "Ремонт машины", "", 2026, 9, 29},
		{"450", 45000, "Другое", "", 2026, 9, 29},
		{"100 купил вафли для кота", 10000, "Другое", "купил вафли для кота", 2026, 9, 29},
		{"250 Пятёрочка молоко", 25000, "Продукты", "Пятёрочка молоко", 2026, 9, 29},
		{"1.5к такси", 150000, "Транспорт", "", 2026, 9, 29},
		{"1,5к такси", 150000, "Транспорт", "", 2026, 9, 29},
		{"1500 такси", 150000, "Транспорт", "", 2026, 9, 29},
		{"1 500 такси", 150000, "Транспорт", "", 2026, 9, 29},
		{"1_500 такси", 150000, "Транспорт", "", 2026, 9, 29},
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
			if y, m, d := e.SpentAt.Date(); y != tc.y || int(m) != tc.m || d != tc.d {
				t.Errorf("дата: got %v, want %d-%02d-%02d", e.SpentAt.Format("2006-01-02"), tc.y, tc.m, tc.d)
			}
		})
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
	got := Split("450 продукты, 1200 такси; 85,5 кофе\n\n300 кино")
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
