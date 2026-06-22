package main

import (
	"errors"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/BurntSushi/toml"
)

const (
	defaultConfigPath = "/etc/nvme-of/config.toml"
	version           = "0.1.3"
	lockWaitTimeout   = 30 * time.Second
	attrWriteTimeout  = 10 * time.Second
)

var (
	nvmetPath        = "/sys/kernel/config/nvmet"
	rdmaCMPath       = "/sys/kernel/config/rdma_cm"
	configfsPath     = "/sys/kernel/config"
	errStatusNonZero = errors.New("status is not active")
	lockFilePath     = "/run/nvme-of-target-manager.lock"
)

type State string

const (
	StateActive   State = "active"
	StateInactive State = "inactive"
	StateDirty    State = "dirty"
	StateBlocked  State = "blocked"
)

type RawConfig struct {
	Subsystem struct {
		NQN string `toml:"nqn"`
	} `toml:"subsystem"`
	Namespace struct {
		ID         int    `toml:"id"`
		BackingDev string `toml:"backing_dev"`
	} `toml:"namespace"`
	Port struct {
		ID            int    `toml:"id"`
		Transport     string `toml:"transport"`
		AddressFamily string `toml:"address_family"`
		Address       string `toml:"address"`
		ServiceID     int    `toml:"service_id"`
	} `toml:"port"`
	Hosts struct {
		AllowAnyHost bool     `toml:"allow_any_host"`
		Allowed      []string `toml:"allowed"`
	} `toml:"hosts"`
	QoS struct {
		Enabled     bool   `toml:"enabled"`
		RDMADevice  string `toml:"rdma_device"`
		RDMAPort    int    `toml:"rdma_port"`
		RoCETOS     int    `toml:"roce_tos"`
		PCPPriority int    `toml:"pcp_priority"`
	} `toml:"qos"`
}

type Config struct {
	NQN            string
	NSID           int
	BackingDev     string
	PortID         int
	Transport      string
	AddressFamily  string
	Address        string
	ServiceID      int
	AllowAnyHost   bool
	AllowedHosts   []string
	QoSEnabled     bool
	QoSRDMADevice  string
	QoSRDMAPort    int
	QoSRoCETOS     int
	QoSPCPPriority int
}

type Paths struct {
	Subsystems     string
	Subsystem      string
	Namespaces     string
	Namespace      string
	Ports          string
	Port           string
	PortSubsystems string
	PortLink       string
	Hosts          string
	AllowedHosts   string
	RDMACMDevice   string
	RDMACMPort     string
	RDMADefaultTOS string
}

type Runtime struct {
	BlockedReason string
}

