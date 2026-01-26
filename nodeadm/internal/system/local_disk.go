package system

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/awslabs/amazon-eks-ami/nodeadm/internal/api"
	"go.uber.org/zap"
)

const (
	defaultMountDir = "/mnt/k8s-disks"
	mdConfigPath    = "/.aws/mdadm.conf"
	mdName          = "kubernetes"
)

func NewLocalDiskAspect() SystemAspect {
	return &localDiskAspect{}
}

type localDiskAspect struct{}

func (a *localDiskAspect) Name() string {
	return "local-disk"
}

func (a *localDiskAspect) Setup(cfg *api.NodeConfig) error {
	strategy := cfg.Spec.Instance.LocalStorage.Strategy
	if strategy == "" {
		zap.L().Info("Not configuring local disks!")
		return nil
	}

	if strategy != api.LocalStorageRAID0 && strategy != api.LocalStorageRAID10 && strategy != api.LocalStorageMount {
		return fmt.Errorf("invalid LocalStorage strategy: %s", strategy)
	}

	disks, err := findEphemeralDisks()
	if err != nil {
		return fmt.Errorf("finding ephemeral disks: %w", err)
	}
	if len(disks) == 0 {
		zap.L().Info("no NVMe instance storage disks found!")
		return nil
	}

	if strategy == api.LocalStorageRAID10 && len(disks) < 4 {
		return fmt.Errorf("RAID10 requires at least 4 disks, but only %d found", len(disks))
	}

	switch strategy {
	case api.LocalStorageRAID0:
		if err := setupRaid(0, disks, cfg); err != nil {
			return fmt.Errorf("setting up RAID0: %w", err)
		}
		zap.L().Info("Successfully setup RAID0", zap.Strings("disks", disks))
	case api.LocalStorageRAID10:
		if err := setupRaid(10, disks, cfg); err != nil {
			return fmt.Errorf("setting up RAID10: %w", err)
		}
		zap.L().Info("Successfully setup RAID10", zap.Strings("disks", disks))
	case api.LocalStorageMount:
		if err := setupMount(disks, cfg); err != nil {
			return fmt.Errorf("setting up disk mounts: %w", err)
		}
		zap.L().Info("Successfully setup disk mounts", zap.Strings("disks", disks))
	}
	return nil
}

func findEphemeralDisks() ([]string, error) {
	byIDPath := "/dev/disk/by-id/"
	if _, err := os.Stat(byIDPath); os.IsNotExist(err) {
		zap.L().Info("no ephemeral disks found!")
		return nil, nil
	}

	entries, err := os.ReadDir(byIDPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", byIDPath, err)
	}

	diskSet := make(map[string]struct{})
	for _, entry := range entries {
		if !strings.Contains(entry.Name(), "NVMe_Instance_Storage_") {
			continue
		}
		linkPath := filepath.Join(byIDPath, entry.Name())
		realPath, err := filepath.EvalSymlinks(linkPath)
		if err != nil {
			continue
		}
		diskSet[realPath] = struct{}{}
	}

	return slices.Sorted(maps.Keys(diskSet)), nil
}

func getMountDir(cfg *api.NodeConfig) string {
	if cfg.Spec.Instance.LocalStorage.MountPath != "" {
		return cfg.Spec.Instance.LocalStorage.MountPath
	}
	return defaultMountDir
}

