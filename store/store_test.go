package store

import (
	"os"
	"testing"
	"time"

	"expensebot/domain"
)

// newTestStore создаёт временную БД для тестов.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	f, err := os.CreateTemp("", "bot_test_*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	s, err := New(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSaveAndList(t *testing.T) {
	s := newTestStore(t)
	loc := time.FixedZone("test", 2*3600)
	now := time.Date(2026, 9, 29, 15, 30, 0, 0, loc)

	e := &domain.Expense{
		UserID:   42,
		Amount:   45000,
		Category: "Продукты",
		Comment:  "молоко",
		SpentAt:  now,
	}
	if err := s.Save(e); err != nil {
		t.Fatalf("save: %v", err)
	}
	if e.ID == 0 {
		t.Fatal("ID не заполнился после Save")
	}

	// Вторая трата
	e2 := &domain.Expense{
		UserID:   42,
		Amount:   120000,
		Category: "Транспорт",
		SpentAt:  now.Add(-24 * time.Hour),
	}
	if err := s.Save(e2); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Чужая трата — не должна попасть в выборку
	e3 := &domain.Expense{
		UserID:   99,
		Amount:   100,
		Category: "Другое",
		SpentAt:  now,
	}
	_ = s.Save(e3)

	got, err := s.List(42, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ожидали 2 записи, получили %d", len(got))
	}
	// Порядок: DESC по дате
	if got[0].Category != "Продукты" {
		t.Errorf("первая запись: got %q, want Продукты", got[0].Category)
	}
}

func TestStatsByCategory(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)

	for _, amt := range []int64{10000, 20000, 5000} {
		_ = s.Save(&domain.Expense{
			UserID:   1,
			Amount:   amt,
			Category: "Еда",
			SpentAt:  now,
		})
	}
	_ = s.Save(&domain.Expense{
		UserID:   1,
		Amount:   50000,
		Category: "Транспорт",
		SpentAt:  now,
	})

	stats, err := s.StatsByCategory(1, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("ожидали 2 категории, получили %d", len(stats))
	}
	// Сортировка по убыванию суммы
	if stats[0].Category != "Транспорт" || stats[0].Total != 50000 || stats[0].Count != 1 {
		t.Errorf("первая категория: got %+v", stats[0])
	}
	if stats[1].Category != "Еда" || stats[1].Total != 35000 || stats[1].Count != 3 {
		t.Errorf("вторая категория: got %+v", stats[1])
	}
}

func TestDelete(t *testing.T) {
	s := newTestStore(t)
	e := &domain.Expense{UserID: 1, Amount: 100, Category: "X", SpentAt: time.Now()}
	_ = s.Save(e)

	if err := s.Delete(e.ID, 1); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Повторное удаление должно вернуть ошибку
	if err := s.Delete(e.ID, 1); err == nil {
		t.Error("ожидали ошибку при повторном удалении")
	}
	// Удаление чужой записи
	e2 := &domain.Expense{UserID: 2, Amount: 200, Category: "Y", SpentAt: time.Now()}
	_ = s.Save(e2)
	if err := s.Delete(e2.ID, 999); err == nil {
		t.Error("ожидали ошибку при удалении чужой записи")
	}
}