type cliOptions struct {
	command    string
	configPath string
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	opts, err := parseCLI(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		usage()
		return 2
	}

	if opts.command == "help" {
		usage()
		return 0
	}
	if opts.command == "version" {
		fmt.Println(version)
		return 0
	}

	unlock, err := acquireLock()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer unlock()

	cfg, err := loadConfig(opts.configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	paths := derivePaths(cfg)

	switch opts.command {
	case "start":
		err = start(cfg, paths)
	case "stop":
		err = stop(cfg, paths)
	case "status":
		err = status(cfg, paths)
	default:
		err = fmt.Errorf("unknown command: %s", opts.command)
	}

	if err != nil {
		if !errors.Is(err, errStatusNonZero) {
			fmt.Fprintln(os.Stderr, err)
		}
		return 1
	}
	return 0
}

func parseCLI(args []string) (cliOptions, error) {
	if len(args) == 0 {
		return cliOptions{}, errors.New("missing command")
	}

	cmd := args[0]
	if cmd == "-h" || cmd == "--help" {
		return cliOptions{command: "help"}, nil
	}

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("c", defaultConfigPath, "config file")
	if err := fs.Parse(args[1:]); err != nil {
		return cliOptions{}, err
	}
	if fs.NArg() != 0 {
		return cliOptions{}, fmt.Errorf("unexpected argument: %s", fs.Arg(0))
	}

	return cliOptions{command: cmd, configPath: *configPath}, nil
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  nvme-of-target-manager start  [-c /path/config.toml]")
	fmt.Fprintln(os.Stderr, "  nvme-of-target-manager stop   [-c /path/config.toml]")
	fmt.Fprintln(os.Stderr, "  nvme-of-target-manager status [-c /path/config.toml]")
	fmt.Fprintln(os.Stderr, "  nvme-of-target-manager version")
}

func loadConfig(path string) (Config, error) {
	if !filepath.IsAbs(path) {
		return Config{}, fmt.Errorf("config path must be absolute: %s", path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return Config{}, fmt.Errorf("stat config: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Config{}, fmt.Errorf("config file must not be a symlink: %s", path)
	}
	if !info.Mode().IsRegular() {
		return Config{}, fmt.Errorf("config file must be a regular file: %s", path)
	}
	if info.Mode().Perm()&0022 != 0 {
		return Config{}, fmt.Errorf("config file must not be group/world writable: %s", path)
	}

	var raw RawConfig
	if _, err := toml.DecodeFile(path, &raw); err != nil {
		return Config{}, fmt.Errorf("load config: %w", err)
	}
	return validateConfig(raw)
}

func validateConfig(raw RawConfig) (Config, error) {
	c := Config{
		NQN:            raw.Subsystem.NQN,
		NSID:           raw.Namespace.ID,
		BackingDev:     raw.Namespace.BackingDev,
		PortID:         raw.Port.ID,
		Transport:      raw.Port.Transport,
		AddressFamily:  raw.Port.AddressFamily,
		Address:        raw.Port.Address,
		ServiceID:      raw.Port.ServiceID,
		AllowAnyHost:   raw.Hosts.AllowAnyHost,
		AllowedHosts:   append([]string(nil), raw.Hosts.Allowed...),
		QoSEnabled:     raw.QoS.Enabled,
		QoSRDMADevice:  raw.QoS.RDMADevice,
		QoSRDMAPort:    raw.QoS.RDMAPort,
		QoSRoCETOS:     raw.QoS.RoCETOS,
		QoSPCPPriority: raw.QoS.PCPPriority,
	}

	for field, value := range map[string]string{
		"subsystem.nqn":         c.NQN,
		"namespace.backing_dev": c.BackingDev,
		"port.transport":        c.Transport,
		"port.address_family":   c.AddressFamily,
		"port.address":          c.Address,
	} {
		if err := rejectOuterWhitespace(field, value); err != nil {
			return Config{}, err
		}
	}
	if !validConfigfsName(c.NQN) || !hasNQNPrefix(c.NQN) {
		return Config{}, fmt.Errorf("invalid subsystem nqn: %q", c.NQN)
	}
	if c.NSID <= 0 {
		return Config{}, errors.New("namespace.id must be greater than 0")
	}
	if !filepath.IsAbs(c.BackingDev) {
		return Config{}, fmt.Errorf("namespace.backing_dev must be absolute: %s", c.BackingDev)
	}
	if c.PortID <= 0 {
		return Config{}, errors.New("port.id must be greater than 0")
	}
	if c.Transport != "tcp" && c.Transport != "rdma" {
		return Config{}, fmt.Errorf("unsupported port.transport: %s", c.Transport)
	}
	if c.ServiceID < 1 || c.ServiceID > 65535 {
		return Config{}, errors.New("port.service_id must be in 1..65535")
	}

	ip, err := netip.ParseAddr(c.Address)
	if err != nil {
		return Config{}, fmt.Errorf("port.address must be a valid IP: %s", raw.Port.Address)
	}
	if ip.Zone() != "" {
		return Config{}, fmt.Errorf("port.address must not contain an IPv6 zone: %s", raw.Port.Address)
	}
	ip = ip.Unmap()
	c.Address = ip.String()
	switch c.AddressFamily {
	case "ipv4":
		if !ip.Is4() {
			return Config{}, errors.New("port.address_family ipv4 requires an IPv4 address")
		}
	case "ipv6":
		if !ip.Is6() {
			return Config{}, errors.New("port.address_family ipv6 requires an IPv6 address")
		}
	default:
		return Config{}, fmt.Errorf("unsupported port.address_family: %s", c.AddressFamily)
	}

	seenHosts := make(map[string]struct{}, len(c.AllowedHosts))
	for i, host := range c.AllowedHosts {
		if err := rejectOuterWhitespace("hosts.allowed", host); err != nil {
			return Config{}, err
		}
		if !validConfigfsName(host) || !hasNQNPrefix(host) {
			return Config{}, fmt.Errorf("invalid host nqn: %q", host)
		}
		if _, ok := seenHosts[host]; ok {
			return Config{}, fmt.Errorf("duplicate host nqn: %q", host)
		}
		seenHosts[host] = struct{}{}
		c.AllowedHosts[i] = host
	}
	if c.AllowAnyHost && len(c.AllowedHosts) != 0 {
		return Config{}, errors.New("hosts.allow_any_host=true requires hosts.allowed=[]")
	}
	if !c.AllowAnyHost && len(c.AllowedHosts) == 0 {
		return Config{}, errors.New("hosts.allow_any_host=false requires at least one allowed host")
	}
	if c.QoSEnabled {
		if err := rejectOuterWhitespace("qos.rdma_device", c.QoSRDMADevice); err != nil {
			return Config{}, err
		}
		if c.Transport != "rdma" {
			return Config{}, errors.New("qos.enabled=true requires port.transport=rdma")
		}
		if !validConfigfsName(c.QoSRDMADevice) {
			return Config{}, fmt.Errorf("invalid qos.rdma_device: %q", c.QoSRDMADevice)
		}
		if c.QoSRDMAPort <= 0 {
			return Config{}, errors.New("qos.rdma_port must be greater than 0")
		}
		if c.QoSRoCETOS < 0 || c.QoSRoCETOS > 255 {
			return Config{}, errors.New("qos.roce_tos must be in 0..255")
		}
		if c.QoSPCPPriority < 0 || c.QoSPCPPriority > 7 {
			return Config{}, errors.New("qos.pcp_priority must be in 0..7")
		}
		if c.QoSRoCETOS>>5 != c.QoSPCPPriority {
			return Config{}, errors.New("qos.roce_tos high 3 bits must match qos.pcp_priority")
		}
	}

	return c, nil
}

func derivePaths(c Config) Paths {
	subsystems := filepath.Join(nvmetPath, "subsystems")
	ports := filepath.Join(nvmetPath, "ports")
	hosts := filepath.Join(nvmetPath, "hosts")

	subsys := filepath.Join(subsystems, c.NQN)
	namespaces := filepath.Join(subsys, "namespaces")
	port := filepath.Join(ports, strconv.Itoa(c.PortID))
	rdmaDevice := filepath.Join(rdmaCMPath, c.QoSRDMADevice)
	rdmaPort := filepath.Join(rdmaDevice, "ports", strconv.Itoa(c.QoSRDMAPort))

	return Paths{
		Subsystems:     subsystems,
		Subsystem:      subsys,
		Namespaces:     namespaces,
		Namespace:      filepath.Join(namespaces, strconv.Itoa(c.NSID)),
		Ports:          ports,
		Port:           port,
		PortSubsystems: filepath.Join(port, "subsystems"),
		PortLink:       filepath.Join(port, "subsystems", c.NQN),
		Hosts:          hosts,
		AllowedHosts:   filepath.Join(subsys, "allowed_hosts"),
		RDMACMDevice:   rdmaDevice,
		RDMACMPort:     rdmaPort,
		RDMADefaultTOS: filepath.Join(rdmaPort, "default_roce_tos"),
	}
}

func prepare(c Config) error {
	if os.Geteuid() != 0 {
		return errors.New("must run as root")
	}

	if err := runCommandQuiet("modprobe", "configfs"); err != nil {
		return err
	}
	if err := runCommandQuiet("modprobe", "nvmet"); err != nil {
		return err
	}
	if err := runCommandQuiet("modprobe", "nvmet-"+c.Transport); err != nil {
		return err
	}
	if c.QoSEnabled {
		if err := runCommandQuiet("modprobe", "rdma_cm"); err != nil {
			return err
		}
	}

	if err := os.MkdirAll(configfsPath, 0755); err != nil {
		return fmt.Errorf("mkdir configfs mountpoint: %w", err)
	}
	if !isDir(nvmetPath) {
		if err := runCommandQuiet("mount", "-t", "configfs", "none", configfsPath); err != nil {
			return err
		}
	}
	if !isDir(nvmetPath) {
		return fmt.Errorf("configfs nvmet path is not available: %s", nvmetPath)
	}
	if c.QoSEnabled && !isDir(rdmaCMPath) {
		return fmt.Errorf("configfs rdma_cm path is not available: %s", rdmaCMPath)
	}

	return nil
}

func observe(c Config, p Paths) (Runtime, State) {
	var r Runtime

	if existsNotDir(p.Subsystems) {
		r.BlockedReason = "subsystems path exists but is not directory"
		return r, StateBlocked
	}
	if existsNotDir(p.Subsystem) {
		r.BlockedReason = "subsystem path exists but is not directory"
		return r, StateBlocked
	}
	if existsNotDir(p.Namespaces) {
		r.BlockedReason = "namespaces path exists but is not directory"
		return r, StateBlocked
	}
	if existsNotDir(p.Namespace) {
		r.BlockedReason = "namespace path exists but is not directory"
		return r, StateBlocked
	}
	if existsNotDir(p.Ports) {
		r.BlockedReason = "ports path exists but is not directory"
		return r, StateBlocked
	}
	if existsNotDir(p.Port) {
		r.BlockedReason = "port path exists but is not directory"
		return r, StateBlocked
	}
	if existsNotDir(p.PortSubsystems) {
		r.BlockedReason = "port subsystems path exists but is not directory"
		return r, StateBlocked
	}
	if existsNotSymlink(p.PortLink) {
		r.BlockedReason = "port subsystem link path exists but is not symlink"
		return r, StateBlocked
	}
	if existsNotDir(p.AllowedHosts) {
		r.BlockedReason = "allowed_hosts path exists but is not directory"
		return r, StateBlocked
	}
	if isDir(p.AllowedHosts) {
		for _, host := range c.AllowedHosts {
			linkPath := filepath.Join(p.AllowedHosts, host)
			if existsNotSymlink(linkPath) {
				r.BlockedReason = "allowed host link path exists but is not symlink"
				return r, StateBlocked
			}
		}
	}
	if len(c.AllowedHosts) != 0 {
		if existsNotDir(p.Hosts) {
			r.BlockedReason = "hosts path exists but is not directory"
			return r, StateBlocked
		}
		for _, host := range c.AllowedHosts {
			hostPath := filepath.Join(p.Hosts, host)
			if existsNotDir(hostPath) {
				r.BlockedReason = "host path exists but is not directory"
				return r, StateBlocked
			}
		}
	}
	if existsNotRegularFile(filepath.Join(p.Subsystem, "attr_allow_any_host")) {
		r.BlockedReason = "subsystem attr_allow_any_host exists but is not regular file"
		return r, StateBlocked
	}
	if existsNotRegularFile(filepath.Join(p.Namespace, "enable")) {
		r.BlockedReason = "namespace enable exists but is not regular file"
		return r, StateBlocked
	}
	if existsNotRegularFile(filepath.Join(p.Namespace, "device_path")) {
		r.BlockedReason = "namespace device_path exists but is not regular file"
		return r, StateBlocked
	}
	if existsNotRegularFile(filepath.Join(p.Port, "addr_trtype")) {
		r.BlockedReason = "port addr_trtype exists but is not regular file"
		return r, StateBlocked
	}
	if existsNotRegularFile(filepath.Join(p.Port, "addr_adrfam")) {
		r.BlockedReason = "port addr_adrfam exists but is not regular file"
		return r, StateBlocked
	}
	if existsNotRegularFile(filepath.Join(p.Port, "addr_traddr")) {
		r.BlockedReason = "port addr_traddr exists but is not regular file"
		return r, StateBlocked
	}
	if existsNotRegularFile(filepath.Join(p.Port, "addr_trsvcid")) {
		r.BlockedReason = "port addr_trsvcid exists but is not regular file"
		return r, StateBlocked
	}
	if c.QoSEnabled {
		if existsNotDir(p.RDMACMDevice) {
			r.BlockedReason = "rdma_cm device path exists but is not directory"
			return r, StateBlocked
		}
		if existsNotDir(p.RDMACMPort) {
			r.BlockedReason = "rdma_cm port path exists but is not directory"
			return r, StateBlocked
		}
		if existsNotRegularFile(p.RDMADefaultTOS) {
			r.BlockedReason = "rdma_cm default_roce_tos exists but is not regular file"
			return r, StateBlocked
		}
	}

	hasArtifact := exists(p.Subsystem) || exists(p.Namespace) || exists(p.PortLink)

	if runtimeMatches(c, p) {
		return r, StateActive
	}
	if !hasArtifact {
		return r, StateInactive
	}
	return r, StateDirty
}

func start(c Config, p Paths) error {
	if err := prepare(c); err != nil {
		return err
	}

	r, st := observe(c, p)
	switch st {
	case StateActive:
		fmt.Println("active")
		return nil
	case StateBlocked:
		return fmt.Errorf("blocked: %s", r.BlockedReason)
	}

	if err := stopArtifacts(p); err != nil {
		return err
	}
	if err := createTarget(c, p); err != nil {
		return err
	}

	fmt.Println("started")
	return nil
}

func stop(c Config, p Paths) error {
	if err := prepare(c); err != nil {
		return err
	}

	r, st := observe(c, p)
	if st == StateInactive {
		fmt.Println("inactive")
		return nil
	}
	if st == StateBlocked {
		return fmt.Errorf("blocked: %s", r.BlockedReason)
	}

	if err := stopArtifacts(p); err != nil {
		return err
	}

	fmt.Println("stopped")
	return nil
}

func status(c Config, p Paths) error {
	if err := prepare(c); err != nil {
		return err
	}

	r, st := observe(c, p)
	if st == StateBlocked {
		fmt.Printf("blocked: %s\n", r.BlockedReason)
		return errStatusNonZero
	}

	fmt.Println(st)
	if st != StateActive {
		return errStatusNonZero
	}
	return nil
}

func runtimeMatches(c Config, p Paths) bool {
	if !isDir(p.Subsystem) || !isDir(p.Namespace) || !isDir(p.Port) {
		return false
	}
	if !isSymlink(p.PortLink) || !sameSymlinkTarget(p.PortLink, p.Subsystem) {
		return false
	}
	if readAttr(filepath.Join(p.Namespace, "enable")) != "1" {
		return false
	}
	if readAttr(filepath.Join(p.Namespace, "device_path")) != c.BackingDev {
		return false
	}
	if readAttr(filepath.Join(p.Port, "addr_trtype")) != c.Transport {
		return false
	}
	if readAttr(filepath.Join(p.Port, "addr_adrfam")) != c.AddressFamily {
		return false
	}
	if normalizeIP(readAttr(filepath.Join(p.Port, "addr_traddr"))) != c.Address {
		return false
	}
	if readAttr(filepath.Join(p.Port, "addr_trsvcid")) != strconv.Itoa(c.ServiceID) {
		return false
	}
	if c.QoSEnabled {
		if readAttr(p.RDMADefaultTOS) != strconv.Itoa(c.QoSRoCETOS) {
			return false
		}
	}
	return hostsMatch(c, p)
}

func createTarget(c Config, p Paths) error {
	if err := configureQoS(c, p); err != nil {
		return err
	}

	if err := os.MkdirAll(p.Subsystem, 0700); err != nil {
		return fmt.Errorf("mkdir subsystem: %w", err)
	}
	if err := configureHosts(c, p); err != nil {
		return err
	}

	if err := os.MkdirAll(p.Namespace, 0700); err != nil {
		return fmt.Errorf("mkdir namespace: %w", err)
	}
	if err := writeAttr(filepath.Join(p.Namespace, "device_path"), c.BackingDev); err != nil {
		return err
	}
	if err := writeAttr(filepath.Join(p.Namespace, "enable"), "1"); err != nil {
		return err
	}

	if err := os.MkdirAll(p.PortSubsystems, 0700); err != nil {
		return fmt.Errorf("mkdir port: %w", err)
	}
	if err := writeAttr(filepath.Join(p.Port, "addr_trtype"), c.Transport); err != nil {
		return err
	}
	if err := writeAttr(filepath.Join(p.Port, "addr_adrfam"), c.AddressFamily); err != nil {
		return err
	}
	if err := writeAttr(filepath.Join(p.Port, "addr_traddr"), c.Address); err != nil {
		return err
	}
	if err := writeAttr(filepath.Join(p.Port, "addr_trsvcid"), strconv.Itoa(c.ServiceID)); err != nil {
		return err
	}

	return replaceSymlink(p.PortLink, p.Subsystem)
}

func configureQoS(c Config, p Paths) error {
	if !c.QoSEnabled {
		return nil
	}
	if err := os.MkdirAll(p.RDMACMDevice, 0700); err != nil {
		return fmt.Errorf("mkdir rdma_cm device: %w", err)
	}
	if !waitForDir(p.RDMACMPort, attrWriteTimeout) {
		return fmt.Errorf("rdma_cm port path is not available: %s", p.RDMACMPort)
	}
	return writeAttr(p.RDMADefaultTOS, strconv.Itoa(c.QoSRoCETOS))
}

func stopArtifacts(p Paths) error {
	if existsNotDir(p.Subsystems) {
		return errors.New("blocked: subsystems path exists but is not directory")
	}
	if existsNotDir(p.Subsystem) {
		return errors.New("blocked: subsystem path exists but is not directory")
	}
	if existsNotDir(p.Namespaces) {
		return errors.New("blocked: namespaces path exists but is not directory")
	}
	if existsNotDir(p.Namespace) {
		return errors.New("blocked: namespace path exists but is not directory")
	}
	if existsNotDir(p.Ports) {
		return errors.New("blocked: ports path exists but is not directory")
	}
	if existsNotDir(p.Port) {
		return errors.New("blocked: port path exists but is not directory")
	}
	if existsNotDir(p.PortSubsystems) {
		return errors.New("blocked: port subsystems path exists but is not directory")
	}
	if existsNotDir(p.AllowedHosts) {
		return errors.New("blocked: allowed_hosts path exists but is not directory")
	}

	if exists(p.PortLink) {
		if !isSymlink(p.PortLink) {
			return fmt.Errorf("refusing to remove non-symlink: %s", p.PortLink)
		}
		if err := os.Remove(p.PortLink); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	enablePath := filepath.Join(p.Namespace, "enable")
	if exists(enablePath) {
		_ = writeAttr(enablePath, "0")
	}
	if err := removeDirIfExists(p.Namespace); err != nil {
		return err
	}
	if err := removeAllowedHostLinks(p); err != nil {
		return err
	}
	if err := removeDirIfExists(p.Subsystem); err != nil {
		return err
	}
	_ = removeDirIfEmpty(p.Port)
	return nil
}

func configureHosts(c Config, p Paths) error {
	if err := os.MkdirAll(p.AllowedHosts, 0700); err != nil {
		return fmt.Errorf("mkdir allowed_hosts: %w", err)
	}
	if err := removeAllowedHostLinks(p); err != nil {
		return err
	}

	if c.AllowAnyHost {
		return writeAttr(filepath.Join(p.Subsystem, "attr_allow_any_host"), "1")
	}
	if err := writeAttr(filepath.Join(p.Subsystem, "attr_allow_any_host"), "0"); err != nil {
		return err
	}

	for _, host := range c.AllowedHosts {
		hostPath := filepath.Join(p.Hosts, host)
		linkPath := filepath.Join(p.AllowedHosts, host)

		if err := os.MkdirAll(hostPath, 0700); err != nil {
			return fmt.Errorf("mkdir host: %w", err)
		}
		if err := replaceSymlink(linkPath, hostPath); err != nil {
			return err
		}
	}
	return nil
}

func hostsMatch(c Config, p Paths) bool {
	allow := readAttr(filepath.Join(p.Subsystem, "attr_allow_any_host"))
	if !isDir(p.AllowedHosts) {
		return false
	}

	entries, err := os.ReadDir(p.AllowedHosts)
	if err != nil {
		return false
	}

	if c.AllowAnyHost {
		return allow == "1" && len(entries) == 0
	}
	if allow != "0" {
		return false
	}

	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		linkPath := filepath.Join(p.AllowedHosts, name)
		hostPath := filepath.Join(p.Hosts, name)
		if !isSymlink(linkPath) {
			return false
		}
		if !isDir(hostPath) {
			return false
		}
		if !sameSymlinkTarget(linkPath, hostPath) {
			return false
		}
		got = append(got, name)
	}
	want := append([]string(nil), c.AllowedHosts...)
	slices.Sort(got)
	slices.Sort(want)
	return slices.Equal(got, want)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func isDir(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir()
}

func isSymlink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

func isRegularFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

func existsNotDir(path string) bool {
	return exists(path) && !isDir(path)
}

func existsNotSymlink(path string) bool {
	return exists(path) && !isSymlink(path)
}

func existsNotRegularFile(path string) bool {
	return exists(path) && !isRegularFile(path)
}

func waitForDir(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if isDir(path) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func readAttr(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := strings.TrimSuffix(string(b), "\n")
	return strings.TrimSuffix(s, "\r")
}

func writeAttr(path, value string) error {
	deadline := time.Now().Add(attrWriteTimeout)
	for {
		err := os.WriteFile(path, []byte(value), 0600)
		if err == nil {
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) || time.Now().After(deadline) {
			return fmt.Errorf("write %s: %w", path, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func removeDirIfExists(path string) error {
	err := os.Remove(path)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return fmt.Errorf("remove %s: %w", path, err)
}

func removeDirIfEmpty(path string) error {
	err := os.Remove(path)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return err
}

func replaceSymlink(link, target string) error {
	if exists(link) {
		if !isSymlink(link) {
			return fmt.Errorf("refusing to replace non-symlink: %s", link)
		}
		if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove symlink: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(link), 0700); err != nil {
		return fmt.Errorf("mkdir symlink parent: %w", err)
	}
	if err := os.Symlink(target, link); err != nil {
		return fmt.Errorf("symlink %s -> %s: %w", link, target, err)
	}
	return nil
}

func sameSymlinkTarget(link, target string) bool {
	got, err := os.Readlink(link)
	if err != nil {
		return false
	}
	if got == target {
		return true
	}
	gotPath := got
	if !filepath.IsAbs(gotPath) {
		gotPath = filepath.Join(filepath.Dir(link), gotPath)
	}
	absGot, err1 := filepath.Abs(gotPath)
	absTarget, err2 := filepath.Abs(target)
	return err1 == nil && err2 == nil && absGot == absTarget
}

func removeAllowedHostLinks(p Paths) error {
	dir := p.AllowedHosts
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read allowed_hosts: %w", err)
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if !isSymlink(path) {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove allowed host link: %w", err)
		}
	}
	return nil
}

func normalizeIP(s string) string {
	ip, err := netip.ParseAddr(s)
	if err != nil || ip.Zone() != "" {
		return s
	}
	return ip.Unmap().String()
}

func rejectOuterWhitespace(field, value string) error {
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("%s must not contain leading or trailing whitespace", field)
	}
	return nil
}

func validConfigfsName(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		if r == '/' || r == 0 || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func hasNQNPrefix(s string) bool {
	return strings.HasPrefix(s, "nqn.") || strings.HasPrefix(s, "eui.")
}

func runCommandQuiet(name string, args ...string) error {
	path, err := exec.LookPath(name)
	if err != nil {
		return fmt.Errorf("%s not found in PATH", name)
	}
	cmd := exec.Command(path, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s failed: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func acquireLock() (func(), error) {
	deadline := time.Now().Add(lockWaitTimeout)
	for {
		err := os.Mkdir(lockFilePath, 0700)
		if err == nil {
			return func() {
				_ = os.Remove(lockFilePath)
			}, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("lock: %w", err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("lock: %w", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
