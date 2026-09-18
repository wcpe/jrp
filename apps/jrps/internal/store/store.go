package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Config 是打开 jrps 数据库的引导参数。
//
// 数据目录与 SQLite 路径属于仅有的三项引导参数之一，变更需要重启（架构不变量 §4）。
type Config struct {
	// Path 是 SQLite 主文件路径，其所在目录会被自动创建。
	Path string
	// BusyTimeout 是写竞争的忙等待超时，零值时使用默认值。
	BusyTimeout time.Duration
	// Logger 是外壳日志器；为空时静默。
	Logger *slog.Logger
	// migrations 允许测试注入迁移步骤，用于验证迁移失败拒绝启动。
	migrations []migration
}

// Store 是 jrps 侧的 SQLite 持久化入口，持有 desired 真源与审计记录。
//
// 它不持有运行态：Core 的 active 与 last-good 归 Core 内存，本层只保存 Apply 结果记录（ADR-0012）。
type Store struct {
	db            *gorm.DB
	sqlDB         *sql.DB
	logger        *slog.Logger
	path          string
	role          string
	schemaVersion uint64
}

// Open 打开本进程独占的数据库，执行事务化迁移并校验数据库归属。
//
// 任一步失败都拒绝启动并返回中文错误，不留下半迁移状态。
func Open(cfg Config) (*Store, error) {
	if strings.TrimSpace(cfg.Path) == "" {
		return nil, errors.New("SQLite 路径不能为空")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o750); err != nil {
		return nil, fmt.Errorf("创建数据目录失败：%w", err)
	}

	db, sqlDB, err := openDatabase(cfg)
	if err != nil {
		return nil, err
	}
	if err := restrictDatabasePermissions(cfg.Path); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	store := &Store{db: db, sqlDB: sqlDB, logger: logger, path: cfg.Path, role: RoleServer}

	if err := store.loadSchemaVersion(); err != nil {
		store.closeQuietly()
		return nil, err
	}
	if err := store.checkRole(); err != nil {
		store.closeQuietly()
		return nil, err
	}
	if err := store.migrate(cfg.migrations); err != nil {
		store.closeQuietly()
		return nil, err
	}
	return store, nil
}

// openDatabase 建立独占连接；被第二个实例占用时返回中文错误。
func openDatabase(cfg Config) (*gorm.DB, *sql.DB, error) {
	timeout := cfg.BusyTimeout
	if timeout <= 0 {
		timeout = defaultBusyTimeout
	}
	dialector := sqlite.Open(databaseDSN(cfg.Path, timeout))
	database, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Discard})
	if err != nil {
		return nil, nil, fmt.Errorf("打开 SQLite 数据库失败：%w", translateSQLError(err))
	}
	sqlDB, err := database.DB()
	if err != nil {
		return nil, nil, fmt.Errorf("获取数据库连接失败：%w", err)
	}
	// 独占锁由单个连接持有，连接池必须收敛为一条，避免同进程内的其他连接争锁。
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		return nil, nil, fmt.Errorf("连接 SQLite 数据库失败：%w", translateSQLError(err))
	}
	return database, sqlDB, nil
}

// Close 释放数据库连接与独占锁。
func (s *Store) Close() error {
	if s == nil || s.sqlDB == nil {
		return nil
	}
	err := s.sqlDB.Close()
	s.sqlDB = nil
	if err != nil {
		return fmt.Errorf("关闭数据库失败：%w", err)
	}
	return nil
}

// DB 暴露底层 GORM 句柄，供迁移检查与外壳其他适配层使用。
func (s *Store) DB() *gorm.DB { return s.db }

// Path 返回数据库文件路径。
func (s *Store) Path() string { return s.path }

// Role 返回数据库归属标记。
func (s *Store) Role() string { return s.role }

// SchemaVersion 返回迁移后的数据库架构版本。
func (s *Store) SchemaVersion() uint64 { return s.schemaVersion }

// Transaction 在单个事务内执行外壳业务写入；失败整体回滚。
func (s *Store) Transaction(ctx context.Context, fn func(tx *Tx) error) error {
	if s == nil || s.sqlDB == nil {
		return errors.New("数据库已关闭，写入被拒绝")
	}
	err := s.db.WithContext(ctx).Transaction(func(gormTx *gorm.DB) error {
		return fn(&Tx{db: gormTx, logger: s.logger})
	})
	if err != nil {
		return fmt.Errorf("数据库写入失败：%w", translateSQLError(err))
	}
	return nil
}

// View 执行只读查询，不开启写事务。
func (s *Store) View(ctx context.Context, fn func(tx *Tx) error) error {
	if s == nil || s.sqlDB == nil {
		return errors.New("数据库已关闭，读取被拒绝")
	}
	return fn(&Tx{db: s.db.WithContext(ctx), logger: s.logger})
}

