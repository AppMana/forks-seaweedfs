package memlimit

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// Available describes the memory this process may use, resolved the way the
// JVM's cgroup support does it.
type Available struct {
	// Bytes is min(effective cgroup memory limit, physical RAM); with no
	// cgroup limit it is physical RAM. Zero when neither could be read.
	Bytes int64
	// CgroupLimit is the smallest memory limit on the process's cgroup and
	// its ancestors, 0 when none applies.
	CgroupLimit int64
	// PhysicalBytes is MemTotal from /proc/meminfo.
	PhysicalBytes int64
	// CgroupDir is the resolved cgroup directory of the process, empty when
	// no memory controller was found.
	CgroupDir string
}

// AvailableMemory resolves the memory available to the process under root,
// "/" in production, using only /proc and cgroup v1/v2 files (Linux 4.4 and
// later):
//
//  1. /proc/self/cgroup names the process's cgroup: the v1 line whose
//     controller list includes "memory", else the v2 "0::" line. A hybrid
//     host with a mounted v1 memory controller uses v1, as the JVM does.
//  2. /proc/self/mountinfo gives that hierarchy's mount (fstype "cgroup" with
//     the "memory" super option, or "cgroup2"). The directory is the
//     mountpoint joined with the cgroup path, minus the mount's root when the
//     path lies under it. That covers a private cgroup namespace ("0::/"), a
//     host namespace with mount root "/", and a bind-mounted subtree.
//  3. The effective limit is the minimum of memory.limit_in_bytes (v1) or
//     memory.max (v2) over that directory and every ancestor up to the
//     mountpoint; a limit set on a parent slice applies to the process even
//     when its own cgroup is unlimited. Missing files are skipped.
//  4. The result is min(limit, MemTotal), or MemTotal without a limit.
func AvailableMemory(root string) (Available, error) {
	var avail Available
	physical, err := readMemTotal(root)
	if err != nil {
		return avail, err
	}
	avail.PhysicalBytes = physical

	dir, v2, err := resolveMemoryCgroupDir(root)
	if err != nil {
		return avail, err
	}
	if dir.dir != "" {
		avail.CgroupDir = dir.dir
		avail.CgroupLimit, err = minLimitUpTo(root, dir, v2)
		if err != nil {
			return avail, err
		}
	}

	avail.Bytes = physical
	if avail.CgroupLimit > 0 && (avail.Bytes == 0 || avail.CgroupLimit < avail.Bytes) {
		avail.Bytes = avail.CgroupLimit
	}
	return avail, nil
}

type cgroupDir struct {
	dir        string // absolute path of the process's cgroup directory
	mountPoint string // absolute path of the hierarchy's mountpoint
}

// resolveMemoryCgroupDir returns the process's memory cgroup directory and
// whether it is a cgroup v2 hierarchy. A zero cgroupDir means no memory
// controller is mounted.
func resolveMemoryCgroupDir(root string) (cgroupDir, bool, error) {
	v1Path, v1Found, v2Path, v2Found, err := readProcSelfCgroup(root)
	if err != nil {
		return cgroupDir{}, false, err
	}
	mounts, err := readMountinfo(root)
	if err != nil {
		return cgroupDir{}, false, err
	}
	if v1Found {
		for _, m := range mounts {
			if m.fsType == "cgroup" && hasOption(m.superOptions, "memory") {
				return cgroupDir{dir: joinCgroupPath(m, v1Path), mountPoint: m.mountPoint}, false, nil
			}
		}
	}
	if v2Found {
		for _, m := range mounts {
			if m.fsType == "cgroup2" {
				return cgroupDir{dir: joinCgroupPath(m, v2Path), mountPoint: m.mountPoint}, true, nil
			}
		}
	}
	return cgroupDir{}, false, nil
}

