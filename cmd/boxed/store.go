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
	Egress    string    // egressAll, or egressIntegrations: it reaches nothing but the gateway
}

func (v VM) IP() string  { return fmt.Sprintf("10.42.%d.%d", v.Index>>8, v.Index&255) }
func (v VM) MAC() string { return fmt.Sprintf("02:42:0a:2a:%02x:%02x", v.Index>>8, v.Index&255) }

// same tells whether o is still v: a VM deleted meanwhile may have passed
// its name and address to someone else's.
func (v VM) same(o VM) bool {
	return v.Owner == o.Owner && v.Index == o.Index && v.Created.Equal(o.Created)
}

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
);
CREATE TABLE IF NOT EXISTS samples (
	vm   TEXT NOT NULL,
	at   INTEGER NOT NULL, -- Unix seconds
	cpu  REAL NOT NULL,    -- percent, 100: one core
	mem  INTEGER NOT NULL, -- bytes
	disk INTEGER NOT NULL  -- bytes
);
CREATE INDEX IF NOT EXISTS samples_vm_at ON samples (vm, at);
-- Secrets of a member, added by the gateway to what their VMs send
-- (integrations.go). In plain text, like the rest of this file.
CREATE TABLE IF NOT EXISTS integrations (
	owner       TEXT NOT NULL,
	name        TEXT NOT NULL,
	kind        TEXT NOT NULL, -- http or github
	target      TEXT NOT NULL, -- http: the URL requests go to
	header_name TEXT NOT NULL, -- http: the header added, or none
	secret      TEXT NOT NULL, -- its value, or the GitHub token
	read_only   INTEGER NOT NULL,
	created_at  TEXT NOT NULL,
	PRIMARY KEY (owner, name)
);
CREATE TABLE IF NOT EXISTS attachments (
	owner       TEXT NOT NULL,
	integration TEXT NOT NULL,
	vm          TEXT NOT NULL,
	PRIMARY KEY (owner, integration, vm)
);`)
	if err != nil {
		return nil, err
	}
	// Added later: when the keys were last fetched from GitHub.
	db.Exec("ALTER TABLE vms ADD COLUMN image TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE vms ADD COLUMN expires_at TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE vms ADD COLUMN keep_awake INTEGER NOT NULL DEFAULT 0")
	db.Exec("ALTER TABLE vms ADD COLUMN egress TEXT NOT NULL DEFAULT 'all'")
	if _, err := db.Exec("ALTER TABLE user_keys ADD COLUMN updated_at TEXT NOT NULL DEFAULT ''"); err == nil {
		db.Exec("UPDATE user_keys SET updated_at = ?", time.Now().UTC().Format(time.RFC3339))
	}
	return &Store{db}, nil
}

func scanVM(row interface{ Scan(...any) error }) (VM, error) {
	var v VM
	var created, expires string
	err := row.Scan(&v.Name, &v.Owner, &v.Index, &v.CPUs, &v.Mem, &created, &v.Image, &expires, &v.KeepAwake, &v.Egress)
	if errors.Is(err, sql.ErrNoRows) {
		return v, errNotFound
	}
	v.Created, _ = time.Parse(time.RFC3339, created)
	v.Expires, _ = time.Parse(time.RFC3339, expires)
	return v, err
}

const vmCols = "name, owner, idx, cpus, mem, created_at, image, expires_at, keep_awake, egress"

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
	v.Created = time.Now().UTC().Truncate(time.Second) // as stored
	if v.Egress == "" {
		v.Egress = egressAll
	}
	_, err = s.db.Exec("INSERT INTO vms ("+vmCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		v.Name, v.Owner, v.Index, v.CPUs, v.Mem, v.Created.Format(time.RFC3339), v.Image, timeCol(v.Expires), v.KeepAwake, v.Egress)
	return err
}

// SetExpires sets when the VM is deleted by itself; zero: never.
func (s *Store) SetExpires(name string, t time.Time) error {
	_, err := s.db.Exec("UPDATE vms SET expires_at = ? WHERE name = ?", timeCol(t), name)
	return err
}

func (s *Store) SetEgress(name, egress string) error {
	_, err := s.db.Exec("UPDATE vms SET egress = ? WHERE name = ?", egress, name)
	return err
}

// GetByIndex is the VM with that index, the one at its IP address.
func (s *Store) GetByIndex(idx int) (VM, error) {
	return scanVM(s.db.QueryRow("SELECT "+vmCols+" FROM vms WHERE idx = ?", idx))
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

// Delete forgets the VM, its share links, integrations and usage: a new VM
// with the same name must not inherit them.
func (s *Store) Delete(name string) error {
	if _, err := s.Unshare(name); err != nil {
		return err
	}
	if _, err := s.db.Exec("DELETE FROM attachments WHERE vm = ?", name); err != nil {
		return err
	}
	if _, err := s.db.Exec("DELETE FROM samples WHERE vm = ?", name); err != nil {
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

// Sample is what a VM used at a time: CPU over the minute before (100: one
// core), RAM taken on the host and space its disk takes, in bytes.
type Sample struct {
	At        time.Time
	CPU       float64
	Mem, Disk int64
}

func (s *Store) AddSample(vm string, x Sample) error {
	_, err := s.db.Exec("INSERT INTO samples (vm, at, cpu, mem, disk) VALUES (?, ?, ?, ?, ?)", vm, x.At.Unix(), x.CPU, x.Mem, x.Disk)
	return err
}

// Samples of vm since a time, oldest first.
func (s *Store) Samples(vm string, since time.Time) ([]Sample, error) {
	rows, err := s.db.Query("SELECT at, cpu, mem, disk FROM samples WHERE vm = ? AND at >= ? ORDER BY at", vm, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Sample
	for rows.Next() {
		var x Sample
		var at int64
		if err := rows.Scan(&at, &x.CPU, &x.Mem, &x.Disk); err != nil {
			return nil, err
		}
		x.At = time.Unix(at, 0)
		list = append(list, x)
	}
	return list, rows.Err()
}

// PruneSamples forgets the samples from before t.
func (s *Store) PruneSamples(t time.Time) error {
	_, err := s.db.Exec("DELETE FROM samples WHERE at < ?", t.Unix())
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

// Integration is a member's secret for an API, which the gateway adds to
// the requests of the VMs it is attached to (integrations.go).
type Integration struct {
	Owner, Name string
	Kind        string // kindHTTP or kindGitHub
	Target      string // kindHTTP: where requests go, e.g. https://api.stripe.com/v1
	HeaderName  string // kindHTTP: the header added, e.g. Authorization; empty: none
	Secret      string // the header's value, or the GitHub token
	ReadOnly    bool   // kindGitHub: fetch, no push, only GET and HEAD to the API
	Created     time.Time
	VMs         []string // attached to, in List only
}

const integrationCols = "owner, name, kind, target, header_name, secret, read_only, created_at"

func scanIntegration(row interface{ Scan(...any) error }) (Integration, error) {
	var ig Integration
	var created string
	err := row.Scan(&ig.Owner, &ig.Name, &ig.Kind, &ig.Target, &ig.HeaderName, &ig.Secret, &ig.ReadOnly, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return ig, errNotFound
	}
	ig.Created, _ = time.Parse(time.RFC3339, created)
	return ig, err
}

func (s *Store) AddIntegration(ig Integration) error {
	_, err := s.db.Exec("INSERT INTO integrations ("+integrationCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		ig.Owner, ig.Name, ig.Kind, ig.Target, ig.HeaderName, ig.Secret, ig.ReadOnly, timeCol(time.Now()))
	return err
}

func (s *Store) Integration(owner, name string) (Integration, error) {
	return scanIntegration(s.db.QueryRow("SELECT "+integrationCols+" FROM integrations WHERE owner = ? AND name = ?", owner, name))
}

// Integrations of owner, with the VMs each is attached to.
func (s *Store) Integrations(owner string) ([]Integration, error) {
	rows, err := s.db.Query("SELECT "+integrationCols+" FROM integrations WHERE owner = ? ORDER BY name", owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Integration
	for rows.Next() {
		ig, err := scanIntegration(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, ig)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].VMs, err = s.strings("SELECT vm FROM attachments WHERE owner = ? AND integration = ? ORDER BY vm", owner, list[i].Name); err != nil {
			return nil, err
		}
	}
	return list, nil
}

func (s *Store) strings(query string, args ...any) ([]string, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []string{}
	for rows.Next() {
		var x string
		if err := rows.Scan(&x); err != nil {
			return nil, err
		}
		list = append(list, x)
	}
	return list, rows.Err()
}

// RemoveIntegration deletes it and its attachments.
func (s *Store) RemoveIntegration(owner, name string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM attachments WHERE owner = ? AND integration = ?", owner, name); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM integrations WHERE owner = ? AND name = ?", owner, name); err != nil {
		return err
	}
	return tx.Commit()
}

// Attach gives v the integration of its owner named name; idempotent.
func (s *Store) Attach(v VM, name string) error {
	_, err := s.db.Exec("INSERT OR IGNORE INTO attachments (owner, integration, vm) VALUES (?, ?, ?)", v.Owner, name, v.Name)
	return err
}

func (s *Store) Detach(v VM, name string) error {
	_, err := s.db.Exec("DELETE FROM attachments WHERE owner = ? AND integration = ? AND vm = ?", v.Owner, name, v.Name)
	return err
}

// CopyAttachments gives the copy to the integrations of from.
func (s *Store) CopyAttachments(from, to VM) error {
	_, err := s.db.Exec("INSERT OR IGNORE INTO attachments (owner, integration, vm) SELECT owner, integration, ? FROM attachments WHERE owner = ? AND vm = ?",
		to.Name, from.Owner, from.Name)
	return err
}

// Attached is the integration v reaches at <name>.int.<suffix>: the one of
// its owner named name and attached to it; for "github", also the GitHub
// integration attached to it, whatever its name.
func (s *Store) Attached(v VM, name string) (Integration, error) {
	return scanIntegration(s.db.QueryRow(`SELECT i.owner, i.name, i.kind, i.target, i.header_name, i.secret, i.read_only, i.created_at
		FROM integrations i JOIN attachments a ON a.owner = i.owner AND a.integration = i.name
		WHERE a.vm = ? AND i.owner = ? AND (i.name = ? OR (? = 'github' AND i.kind = 'github'))
		ORDER BY i.name = ? DESC LIMIT 1`, v.Name, v.Owner, name, name, name))
}
