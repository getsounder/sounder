package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

func (p *Postgres) EnsureDevice(d Device) (Device, error) {
	now := time.Now()
	if d.ID == "" {
		d.ID = NewDeviceID()
	}
	if d.Name == "" {
		d.Name = DefaultDeviceName(d.Kind, d.WorkerID)
	}
	if d.Kind == "" {
		d.Kind = "cloud"
	}

	const q = `
INSERT INTO devices (id, user_id, worker_id, name, kind, form, platform, version, last_seen_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)
ON CONFLICT (user_id, worker_id) DO UPDATE SET
    last_seen_at = EXCLUDED.last_seen_at,
    name = CASE WHEN devices.name = '' THEN EXCLUDED.name ELSE devices.name END,
    kind = CASE WHEN EXCLUDED.kind = '' THEN devices.kind ELSE EXCLUDED.kind END,
    form = CASE WHEN EXCLUDED.form = '' THEN devices.form ELSE EXCLUDED.form END,
    platform = CASE WHEN EXCLUDED.platform = '' THEN devices.platform ELSE EXCLUDED.platform END,
    version = CASE WHEN EXCLUDED.version = '' THEN devices.version ELSE EXCLUDED.version END
RETURNING id, user_id, worker_id, name, kind, form, platform, version, last_seen_at, created_at`

	out := d
	if out.Form == "" {
		out.Form = "sandbox"
	}
	err := p.db.QueryRow(q, d.ID, d.UserID, d.WorkerID, d.Name, d.Kind, out.Form, d.Platform, d.Version, now).
		Scan(&out.ID, &out.UserID, &out.WorkerID, &out.Name, &out.Kind, &out.Form, &out.Platform, &out.Version, &out.LastSeenAt, &out.CreatedAt)
	if err != nil {
		return Device{}, err
	}
	return out, nil
}

func (p *Postgres) ListDevices(userID string) ([]Device, error) {
	const q = `
SELECT id, user_id, worker_id, name, kind, form, platform, version, last_seen_at, created_at
FROM devices WHERE user_id = $1 ORDER BY last_seen_at DESC`
	rows, err := p.db.Query(q, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Device
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.UserID, &d.WorkerID, &d.Name, &d.Kind, &d.Form, &d.Platform, &d.Version, &d.LastSeenAt, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (p *Postgres) RenameDevice(id, userID, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("name required")
	}
	res, err := p.db.Exec(`UPDATE devices SET name = $1 WHERE id = $2 AND user_id = $3`, name, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) DeleteDevice(id, userID string) error {
	res, err := p.db.Exec(`DELETE FROM devices WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

var _ = sql.ErrNoRows

// ---------- 配额 ----------

func (p *Postgres) GetQuota(userID string) (Quota, error) {
	q := DefaultQuota(userID)
	err := p.db.QueryRow(
		`SELECT max_devices, device_cpu, device_mem_mb, device_disk_gb FROM user_quotas WHERE user_id = $1`,
		userID,
	).Scan(&q.MaxDevices, &q.DeviceCPU, &q.DeviceMemMB, &q.DeviceDiskGB)
	if errors.Is(err, sql.ErrNoRows) {
		return q, nil
	}
	if err != nil {
		return q, err
	}
	return q, nil
}

func (p *Postgres) SetQuota(q Quota) error {
	if q.UserID == "" {
		return errors.New("userId required")
	}
	_, err := p.db.Exec(
		`INSERT INTO user_quotas (user_id, max_devices, device_cpu, device_mem_mb, device_disk_gb, updated_at)
		 VALUES ($1, $2, $3, $4, $5, now())
		 ON CONFLICT (user_id) DO UPDATE SET
		   max_devices = EXCLUDED.max_devices,
		   device_cpu = EXCLUDED.device_cpu,
		   device_mem_mb = EXCLUDED.device_mem_mb,
		   device_disk_gb = EXCLUDED.device_disk_gb,
		   updated_at = now()`,
		q.UserID, q.MaxDevices, q.DeviceCPU, q.DeviceMemMB, q.DeviceDiskGB,
	)
	return err
}