func setupRaid(level int, disks []string, cfg *api.NodeConfig) error {
	mdDevice := fmt.Sprintf("/dev/md/%s", mdName)
	arrayMountPoint := filepath.Join(getMountDir(cfg), "0")

	if err := os.MkdirAll(filepath.Dir(mdConfigPath), 0750); err != nil {
		return fmt.Errorf("creating mdadm config dir: %w", err)
	}

	if info, err := os.Stat(mdConfigPath); err != nil || info.Size() == 0 {
		args := []string{"--create", "--force", "--verbose", mdDevice,
			fmt.Sprintf("--level=%d", level), fmt.Sprintf("--name=%s", mdName),
			fmt.Sprintf("--raid-devices=%d", len(disks))}
		args = append(args, disks...)
		if err := runCmd("mdadm", args...); err != nil {
			return fmt.Errorf("creating RAID array: %w", err)
		}
		out, err := exec.Command("mdadm", "--detail", "--scan").Output()
		if err != nil {
			return fmt.Errorf("scanning RAID array: %w", err)
		}
		if err := os.WriteFile(mdConfigPath, out, 0600); err != nil {
			return fmt.Errorf("writing mdadm config: %w", err)
		}
	}

	if entries, err := filepath.Glob(fmt.Sprintf("/dev/md/%s*", mdName)); err == nil && len(entries) > 0 {
		sort.Strings(entries)
		mdDevice = entries[len(entries)-1]
	}

	if !isFormatted(mdDevice) {
		if err := runCmd("mkfs.xfs", "-K", "-l", "su=8b", mdDevice); err != nil {
			return fmt.Errorf("formatting %s: %w", mdDevice, err)
		}
	}

	if err := os.MkdirAll(arrayMountPoint, 0750); err != nil {
		return fmt.Errorf("creating mount point %s: %w", arrayMountPoint, err)
	}

	uuid, err := getBlkUUID(mdDevice)
	if err != nil {
		return fmt.Errorf("getting UUID for %s: %w", mdDevice, err)
	}

	mountUnit := systemdEscapePath(arrayMountPoint) + ".mount"
	unitContent := fmt.Sprintf(`[Unit]
Description=Mount EC2 Instance Store NVMe disk RAID%d
[Mount]
What=UUID=%s
Where=%s
Type=xfs
Options=defaults,noatime
[Install]
WantedBy=multi-user.target
`, level, uuid, arrayMountPoint)

	if err := writeAndEnableUnit(mountUnit, unitContent); err != nil {
		return fmt.Errorf("enabling mount unit %s: %w", mountUnit, err)
	}

	if err := setupBindMounts(level, arrayMountPoint, cfg); err != nil {
		return fmt.Errorf("setting up bind mounts: %w", err)
	}
	return nil
}

type bindMount struct {
	path          string
	dependentUnit string
}

func (bm *bindMount) mountUnit() string {
	return systemdEscapePath(bm.path) + ".mount"
}

func setupBindMounts(raidLevel int, arrayMountPoint string, cfg *api.NodeConfig) error {
	var bindMounts []bindMount
	disabled := cfg.Spec.Instance.LocalStorage.DisabledMounts
	if !slices.Contains(disabled, api.DisabledMountContainerd) {
		bindMounts = append(bindMounts, bindMount{"/var/lib/containerd", "containerd.service"})
	}
	if !slices.Contains(disabled, api.DisabledMountPodLogs) {
		bindMounts = append(bindMounts, bindMount{"/var/log/pods", "kubelet.service"})
	}
	if !slices.Contains(disabled, api.DisabledMountSOCI) {
		bindMounts = append(bindMounts, bindMount{"/var/lib/soci-snapshotter-grpc", "soci-snapshotter.service"})
	}
	// Kubelet is always bound (no DisabledMount constant for it)
	bindMounts = append(bindMounts, bindMount{"/var/lib/kubelet", "kubelet.service"})

	var prevRunning []string
	var needsLinking []bindMount
	for _, bm := range bindMounts {
		if isUnitActive(bm.mountUnit()) {
			continue
		}
		needsLinking = append(needsLinking, bm)
		if isUnitActive(bm.dependentUnit) {
			prevRunning = append(prevRunning, bm.dependentUnit)
		}

	}

	if len(prevRunning) > 0 {
		args := []string{"stop"}
		args = append(args, prevRunning...)
		runCmd("systemctl", args...)
	}

	for _, bm := range needsLinking {
		dir := filepath.Base(bm.path)
		arrayUnit := filepath.Join(arrayMountPoint, dir)
		if err := os.MkdirAll(bm.path, 0755); err != nil {
			return fmt.Errorf("creating directory %s: %w", bm.path, err)
		}
		zap.L().Info("Copying directory", zap.String("from", bm.path), zap.String("to", arrayUnit))
		if err := runCmd("cp", "-a", bm.path+"/", arrayUnit+"/"); err != nil {
			return fmt.Errorf("copying %s to %s: %w", bm.path, arrayUnit, err)
		}

		mountUnit := bm.mountUnit()
		unitContent := fmt.Sprintf(`[Unit]
Description=Mount %s on EC2 Instance Store NVMe RAID%d
[Mount]
What=%s
Where=%s
Type=none
Options=bind
[Install]
WantedBy=multi-user.target
`, bm.path, raidLevel, arrayUnit, bm.path)

		if err := writeAndEnableUnit(mountUnit, unitContent); err != nil {
			return fmt.Errorf("enabling bind mount unit %s: %w", mountUnit, err)
		}
	}

	if len(prevRunning) > 0 {
		args := []string{"start"}
		args = append(args, prevRunning...)
		runCmd("systemctl", args...)
	}
	return nil
}

