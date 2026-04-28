package main

import (
	"os"
	"path/filepath"
	"testing"
)

func testConfig() Config {
	return Config{
		NQN:           "nqn.2026-04.local.test",
		NSID:          1,
		BackingDev:    "/dev/test",
		PortID:        1,
		Transport:     "tcp",
		AddressFamily: "ipv4",
		Address:       "192.0.2.10",
		ServiceID:     4420,
		AllowAnyHost:  false,
		AllowedHosts:  []string{"nqn.2014-08.org.nvmexpress.uuid.test"},
	}
}

func withFakeNvmet(t *testing.T) Config {
	t.Helper()
	old := nvmetPath
	nvmetPath = filepath.Join(t.TempDir(), "nvmet")
	for _, dir := range []string{"subsystems", "ports", "hosts"} {
		if err := os.MkdirAll(filepath.Join(nvmetPath, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { nvmetPath = old })
	return testConfig()
}

func requireSymlink(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink creation is not available in this test environment: %v", err)
	}
}

func writeFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestStatusInactive(t *testing.T) {
	cfg := withFakeNvmet(t)
	_, st := observe(cfg, derivePaths(cfg))
	if st != StateInactive {
		t.Fatalf("observe() = %s, want %s", st, StateInactive)
	}
}

func TestStartCreatesTarget(t *testing.T) {
	requireSymlink(t)
	cfg := withFakeNvmet(t)
	paths := derivePaths(cfg)

	if err := createTarget(cfg, paths); err != nil {
		t.Fatalf("createTarget() error = %v", err)
	}

	if !isDir(paths.Subsystem) || !isDir(paths.Namespace) || !isDir(paths.Port) {
		t.Fatal("target directories were not created")
	}
	if !sameSymlinkTarget(paths.PortLink, paths.Subsystem) {
		t.Fatal("port subsystem link does not point at subsystem")
	}
	for _, host := range cfg.AllowedHosts {
		if !isDir(filepath.Join(nvmetPath, "hosts", host)) {
			t.Fatalf("host object was not created: %s", host)
		}
		if !isSymlink(filepath.Join(paths.Subsystem, "allowed_hosts", host)) {
			t.Fatalf("allowed host link was not created: %s", host)
		}
	}
}

func TestStatusActive(t *testing.T) {
	requireSymlink(t)
	cfg := withFakeNvmet(t)
	paths := derivePaths(cfg)
	if err := createTarget(cfg, paths); err != nil {
		t.Fatalf("createTarget() error = %v", err)
	}

	_, st := observe(cfg, paths)
	if st != StateActive {
		t.Fatalf("observe() = %s, want %s", st, StateActive)
	}
}

func TestStatusDirty(t *testing.T) {
	requireSymlink(t)
	cfg := withFakeNvmet(t)
	paths := derivePaths(cfg)
	if err := createTarget(cfg, paths); err != nil {
		t.Fatalf("createTarget() error = %v", err)
	}

	writeFile(t, filepath.Join(paths.Namespace, "device_path"), "/dev/other")
	_, st := observe(cfg, paths)
	if st != StateDirty {
		t.Fatalf("observe() = %s, want %s", st, StateDirty)
	}
}

func TestStopRemovesArtifacts(t *testing.T) {
	requireSymlink(t)
	cfg := testConfig()
	base := t.TempDir()
	paths := Paths{
		Subsystems:     filepath.Join(base, "subsystems"),
		Subsystem:      filepath.Join(base, "subsystem"),
		Namespaces:     filepath.Join(base, "namespaces"),
		Namespace:      filepath.Join(base, "namespace"),
		Ports:          filepath.Join(base, "ports"),
		Port:           filepath.Join(base, "port"),
		PortSubsystems: filepath.Join(base, "port-subsystems"),
		PortLink:       filepath.Join(base, "port-subsystems", cfg.NQN),
		Hosts:          filepath.Join(base, "hosts"),
		AllowedHosts:   filepath.Join(base, "allowed_hosts"),
	}

	for _, dir := range []string{paths.Subsystems, paths.Subsystem, paths.Namespaces, paths.Ports, paths.Port} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(paths.Namespace, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.PortSubsystems, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(paths.Hosts, cfg.AllowedHosts[0]), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.AllowedHosts, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(paths.Subsystem, paths.PortLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(paths.Hosts, cfg.AllowedHosts[0]), filepath.Join(paths.AllowedHosts, cfg.AllowedHosts[0])); err != nil {
		t.Fatal(err)
	}

	if err := stopArtifacts(paths); err != nil {
		t.Fatalf("stopArtifacts() error = %v", err)
	}

	if exists(paths.PortLink) || exists(paths.Namespace) || exists(paths.Subsystem) {
		t.Fatal("configured artifacts still exist after stop")
	}
	if exists(paths.Port) {
		t.Fatal("empty port should be removed")
	}
	for _, host := range cfg.AllowedHosts {
		if !isDir(filepath.Join(paths.Hosts, host)) {
			t.Fatalf("global host object should be preserved: %s", host)
		}
	}
}

func TestBlockedWrongType(t *testing.T) {
	cfg := withFakeNvmet(t)
	paths := derivePaths(cfg)
	writeFile(t, paths.PortLink, "not a link")

	r, st := observe(cfg, paths)
	if st != StateBlocked {
		t.Fatalf("observe() = %s, want %s", st, StateBlocked)
	}
	if r.BlockedReason == "" {
		t.Fatal("blocked state should include a reason")
	}
	if err := stopArtifacts(paths); err == nil {
		t.Fatal("stopArtifacts should refuse to remove a non-symlink port link")
	}
	if !exists(paths.PortLink) {
		t.Fatal("blocked path was removed")
	}
}

func TestBlockedIntermediateWrongType(t *testing.T) {
	cfg := withFakeNvmet(t)
	paths := derivePaths(cfg)

	writeFile(t, paths.AllowedHosts, "not a directory")
	r, st := observe(cfg, paths)
	if st != StateBlocked {
		t.Fatalf("observe() = %s, want %s", st, StateBlocked)
	}
	if r.BlockedReason != "allowed_hosts path exists but is not directory" {
		t.Fatalf("blocked reason = %q", r.BlockedReason)
	}
}

func TestBlockedParentWrongType(t *testing.T) {
	cfg := withFakeNvmet(t)
	paths := derivePaths(cfg)

	writeFile(t, paths.Namespaces, "not a directory")
	r, st := observe(cfg, paths)
	if st != StateBlocked {
		t.Fatalf("observe() = %s, want %s", st, StateBlocked)
	}
	if r.BlockedReason != "namespaces path exists but is not directory" {
		t.Fatalf("blocked reason = %q", r.BlockedReason)
	}
}

func TestBlockedConfiguredAllowedHostLinkWrongType(t *testing.T) {
	cfg := withFakeNvmet(t)
	paths := derivePaths(cfg)

	writeFile(t, filepath.Join(paths.AllowedHosts, cfg.AllowedHosts[0]), "not a link")
	r, st := observe(cfg, paths)
	if st != StateBlocked {
		t.Fatalf("observe() = %s, want %s", st, StateBlocked)
	}
	if r.BlockedReason != "allowed host link path exists but is not symlink" {
		t.Fatalf("blocked reason = %q", r.BlockedReason)
	}
}

func TestBlockedAttributeWrongType(t *testing.T) {
	requireSymlink(t)
	cfg := withFakeNvmet(t)
	paths := derivePaths(cfg)
	if err := createTarget(cfg, paths); err != nil {
		t.Fatalf("createTarget() error = %v", err)
	}

	target := filepath.Join(t.TempDir(), "target")
	writeFile(t, target, "1\n")
	attr := filepath.Join(paths.Namespace, "enable")
	if err := os.Remove(attr); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, attr); err != nil {
		t.Fatal(err)
	}

	r, st := observe(cfg, paths)
	if st != StateBlocked {
		t.Fatalf("observe() = %s, want %s", st, StateBlocked)
	}
	if r.BlockedReason != "namespace enable exists but is not regular file" {
		t.Fatalf("blocked reason = %q", r.BlockedReason)
	}
}

func TestBlockedHostPathWrongType(t *testing.T) {
	cfg := withFakeNvmet(t)
	paths := derivePaths(cfg)
	writeFile(t, filepath.Join(paths.Hosts, cfg.AllowedHosts[0]), "not a directory")

	r, st := observe(cfg, paths)
	if st != StateBlocked {
		t.Fatalf("observe() = %s, want %s", st, StateBlocked)
	}
	if r.BlockedReason != "host path exists but is not directory" {
		t.Fatalf("blocked reason = %q", r.BlockedReason)
	}
}

func TestHostsMatchRequiresExactLinks(t *testing.T) {
	requireSymlink(t)
	cfg := withFakeNvmet(t)
	paths := derivePaths(cfg)
	if err := createTarget(cfg, paths); err != nil {
		t.Fatalf("createTarget() error = %v", err)
	}

	if err := os.Remove(filepath.Join(paths.AllowedHosts, cfg.AllowedHosts[0])); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(paths.Subsystem, filepath.Join(paths.AllowedHosts, cfg.AllowedHosts[0])); err != nil {
		t.Fatal(err)
	}
	if _, st := observe(cfg, paths); st != StateDirty {
		t.Fatalf("wrong allowed host target produced %s, want %s", st, StateDirty)
	}

	if err := os.Remove(filepath.Join(paths.AllowedHosts, cfg.AllowedHosts[0])); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(nvmetPath, "hosts", cfg.AllowedHosts[0]), filepath.Join(paths.AllowedHosts, cfg.AllowedHosts[0])); err != nil {
		t.Fatal(err)
	}
	cfg.AllowAnyHost = true
	cfg.AllowedHosts = nil
	writeFile(t, filepath.Join(paths.Subsystem, "attr_allow_any_host"), "1")
	if _, st := observe(cfg, paths); st != StateDirty {
		t.Fatalf("allow_any_host with stale allowed host produced %s, want %s", st, StateDirty)
	}
}

