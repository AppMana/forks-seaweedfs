package winfsp

import "testing"

// These use the same real mounted fixtures as the canonical phase tests, not
// an NTFS temporary directory or mocked Windows API. Run after the write phase.
func TestWindowsAttributesPersistenceRepeatVerify(t *testing.T) {
	if *mountPoint == "" || *phase != "verify" {
		t.Skip("requires real mounted persistence fixtures and -phase=verify")
	}
	t.Run("first", TestWindowsAttributesPersistence)
	t.Run("second", TestWindowsAttributesPersistence)
}

func TestWindowsPermissionsPersistenceRepeatVerify(t *testing.T) {
	if *mountPoint == "" || *phase != "verify" {
		t.Skip("requires real mounted basic-permission fixtures and -phase=verify")
	}
	t.Run("first", TestWindowsPermissionsPersistence)
	t.Run("second", TestWindowsPermissionsPersistence)
}