func setupMount(disks []string, cfg *api.NodeConfig) error {
	mountDir := getMountDir(cfg)
	for idx, dev := range disks {
		if !isFormatted(dev) {
			if err := runCmd("mkfs.xfs", "-l", "su=8b", dev); err != nil {
				return fmt.Errorf("formatting %s: %w", dev, err)
			}
		}

		if isMounted(dev) {
			zap.L().Info("Device already mounted", zap.String("device", dev))
			continue
		}

		mp := filepath.Join(mountDir, fmt.Sprintf("%d", idx+1))
		if err := os.MkdirAll(mp, 0755); err != nil {
			return fmt.Errorf("creating mount point %s: %w", mp, err)
		}

		mountUnit := systemdEscapePath(mp)
		unitContent := fmt.Sprintf(`[Unit]
Description=Mount EC2 Instance Store NVMe disk %d
[Mount]
What=%s
Where=%s
Type=xfs
Options=defaults,noatime
[Install]
WantedBy=multi-user.target
`, idx+1, dev, mp)

		if err := writeAndEnableUnit(mountUnit+".mount", unitContent); err != nil {
			return fmt.Errorf("enabling mount unit for %s: %w", dev, err)
		}
	}
	return nil
}

func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func isFormatted(dev string) bool {
	out, err := exec.Command("lsblk", dev, "-o", "fstype", "--noheadings").Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

func isMounted(dev string) bool {
	out, err := exec.Command("lsblk", dev, "-o", "MOUNTPOINT", "--noheadings").Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

func getBlkUUID(dev string) (string, error) {
	out, err := exec.Command("blkid", "-s", "UUID", "-o", "value", dev).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func systemdEscapePath(path string) string {
	// Handle root path
	if path == "/" {
		return "-"
	}
	// Remove leading, trailing, and collapse duplicate slashes
	path = filepath.Clean(path)
	path = strings.Trim(path, "/")
	if path == "" {
		return "-"
	}

	var result strings.Builder
	for i, c := range path {
		switch {
		case c == '/':
			result.WriteByte('-')
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == ':' || c == '_':
			result.WriteRune(c)
		case c == '.' && i != 0:
			result.WriteByte('.')
		default:
			fmt.Fprintf(&result, "\\x%02x", c)
		}
	}
	return result.String()
}

func isUnitActive(unit string) bool {
	err := exec.Command("systemctl", "is-active", unit).Run()
	return err == nil
}

func writeAndEnableUnit(unitName, content string) error {
	unitPath := filepath.Join("/etc/systemd/system", unitName)
	if err := os.WriteFile(unitPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("writing unit file: %w", err)
	}
	if err := runCmd("systemd-analyze", "verify", unitName); err != nil {
		return fmt.Errorf("verifying unit: %w", err)
	}
	if err := runCmd("systemctl", "enable", unitName, "--now"); err != nil {
		return fmt.Errorf("enabling unit: %w", err)
	}
	return nil
}
