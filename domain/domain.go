// Package domain содержит базовые структуры данных бота.
package domain

import "time"

// Expense — одна трата. Сумма хранится в копейках, чтобы не ловить
// ошибки округления, которые бывают у float.
type Expense struct {
	ID       int64
	UserID   int64
	Amount   int64
	Category string
	Comment  string
	SpentAt  time.Time
}

// CategorySum — итог по одной категории за период.
type CategorySum struct {
	Category string
	Total    int64
	Count    int
}