func TestHostsMatchRequiresHostObject(t *testing.T) {
	requireSymlink(t)
	cfg := withFakeNvmet(t)
	paths := derivePaths(cfg)
	if err := createTarget(cfg, paths); err != nil {
		t.Fatalf("createTarget() error = %v", err)
	}

	if err := os.Remove(filepath.Join(nvmetPath, "hosts", cfg.AllowedHosts[0])); err != nil {
		t.Fatal(err)
	}
	if _, st := observe(cfg, paths); st != StateDirty {
		t.Fatalf("missing host object produced %s, want %s", st, StateDirty)
	}
}

func TestValidateRejectsDuplicateHosts(t *testing.T) {
	raw := RawConfig{}
	raw.Subsystem.NQN = "nqn.2026-04.local.test"
	raw.Namespace.ID = 1
	raw.Namespace.BackingDev = "/dev/test"
	raw.Port.ID = 1
	raw.Port.Transport = "tcp"
	raw.Port.AddressFamily = "ipv4"
	raw.Port.Address = "192.0.2.10"
	raw.Port.ServiceID = 4420
	raw.Hosts.Allowed = []string{"nqn.2026-04.local.host", "nqn.2026-04.local.host"}

	if _, err := validateConfig(raw); err == nil {
		t.Fatal("validateConfig should reject duplicate hosts")
	}
}

