package contextguru

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	cgstore "github.com/rossoctl/context-guru/store"

	// The pure-Go SQLite driver context-guru's own proxy uses. Imported here, in the plugin
	// package, so only a build that links context-guru (include_plugin_contextguru) carries it.
	_ "modernc.org/sqlite"
)

// stateDBFileName is the session-state database: context-guru's store.Disk table, which keeps the
// keys a session needs between turns (summary checkpoint, last billed input, freeze decisions).
const stateDBFileName = "context-guru-state.db"

// stateDBs holds one *sql.DB per path for the life of the process. A config reload builds a new
// plugin instance, and the old one keeps serving its in-flight requests until it drains: both
// write through the same handle, so SQLite serializes them, and the database is never closed under
// a draining instance. Each instance has its own store.Disk on the handle and closes (flushes) it
// in Shutdown.
var (
	stateDBMu sync.Mutex
	stateDBs  = map[string]*sql.DB{}
)

// stateDBPath is where the session state is kept: ~/.cortex on a local install, beside the rest of
// Cortex's laptop state; otherwise the user's cache directory, so a shared deployment keeps its
// state across a restart of the process (not of a pod whose filesystem goes with it).
func stateDBPath(local bool) (string, error) {
	if local {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", fmt.Errorf("no home directory: %w", err)
		}
		return filepath.Join(home, ".cortex", stateDBFileName), nil
	}
	dir, err := os.UserCacheDir()
	if err != nil || dir == "" {
		return "", fmt.Errorf("no cache directory: %w", err)
	}
	return filepath.Join(dir, "cortex", stateDBFileName), nil
}

// stateDBPathFor is stateDBPath, a var so this package's tests put the database in a temp
// directory and never in the developer's own ~/.cortex.
var stateDBPathFor = stateDBPath

// uriEscaper escapes the characters SQLite gives a meaning in a file: URI. Without it a '#' in the
// path (in $HOME, $XDG_CACHE_HOME or a username) ends the path at the '#', so SQLite opens another
// file, with the umask's mode, outside the 0700 directory; a '?' does the same, and a '%XX' is
// decoded.
var uriEscaper = strings.NewReplacer("%", "%25", "#", "%23", "?", "%3f")

// sqliteURI is the file: URI that names path exactly.
func sqliteURI(path string) string { return "file:" + uriEscaper.Replace(path) }

// openStateDB returns the process's handle for path, opening it on first use. The file is created
// mode 0600 in a 0700 directory: it holds text the plugin made from the conversation (the summary,
// extract rewrites), as context-guru's own proxy keeps it.
func openStateDB(path string) (*sql.DB, error) {
	stateDBMu.Lock()
	defer stateDBMu.Unlock()
	if db, ok := stateDBs[path]; ok {
		return db, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// Created here, before SQLite opens it, so the mode is 0600 and not the umask's.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	// The pragmas context-guru's tooldefer file uses: wait for a lock rather than fail, WAL so a
	// read does not wait for a write, NORMAL sync (safe under WAL; a power loss costs at most the
	// last commit).
	db, err := sql.Open("sqlite", sqliteURI(path)+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	stateDBs[path] = db
	return db, nil
}

// stateDBFor opens the database a plugin instance keeps its session state in, and returns nil
// when there is none. It starts no goroutine: the handle is shared and lives for the process, so a
// Configure that later fails, or a pipeline that is never started, holds nothing. It FAILS OPEN:
// with no usable file the state stays in memory, as before, with one WARN.
func stateDBFor(local bool) (*sql.DB, string) {
	path, err := stateDBPathFor(local)
	if err != nil {
		slog.Warn("context-guru: session state stays in memory; no place for its database", "error", err)
		return nil, ""
	}
	db, err := openStateDB(path)
	if err != nil {
		slog.Warn("context-guru: session state stays in memory; its database could not be opened",
			"path", path, "error", err)
		return nil, ""
	}
	return db, path
}

// attachStateDB gives st a disk copy of its session state on db, so the state survives a restart
// and a config reload. Always on when the plugin is: there is no setting for it. It starts the
// disk's flusher, so it runs in Init and the copy is closed in Shutdown. It FAILS OPEN: with an
// unusable table the state stays in memory, with one WARN. A store that is not a *store.Memory
// (store.enabled: false gives a no-op store) has no state to keep.
//
// Everything is kept, content keys included (DiskOptions.MetadataOnly false, the standalone
// proxy's default): the summary checkpoint is what keeps the prompt prefix, and so the provider's
// cache, stable across a restart. Rows live DefaultDiskRetention after their last write.
func attachStateDB(st cgstore.Store, db *sql.DB, path string) *cgstore.Disk {
	mem, ok := st.(*cgstore.Memory)
	if !ok || db == nil {
		return nil
	}
	d, err := cgstore.OpenDisk(db, cgstore.DiskOptions{})
	if err != nil {
		slog.Warn("context-guru: session state stays in memory; its table is unusable", "path", path, "error", err)
		return nil
	}
	mem.AttachDisk(d)
	slog.Info("context-guru: session state is kept on disk", "path", path,
		"memory_ttl", cgstore.DefaultStateTTL, "retention", cgstore.DefaultDiskRetention)
	return d
}

// stateTTLDefault is the memory lifetime of the state keys when a database backs them: the
// standalone proxy's rule. A session that returns later is loaded from the database. An explicit
// store.state_ttl_seconds wins. With no database the engine's own default applies.
const stateTTLDefault = cgstore.DefaultStateTTL
