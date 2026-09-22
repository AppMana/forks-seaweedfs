package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	labv1 "github.com/appmana/labcontainers/api/v1"
	"github.com/appmana/labcontainers/pkg/client"
)

const (
	controller = "controller"
	masterIP   = "192.0.2.10"
)

var volumes = []string{"volume1", "volume2", "volume3"}
var addresses = map[string]string{
	controller: "192.0.2.10",
	"volume1":  "192.0.2.11",
	"volume2":  "192.0.2.12",
	"volume3":  "192.0.2.13",
}

type config struct {
	candidate  string
	baseline   string
	labd       string
	image      string
	filesystem string
	scenario   string
	results    string
	keep       bool
}

type harness struct {
	cfg       config
	ctx       context.Context
	client    *client.Client
	lab       *client.Session
	candidate []byte
	baseline  []byte
	manifest  map[string]any
}

func main() {
	cfg := parseFlags()
	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "VM storage lab failed:", err)
		os.Exit(1)
	}
}

func parseFlags() config {
	var cfg config
	flag.StringVar(&cfg.candidate, "candidate", "", "absolute path to candidate weed binary")
	flag.StringVar(&cfg.baseline, "baseline", "", "absolute path to baseline weed binary (required for migration)")
	flag.StringVar(&cfg.labd, "labd", "labd", "path to the labd binary")
	flag.StringVar(&cfg.image, "image", "labcontainers/vm-ubuntu:jammy", "qualified Labcontainers Ubuntu VM image")
	flag.StringVar(&cfg.filesystem, "filesystem", "ext4", "attached volume filesystem: ext4, xfs, or btrfs")
	flag.StringVar(&cfg.scenario, "scenario", "all", "replicated-vacuum, power-loss, migration, vacuum-power-loss, or all")
	flag.StringVar(&cfg.results, "results", "", "result directory (default: a new /tmp directory)")
	flag.BoolVar(&cfg.keep, "keep", false, "retain the failed Labcontainers session for bounded debugging")
	flag.Parse()
	return cfg
}

