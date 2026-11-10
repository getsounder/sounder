package store

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"
)

// TestPostgresMigrateFreshDatabase 在真实 Postgres 上验证「空库首次启动」：
// 迁移能按顺序建表、可重复执行、users→devices 外键顺序正确。
// 需要设置 SOUNDER_TEST_POSTGRES（例如 compose 里的 DSN），否则跳过。
func TestPostgresMigrateFreshDatabase(t *testing.T) {
	adminDSN := os.Getenv("SOUNDER_TEST_POSTGRES")
	if adminDSN == "" {
		t.Skip("SOUNDER_TEST_POSTGRES 未设置，跳过真实 Postgres 迁移测试")
	}

	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		t.Fatalf("ping admin: %v", err)
	}

	name := fmt.Sprintf("sounder_mig_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
	})

	base, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	base.Path = "/" + name

	db, err := sql.Open("pgx", base.String())
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	defer db.Close()

	p := &Postgres{db: db}
	if err := p.Migrate(); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if err := p.Migrate(); err != nil {
		t.Fatalf("second migrate (should be no-op): %v", err)
	}

	applied, err := p.MigrationStatus()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(applied) != 2 {
		t.Fatalf("applied = %v, want 2 migrations", applied)
	}

	// users 必须先于 devices 建好，否则外键创建会失败。
	if _, err := db.Exec(`INSERT INTO users (id, email) VALUES ('usr_test', 'test@example.com')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO devices (id, user_id, worker_id) VALUES ('dev_test', 'usr_test', 'w1')`); err != nil {
		t.Fatalf("insert device: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM users WHERE id = 'usr_test'`); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	var devices int
	if err := db.QueryRow(`SELECT count(*) FROM devices`).Scan(&devices); err != nil {
		t.Fatalf("count devices: %v", err)
	}
	if devices != 0 {
		t.Fatalf("devices after user delete = %d, want 0 (FK cascade)", devices)
	}

	var hasIndex bool
	if err := db.QueryRow(`SELECT to_regclass('public.sessions_state_idx') IS NOT NULL`).Scan(&hasIndex); err != nil {
		t.Fatalf("check index: %v", err)
	}
	if !hasIndex {
		t.Fatal("sessions_state_idx 未创建")
	}
}
