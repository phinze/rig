package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const teardownJobVersion = 1

// teardownJob is the durable handoff between deciding a rig is disposable and
// actually dismantling it. It deliberately lives outside the basedir: cleanup
// can be retried after the active path has been quarantined, and a process dying
// halfway through teardown does not erase the only inventory of what remains.
type teardownJob struct {
	Version       int                 `json:"version"`
	ID            string              `json:"id"`
	Basedir       string              `json:"basedir"`
	Quarantined   string              `json:"quarantined,omitempty"`
	Session       string              `json:"session"`
	Created       time.Time           `json:"created"`
	ISOWorkspaces []isoCleanup        `json:"iso_workspaces,omitempty"`
	Compose       []string            `json:"compose_projects,omitempty"`
	ForgetGroups  map[string][]string `json:"forget_groups,omitempty"`
	ScratchDirs   []string            `json:"scratch_dirs,omitempty"`
	path          string
}

type isoCleanup struct {
	Workspace string `json:"workspace"`
	Session   string `json:"session"`
}

var errRigBusy = errors.New("rig cleanup is already in progress")

type rigLock struct{ file *os.File }

func (l *rigLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	return l.file.Close()
}

// acquireRigLock serializes manifest mutations and teardown by basedir. Locks
// live in runtime state rather than inside the rig, so quarantine cannot make a
// second process unknowingly take a fresh lock at the old path.
func acquireRigLock(basedir string, nonblocking bool) (*rigLock, error) {
	dir, err := rigRuntimeDir()
	if err != nil {
		return nil, err
	}
	dir = filepath.Join(dir, "locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(resolvePath(basedir)))
	path := filepath.Join(dir, fmt.Sprintf("%x.lock", sum[:12]))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	how := unix.LOCK_EX
	if nonblocking {
		how |= unix.LOCK_NB
	}
	if err := unix.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errRigBusy
		}
		return nil, err
	}
	return &rigLock{file: f}, nil
}

func acquireRigMutationLock(basedir string) (*rigLock, error) {
	lock, err := acquireRigLock(basedir, false)
	if err != nil {
		return nil, err
	}
	jobs, err := pendingTeardownJobs()
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	for _, path := range jobs {
		job, err := readTeardownJob(path)
		if err == nil && resolvePath(job.Basedir) == resolvePath(basedir) {
			_ = lock.Close()
			return nil, fmt.Errorf("teardown already pending for %s; run `rig reap` to retry it", job.ID)
		}
	}
	return lock, nil
}

func rigStateDir() (string, error) {
	if state := os.Getenv("XDG_STATE_HOME"); state != "" {
		return filepath.Join(state, "rig"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "rig"), nil
}

func rigRuntimeDir() (string, error) {
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		return filepath.Join(runtimeDir, "rig"), nil
	}
	state, err := rigStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, "runtime"), nil
}

func teardownJobPath(id, basedir string) (string, error) {
	state, err := rigStateDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(resolvePath(basedir)))
	name := slugify(id)
	if name == "" {
		name = "rig"
	}
	return filepath.Join(state, "reaping", fmt.Sprintf("%s-%x.json", name, sum[:8])), nil
}

