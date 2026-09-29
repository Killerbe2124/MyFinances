// Package store отвечает за сохранение и чтение трат в SQLite.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"expensebot/domain"

	_ "modernc.org/sqlite" // регистрация драйвера "sqlite"
)

// Store — хранилище трат.
type Store struct {
	db *sql.DB
}

// New открывает (или создаёт) файл БД и готовит таблицы.
func New(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// Ограничиваем пул соединений: SQLite плохо дружит с большим числом писателей.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// migrate создаёт таблицы, если их ещё нет.
func (s *Store) migrate() error {
	query := `
	CREATE TABLE IF NOT EXISTS expenses (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id    INTEGER NOT NULL,
		amount     INTEGER NOT NULL,
		category   TEXT    NOT NULL,
		comment    TEXT    NOT NULL DEFAULT '',
		spent_at   TEXT    NOT NULL  -- ISO-8601 в UTC
	);
	CREATE INDEX IF NOT EXISTS idx_expenses_user_spent
		ON expenses(user_id, spent_at);
	CREATE INDEX IF NOT EXISTS idx_expenses_category
		ON expenses(user_id, category);
	`
	_, err := s.db.Exec(query)
	return err
}

// Save сохраняет трату и заполняет expense.ID.
func (s *Store) Save(expense *domain.Expense) error {
	if expense.UserID == 0 {
		return errors.New("user_id не задан")
	}
	if expense.Amount <= 0 {
		return errors.New("amount должен быть > 0")
	}

	// Храним время в UTC — так проще считать статистику и не зависеть от часового пояса сервера.
	spentUTC := expense.SpentAt.UTC().Format(time.RFC3339)

	res, err := s.db.Exec(`
		INSERT INTO expenses (user_id, amount, category, comment, spent_at)
		VALUES (?, ?, ?, ?, ?)`,
		expense.UserID,
		expense.Amount,
		expense.Category,
		expense.Comment,
		spentUTC,
	)
	if err != nil {
		return fmt.Errorf("insert: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("last insert id: %w", err)
	}
	expense.ID = id
	return nil
}

// List возвращает траты пользователя за период [from, to).
// Если from или to нулевые — границы не применяются.
func (s *Store) List(userID int64, from, to time.Time) ([]domain.Expense, error) {
	query := `SELECT id, user_id, amount, category, comment, spent_at
	          FROM expenses WHERE user_id = ?`
	args := []interface{}{userID}

	if !from.IsZero() {
		query += " AND spent_at >= ?"
		args = append(args, from.UTC().Format(time.RFC3339))
	}
	if !to.IsZero() {
		query += " AND spent_at < ?"
		args = append(args, to.UTC().Format(time.RFC3339))
	}
	query += " ORDER BY spent_at DESC"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	var out []domain.Expense
	for rows.Next() {
		var e domain.Expense
		var spentStr string
		if err := rows.Scan(&e.ID, &e.UserID, &e.Amount, &e.Category, &e.Comment, &spentStr); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		t, err := time.Parse(time.RFC3339, spentStr)
		if err != nil {
			return nil, fmt.Errorf("parse time: %w", err)
		}
		e.SpentAt = t
		out = append(out, e)
	}
	return out, rows.Err()
}

// StatsByCategory считает сумму и количество трат по категориям за период.
func (s *Store) StatsByCategory(userID int64, from, to time.Time) ([]domain.CategorySum, error) {
	query := `SELECT category, SUM(amount), COUNT(*)
	          FROM expenses WHERE user_id = ?`
	args := []interface{}{userID}

	if !from.IsZero() {
		query += " AND spent_at >= ?"
		args = append(args, from.UTC().Format(time.RFC3339))
	}
	if !to.IsZero() {
		query += " AND spent_at < ?"
		args = append(args, to.UTC().Format(time.RFC3339))
	}
	query += " GROUP BY category ORDER BY SUM(amount) DESC"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	var out []domain.CategorySum
	for rows.Next() {
		var cs domain.CategorySum
		if err := rows.Scan(&cs.Category, &cs.Total, &cs.Count); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, cs)
	}
	return out, rows.Err()
}

// Delete удаляет трату. userID нужен, чтобы пользователь не мог удалить чужую запись.
func (s *Store) Delete(id, userID int64) error {
	res, err := s.db.Exec(`DELETE FROM expenses WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("запись не найдена или принадлежит другому пользователю")
	}
	return nil
}

// Close закрывает соединение с БД.
func (s *Store) Close() error {
	return s.db.Close()
}
