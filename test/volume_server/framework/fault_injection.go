package framework

// CrashVolumeServer kills only the process owned by this test harness, without
// graceful flush/cleanup. Pair with RestartVolumeServer to test process-crash
// recovery. This does NOT simulate power loss: the host page cache survives.
func (c *Cluster) CrashVolumeServer() {
	c.testingTB.Helper()
	if c.volumeCmd == nil || c.volumeCmd.Process == nil {
		c.testingTB.Fatal("volume server is not running")
	}
	if err := c.volumeCmd.Process.Kill(); err != nil {
		c.testingTB.Fatalf("kill test volume server: %v", err)
	}
	_ = c.volumeCmd.Wait()
	c.volumeCmd = nil
}
