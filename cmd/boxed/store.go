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
	Name      string
	Owner     string
	Index     int
	CPUs      int
	Mem       string
	Created   time.Time
	Image     string    // base image snapshot the disk comes from
	Expires   time.Time // when it is deleted by itself (--ttl); zero: never
	KeepAwake bool      // never goes to sleep when unused
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
);
CREATE TABLE IF NOT EXISTS shares (
	token      TEXT PRIMARY KEY,
	vm         TEXT NOT NULL,
	idx        INTEGER NOT NULL,
	port       INTEGER NOT NULL,
	created_at TEXT NOT NULL
);`)
	if err != nil {
		return nil, err
	}
	// Added later: when the keys were last fetched from GitHub.
	db.Exec("ALTER TABLE vms ADD COLUMN image TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE vms ADD COLUMN expires_at TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE vms ADD COLUMN keep_awake INTEGER NOT NULL DEFAULT 0")
	if _, err := db.Exec("ALTER TABLE user_keys ADD COLUMN updated_at TEXT NOT NULL DEFAULT ''"); err == nil {
		db.Exec("UPDATE user_keys SET updated_at = ?", time.Now().UTC().Format(time.RFC3339))
	}
	return &Store{db}, nil
}

func scanVM(row interface{ Scan(...any) error }) (VM, error) {
	var v VM
	var created, expires string
	err := row.Scan(&v.Name, &v.Owner, &v.Index, &v.CPUs, &v.Mem, &created, &v.Image, &expires, &v.KeepAwake)
	if errors.Is(err, sql.ErrNoRows) {
		return v, errNotFound
	}
	v.Created, _ = time.Parse(time.RFC3339, created)
	v.Expires, _ = time.Parse(time.RFC3339, expires)
	return v, err
}

const vmCols = "name, owner, idx, cpus, mem, created_at, image, expires_at, keep_awake"

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

// Insert records v with the lowest free index (from 2: .0.1 is the host).
func (s *Store) Insert(v *VM) error {
	err := s.db.QueryRow(`SELECT COALESCE(MIN(i), 2) FROM (
		SELECT 2 AS i WHERE NOT EXISTS (SELECT 1 FROM vms WHERE idx = 2)
		UNION SELECT idx + 1 FROM vms WHERE idx + 1 < 65535 AND idx + 1 NOT IN (SELECT idx FROM vms))`).Scan(&v.Index)
	if err != nil {
		return err
	}
	v.Created = time.Now().UTC()
	_, err = s.db.Exec("INSERT INTO vms ("+vmCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		v.Name, v.Owner, v.Index, v.CPUs, v.Mem, v.Created.Format(time.RFC3339), v.Image, timeCol(v.Expires), v.KeepAwake)
	return err
}

// SetExpires sets when the VM is deleted by itself; zero: never.
func (s *Store) SetExpires(name string, t time.Time) error {
	_, err := s.db.Exec("UPDATE vms SET expires_at = ? WHERE name = ?", timeCol(t), name)
	return err
}

func (s *Store) SetKeepAwake(name string, on bool) error {
	_, err := s.db.Exec("UPDATE vms SET keep_awake = ? WHERE name = ?", on, name)
	return err
}

// timeCol is how times are stored: RFC 3339 in UTC, empty for the zero time.
func timeCol(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// Delete forgets the VM and its share links: a new VM with the same name
// must not inherit them.
func (s *Store) Delete(name string) error {
	if _, err := s.Unshare(name); err != nil {
		return err
	}
	_, err := s.db.Exec("DELETE FROM vms WHERE name = ?", name)
	return err
}

// Share returns the token of the link to port of v, made on first use.
func (s *Store) Share(v VM, port int) (string, error) {
	var token string
	err := s.db.QueryRow("SELECT token FROM shares WHERE vm = ? AND idx = ? AND port = ?", v.Name, v.Index, port).Scan(&token)
	if !errors.Is(err, sql.ErrNoRows) {
		return token, err
	}
	token = randomString()
	_, err = s.db.Exec("INSERT INTO shares (token, vm, idx, port, created_at) VALUES (?, ?, ?, ?, ?)",
		token, v.Name, v.Index, port, timeCol(time.Now()))
	return token, err
}

// Shared says which VM (name and index) and port a share token opens.
func (s *Store) Shared(token string) (vm string, idx, port int, ok bool) {
	err := s.db.QueryRow("SELECT vm, idx, port FROM shares WHERE token = ?", token).Scan(&vm, &idx, &port)
	return vm, idx, port, err == nil
}

// Unshare ends the share links of the VM named name; it returns how many.
func (s *Store) Unshare(name string) (int, error) {
	res, err := s.db.Exec("DELETE FROM shares WHERE vm = ?", name)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
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