func writeTeardownJob(job *teardownJob) error {
	if job.path == "" {
		path, err := teardownJobPath(job.ID, job.Basedir)
		if err != nil {
			return err
		}
		job.path = path
	}
	if err := os.MkdirAll(filepath.Dir(job.path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	tmp := job.path + fmt.Sprintf(".tmp-%d", os.Getpid())
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, job.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func readTeardownJob(path string) (*teardownJob, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var job teardownJob
	if err := json.Unmarshal(body, &job); err != nil {
		return nil, err
	}
	if job.Version != teardownJobVersion {
		return nil, fmt.Errorf("unsupported teardown job version %d", job.Version)
	}
	job.path = path
	return &job, nil
}

func pendingTeardownJobs() ([]string, error) {
	state, err := rigStateDir()
	if err != nil {
		return nil, err
	}
	paths, err := filepath.Glob(filepath.Join(state, "reaping", "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

// prepareTeardownJob snapshots every resource whose discovery depends on the
// live workspace. Once this file is durable, the cleanup worker can retry from
// any cgroup and even after the basedir has moved out of the active namespace.
func prepareTeardownJob(basedir string, m manifest) (*teardownJob, error) {
	entries, err := os.ReadDir(basedir)
	if err != nil {
		return nil, err
	}
	job := &teardownJob{
		Version:      teardownJobVersion,
		ID:           m.ID,
		Basedir:      basedir,
		Session:      tmuxSessionName(basedir),
		Created:      time.Now(),
		ForgetGroups: map[string][]string{},
		ScratchDirs:  claudeScratchDirs(basedir),
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(basedir, e.Name())
		if dirExists(filepath.Join(p, ".iso")) {
			job.ISOWorkspaces = append(job.ISOWorkspaces, isoCleanup{
				Workspace: p,
				Session:   isoSessionName(m.ID, e.Name()),
			})
		}
		if !dirExists(filepath.Join(p, ".jj")) {
			continue
		}
		source, err := jjSourceRepo(p)
		if err != nil {
			return nil, fmt.Errorf("resolving source repo for %s: %w", p, err)
		}
		job.ForgetGroups[source] = append(job.ForgetGroups[source], jjWorkspaceName(m.ID, e.Name()))
	}
	for source := range job.ForgetGroups {
		sort.Strings(job.ForgetGroups[source])
	}

	if _, err := exec.LookPath("docker"); err == nil {
		projects, err := discoverComposeProjects(basedir)
		if err != nil {
			return nil, fmt.Errorf("discovering compose projects: %w", err)
		}
		for project := range projects {
			job.Compose = append(job.Compose, project)
		}
		sort.Strings(job.Compose)
	}
	if err := writeTeardownJob(job); err != nil {
		return nil, fmt.Errorf("persisting teardown job: %w", err)
	}
	return job, nil
}

// executeTeardownJob is intentionally idempotent. Every destructive step can
// be replayed after a crash; the tombstone disappears only once processes,
// external resources, workspace registrations, scratch, and files are gone.
func executeTeardownJob(job *teardownJob) error {
	return executeTeardownJobForPlatform(job, runtime.GOOS)
}

// executeTeardownJobForPlatform keeps the process-owning step native to each
// host. Linux runs from an external systemd worker, so it can kill tmux and all
// RIG_ID scopes first, preventing processes from recreating resources during
// cleanup. Darwin has no external worker or cgroup ownership: killing the tmux
// session would SIGHUP this very process, so it finishes cleanup and clears the
// tombstone before killing the session last.
func executeTeardownJobForPlatform(job *teardownJob, platform string) error {
	killFirst := platform == "linux"
	if killFirst {
		if err := tmuxKillSession(job.Session); err != nil {
			return fmt.Errorf("tmux kill-session %s: %w", job.Session, err)
		}
		if err := stopRigProcessScopes(job.ID); err != nil {
			return err
		}
	}
	for _, dir := range job.ScratchDirs {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("removing agent scratch %s: %w", dir, err)
		}
	}

	if len(job.ISOWorkspaces) > 0 {
		if _, err := exec.LookPath("iso"); err != nil {
			return fmt.Errorf("iso cleanup required but iso is unavailable")
		}
		for _, item := range job.ISOWorkspaces {
			if !dirExists(item.Workspace) {
				continue
			}
			fmt.Fprintf(os.Stderr, "rig: iso stop --session %s\n", item.Session)
			if err := isoStop(item.Workspace, item.Session); err != nil {
				return fmt.Errorf("iso stop %s: %w", item.Session, err)
			}
		}
	}
	for _, project := range job.Compose {
		fmt.Fprintf(os.Stderr, "rig: removing docker compose project %s\n", project)
		if err := removeComposeProject(project); err != nil {
			return err
		}
	}

	for source, names := range job.ForgetGroups {
		var registered []string
		for _, name := range names {
			if workspaceRegistered(source, name) {
				registered = append(registered, name)
			}
		}
		if len(registered) == 0 {
			continue
		}
		fmt.Fprintf(os.Stderr, "rig: jj workspace forget %v (from %s)\n", registered, source)
		if err := jjWorkspaceForget(source, registered); err != nil {
			return fmt.Errorf("jj workspace forget: %w", err)
		}
	}

	if job.Quarantined == "" {
		if _, err := os.Stat(job.Basedir); err == nil {
			quarantined, err := quarantineBasedir(job.Basedir)
			if err != nil {
				return fmt.Errorf("quarantining basedir: %w", err)
			}
			job.Quarantined = quarantined
			if err := writeTeardownJob(job); err != nil {
				return fmt.Errorf("recording quarantine path: %w", err)
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if job.Quarantined != "" {
		if err := os.RemoveAll(job.Quarantined); err != nil {
			return fmt.Errorf("removing quarantined basedir %s: %w", job.Quarantined, err)
		}
		_ = os.Remove(filepath.Dir(job.Quarantined))
	}
	if err := os.Remove(job.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing teardown job: %w", err)
	}
	if !killFirst {
		if err := tmuxKillSession(job.Session); err != nil {
			// Everything else is already idempotently gone, but retain a retry
			// record when tmux itself refused the final step.
			if persistErr := writeTeardownJob(job); persistErr != nil {
				return fmt.Errorf("tmux kill-session %s: %w (also restoring teardown job: %v)", job.Session, err, persistErr)
			}
			return fmt.Errorf("tmux kill-session %s: %w", job.Session, err)
		}
	}
	return nil
}

func executeTeardownJobFile(path string, nonblocking bool) error {
	job, err := readTeardownJob(path)
	if err != nil {
		return err
	}
	lock, err := acquireRigLock(job.Basedir, nonblocking)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	return executeTeardownJob(job)
}

func startTeardownWorker(job *teardownJob) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("background teardown requires systemd")
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return fmt.Errorf("background teardown requires systemd-run")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(job.path + strconv.FormatInt(time.Now().UnixNano(), 10)))
	unit := fmt.Sprintf("rig-teardown-%x", sum[:8])
	cmd := exec.Command("systemd-run", "--user", "--collect", "--quiet",
		"--service-type=exec", "--unit", unit,
		"env", "-u", "RIG_ID", "-u", "RIG_BASEDIR", "-u", "RIG_WORKSPACE",
		exe, "__teardown", job.path)
	cmd.Dir = home
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// claudeScratchDirs returns per-cwd temp roots for this rig. Claude's project
// mangling is the same one used by its durable session store, so a prefix match
// captures the rig root and every repo cwd without touching another rig.
func claudeScratchDirs(basedir string) []string {
	root := filepath.Join(os.TempDir(), fmt.Sprintf("claude-%d", os.Getuid()))
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	prefix := claudeProjectDirName(basedir)
	var dirs []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if entry.Name() == prefix || strings.HasPrefix(entry.Name(), prefix+"-") {
			dirs = append(dirs, filepath.Join(root, entry.Name()))
		}
	}
	sort.Strings(dirs)
	return dirs
}

// discoverComposeProjects is the fail-closed form of compose discovery. An
// unavailable daemon must not erase the workspace and with it the only useful
// working-dir labels for a later retry.
func discoverComposeProjects(basedir string) (map[string]string, error) {
	out, err := exec.Command("docker", "ps", "-a",
		"--filter", "label=com.docker.compose.project",
		"--format", `{{.Label "com.docker.compose.project"}}`+"\t"+`{{.Label "com.docker.compose.project.working_dir"}}`,
	).Output()
	if err != nil {
		return nil, err
	}
	base := resolvePath(basedir)
	found := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		project, workdir, ok := strings.Cut(line, "\t")
		if !ok || project == "" || workdir == "" {
			continue
		}
		if isUnder(resolvePath(workdir), base) {
			found[project] = workdir
		}
	}
	return found, nil
}

func dockerIDs(kind string, args ...string) ([]string, error) {
	cmdArgs := append([]string{kind, "ls", "-q"}, args...)
	if kind == "container" {
		cmdArgs = append([]string{"ps", "-aq"}, args...)
	}
	out, err := exec.Command("docker", cmdArgs...).Output()
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

func removeComposeProject(project string) error {
	filter := "label=com.docker.compose.project=" + project
	containers, err := dockerIDs("container", "--filter", filter)
	if err != nil {
		return fmt.Errorf("listing compose containers for %s: %w", project, err)
	}
	if len(containers) > 0 {
		args := append([]string{"rm", "-f"}, containers...)
		if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("removing compose containers for %s: %w: %s", project, err, strings.TrimSpace(string(out)))
		}
	}
	for _, kind := range []string{"network", "volume"} {
		ids, err := dockerIDs(kind, "--filter", filter)
		if err != nil {
			return fmt.Errorf("listing compose %ss for %s: %w", kind, project, err)
		}
		if len(ids) == 0 {
			continue
		}
		args := append([]string{kind, "rm"}, ids...)
		if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("removing compose %ss for %s: %w: %s", kind, project, err, strings.TrimSpace(string(out)))
		}
	}
	for _, kind := range []string{"container", "network", "volume"} {
		ids, err := dockerIDs(kind, "--filter", filter)
		if err != nil {
			return err
		}
		if len(ids) > 0 {
			return fmt.Errorf("compose project %s still has %s resources: %v", project, kind, ids)
		}
	}
	return nil
}

// stopRigProcessScopes finds tmux 3.6's transient pane scopes by cgroup, then
// identifies ownership from the inherited RIG_ID environment. Killing the
// scope, rather than just the tmux session, also catches daemonized descendants
// whose cwd and log files outlive the pane.
func stopRigProcessScopes(rigID string) error {
	if runtime.GOOS != "linux" || rigID == "" {
		return nil
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil
	}
	scopes, err := rigProcessScopes(rigID)
	if err != nil {
		return fmt.Errorf("discovering process scopes: %w", err)
	}
	for _, scope := range scopes {
		fmt.Fprintf(os.Stderr, "rig: systemctl --user stop %s\n", scope)
		if err := stopUserScope(scope); err != nil {
			return err
		}
	}
	remaining, err := rigProcessScopes(rigID)
	if err != nil {
		return fmt.Errorf("verifying process scopes: %w", err)
	}
	if len(remaining) > 0 {
		return fmt.Errorf("process scopes still running for %s: %v", rigID, remaining)
	}
	return nil
}

type rigProcessScope struct {
	Unit    string
	ID      string
	Basedir string
}

// cleanupOrphanedRigRuntime is the frequent host backstop. A scope carrying a
// RIG_ID but no live manifest is an escaped pane from an already-reaped rig;
// pending teardown IDs are also safe to stop. Capture RIG_BASEDIR before the
// kill so the matching Claude scratch can be removed afterward.
func cleanupOrphanedRigRuntime(active, tearingDown map[string]bool, dryRun bool) (int, error) {
	if runtime.GOOS != "linux" {
		return 0, nil
	}
	scopes, err := allRigProcessScopes()
	if err != nil {
		return 0, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return 0, err
	}
	workspaceRoot := filepath.Join(home, "workspaces")
	cleaned := 0
	for _, scope := range scopes {
		if active[scope.ID] && !tearingDown[scope.ID] {
			continue
		}
		if dryRun {
			fmt.Fprintf(os.Stderr, "rig: would stop orphan runtime %s (%s)\n", scope.ID, scope.Unit)
			cleaned++
			continue
		}
		fmt.Fprintf(os.Stderr, "rig: stopping orphan runtime %s (%s)\n", scope.ID, scope.Unit)
		if err := stopUserScope(scope.Unit); err != nil {
			return cleaned, err
		}
		if scope.Basedir != "" && pathInside(resolvePath(workspaceRoot), resolvePath(scope.Basedir)) {
			for _, dir := range claudeScratchDirs(scope.Basedir) {
				if err := os.RemoveAll(dir); err != nil {
					return cleaned, fmt.Errorf("removing orphan scratch %s: %w", dir, err)
				}
			}
		}
		cleaned++
	}
	return cleaned, nil
}

func stopUserScope(scope string) error {
	out, err := exec.Command("systemctl", "--user", "stop", scope).CombinedOutput()
	if err != nil {
		return fmt.Errorf("stopping %s: %w: %s", scope, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func rigProcessScopes(rigID string) ([]string, error) {
	records, err := allRigProcessScopes()
	if err != nil {
		return nil, err
	}
	var scopes []string
	for _, record := range records {
		if record.ID == rigID {
			scopes = append(scopes, record.Unit)
		}
	}
	sort.Strings(scopes)
	return scopes, nil
}

func allRigProcessScopes() ([]rigProcessScope, error) {
	root, err := userManagerCgroupRoot()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	dirs, err := filepath.Glob(filepath.Join(root, "tmux-spawn-*.scope"))
	if err != nil {
		return nil, err
	}
	var scopes []rigProcessScope
	for _, dir := range dirs {
		body, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
		if err != nil {
			continue
		}
		var owned rigProcessScope
		for _, field := range strings.Fields(string(body)) {
			pid, err := strconv.Atoi(field)
			if err != nil {
				continue
			}
			id, basedir := processRigEnv(pid)
			if id != "" {
				owned = rigProcessScope{Unit: filepath.Base(dir), ID: id, Basedir: basedir}
				break
			}
		}
		if owned.ID != "" {
			scopes = append(scopes, owned)
		}
	}
	sort.Slice(scopes, func(i, j int) bool { return scopes[i].Unit < scopes[j].Unit })
	return scopes, nil
}

func userManagerCgroupRoot() (string, error) {
	// A caller reached over SSH lives in session-N.scope, outside the user
	// manager subtree it needs to inspect. Prefer systemd's stable per-user
	// manager path; the /proc fallback below covers nonstandard layouts.
	fixed := filepath.Join("/sys/fs/cgroup/user.slice",
		fmt.Sprintf("user-%d.slice", os.Getuid()),
		fmt.Sprintf("user@%d.service", os.Getuid()))
	if info, err := os.Stat(fixed); err == nil && info.IsDir() {
		return fixed, nil
	}
	body, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	needle := fmt.Sprintf("/user@%d.service", os.Getuid())
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		_, path, ok := strings.Cut(line, "::")
		if !ok {
			continue
		}
		if i := strings.Index(path, needle); i >= 0 {
			return filepath.Join("/sys/fs/cgroup", path[:i+len(needle)]), nil
		}
	}
	return "", os.ErrNotExist
}

func processRigEnv(pid int) (id, basedir string) {
	body, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil {
		return "", ""
	}
	for _, field := range strings.Split(string(body), "\x00") {
		if value, ok := strings.CutPrefix(field, "RIG_ID="); ok {
			id = value
		}
		if value, ok := strings.CutPrefix(field, "RIG_BASEDIR="); ok {
			basedir = value
		}
	}
	return id, basedir
}
