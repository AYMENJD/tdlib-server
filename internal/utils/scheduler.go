package utils

import (
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Scheduler struct {
	db     *sql.DB
	db_dir string

	event_callback func(int64, string)

	create_stmt *sql.Stmt
	stopChan    chan struct{}
	mu          sync.Mutex
}

func (sched *Scheduler) Start() {
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(sched.db_dir, "scheduler.db")+"?_journal_mode=WAL&_synchronous=1")
	PanicOnErr(err, "Could not open scheduler DB: %v", err, true)

	sched.db = db

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS scheduled_events (
			event_id INTEGER PRIMARY KEY AUTOINCREMENT,
			send_at INTEGER NOT NULL,
			payload VARCHAR(4096) NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_send_at ON scheduled_events (send_at);
	`)
	PanicOnErr(err, "Failed to create table: %v", err, true)

	istmt, err := db.Prepare(`INSERT INTO scheduled_events (send_at, payload) VALUES (?, ?)`)
	PanicOnErr(err, "Failed to prepare insert statement: %v", err, true)
	sched.create_stmt = istmt

	go sched.loop()
}

func (sched *Scheduler) CreateEvent(sendAt int64, payload string) (int64, error) {
	if len(payload) > 4096 {
		return 0, errors.New("payload must be less than 4096 in length")
	}

	sched.mu.Lock()
	defer sched.mu.Unlock()
	res, err := sched.create_stmt.Exec(sendAt, payload)
	if err != nil {
		return 0, err
	}

	return res.LastInsertId()
}

func (sched *Scheduler) CancelEvent(eventID int64) int64 {
	sched.mu.Lock()
	defer sched.mu.Unlock()

	res, err := sched.db.Exec(`DELETE FROM scheduled_events WHERE event_id = ?`, eventID)
	if err != nil {
		return -1
	}

	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return -1
	}

	return rowsAffected
}

func (sched *Scheduler) Close() error {
	sched.mu.Lock()
	defer sched.mu.Unlock()

	close(sched.stopChan)

	sched.create_stmt.Close()
	return sched.db.Close()
}

func (sched *Scheduler) loop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	selectStmt, err := sched.db.Prepare(`
		SELECT event_id, payload FROM scheduled_events 
		WHERE send_at <= ? 
		ORDER BY send_at LIMIT 100`)
	PanicOnErr(err, "Failed to prepare select statement: %v", err, true)
	defer selectStmt.Close()

	deleteStmt, err := sched.db.Prepare(`DELETE FROM scheduled_events WHERE event_id = ?`)
	PanicOnErr(err, "Failed to prepare delete statement: %v", err, true)
	defer deleteStmt.Close()

	for {
		select {
		case now := <-ticker.C:
			rows, err := selectStmt.Query(now.Unix())
			if err != nil {
				continue
			}

			var toDelete []int64

			for rows.Next() {
				var event_id int64
				var payload string
				if err := rows.Scan(&event_id, &payload); err != nil {
					continue
				}

				toDelete = append(toDelete, event_id)
				go sched.event_callback(event_id, payload)
			}
			rows.Close()

			if len(toDelete) > 0 {
				tx, err := sched.db.Begin()
				if err != nil {
					continue
				}

				for _, id := range toDelete {
					tx.Stmt(deleteStmt).Exec(id)
				}
				tx.Commit()
			}

		case <-sched.stopChan:
			return
		}
	}
}

func NewScheduler(db_dir string, event_callback func(int64, string)) *Scheduler {
	return &Scheduler{
		db_dir:         db_dir,
		event_callback: event_callback,
		stopChan:       make(chan struct{}),
	}
}
