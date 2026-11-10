package store

import (
	"testing"
	"time"
)

func TestDeleteByWorkerRemovesOnlyOwnerSessions(t *testing.T) {
	m := NewMemory()
	now := time.Now()
	for _, s := range []Session{
		{ID: "ses_1", OwnerID: "usr_a", WorkerID: "w1", CreatedAt: now},
		{ID: "ses_2", OwnerID: "usr_a", WorkerID: "w2", CreatedAt: now},
		{ID: "ses_3", OwnerID: "usr_b", WorkerID: "w1", CreatedAt: now},
	} {
		if err := m.Create(s); err != nil {
			t.Fatal(err)
		}
	}

	if err := m.DeleteByWorker("w1", "usr_a"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get("ses_1"); err != ErrNotFound {
		t.Fatalf("ses_1 应被删除, err=%v", err)
	}
	if _, err := m.Get("ses_2"); err != nil {
		t.Fatalf("ses_2 不应被删除: %v", err)
	}
	if _, err := m.Get("ses_3"); err != nil {
		t.Fatalf("其他用户的会话不应被删除: %v", err)
	}
}

func TestGetDeviceScopesUser(t *testing.T) {
	m := NewMemory()
	dev, err := m.EnsureDevice(Device{UserID: "usr_a", WorkerID: "w1"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := m.GetDevice(dev.ID, "usr_a"); err != nil || got.WorkerID != "w1" {
		t.Fatalf("owner lookup = %+v, %v", got, err)
	}
	if _, err := m.GetDevice(dev.ID, "usr_b"); err != ErrNotFound {
		t.Fatalf("其他用户不应看到设备, err=%v", err)
	}
}
