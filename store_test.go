package store

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"expensebot/domain"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func save(t *testing.T, s *Store, userID, amount int64, category, comment string, at time.Time) int64 {
	t.Helper()
	e := domain.Expense{UserID: userID, Amount: amount, Category: category, Comment: comment, SpentAt: at}
	if err := s.Save(&e); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if e.ID == 0 {
		t.Fatal("Save не заполнил ID")
	}
	return e.ID
}

var day = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

func TestSaveListDelete(t *testing.T) {
	s := newTestStore(t)
	a := save(t, s, 1, 45000, "Продукты", "", day)
	b := save(t, s, 1, 120000, "Транспорт", "такси", day) // то же время: порядок решает id DESC
	save(t, s, 2, 999, "Чужое", "", day)

	list, err := s.List(1, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != b || list[1].ID != a {
		t.Fatalf("ожидал [%d %d] (новые сверху), got %+v", b, a, list)
	}

	if err := s.Delete(a, 2); !errors.Is(err, ErrNotFound) {
		t.Errorf("чужую трату удалить нельзя: got %v, want ErrNotFound", err)
	}
	if err := s.Delete(a, 1); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if err := s.Delete(a, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("повторное удаление: got %v, want ErrNotFound", err)
	}
}

func TestGetAndUpdateCategory(t *testing.T) {
	s := newTestStore(t)
	id := save(t, s, 1, 9000, "Другое", "барбершоп", day)

	e, err := s.Get(id, 1)
	if err != nil || e.Comment != "барбершоп" || e.Amount != 9000 || !e.SpentAt.Equal(day) {
		t.Fatalf("Get: %+v, %v", e, err)
	}
	if _, err := s.Get(id, 2); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get чужой траты: got %v, want ErrNotFound", err)
	}

	if err := s.UpdateCategory(id, 2, "Взлом"); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateCategory чужой траты: got %v, want ErrNotFound", err)
	}
	if err := s.UpdateCategory(id, 1, "Красота"); err != nil {
		t.Fatalf("UpdateCategory: %v", err)
	}
	if e, _ := s.Get(id, 1); e.Category != "Красота" {
		t.Errorf("категория не обновилась: %q", e.Category)
	}
	// то же значение повторно — не "не найдено"
	if err := s.UpdateCategory(id, 1, "Красота"); err != nil {
		t.Errorf("повторное обновление тем же значением: %v", err)
	}
}

func TestAliases(t *testing.T) {
	s := newTestStore(t)

	id1, err := s.SetAlias(1, "барбершоп", "Красота")
	if err != nil || id1 == 0 {
		t.Fatalf("SetAlias: %d, %v", id1, err)
	}
	// повторное правило для того же слова меняет категорию, id остаётся тем же
	id2, err := s.SetAlias(1, "барбершоп", "Здоровье")
	if err != nil || id2 != id1 {
		t.Fatalf("SetAlias повторно: id %d (want %d), %v", id2, id1, err)
	}
	if _, err := s.SetAlias(1, "шаурма", "Кафе и рестораны"); err != nil {
		t.Fatal(err)
	}
	other, err := s.SetAlias(2, "барбершоп", "Личное")
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Aliases(1)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"барбершоп": "Здоровье", "шаурма": "Кафе и рестораны"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Aliases(1) = %v, want %v", got, want)
	}

	if empty, err := s.Aliases(3); err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("у пользователя без правил ожидаю пустую карту, got %v, %v", empty, err)
	}

	if err := s.DeleteAlias(other, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("чужое правило удалить нельзя: got %v", err)
	}
	if err := s.DeleteAlias(id1, 1); err != nil {
		t.Errorf("DeleteAlias: %v", err)
	}
	if err := s.DeleteAlias(id1, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("повторное удаление правила: got %v", err)
	}
	if got, _ := s.Aliases(1); len(got) != 1 {
		t.Errorf("после удаления должно остаться одно правило, got %v", got)
	}
}

func TestCategories(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 3; i++ {
		save(t, s, 1, 100, "Продукты", "", day)
	}
	for i := 0; i < 2; i++ {
		save(t, s, 1, 100, "Красота", "", day)
	}
	save(t, s, 1, 100, "Другое", "что-то", day)
	save(t, s, 2, 100, "Чужая", "", day)

	got, err := s.Categories(1, "Другое", 10)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Продукты", "Красота"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Categories = %v, want %v", got, want)
	}
	if got, _ := s.Categories(1, "Другое", 1); len(got) != 1 || got[0] != "Продукты" {
		t.Errorf("limit не сработал: %v", got)
	}
}