func run(cfg config) (runErr error) {
	if cfg.candidate == "" {
		return errors.New("--candidate is required")
	}
	for _, value := range []string{cfg.candidate, cfg.labd} {
		if !filepath.IsAbs(value) {
			return fmt.Errorf("path must be absolute: %s", value)
		}
	}
	if cfg.filesystem != "ext4" && cfg.filesystem != "xfs" && cfg.filesystem != "btrfs" {
		return fmt.Errorf("unsupported filesystem %q", cfg.filesystem)
	}
	validScenario := map[string]bool{"replicated-vacuum": true, "power-loss": true, "migration": true, "vacuum-power-loss": true, "all": true}
	if !validScenario[cfg.scenario] {
		return fmt.Errorf("unsupported scenario %q", cfg.scenario)
	}
	if (cfg.scenario == "migration" || cfg.scenario == "all") && cfg.baseline == "" {
		return errors.New("--baseline is required for migration/all")
	}
	if cfg.baseline != "" && !filepath.IsAbs(cfg.baseline) {
		return errors.New("--baseline must be absolute")
	}
	if cfg.results == "" {
		var err error
		cfg.results, err = os.MkdirTemp("/tmp", "seaweedfs-vm-lab-results-")
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(cfg.results, 0o700); err != nil {
		return err
	}
	fmt.Println("Results:", cfg.results)

	candidate, err := checkedArtifact(cfg.candidate)
	if err != nil {
		return err
	}
	var baseline []byte
	if cfg.baseline != "" {
		baseline, err = checkedArtifact(cfg.baseline)
		if err != nil {
			return err
		}
		if sha(candidate) == sha(baseline) {
			return errors.New("baseline and candidate are byte-identical")
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
	defer cancel()
	imageID, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", cfg.image).Output()
	if err != nil {
		return fmt.Errorf("resolve VM image identity: %w", err)
	}
	c, err := client.Launch(ctx, client.Options{LabdPath: cfg.labd})
	if err != nil {
		return err
	}
	h := &harness{cfg: cfg, ctx: ctx, client: c, candidate: candidate, baseline: baseline}
	h.manifest = map[string]any{
		"scenario": cfg.scenario, "filesystem": cfg.filesystem,
		"vm_image": cfg.image, "vm_image_id": strings.TrimSpace(string(imageID)),
		"candidate_sha256": sha(candidate), "baseline_sha256": sha(baseline),
		"topology":          "one controller VM and three rack-separated volume VMs",
		"production_access": false, "started": time.Now().UTC(), "status": "running",
	}
	defer func() {
		if runErr != nil && cfg.keep && h.lab != nil {
			if err := h.lab.Keep(context.Background(), 2*time.Hour); err != nil {
				runErr = errors.Join(runErr, fmt.Errorf("keep failed session: %w", err))
			}
		}
		if err := c.Close(); err != nil {
			runErr = errors.Join(runErr, err)
		}
		if runErr == nil {
			h.manifest["status"] = "passed"
		} else {
			h.manifest["status"] = "failed"
			h.manifest["error"] = runErr.Error()
		}
		h.manifest["finished"] = time.Now().UTC()
		data, _ := json.MarshalIndent(h.manifest, "", "  ")
		_ = os.WriteFile(filepath.Join(cfg.results, "manifest.json"), append(data, '\n'), 0o600)
	}()

	networkDir, err := os.MkdirTemp("", "seaweedfs-vm-net-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(networkDir)
	if err := writeNetworkConfigs(networkDir); err != nil {
		return err
	}
	topology := topologyYAML(networkDir, cfg.image)
	if err := os.WriteFile(filepath.Join(cfg.results, "topology.clab.yml"), topology, 0o600); err != nil {
		return err
	}
	nodes := map[string]*labv1.NodeExtension{controller: {Control: "qga"}}
	for _, name := range volumes {
		nodes[name] = &labv1.NodeExtension{Control: "qga", Disks: []*labv1.Disk{{Name: "volume", SizeBytes: 2 << 30}}}
	}
	h.lab, err = c.Start(ctx, &labv1.LabSpec{
		Topology:          &labv1.TopologySource{Source: &labv1.TopologySource_Yaml{Yaml: topology}},
		Nodes:             nodes,
		ArtifactDirectory: cfg.results,
	}, 40*time.Minute)
	if err != nil {
		return err
	}
	h.manifest["session_id"] = h.lab.ID()

	if err := h.execOK("switch", "sh", "-ec", "ip link add br0 type bridge; for i in eth1 eth2 eth3 eth4; do ip link set $i master br0; ip link set $i up; done; ip link set br0 up"); err != nil {
		return fmt.Errorf("configure isolated switch: %w", err)
	}
	for _, name := range append([]string{controller}, volumes...) {
		if err := h.waitOK(name, 3*time.Minute, "sh", "-ec", "cloud-init status --wait >/dev/null; ip -4 address show | grep -q '192.0.2.'"); err != nil {
			return fmt.Errorf("%s readiness: %w", name, err)
		}
	}
	if err := h.install(controller, candidate); err != nil {
		return err
	}
	if err := h.startMaster(); err != nil {
		return err
	}
	initial := candidate
	if len(baseline) != 0 {
		initial = baseline
	}
	for _, name := range volumes {
		if err := h.provisionVolume(name, initial, true); err != nil {
			return err
		}
	}

	initialOverwrites := 24
	if cfg.scenario == "vacuum-power-loss" {
		initialOverwrites = 4
	}
	fid, err := h.seedAndOverwrite(initialOverwrites)
	if err != nil {
		return err
	}
	if cfg.scenario == "migration" || cfg.scenario == "all" {
		if err := h.rollingMigration(fid); err != nil {
			return err
		}
	}
	if cfg.scenario == "replicated-vacuum" || cfg.scenario == "all" {
		if err := h.concurrentVacuum(fid); err != nil {
			return err
		}
	}
	if cfg.scenario == "power-loss" || cfg.scenario == "all" {
		if err := h.powerLoss(fid, "volume1"); err != nil {
			return err
		}
	}
	if cfg.scenario == "vacuum-power-loss" || cfg.scenario == "all" {
		if err := h.vacuumPowerLoss(fid, "volume2"); err != nil {
			return err
		}
	}
	return nil
}

func checkedArtifact(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 || info.Size() > 256<<20 {
		return nil, fmt.Errorf("artifact must be an executable regular file no larger than 256 MiB: %s", path)
	}
	return os.ReadFile(path)
}

func sha(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func writeNetworkConfigs(dir string) error {
	if err := os.Chmod(dir, 0o755); err != nil {
		return err
	}
	for name, address := range addresses {
		text := fmt.Sprintf("version: 2\nethernets:\n  topology:\n    match:\n      name: 'en*'\n    addresses: [%s/24]\n    optional: true\n", address)
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(text), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func topologyYAML(networkDir, image string) []byte {
	var b strings.Builder
	b.WriteString("name: ignored\ntopology:\n  nodes:\n")
	for _, name := range append([]string{controller}, volumes...) {
		fmt.Fprintf(&b, "    %s:\n      kind: generic_vm\n      image: %s\n      network-mode: none\n      binds:\n        - %s\n", name, strconv.Quote(image), strconv.Quote(filepath.Join(networkDir, name+".yaml")+":/extra-network.yaml:ro"))
	}
	b.WriteString("    switch:\n      kind: linux\n      image: alpine:3.20\n      network-mode: none\n  links:\n")
	for i, name := range append([]string{controller}, volumes...) {
		fmt.Fprintf(&b, "    - endpoints: [%s:eth1, switch:eth%d]\n", name, i+1)
	}
	return []byte(b.String())
}

func (h *harness) execOK(node string, argv ...string) error {
	result, err := h.lab.Node(node).Exec(h.ctx, argv...)
	if err != nil {
		return err
	}
	if result.GetExitCode() != 0 {
		return fmt.Errorf("exit %d: %s%s", result.GetExitCode(), result.GetStdout(), result.GetStderr())
	}
	return nil
}

func (h *harness) output(node string, argv ...string) (string, error) {
	result, err := h.lab.Node(node).Exec(h.ctx, argv...)
	if err != nil {
		return "", err
	}
	if result.GetExitCode() != 0 {
		return "", fmt.Errorf("exit %d: %s%s", result.GetExitCode(), result.GetStdout(), result.GetStderr())
	}
	return strings.TrimSpace(string(result.GetStdout())), nil
}

func (h *harness) waitOK(node string, timeout time.Duration, argv ...string) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		last = h.execOK(node, argv...)
		if last == nil {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return last
}

func (h *harness) install(node string, binary []byte) error {
	if err := h.lab.Node(node).Put(h.ctx, "/opt/weed", 0o755, binary); err != nil {
		return fmt.Errorf("install weed on %s: %w", node, err)
	}
	return h.execOK(node, "/opt/weed", "version")
}

func (h *harness) startMaster() error {
	command := "mkdir -p /var/lib/weed/master /var/log/seaweedfs; nohup /opt/weed master -ip=" + masterIP + " -ip.bind=0.0.0.0 -port=9333 -mdir=/var/lib/weed/master -volumeSizeLimitMB=64 -defaultReplication=020 >/var/log/seaweedfs/master.log 2>&1 </dev/null &"
	if err := h.execOK(controller, "sh", "-ec", command); err != nil {
		return fmt.Errorf("start master: %w", err)
	}
	return h.waitOK(controller, 2*time.Minute, "python3", "-c", "import urllib.request; urllib.request.urlopen('http://192.0.2.10:9333/cluster/status', timeout=2).read()")
}

func (h *harness) provisionVolume(name string, binary []byte, format bool) error {
	if err := h.install(name, binary); err != nil {
		return err
	}
	device := "/dev/disk/by-id/virtio-lc-volume"
	var mkfs string
	switch h.cfg.filesystem {
	case "ext4":
		mkfs = "mkfs.ext4 -F"
	case "xfs":
		mkfs = "mkfs.xfs -f"
	case "btrfs":
		mkfs = "mkfs.btrfs -f"
	}
	setup := "test -b " + device + "; command -v " + strings.Fields(mkfs)[0] + " >/dev/null; "
	if format {
		setup += "blkid " + device + " >/dev/null 2>&1 || " + mkfs + " " + device + " >/dev/null; "
	}
	setup += "mkdir -p /mnt/volume /var/log/seaweedfs; mountpoint -q /mnt/volume || mount -t " + h.cfg.filesystem + " -o noatime " + device + " /mnt/volume"
	if err := h.waitOK(name, 3*time.Minute, "sh", "-ec", setup); err != nil {
		return fmt.Errorf("prepare %s disk on %s: %w", h.cfg.filesystem, name, err)
	}
	index := strings.TrimPrefix(name, "volume")
	ip := addresses[name]
	command := fmt.Sprintf("nohup /opt/weed volume -master=%s:9333 -ip=%s -ip.bind=0.0.0.0 -port=8080 -dir=/mnt/volume -max=8 -dataCenter=lab -rack=r%s -index=leveldb -compactionMBps=4 >/var/log/seaweedfs/volume.log 2>&1 </dev/null & echo $! >/run/weed-volume.pid", masterIP, ip, index)
	if err := h.execOK(name, "sh", "-ec", command); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	if err := h.waitOK(name, 2*time.Minute, "python3", "-c", "import urllib.request; urllib.request.urlopen('http://"+ip+":8080/status', timeout=2).read()"); err != nil {
		log, _ := h.output(name, "tail", "-100", "/var/log/seaweedfs/volume.log")
		_ = os.WriteFile(filepath.Join(h.cfg.results, name+"-startup.log"), []byte(log), 0o600)
		return fmt.Errorf("%s readiness: %w; volume log: %s", name, err, log)
	}
	return nil
}

func (h *harness) waitReplicas(fid string) error {
	command := "import json,urllib.request; d=json.load(urllib.request.urlopen('http://192.0.2.10:9333/dir/lookup?volumeId=" + volumeID(fid) + "',timeout=3)); assert len(d.get('locations',[]))==3,d"
	return h.waitOK(controller, 3*time.Minute, "python3", "-c", command)
}

const workload = `import hashlib,json,sys,time,urllib.request
MASTER='http://192.0.2.10:9333'
def payload(seq):
    head=('sequence:%08d\n'%seq).encode()
    return head + hashlib.sha256(head).digest() * 65536
def post(url,data):
    boundary='labcontainers-seaweedfs'
    body=('--'+boundary+'\r\nContent-Disposition: form-data; name="file"; filename="payload"\r\nContent-Type: application/octet-stream\r\n\r\n').encode()+data+('\r\n--'+boundary+'--\r\n').encode()
    req=urllib.request.Request(url+'?fsync=true',data=body,headers={'Content-Type':'multipart/form-data; boundary='+boundary})
    with urllib.request.urlopen(req,timeout=30) as r: return r.read()
cmd=sys.argv[1]
if cmd=='seed':
    a=json.load(urllib.request.urlopen(MASTER+'/dir/assign?replication=020',timeout=10)); post('http://'+a['url']+'/'+a['fid'],payload(0)); print(a['fid'])
elif cmd=='seedmany':
    n=int(sys.argv[2]); a=json.load(urllib.request.urlopen(MASTER+'/dir/assign?replication=020&count='+str(n),timeout=10))
    fids=[a['fid']]+[a['fid']+'_'+str(i) for i in range(1,n)]
    for fid in fids: post('http://'+a['url']+'/'+fid,payload(0))
    print(json.dumps(fids))
elif cmd=='write':
    post('http://'+sys.argv[3]+':8080/'+sys.argv[2],payload(int(sys.argv[4])))
elif cmd=='read':
    got=urllib.request.urlopen('http://'+sys.argv[3]+':8080/'+sys.argv[2],timeout=10).read(); want=payload(int(sys.argv[4])); assert got==want,(len(got),len(want)); print(hashlib.sha256(got).hexdigest())
elif cmd=='loop':
    fid,target,ledger,stop=sys.argv[2:6]
    seq=int(sys.argv[6])
    while not __import__('os').path.exists(stop):
        seq+=1; post('http://'+target+':8080/'+fid,payload(seq)); open(ledger,'w').write(str(seq)); __import__('os').sync(); time.sleep(.03)
`

func (h *harness) seedAndOverwrite(count int) (string, error) {
	if err := h.lab.Node(controller).Put(h.ctx, "/opt/workload.py", 0o755, []byte(workload)); err != nil {
		return "", err
	}
	var fid string
	var err error
	for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		fid, err = h.output(controller, "python3", "/opt/workload.py", "seed")
		if err == nil && fid != "" {
			break
		}
	}
	if err != nil || fid == "" {
		return "", fmt.Errorf("seed replicated volume: %w", err)
	}
	for seq := 1; seq <= count; seq++ {
		if err := h.execOK(controller, "python3", "/opt/workload.py", "write", fid, addresses["volume1"], strconv.Itoa(seq)); err != nil {
			return "", fmt.Errorf("overwrite %d: %w", seq, err)
		}
	}
	if err := h.verify(fid, count); err != nil {
		return "", err
	}
	h.manifest["file_id"] = fid
	return fid, nil
}

func (h *harness) verify(fid string, sequence int) error {
	for _, name := range volumes {
		digest, err := h.output(controller, "python3", "/opt/workload.py", "read", fid, addresses[name], strconv.Itoa(sequence))
		if err != nil {
			return fmt.Errorf("verify %s sequence %d: %w", name, sequence, err)
		}
		checks, _ := h.manifest["verified_reads"].([]map[string]any)
		h.manifest["verified_reads"] = append(checks, map[string]any{"file_id": fid, "sequence": sequence, "replica": name, "sha256": digest})
	}
	return nil
}

func volumeID(fid string) string {
	return strings.SplitN(fid, ",", 2)[0]
}

func (h *harness) runVacuum(fid string) error {
	commands := "lock\nvolume.vacuum -volumeId=" + volumeID(fid) + " -garbageThreshold=0\nunlock\n"
	if err := h.lab.Node(controller).Put(h.ctx, "/opt/vacuum.shell", 0o600, []byte(commands)); err != nil {
		return err
	}
	return h.execOK(controller, "sh", "-ec", "/opt/weed shell -master=192.0.2.10:9333 < /opt/vacuum.shell")
}

func (h *harness) concurrentVacuum(fid string) error {
	_ = h.execOK(controller, "rm", "-f", "/tmp/writer.stop", "/tmp/writer.ledger")
	command := "nohup python3 /opt/workload.py loop " + fid + " " + addresses["volume1"] + " /tmp/writer.ledger /tmp/writer.stop 24 >/tmp/writer.log 2>&1 </dev/null & echo $! >/tmp/writer.pid"
	if err := h.execOK(controller, "sh", "-ec", command); err != nil {
		return err
	}
	if err := h.waitOK(controller, time.Minute, "sh", "-ec", "test -s /tmp/writer.ledger"); err != nil {
		return err
	}
	if err := h.runVacuum(fid); err != nil {
		return fmt.Errorf("concurrent vacuum: %w", err)
	}
	if err := h.execOK(controller, "sh", "-ec", "touch /tmp/writer.stop; for i in $(seq 1 100); do kill -0 $(cat /tmp/writer.pid) 2>/dev/null || exit 0; sleep .1; done; exit 1"); err != nil {
		return err
	}
	sequenceText, err := h.output(controller, "cat", "/tmp/writer.ledger")
	if err != nil {
		return err
	}
	sequence, err := strconv.Atoi(sequenceText)
	if err != nil {
		return err
	}
	h.manifest["post_vacuum_sequence"] = sequence
	return h.verify(fid, sequence)
}

func (h *harness) powerLoss(fid, victim string) error {
	sequence := 1001
	if err := h.execOK(controller, "python3", "/opt/workload.py", "write", fid, addresses[victim], strconv.Itoa(sequence)); err != nil {
		return err
	}
	if err := h.verify(fid, sequence); err != nil {
		return fmt.Errorf("pre-cut replica convergence: %w", err)
	}
	if err := h.lab.Node(victim).Crash(h.ctx); err != nil {
		return err
	}
	if err := h.lab.Node(victim).Start(h.ctx); err != nil {
		return err
	}
	if err := h.waitOK(victim, 3*time.Minute, "sh", "-ec", "cloud-init status --wait >/dev/null; ip -4 address show | grep -q '192.0.2.'"); err != nil {
		return err
	}
	if err := h.provisionVolume(victim, h.candidate, false); err != nil {
		return err
	}
	if err := h.waitReplicas(fid); err != nil {
		return err
	}
	return h.verify(fid, sequence)
}

func (h *harness) rollingMigration(fid string) error {
	for _, binary := range [][]byte{h.candidate, h.baseline, h.candidate} {
		for _, name := range volumes {
			if err := h.gracefulStopVolume(name); err != nil {
				return fmt.Errorf("gracefully stop %s: %w", name, err)
			}
			if err := h.lab.Node(name).PowerOff(h.ctx); err != nil {
				return err
			}
			if err := h.lab.Node(name).Start(h.ctx); err != nil {
				return err
			}
			if err := h.waitOK(name, 3*time.Minute, "sh", "-ec", "cloud-init status --wait >/dev/null; ip -4 address show | grep -q '192.0.2.'"); err != nil {
				return err
			}
			if err := h.provisionVolume(name, binary, false); err != nil {
				return err
			}
			if err := h.waitReplicas(fid); err != nil {
				return err
			}
			if err := h.verify(fid, 24); err != nil {
				return fmt.Errorf("rolling migration at %s: %w", name, err)
			}
		}
	}
	return nil
}

func (h *harness) gracefulStopVolume(name string) error {
	return h.execOK(name, "sh", "-ec",
		"test -s /run/weed-volume.pid; pid=$(cat /run/weed-volume.pid); kill -TERM $pid; "+
			"for i in $(seq 1 400); do kill -0 $pid 2>/dev/null || { sync; exit 0; }; sleep .1; done; exit 1")
}

func (h *harness) vacuumPowerLoss(fid, victim string) error {
	// A larger overwrite history makes the copy phase observable without adding
	// a test-only hook to SeaweedFS core code. If no .cpd is observed, fail rather
	// than pretending a power cut happened during vacuum.
	liveJSON, err := h.output(controller, "python3", "/opt/workload.py", "seedmany", "20")
	if err != nil {
		return fmt.Errorf("seed same-volume live vacuum set: %w", err)
	}
	var liveFIDs []string
	if err := json.Unmarshal([]byte(liveJSON), &liveFIDs); err != nil || len(liveFIDs) != 20 {
		return fmt.Errorf("decode live vacuum set %q: %w", liveJSON, err)
	}
	fid = liveFIDs[0]
	h.manifest["vacuum_live_fids"] = liveFIDs
	h.manifest["vacuum_overwrite_sequence"] = 1159
	for sequence := 1100; sequence < 1160; sequence++ {
		if err := h.execOK(controller, "python3", "/opt/workload.py", "write", fid, addresses["volume1"], strconv.Itoa(sequence)); err != nil {
			return err
		}
	}
	commands := "lock\nvolume.vacuum -volumeId=" + volumeID(fid) + " -garbageThreshold=0\nunlock\n"
	if err := h.lab.Node(controller).Put(h.ctx, "/opt/vacuum.shell", 0o600, []byte(commands)); err != nil {
		return err
	}
	if err := h.execOK(controller, "sh", "-ec", "nohup sh -c '/opt/weed shell -master=192.0.2.10:9333 < /opt/vacuum.shell' >/tmp/vacuum-power.log 2>&1 </dev/null & echo $! >/tmp/vacuum-power.pid"); err != nil {
		return err
	}
	ref := &labv1.NodeRef{SessionId: h.lab.ID(), Node: victim}
	result, err := h.lab.RunTimeline(h.ctx,
		&labv1.TimelineAction{Action: &labv1.TimelineAction_WaitExec{WaitExec: &labv1.WaitExec{
			Exec:        &labv1.ExecRequest{Node: ref, Argv: []string{"sh", "-ec", "find /mnt/volume -name '*.cpd' -type f | grep -q ."}, TimeoutMillis: 5000},
			RetryMillis: 100, TimeoutMillis: 90000,
		}}},
		&labv1.TimelineAction{Action: &labv1.TimelineAction_Lifecycle{Lifecycle: &labv1.LifecycleRequest{Node: ref, Action: labv1.LifecycleAction_CRASH}}},
	)
	if err != nil || result.GetCompleted() != 2 {
		return fmt.Errorf("vacuum copy marker was not observed and power cut: completed=%d: %w", result.GetCompleted(), err)
	}
	if err := h.lab.Node(victim).Start(h.ctx); err != nil {
		return err
	}
	if err := h.waitOK(victim, 3*time.Minute, "sh", "-ec", "cloud-init status --wait >/dev/null; ip -4 address show | grep -q '192.0.2.'"); err != nil {
		return err
	}
	if err := h.provisionVolume(victim, h.candidate, false); err != nil {
		return err
	}
	if err := h.waitReplicas(fid); err != nil {
		return err
	}
	if err := h.verify(fid, 1159); err != nil {
		return err
	}
	for _, liveFID := range liveFIDs[1:] {
		if err := h.verify(liveFID, 0); err != nil {
			return err
		}
	}
	return nil
}
