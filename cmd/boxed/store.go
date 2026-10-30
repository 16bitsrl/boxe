package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
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
	NextJob   time.Time // asleep: when its next scheduled job runs (schedule.go); zero: none
	Schedule  string    // what boxe-agent said was scheduled in it when it went to sleep
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
	// It holds the members' API keys: root only. SQLite gives its -wal and
	// -shm files the database's mode.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
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
CREATE TABLE IF NOT EXISTS llm_keys (
	user       TEXT NOT NULL,
	provider   TEXT NOT NULL,
	key        TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	PRIMARY KEY (user, provider)
);
CREATE TABLE IF NOT EXISTS llm_usage (
	at          INTEGER NOT NULL, -- Unix seconds
	user        TEXT NOT NULL,
	vm          TEXT NOT NULL,
	provider    TEXT NOT NULL,
	model       TEXT NOT NULL,
	input       INTEGER NOT NULL, -- tokens, without those read from or written to the cache
	output      INTEGER NOT NULL,
	cache_read  INTEGER NOT NULL,
	cache_write INTEGER NOT NULL,
	cost        REAL              -- USD at the prices of the time; NULL: unknown model
);
CREATE INDEX IF NOT EXISTS llm_usage_user_at ON llm_usage (user, at);
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
);
CREATE TABLE IF NOT EXISTS tokens (
	id         TEXT PRIMARY KEY,
	user       TEXT NOT NULL,
	hash       TEXT NOT NULL UNIQUE, -- SHA-256 of the token, hex: the token itself is never kept
	label      TEXT NOT NULL,
	cmds       TEXT NOT NULL,        -- comma-separated commands it may run; empty: all
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	used_at    TEXT NOT NULL         -- empty: never
);`)
	if err != nil {
		return nil, err
	}
	// Added later: when the keys were last fetched from GitHub.
	db.Exec("ALTER TABLE vms ADD COLUMN image TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE vms ADD COLUMN expires_at TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE vms ADD COLUMN keep_awake INTEGER NOT NULL DEFAULT 0")
	db.Exec("ALTER TABLE vms ADD COLUMN egress TEXT NOT NULL DEFAULT 'all'")
	db.Exec("ALTER TABLE vms ADD COLUMN next_job TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE vms ADD COLUMN schedule TEXT NOT NULL DEFAULT ''")
	if _, err := db.Exec("ALTER TABLE user_keys ADD COLUMN updated_at TEXT NOT NULL DEFAULT ''"); err == nil {
		db.Exec("UPDATE user_keys SET updated_at = ?", time.Now().UTC().Format(time.RFC3339))
	}
	return &Store{db}, nil
}

func scanVM(row interface{ Scan(...any) error }) (VM, error) {
	var v VM
	var created, expires, next string
	err := row.Scan(&v.Name, &v.Owner, &v.Index, &v.CPUs, &v.Mem, &created, &v.Image, &expires, &v.KeepAwake, &v.Egress, &next, &v.Schedule)
	if errors.Is(err, sql.ErrNoRows) {
		return v, errNotFound
	}
	v.Created, _ = time.Parse(time.RFC3339, created)
	v.Expires, _ = time.Parse(time.RFC3339, expires)
	v.NextJob, _ = time.Parse(time.RFC3339, next)
	return v, err
}

const vmCols = "name, owner, idx, cpus, mem, created_at, image, expires_at, keep_awake, egress, next_job, schedule"

func (s *Store) Get(name string) (VM, error) {
	return scanVM(s.db.QueryRow("SELECT "+vmCols+" FROM vms WHERE name = ?", name))
}

// ByIndex is the VM with the address of index i.
func (s *Store) ByIndex(i int) (VM, error) {
	return scanVM(s.db.QueryRow("SELECT "+vmCols+" FROM vms WHERE idx = ?", i))
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
	_, err = s.db.Exec("INSERT INTO vms ("+vmCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		v.Name, v.Owner, v.Index, v.CPUs, v.Mem, v.Created.Format(time.RFC3339), v.Image, timeCol(v.Expires), v.KeepAwake, v.Egress,
		timeCol(v.NextJob), v.Schedule)
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

func (s *Store) SetEgress(name, egress string) error {
	_, err := s.db.Exec("UPDATE vms SET egress = ? WHERE name = ?", egress, name)
	return err
}

// GetByIndex is the VM with that index, the one at its IP address.
func (s *Store) GetByIndex(idx int) (VM, error) {
	return scanVM(s.db.QueryRow("SELECT "+vmCols+" FROM vms WHERE idx = ?", idx))
}

// SetNextJob records, as the VM goes to sleep, when its next job runs (zero:
// none) and what is scheduled in it, to find the one after.
func (s *Store) SetNextJob(name string, t time.Time, schedule string) error {
	_, err := s.db.Exec("UPDATE vms SET next_job = ?, schedule = ? WHERE name = ?", timeCol(t), schedule, name)
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

// Token is an API token of a member, without the token itself.
type Token struct {
	ID, User, Label        string
	Cmds                   []string // the commands it may run; none: all
	Created, Expires, Used time.Time
}

const tokenCols = "id, user, label, cmds, created_at, expires_at, used_at"

func scanToken(row interface{ Scan(...any) error }) (Token, error) {
	var t Token
	var cmds, created, expires, used string
	err := row.Scan(&t.ID, &t.User, &t.Label, &cmds, &created, &expires, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return t, errNotFound
	}
	if cmds != "" {
		t.Cmds = strings.Split(cmds, ",")
	}
	t.Created, _ = time.Parse(time.RFC3339, created)
	t.Expires, _ = time.Parse(time.RFC3339, expires)
	t.Used, _ = time.Parse(time.RFC3339, used)
	return t, err
}

// AddToken records t, which hash (of the token) stands for.
func (s *Store) AddToken(t Token, hash string) error {
	_, err := s.db.Exec("INSERT INTO tokens ("+tokenCols+", hash) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		t.ID, t.User, t.Label, strings.Join(t.Cmds, ","), timeCol(t.Created), timeCol(t.Expires), timeCol(t.Used), hash)
	return err
}

// Tokens are user's, oldest first.
func (s *Store) Tokens(user string) ([]Token, error) {
	rows, err := s.db.Query("SELECT "+tokenCols+" FROM tokens WHERE user = ? ORDER BY created_at, id", user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Token{}
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, t)
	}
	return list, rows.Err()
}

// TokenOf is the token whose hash is hash.
func (s *Store) TokenOf(hash string) (Token, error) {
	return scanToken(s.db.QueryRow("SELECT "+tokenCols+" FROM tokens WHERE hash = ?", hash))
}

// UsedToken records that the token id was used at t.
func (s *Store) UsedToken(id string, t time.Time) error {
	_, err := s.db.Exec("UPDATE tokens SET used_at = ? WHERE id = ?", timeCol(t), id)
	return err
}

// DeleteToken deletes user's token with the id or the label idOrLabel.
func (s *Store) DeleteToken(user, idOrLabel string) (Token, error) {
	t, err := scanToken(s.db.QueryRow("SELECT "+tokenCols+" FROM tokens WHERE user = ? AND (id = ? OR label = ?) ORDER BY id = ? DESC",
		user, idOrLabel, idOrLabel, idOrLabel))
	if err != nil {
		return t, err
	}
	_, err = s.db.Exec("DELETE FROM tokens WHERE id = ?", t.ID)
	return t, err
}

// Dump writes a consistent copy of the database to path (for backups).
func (s *Store) Dump(path string) error {
	_, err := s.db.Exec("VACUUM INTO ?", path)
	return err
}

// LLMKeys are user's API keys, by provider.
func (s *Store) LLMKeys(user string) (map[string]string, error) {
	rows, err := s.db.Query("SELECT provider, key FROM llm_keys WHERE user = ?", user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := map[string]string{}
	for rows.Next() {
		var p, k string
		if err := rows.Scan(&p, &k); err != nil {
			return nil, err
		}
		keys[p] = k
	}
	return keys, rows.Err()
}

func (s *Store) SetLLMKey(user, provider, key string) error {
	_, err := s.db.Exec(`INSERT INTO llm_keys (user, provider, key, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(user, provider) DO UPDATE SET key = excluded.key, updated_at = excluded.updated_at`,
		user, provider, key, time.Now().UTC().Format(time.RFC3339))
	return err
}

func (s *Store) DeleteLLMKey(user, provider string) error {
	_, err := s.db.Exec("DELETE FROM llm_keys WHERE user = ? AND provider = ?", user, provider)
	return err
}

func (s *Store) AddLLMUsage(at time.Time, user, vm, provider string, u llmUsage, cost *float64) error {
	_, err := s.db.Exec(`INSERT INTO llm_usage (at, user, vm, provider, model, input, output, cache_read, cache_write, cost)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, at.Unix(), user, vm, provider, u.Model, u.Input, u.Output, u.CacheRead, u.CacheWrite, cost)
	return err
}

// LLMUsage is what user's VMs used since a time, per VM and model.
func (s *Store) LLMUsage(user string, since time.Time) ([]llmUsageJSON, error) {
	rows, err := s.db.Query(`SELECT vm, provider, model, COUNT(*), SUM(input), SUM(output), SUM(cache_read), SUM(cache_write),
		CASE WHEN COUNT(cost) = COUNT(*) THEN SUM(cost) END
		FROM llm_usage WHERE user = ? AND at >= ? GROUP BY vm, provider, model`, user, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []llmUsageJSON
	for rows.Next() {
		var u llmUsageJSON
		if err := rows.Scan(&u.VM, &u.Provider, &u.Model, &u.Requests, &u.Input, &u.Output, &u.CacheRead, &u.CacheWrite, &u.CostUSD); err != nil {
			return nil, err
		}
		list = append(list, u)
	}
	return list, rows.Err()
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