func TestValidateRejectsOuterWhitespace(t *testing.T) {
	raw := RawConfig{}
	raw.Subsystem.NQN = " nqn.2026-04.local.test"
	raw.Namespace.ID = 1
	raw.Namespace.BackingDev = "/dev/test"
	raw.Port.ID = 1
	raw.Port.Transport = "tcp"
	raw.Port.AddressFamily = "ipv4"
	raw.Port.Address = "192.0.2.10"
	raw.Port.ServiceID = 4420
	raw.Hosts.Allowed = []string{"nqn.2026-04.local.host"}

	if _, err := validateConfig(raw); err == nil {
		t.Fatal("validateConfig should reject outer whitespace")
	}

	raw.Subsystem.NQN = "nqn.2026-04.local.test"
	raw.Hosts.Allowed = []string{"nqn.2026-04.local.host "}
	if _, err := validateConfig(raw); err == nil {
		t.Fatal("validateConfig should reject host outer whitespace")
	}
}

func TestLoadConfigRejectsNonRegularFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadConfig(dir); err == nil {
		t.Fatal("loadConfig should reject a directory")
	}
}

func TestReadAttrOnlyTrimsLineEnding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attr")
	if err := os.WriteFile(path, []byte("value \n"), 0600); err != nil {
		t.Fatal(err)
	}

	if got := readAttr(path); got != "value " {
		t.Fatalf("readAttr() = %q, want %q", got, "value ")
	}
}

func TestValidConfigfsNameRejectsWhitespace(t *testing.T) {
	for _, name := range []string{"nqn.2026-04.local.host\vbad", "nqn.2026-04.local.host\fbad", "nqn.2026-04.local.host\u2003bad"} {
		if validConfigfsName(name) {
			t.Fatalf("validConfigfsName(%q) = true, want false", name)
		}
	}
}

func TestRemoveDirIfExistsDoesNotDeleteChildren(t *testing.T) {
	dir := t.TempDir()
	child := filepath.Join(dir, "enable")
	writeFile(t, child, "1")

	if err := removeDirIfExists(dir); err == nil {
		t.Fatal("removeDirIfExists should fail on a non-empty ordinary directory")
	}
	if !exists(child) {
		t.Fatal("removeDirIfExists deleted a child file")
	}
}

func TestStopArtifactsRejectsWrongTypeParent(t *testing.T) {
	cfg := withFakeNvmet(t)
	paths := derivePaths(cfg)
	writeFile(t, paths.PortSubsystems, "not a directory")

	if err := stopArtifacts(paths); err == nil {
		t.Fatal("stopArtifacts should reject wrong-type parent path")
	}
	if !exists(paths.PortSubsystems) {
		t.Fatal("wrong-type parent was removed")
	}
}

func TestRunReleasesLockOnCommandError(t *testing.T) {
	lockDir := filepath.Join(t.TempDir(), "lock")
	oldLock := lockFilePath
	oldConfigfs := configfsPath
	lockFilePath = lockDir
	configfsPath = t.TempDir()
	t.Cleanup(func() {
		lockFilePath = oldLock
		configfsPath = oldConfigfs
	})

	code := run([]string{"unknown"})
	if code != 1 {
		t.Fatalf("run() exit code = %d, want 1", code)
	}
	if exists(lockDir) {
		t.Fatal("lock directory still exists after command error")
	}
}
