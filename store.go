package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Message struct {
	ID         string    `json:"id"`
	ReceivedAt time.Time `json:"received_at"`
	Sender     string    `json:"sender"`
	Recipients []string  `json:"recipients"`
	Size       int       `json:"size"`
}

type StoredMessage struct {
	Message
	Raw []byte `json:"-"`
}

type MaildropMessage struct {
	ID   string
	Size int
}

type Store struct {
	db    *sql.DB
	locks sync.Map
}

func (s *Store) TryLockMaildrop(recipient string) bool {
	_, loaded := s.locks.LoadOrStore(recipient, struct{}{})
	return !loaded
}

func (s *Store) UnlockMaildrop(recipient string) {
	s.locks.Delete(recipient)
}

func NewStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	statements := `
CREATE TABLE IF NOT EXISTS messages (
	id TEXT PRIMARY KEY,
	received_at TEXT NOT NULL,
	sender TEXT NOT NULL,
	raw BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS message_recipients (
	message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
	recipient TEXT NOT NULL,
	PRIMARY KEY(message_id, recipient)
);
CREATE INDEX IF NOT EXISTS idx_message_recipients_recipient ON message_recipients(recipient);
`
	_, err := s.db.ExecContext(ctx, statements)
	return err
}

func (s *Store) Save(ctx context.Context, sender string, recipients []string, raw []byte) (string, error) {
	id, err := newMessageID()
	if err != nil {
		return "", err
	}
	receivedAt := time.Now().UTC().Format(time.RFC3339Nano)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO messages(id, received_at, sender, raw) VALUES (?, ?, ?, ?)`,
		id, receivedAt, sender, raw); err != nil {
		return "", err
	}
	for _, recipient := range recipients {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO message_recipients(message_id, recipient) VALUES (?, ?)`,
			id, recipient); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) ListByRecipient(ctx context.Context, recipient string) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT m.id, m.received_at, m.sender, length(m.raw)
FROM messages m
JOIN message_recipients r ON r.message_id = m.id
WHERE r.recipient = ?
ORDER BY m.received_at DESC, m.id DESC`, recipient)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type messageRow struct {
		message    Message
		receivedAt string
	}
	rowsData := make([]messageRow, 0)
	for rows.Next() {
		var message Message
		var receivedAt string
		if err := rows.Scan(&message.ID, &receivedAt, &message.Sender, &message.Size); err != nil {
			return nil, err
		}
		message.ReceivedAt, err = time.Parse(time.RFC3339Nano, receivedAt)
		if err != nil {
			return nil, err
		}
		rowsData = append(rowsData, messageRow{message: message, receivedAt: receivedAt})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	messages := make([]Message, 0, len(rowsData))
	for _, row := range rowsData {
		message := row.message
		var err error
		message.ReceivedAt, err = time.Parse(time.RFC3339Nano, row.receivedAt)
		if err != nil {
			return nil, err
		}
		message.Recipients, err = s.recipientsFor(ctx, message.ID)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

func (s *Store) GetForRecipient(ctx context.Context, id, recipient string, includeRaw bool) (StoredMessage, bool, error) {
	if !validMessageID(id) {
		return StoredMessage{}, false, nil
	}
	var stored StoredMessage
	var receivedAt string
	rawColumn := "x''"
	if includeRaw {
		rawColumn = "m.raw"
	}
	err := s.db.QueryRowContext(ctx, `
SELECT m.id, m.received_at, m.sender, `+rawColumn+`, length(m.raw)
FROM messages m
JOIN message_recipients requested ON requested.message_id = m.id AND requested.recipient = ?
WHERE m.id = ?`, recipient, id).
		Scan(&stored.ID, &receivedAt, &stored.Sender, &stored.Raw, &stored.Size)
	if err == sql.ErrNoRows {
		return StoredMessage{}, false, nil
	}
	if err != nil {
		return StoredMessage{}, false, err
	}
	stored.ReceivedAt, err = time.Parse(time.RFC3339Nano, receivedAt)
	if err != nil {
		return StoredMessage{}, false, err
	}
	stored.Recipients, err = s.recipientsFor(ctx, id)
	if err != nil {
		return StoredMessage{}, false, err
	}
	return stored, true, nil
}

func (s *Store) MaildropSnapshot(ctx context.Context, recipient string) ([]MaildropMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT m.id, length(m.raw)
FROM messages m
JOIN message_recipients r ON r.message_id = m.id
WHERE r.recipient = ?
ORDER BY m.received_at ASC, m.id ASC`, recipient)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages := make([]MaildropMessage, 0)
	for rows.Next() {
		var message MaildropMessage
		if err := rows.Scan(&message.ID, &message.Size); err != nil {
			return nil, err
		}
		if !validMessageID(message.ID) {
			return nil, fmt.Errorf("invalid message id in database")
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

func (s *Store) DeleteForRecipient(ctx context.Context, recipient string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	args := make([]any, 0, len(ids)+1)
	args = append(args, recipient)
	placeholders := make([]string, 0, len(ids))
	for _, id := range ids {
		if !validMessageID(id) {
			return fmt.Errorf("invalid message id")
		}
		args = append(args, id)
		placeholders = append(placeholders, "?")
	}
	statement := `
DELETE FROM message_recipients
WHERE recipient = ? AND message_id IN (` + strings.Join(placeholders, ",") + `)`
	if _, err := tx.ExecContext(ctx, statement, args...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM messages
WHERE id IN (`+strings.Join(placeholders, ",")+`)
AND NOT EXISTS (SELECT 1 FROM message_recipients WHERE message_id = messages.id)`, args[1:]...); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) recipientsFor(ctx context.Context, id string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT recipient FROM message_recipients WHERE message_id = ? ORDER BY recipient`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	recipients := make([]string, 0)
	for rows.Next() {
		var recipient string
		if err := rows.Scan(&recipient); err != nil {
			return nil, err
		}
		recipients = append(recipients, recipient)
	}
	return recipients, rows.Err()
}

func newMessageID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func validMessageID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, char := range id {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}
