package main

import "fmt"

// A successful VM restart alone does not prove volatile writes were lost.
// Keep a flushed control page on the tested data disk, overwrite it without
// fsync immediately before the crash, and require the old page after recovery.
// This qualifies loss of guest page cache, not physical controller write cache.
const prepareVolatileWitness = `import os
p='/mnt/volume/.power-loss-witness'
with open(p,'wb',buffering=0) as f:
    f.write(b'D'*4096)
    os.fsync(f.fileno())
d=os.open('/mnt/volume',os.O_RDONLY)
os.fsync(d)
os.close(d)
for setting in ('dirty_expire_centisecs','dirty_writeback_centisecs'):
    with open('/proc/sys/vm/'+setting,'w') as f: f.write('60000')
`

const dirtyVolatileWitness = `import os
p='/mnt/volume/.power-loss-witness'
with open(p,'r+b',buffering=0) as f:
    assert f.read()==b'D'*4096, 'durable witness baseline missing'
    f.seek(0)
    assert f.write(b'V'*4096)==4096
with open(p,'rb') as f: assert f.read()==b'V'*4096, 'volatile write not observed'
`

const checkVolatileWitness = `with open('/mnt/volume/.power-loss-witness','rb') as f:
    assert f.read()==b'D'*4096, 'fault did not demonstrably discard the volatile control write'
print('VOLATILE_WRITE_LOSS_VERIFIED')
`

func (h *harness) verifyVolatileLoss(victim string) error {
	out, err := h.output(victim, "python3", "-c", checkVolatileWitness)
	if err != nil || out != "VOLATILE_WRITE_LOSS_VERIFIED" {
		return fmt.Errorf("%s power-loss witness: %q: %w", victim, out, err)
	}
	verified, _ := h.manifest["volatile_write_loss_verified"].([]string)
	h.manifest["volatile_write_loss_verified"] = append(verified, victim)
	return nil
}
