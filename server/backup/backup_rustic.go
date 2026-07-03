package backup

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"emperror.dev/errors"

	"github.com/realmctl/wings/config"
	"github.com/realmctl/wings/remote"
	"github.com/realmctl/wings/server/filesystem"
)

// repoLocks serializes access to a rustic repository. rustic operations that
// mutate (backup, forget, prune, init) cannot run concurrently against the same
// repository, and since all servers on a node share a single repository we guard
// every invocation with a per-repository mutex.
var repoLocks sync.Map

func repoLock(repository string) *sync.Mutex {
	v, _ := repoLocks.LoadOrStore(repository, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// rusticSnapshot is the subset of the JSON emitted by `rustic backup --json`
// and `rustic snapshots --json` that we care about.
type rusticSnapshot struct {
	Id      string         `json:"id"`
	Tags    []string       `json:"tags"`
	Summary *rusticSummary `json:"summary"`
}

type rusticSummary struct {
	TotalBytesProcessed int64 `json:"total_bytes_processed"`
	DataAdded           int64 `json:"data_added"`
}

type RusticBackup struct {
	Backup

	// snapshotId is the rustic snapshot identifier produced by Generate. It is
	// also used as the checksum reported to the panel (a rustic snapshot id is a
	// sha256 hash of the snapshot contents).
	snapshotId string
	// size is the logical size of the backed up data as reported by rustic.
	// Deduplication means the physical repository growth is typically far
	// smaller; the panel displays this logical size to users.
	size int64
}

var _ BackupInterface = (*RusticBackup)(nil)

func NewRustic(client remote.Client, uuid string, ignore string) *RusticBackup {
	return &RusticBackup{
		Backup: Backup{
			client:  client,
			Uuid:    uuid,
			Ignore:  ignore,
			adapter: RusticBackupAdapter,
		},
	}
}

func (b *RusticBackup) WithLogContext(c map[string]interface{}) {
	b.logContext = c
}

// repository returns the effective repository location for this node.
func (b *RusticBackup) repository() string {
	c := config.Get()
	return c.System.Backups.RusticRepository(c.System.BackupDirectory)
}

// command builds a rustic command with the repository and credentials injected
// through the environment so they never appear in the process argument list.
func (b *RusticBackup) command(ctx context.Context, args ...string) (*exec.Cmd, error) {
	rc := config.Get().System.Backups.Rustic
	base := []string{"--repository", b.repository()}
	cmd := exec.CommandContext(ctx, rc.BinaryPath, append(base, args...)...)

	env := append(os.Environ(), "RUSTIC_REPOSITORY="+b.repository())
	switch {
	case rc.PasswordFile != "":
		env = append(env, "RUSTIC_PASSWORD_FILE="+rc.PasswordFile)
	case rc.Password != "":
		env = append(env, "RUSTIC_PASSWORD="+rc.Password)
	default:
		return nil, errors.New("backup: rustic adapter requires a password or password_file to be configured")
	}
	cmd.Env = env
	return cmd, nil
}

// run executes a rustic command and returns its stdout, wrapping failures with
// the captured stderr for easier debugging.
func (b *RusticBackup) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd, err := b.command(ctx, args...)
	if err != nil {
		return nil, err
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, errors.Wrap(err, "backup: rustic "+strings.Join(args, " ")+" failed: "+strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// ensureRepository initializes the repository if it does not already exist. It
// is safe to call before every backup; an already-initialized repository is a
// no-op.
func (b *RusticBackup) ensureRepository(ctx context.Context) error {
	// `cat config` succeeds only when the repository exists and the password is
	// correct. Any failure is treated as "needs init".
	if _, err := b.run(ctx, "cat", "config"); err == nil {
		return nil
	}
	if _, err := b.run(ctx, "init"); err != nil {
		return errors.WrapIf(err, "backup: failed to initialize rustic repository")
	}
	return nil
}

// Generate creates a new rustic snapshot of the server files tagged with the
// backup UUID.
func (b *RusticBackup) Generate(ctx context.Context, fsys *filesystem.Filesystem, ignore string) (*ArchiveDetails, error) {
	if err := b.validateIdentifier(); err != nil {
		return nil, err
	}
	rc := config.Get().System.Backups.Rustic
	if !rc.Enabled {
		return nil, errors.New("backup: rustic adapter is not enabled on this node")
	}

	lock := repoLock(b.repository())
	lock.Lock()
	defer lock.Unlock()

	if err := b.ensureRepository(ctx); err != nil {
		return nil, err
	}

	args := []string{"backup", fsys.Path(), "--tag", b.Uuid, "--as-path", "/", "--json"}
	if rc.Compression != "" {
		args = append(args, "--compression", rc.Compression)
	}
	// Translate the gitignore-style ignore list into rustic glob-file exclusions.
	if globFile, cleanup, err := writeGlobFile(ignore); err != nil {
		return nil, err
	} else if globFile != "" {
		defer cleanup()
		args = append(args, "--glob-file", globFile)
	}

	b.log().WithField("repository", b.repository()).Info("creating rustic backup for server")
	out, err := b.run(ctx, args...)
	if err != nil {
		return nil, err
	}

	snap, err := parseSnapshot(out)
	if err != nil {
		// Fall back to querying the repository for the snapshot we just tagged.
		if snap, err = b.latestSnapshot(ctx); err != nil {
			return nil, errors.WrapIf(err, "backup: rustic backup succeeded but snapshot details could not be determined")
		}
	}
	b.snapshotId = snap.Id
	if snap.Summary != nil {
		b.size = snap.Summary.TotalBytesProcessed
	}
	b.log().WithField("snapshot", b.snapshotId).Info("created rustic backup successfully")

	return b.Details(ctx, nil)
}

// latestSnapshot queries the repository for the most recent snapshot carrying
// this backup's tag.
func (b *RusticBackup) latestSnapshot(ctx context.Context) (*rusticSnapshot, error) {
	out, err := b.run(ctx, "snapshots", "--filter-tags-exact", b.Uuid, "--json")
	if err != nil {
		return nil, err
	}
	snaps, err := parseSnapshots(out)
	if err != nil {
		return nil, err
	}
	if len(snaps) == 0 {
		return nil, errors.New("backup: no rustic snapshot found for tag " + b.Uuid)
	}
	return &snaps[len(snaps)-1], nil
}

// Details returns the archive details built from the rustic snapshot metadata.
// Unlike the file-based adapters this does not stat a file on disk.
func (b *RusticBackup) Details(_ context.Context, parts []remote.BackupPart) (*ArchiveDetails, error) {
	return &ArchiveDetails{
		Checksum:     b.snapshotId,
		ChecksumType: "sha256",
		Size:         b.size,
		Parts:        parts,
	}, nil
}

func (b *RusticBackup) Checksum() ([]byte, error) {
	return hex.DecodeString(b.snapshotId)
}

func (b *RusticBackup) Size() (int64, error) {
	return b.size, nil
}

// Path is not meaningful for a repository-backed adapter; return the repository
// location so log output remains useful.
func (b *RusticBackup) Path() string {
	return b.repository()
}

// Remove forgets every snapshot tagged with this backup's UUID and optionally
// prunes the repository to reclaim space.
func (b *RusticBackup) Remove() error {
	if err := b.validateIdentifier(); err != nil {
		return err
	}
	ctx := context.Background()

	lock := repoLock(b.repository())
	lock.Lock()
	defer lock.Unlock()

	// --keep-none is required by rustic when forgetting via a filter with no
	// retention policy; it allows every snapshot matching the tag to be removed.
	if _, err := b.run(ctx, "forget", "--filter-tags-exact", b.Uuid, "--keep-none"); err != nil {
		return err
	}
	if config.Get().System.Backups.Rustic.Prune {
		if _, err := b.run(ctx, "prune"); err != nil {
			return errors.WrapIf(err, "backup: rustic snapshot forgotten but prune failed")
		}
	}
	return nil
}

// Restore locates the snapshot for this backup, restores it into a temporary
// directory, and then walks the restored files invoking the callback for each
// one. The reader argument is unused; rustic reads directly from its repository.
func (b *RusticBackup) Restore(ctx context.Context, _ io.Reader, callback RestoreCallback) error {
	if err := b.validateIdentifier(); err != nil {
		return err
	}

	lock := repoLock(b.repository())
	lock.Lock()
	defer lock.Unlock()

	snap, err := b.latestSnapshot(ctx)
	if err != nil {
		return err
	}

	tmp, err := os.MkdirTemp(config.Get().System.TmpDirectory, "rustic-restore-")
	if err != nil {
		return errors.WrapIf(err, "backup: failed to create temporary restore directory")
	}
	defer os.RemoveAll(tmp)

	// Snapshots are stored with "/" as the source path (see Generate), so restore
	// the root of the snapshot into our temporary directory.
	if _, err := b.run(ctx, "restore", snap.Id+":/", tmp); err != nil {
		return err
	}

	return filepath.Walk(tmp, func(p string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(tmp, p)
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		return callback(rel, info, f)
	})
}

// writeGlobFile converts a gitignore-style ignore list into a rustic glob file.
// rustic includes matching paths by default and excludes those prefixed with
// "!", which is the inverse of gitignore, so each ignore pattern is negated. An
// empty ignore list returns an empty path and a no-op cleanup.
func writeGlobFile(ignore string) (string, func(), error) {
	noop := func() {}
	if strings.TrimSpace(ignore) == "" {
		return "", noop, nil
	}
	var b strings.Builder
	for _, line := range strings.Split(ignore, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "!") {
			// A gitignore re-include becomes a rustic include.
			b.WriteString(strings.TrimPrefix(trimmed, "!"))
		} else {
			// A gitignore exclude becomes a rustic exclusion.
			b.WriteString("!" + trimmed)
		}
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		return "", noop, nil
	}
	f, err := os.CreateTemp(config.Get().System.TmpDirectory, "rustic-glob-")
	if err != nil {
		return "", noop, errors.WrapIf(err, "backup: failed to create rustic glob file")
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", noop, err
	}
	f.Close()
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// parseSnapshot parses the JSON emitted by `rustic backup --json`. Depending on
// the rustic version this is either a single snapshot object or a two-element
// array of [summary, snapshot]; both are handled.
func parseSnapshot(out []byte) (*rusticSnapshot, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, errors.New("backup: empty rustic output")
	}
	if trimmed[0] == '[' {
		snaps, err := parseSnapshots(trimmed)
		if err != nil {
			return nil, err
		}
		if len(snaps) == 0 {
			return nil, errors.New("backup: no snapshot in rustic output")
		}
		return &snaps[len(snaps)-1], nil
	}
	var snap rusticSnapshot
	if err := json.Unmarshal(trimmed, &snap); err != nil {
		return nil, errors.WrapIf(err, "backup: failed to parse rustic snapshot output")
	}
	if snap.Id == "" {
		return nil, errors.New("backup: rustic snapshot output missing id")
	}
	return &snap, nil
}

func parseSnapshots(out []byte) ([]rusticSnapshot, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, nil
	}
	// A flat array of snapshot objects (older/ungrouped output).
	var flat []rusticSnapshot
	if err := json.Unmarshal(trimmed, &flat); err == nil && len(flat) > 0 && flat[0].Id != "" {
		return flat, nil
	}
	// `rustic snapshots --json` emits an array of group objects, each shaped
	// { "group_key": {...}, "snapshots": [ {snapshot}, ... ] }.
	var groups []struct {
		Snapshots []rusticSnapshot `json:"snapshots"`
	}
	if err := json.Unmarshal(trimmed, &groups); err != nil {
		return nil, errors.WrapIf(err, "backup: failed to parse rustic snapshots output")
	}
	var result []rusticSnapshot
	for _, g := range groups {
		result = append(result, g.Snapshots...)
	}
	return result, nil
}