func (s *Store) loadSchemaVersion() error {
	var version int
	if err := s.db.Raw("PRAGMA user_version").Scan(&version).Error; err != nil {
		return fmt.Errorf("读取数据库架构版本失败：%w", translateSQLError(err))
	}
	if version < 0 {
		version = 0
	}
	s.schemaVersion = uint64(version)
	if s.schemaVersion > currentSchemaVersion {
		return fmt.Errorf("数据库架构版本 %d 高于当前程序支持的 %d，请使用更新的 jrps", s.schemaVersion, currentSchemaVersion)
	}
	return nil
}

// checkRole 阻止两侧外壳复用同一个数据库文件。
func (s *Store) checkRole() error {
	if !s.db.Migrator().HasTable(&StoreMeta{}) {
		return nil
	}
	var record StoreMeta
	if err := s.db.First(&record, "key = ?", metaKeyRole).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return fmt.Errorf("读取数据库归属标记失败：%w", translateSQLError(err))
	}
	if record.Value != RoleServer {
		return fmt.Errorf("数据库文件归属为 %s，jrps 与 jrpc 必须使用各自独立的 SQLite 文件", record.Value)
	}
	return nil
}

// migrate 在单个事务中执行迁移；任一步失败整体回滚并拒绝启动。
func (s *Store) migrate(entries []migration) error {
	if entries == nil {
		entries = migrations()
	}
	pending := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.version > s.schemaVersion {
			pending = append(pending, entry)
		}
	}
	if len(pending) == 0 {
		return s.ensureRoleMarker()
	}

	err := s.db.Transaction(func(tx *gorm.DB) error {
		for _, entry := range pending {
			s.logger.Info("开始执行数据库迁移", "版本", entry.version, "说明", entry.name)
			if err := entry.apply(tx); err != nil {
				return fmt.Errorf("迁移步骤 %d（%s）失败：%w", entry.version, entry.name, err)
			}
			if err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", entry.version)).Error; err != nil {
				return fmt.Errorf("写入架构版本 %d 失败：%w", entry.version, translateSQLError(err))
			}
			s.schemaVersion = entry.version
			s.logger.Info("数据库迁移完成", "版本", entry.version)
		}
		return writeRoleMarker(tx, RoleServer)
	})
	if err != nil {
		s.logger.Error("数据库迁移失败，拒绝启动", "错误", err)
		return fmt.Errorf("数据库迁移失败，拒绝启动：%w", err)
	}
	return nil
}

// ensureRoleMarker 在已是最新版本时补齐归属标记。
func (s *Store) ensureRoleMarker() error {
	if err := s.db.Exec(
		"INSERT INTO store_meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO NOTHING",
		metaKeyRole, RoleServer,
	).Error; err != nil {
		return fmt.Errorf("写入数据库归属标记失败：%w", translateSQLError(err))
	}
	return nil
}

// writeRoleMarker 写入归属标记，供两侧外壳互相识别。
func writeRoleMarker(tx *gorm.DB, role string) error {
	err := tx.Exec(
		"INSERT INTO store_meta (key, value) VALUES (?, ?) "+
			"ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		metaKeyRole, role,
	).Error
	if err != nil {
		return fmt.Errorf("写入数据库归属标记失败：%w", translateSQLError(err))
	}
	return nil
}

func (s *Store) closeQuietly() {
	if s.sqlDB != nil {
		_ = s.sqlDB.Close()
		s.sqlDB = nil
	}
}

// Tx 是一次事务内的写入或读取句柄。
type Tx struct {
	db     *gorm.DB
	logger *slog.Logger
}

// DB 暴露事务内的 GORM 句柄，供外壳适配层执行本包尚未封装的定义查询。
func (tx *Tx) DB() *gorm.DB { return tx.db }

// translateSQLError 把 SQLite 的英文底层错误映射为中文可运维描述。
func translateSQLError(err error) error {
	if err == nil {
		return nil
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "database is locked"), strings.Contains(message, "sqlite_busy"):
		return fmt.Errorf("数据库文件已被占用（可能存在第二个实例正在运行）：%w", err)
	case strings.Contains(message, "database or disk is full"), strings.Contains(message, "disk i/o error"):
		return fmt.Errorf("磁盘空间不足，写入失败：%w", err)
	case strings.Contains(message, "readonly"), strings.Contains(message, "read-only"):
		return fmt.Errorf("数据库文件不可写：%w", err)
	default:
		return err
	}
}

// restrictDatabasePermissions 把数据库文件与 WAL/SHM 的权限收紧到仅运行账户可读写。
//
// SQLite 驱动按进程 umask 创建文件（通常 0644），而库内含明文 token 与配置，
// 数据目录是信任边界：同机其他用户不得读取。Windows 上权限由 ACL 管理，
// 本函数只在类 Unix 平台生效。
func restrictDatabasePermissions(path string) error {
	for _, target := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(target, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("收紧数据库文件权限失败：%w", err)
		}
	}
	return nil
}