// joinCgroupPath maps a cgroup path from /proc/self/cgroup to a directory
// under the hierarchy's mountpoint.
func joinCgroupPath(m mountInfo, cgroupPath string) string {
	cgroupPath = path.Clean("/" + cgroupPath)
	rel := cgroupPath
	switch {
	case m.root == "/":
	case cgroupPath == m.root:
		rel = "/"
	case strings.HasPrefix(cgroupPath, m.root+"/"):
		rel = strings.TrimPrefix(cgroupPath, m.root)
	default:
		// The cgroup lies outside the mounted subtree, so only the mounted
		// root is visible; the JVM uses the mountpoint in this case too.
		rel = "/"
	}
	return path.Join(m.mountPoint, rel)
}

// minLimitUpTo returns the smallest limit from dir up to its mountpoint.
func minLimitUpTo(root string, d cgroupDir, v2 bool) (int64, error) {
	file := "memory.limit_in_bytes"
	if v2 {
		file = "memory.max"
	}
	var limit int64
	for dir := d.dir; ; dir = path.Dir(dir) {
		l, ok, err := readLimit(filepath.Join(root, dir, file), v2)
		if err != nil {
			return 0, err
		}
		if ok && (limit == 0 || l < limit) {
			limit = l
		}
		if dir == d.mountPoint || dir == "/" || !strings.HasPrefix(dir, d.mountPoint) {
			return limit, nil
		}
	}
}

func readLimit(file string, v2 bool) (int64, bool, error) {
	raw, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read %s: %w", file, err)
	}
	value := strings.TrimSpace(string(raw))
	if v2 && value == "max" {
		return 0, false, nil
	}
	limit, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("parse %s %q: %w", file, value, err)
	}
	if limit <= 0 || limit >= cgroupV1Unlimited {
		return 0, false, nil
	}
	return limit, true, nil
}

// readProcSelfCgroup returns the v1 memory controller's cgroup path and the
// v2 unified path from /proc/self/cgroup.
func readProcSelfCgroup(root string) (v1Path string, v1Found bool, v2Path string, v2Found bool, err error) {
	f, err := os.Open(filepath.Join(root, "proc/self/cgroup"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, "", false, nil
	}
	if err != nil {
		return "", false, "", false, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		// hierarchy-ID:controller-list:cgroup-path
		fields := strings.SplitN(scanner.Text(), ":", 3)
		if len(fields) != 3 {
			continue
		}
		if fields[0] == "0" && fields[1] == "" {
			v2Path, v2Found = fields[2], true
			continue
		}
		for _, controller := range strings.Split(fields[1], ",") {
			if controller == "memory" {
				v1Path, v1Found = fields[2], true
			}
		}
	}
	return v1Path, v1Found, v2Path, v2Found, scanner.Err()
}

type mountInfo struct {
	root         string
	mountPoint   string
	fsType       string
	superOptions string
}

// readMountinfo parses /proc/self/mountinfo (proc(5)): fields 4 and 5 are
// the mount's root and mountpoint, then optional fields up to "-", then the
// filesystem type, source and super options.
func readMountinfo(root string) ([]mountInfo, error) {
	f, err := os.Open(filepath.Join(root, "proc/self/mountinfo"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var mounts []mountInfo
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		sep := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				sep = i
				break
			}
		}
		if len(fields) < 5 || sep < 0 || sep+3 > len(fields) {
			continue
		}
		m := mountInfo{
			root:       unescapeMountPath(fields[3]),
			mountPoint: unescapeMountPath(fields[4]),
			fsType:     fields[sep+1],
		}
		if sep+3 < len(fields) {
			m.superOptions = fields[sep+3]
		}
		mounts = append(mounts, m)
	}
	return mounts, scanner.Err()
}

// unescapeMountPath undoes mountinfo's octal escapes (\040 for a space, etc.).
func unescapeMountPath(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func hasOption(options, want string) bool {
	for _, o := range strings.Split(options, ",") {
		if o == want {
			return true
		}
	}
	return false
}

// readMemTotal returns MemTotal from /proc/meminfo in bytes, 0 if absent.
func readMemTotal(root string) (int64, error) {
	f, err := os.Open(filepath.Join(root, "proc/meminfo"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kib, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parse MemTotal %q: %w", fields[1], err)
			}
			return kib * 1024, nil
		}
	}
	return 0, scanner.Err()
}
