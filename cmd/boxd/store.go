package main

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// VM is a microVM as recorded in the database. Its index fixes IP and MAC.
type VM struct {
	Name    string
	Owner   string
	Index   int
	CPUs    int
	Mem     string
	Created time.Time
	Image   string // base image snapshot the disk comes from
}

func (v VM) IP() string  { return fmt.Sprintf("10.42.%d.%d", v.Index>>8, v.Index&255) }
func (v VM) MAC() string { return fmt.Sprintf("02:42:0a:2a:%02x:%02x", v.Index>>8, v.Index&255) }

var errNotFound = errors.New("not found")

type Store struct{ db *sql.DB }

func openStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS vms (
	name       TEXT PRIMARY KEY,
	owner      TEXT NOT NULL,
	idx        INTEGER NOT NULL UNIQUE,
	cpus       INTEGER NOT NULL,
	mem        TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS web_logouts (
	email TEXT PRIMARY KEY,
	at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS user_keys (
	user TEXT PRIMARY KEY,
	keys TEXT NOT NULL
);`)
	if err != nil {
		return nil, err
	}
	// Added later: when the keys were last fetched from GitHub.
	db.Exec("ALTER TABLE vms ADD COLUMN image TEXT NOT NULL DEFAULT ''")
	if _, err := db.Exec("ALTER TABLE user_keys ADD COLUMN updated_at TEXT NOT NULL DEFAULT ''"); err == nil {
		db.Exec("UPDATE user_keys SET updated_at = ?", time.Now().UTC().Format(time.RFC3339))
	}
	return &Store{db}, nil
}

func scanVM(row interface{ Scan(...any) error }) (VM, error) {
	var v VM
	var created string
	err := row.Scan(&v.Name, &v.Owner, &v.Index, &v.CPUs, &v.Mem, &created, &v.Image)
	if errors.Is(err, sql.ErrNoRows) {
		return v, errNotFound
	}
	v.Created, _ = time.Parse(time.RFC3339, created)
	return v, err
}

const vmCols = "name, owner, idx, cpus, mem, created_at, image"

func (s *Store) Get(name string) (VM, error) {
	return scanVM(s.db.QueryRow("SELECT "+vmCols+" FROM vms WHERE name = ?", name))
}

// List returns the VMs of owner, or all of them when owner is empty.
func (s *Store) List(owner string) ([]VM, error) {
	rows, err := s.db.Query("SELECT "+vmCols+" FROM vms WHERE ? = '' OR owner = ? ORDER BY name", owner, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var vms []VM
	for rows.Next() {
		v, err := scanVM(rows)
		if err != nil {
			return nil, err
		}
		vms = append(vms, v)
	}
	return vms, rows.Err()
}

// Insert records v with the lowest free index (from 2: .0.1 is exe-host).
func (s *Store) Insert(v *VM) error {
	err := s.db.QueryRow(`SELECT COALESCE(MIN(i), 2) FROM (
		SELECT 2 AS i WHERE NOT EXISTS (SELECT 1 FROM vms WHERE idx = 2)
		UNION SELECT idx + 1 FROM vms WHERE idx + 1 < 65535 AND idx + 1 NOT IN (SELECT idx FROM vms))`).Scan(&v.Index)
	if err != nil {
		return err
	}
	v.Created = time.Now().UTC()
	_, err = s.db.Exec("INSERT INTO vms ("+vmCols+") VALUES (?, ?, ?, ?, ?, ?, ?)",
		v.Name, v.Owner, v.Index, v.CPUs, v.Mem, v.Created.Format(time.RFC3339), v.Image)
	return err
}

func (s *Store) Delete(name string) error {
	_, err := s.db.Exec("DELETE FROM vms WHERE name = ?", name)
	return err
}

func (s *Store) SaveKeys(user, keys string) error {
	_, err := s.db.Exec(`INSERT INTO user_keys (user, keys, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(user) DO UPDATE SET keys = excluded.keys, updated_at = excluded.updated_at`,
		user, keys, time.Now().UTC().Format(time.RFC3339))
	return err
}

// CachedKeys are the keys last fetched for user, if fetched within maxAge.
func (s *Store) CachedKeys(user string, maxAge time.Duration) (string, bool) {
	var keys, updated string
	if err := s.db.QueryRow("SELECT keys, updated_at FROM user_keys WHERE user = ?", user).Scan(&keys, &updated); err != nil {
		return "", false
	}
	t, err := time.Parse(time.RFC3339, updated)
	return keys, err == nil && time.Since(t) < maxAge
}

// Logout ends every web session of email that started before t.
func (s *Store) Logout(email string, t time.Time) error {
	_, err := s.db.Exec("INSERT INTO web_logouts (email, at) VALUES (?, ?) ON CONFLICT(email) DO UPDATE SET at = excluded.at", email, t.UnixMilli())
	return err
}

// LogoutAt is when email last logged out, in Unix milliseconds; 0 if never.
func (s *Store) LogoutAt(email string) (int64, error) {
	var at int64
	err := s.db.QueryRow("SELECT at FROM web_logouts WHERE email = ?", email).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return at, err
}

// Dump writes a consistent copy of the database to path (for backups).
func (s *Store) Dump(path string) error {
	_, err := s.db.Exec("VACUUM INTO ?", path)
	return err
}
