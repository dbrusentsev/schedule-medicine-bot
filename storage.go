package main

import (
	"context"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Storage struct {
	pool *pgxpool.Pool
}

func NewStorage(databaseURL string) (*Storage, error) {
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	if err := pool.Ping(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	storage := &Storage{pool: pool}
	if err := storage.createTables(); err != nil {
		return nil, fmt.Errorf("failed to create tables: %w", err)
	}

	log.Println("Connected to PostgreSQL")
	return storage, nil
}

func (s *Storage) createTables() error {
	ctx := context.Background()

	_, err := s.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS users (
			chat_id BIGINT PRIMARY KEY,
			active BOOLEAN DEFAULT true,
			created_at TIMESTAMP DEFAULT NOW()
		);

		CREATE TABLE IF NOT EXISTS reminders (
			id SERIAL PRIMARY KEY,
			chat_id BIGINT REFERENCES users(chat_id) ON DELETE CASCADE,
			medicine VARCHAR(255) NOT NULL,
			hour INT NOT NULL,
			minute INT NOT NULL,
			course_days INT DEFAULT 0,
			doses_taken INT DEFAULT 0,
			created_at TIMESTAMP DEFAULT NOW()
		);

		CREATE INDEX IF NOT EXISTS idx_reminders_chat_id ON reminders(chat_id);
		CREATE INDEX IF NOT EXISTS idx_reminders_time ON reminders(hour, minute);
	`)

	return err
}

func (s *Storage) Close() {
	s.pool.Close()
}

// GetOrCreateUser возвращает пользователя, создаёт если не существует
func (s *Storage) GetOrCreateUser(chatID int64) (*User, error) {
	ctx := context.Background()

	_, err := s.pool.Exec(ctx, `
		INSERT INTO users (chat_id, active) VALUES ($1, true)
		ON CONFLICT (chat_id) DO NOTHING
	`, chatID)
	if err != nil {
		return nil, err
	}

	return s.GetUser(chatID)
}

// GetUser возвращает пользователя по chat_id
func (s *Storage) GetUser(chatID int64) (*User, error) {
	ctx := context.Background()

	var active bool
	err := s.pool.QueryRow(ctx, `
		SELECT active FROM users WHERE chat_id = $1
	`, chatID).Scan(&active)

	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	reminders, err := s.GetReminders(chatID)
	if err != nil {
		return nil, err
	}

	return &User{
		ChatID:    chatID,
		Active:    active,
		Reminders: reminders,
	}, nil
}

// SetUserActive устанавливает статус активности пользователя
func (s *Storage) SetUserActive(chatID int64, active bool) error {
	ctx := context.Background()
	_, err := s.pool.Exec(ctx, `
		UPDATE users SET active = $1 WHERE chat_id = $2
	`, active, chatID)
	return err
}

// GetReminders возвращает все напоминания пользователя
func (s *Storage) GetReminders(chatID int64) ([]Reminder, error) {
	ctx := context.Background()

	rows, err := s.pool.Query(ctx, `
		SELECT id, medicine, hour, minute, course_days, doses_taken
		FROM reminders WHERE chat_id = $1
		ORDER BY hour, minute
	`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var reminders []Reminder
	for rows.Next() {
		var r Reminder
		if err := rows.Scan(&r.ID, &r.Medicine, &r.Hour, &r.Minute, &r.CourseDays, &r.DosesTaken); err != nil {
			return nil, err
		}
		reminders = append(reminders, r)
	}

	return reminders, rows.Err()
}

// AddReminder добавляет напоминание и возвращает его ID
func (s *Storage) AddReminder(chatID int64, medicine string, hour, minute, courseDays int) (int, error) {
	ctx := context.Background()

	var id int
	err := s.pool.QueryRow(ctx, `
		INSERT INTO reminders (chat_id, medicine, hour, minute, course_days)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id
	`, chatID, medicine, hour, minute, courseDays).Scan(&id)

	return id, err
}

// DeleteReminder удаляет напоминание
func (s *Storage) DeleteReminder(chatID int64, reminderID int) error {
	ctx := context.Background()
	_, err := s.pool.Exec(ctx, `
		DELETE FROM reminders WHERE id = $1 AND chat_id = $2
	`, reminderID, chatID)
	return err
}

// GetRemindersForTime возвращает напоминания для указанного времени
func (s *Storage) GetRemindersForTime(hour, minute int) (map[int64][]Reminder, error) {
	ctx := context.Background()

	rows, err := s.pool.Query(ctx, `
		SELECT r.chat_id, r.id, r.medicine, r.hour, r.minute, r.course_days, r.doses_taken
		FROM reminders r
		JOIN users u ON r.chat_id = u.chat_id
		WHERE r.hour = $1 AND r.minute = $2
		  AND u.active = true
		  AND (r.course_days = 0 OR r.doses_taken < r.course_days)
	`, hour, minute)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int64][]Reminder)
	for rows.Next() {
		var chatID int64
		var r Reminder
		if err := rows.Scan(&chatID, &r.ID, &r.Medicine, &r.Hour, &r.Minute, &r.CourseDays, &r.DosesTaken); err != nil {
			return nil, err
		}
		result[chatID] = append(result[chatID], r)
	}

	return result, rows.Err()
}

// IncrementDoseTaken увеличивает счётчик и возвращает информацию о напоминании
func (s *Storage) IncrementDoseTaken(chatID int64, reminderID int) (medicineName string, newCount int, total int, completed bool, err error) {
	ctx := context.Background()

	err = s.pool.QueryRow(ctx, `
		UPDATE reminders
		SET doses_taken = doses_taken + 1
		WHERE id = $1 AND chat_id = $2
		RETURNING medicine, doses_taken, course_days
	`, reminderID, chatID).Scan(&medicineName, &newCount, &total)

	if err == pgx.ErrNoRows {
		return "", 0, 0, false, nil
	}
	if err != nil {
		return "", 0, 0, false, err
	}

	completed = total > 0 && newCount >= total
	if completed {
		s.DeleteReminder(chatID, reminderID)
	}

	return medicineName, newCount, total, completed, nil
}

// GetStats возвращает статистику для админа
func (s *Storage) GetStats() (totalUsers, activeUsers, totalReminders, finiteCourses, infiniteCourses, totalDosesTaken, totalDosesPlanned int, err error) {
	ctx := context.Background()

	err = s.pool.QueryRow(ctx, `
		SELECT
			(SELECT COUNT(*) FROM users),
			(SELECT COUNT(*) FROM users WHERE active = true),
			(SELECT COUNT(*) FROM reminders),
			(SELECT COUNT(*) FROM reminders WHERE course_days > 0),
			(SELECT COUNT(*) FROM reminders WHERE course_days = 0),
			(SELECT COALESCE(SUM(doses_taken), 0) FROM reminders),
			(SELECT COALESCE(SUM(course_days), 0) FROM reminders WHERE course_days > 0)
	`).Scan(&totalUsers, &activeUsers, &totalReminders, &finiteCourses, &infiniteCourses, &totalDosesTaken, &totalDosesPlanned)

	return
}

// GetAllUsers возвращает все chat_id пользователей
func (s *Storage) GetAllUsers() ([]int64, error) {
	ctx := context.Background()

	rows, err := s.pool.Query(ctx, `SELECT chat_id FROM users`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chatIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		chatIDs = append(chatIDs, id)
	}

	return chatIDs, rows.Err()
}
