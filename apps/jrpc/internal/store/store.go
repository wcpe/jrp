// Package store 是 jrpc 外壳私有的 SQLite 持久化层。
//
// 它只保存属于本客户端身份的 desired state、本地应用结果与本地运行元数据；
// jrps 侧有独立的 models 与迁移，两侧互不导入、不共享 schema 文件（FR-09 规格 §3.1）。
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

// currentSchemaVersion 是当前程序期望的数据库架构版本，落库到 PRAGMA user_version。
const currentSchemaVersion = 1

// 数据库角色标记：用于识别数据库文件归属于哪一侧外壳。
const (
	RoleClient = "jrpc"
	RoleServer = "jrps"
)

// defaultBusyTimeout 是写竞争的默认忙等待超时。
const defaultBusyTimeout = 5 * time.Second

// Config 是打开 jrpc 数据库的引导参数。
//
// 数据目录与 SQLite 路径属于仅有的三项引导参数之一，变更需要重启。
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

// Store 是 jrpc 侧的 SQLite 持久化入口。
//
// 与 jrps 侧一致：它持有 desired 真源，不持有运行态。Core 的 active 与 last-good 归
// Core 内存，本层只保存 Apply 结果记录（ADR-0012）。
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
	store := &Store{db: db, sqlDB: sqlDB, logger: logger, path: cfg.Path, role: RoleClient}

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
		return fn(&Tx{db: gormTx})
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
	return fn(&Tx{db: s.db.WithContext(ctx)})
}

// Tx 是一次事务内的写入或读取句柄。
type Tx struct {
	db *gorm.DB
}

// DB 暴露事务内的 GORM 句柄。
func (tx *Tx) DB() *gorm.DB { return tx.db }

// Transaction 在已有事务句柄上执行嵌套事务，失败整体回滚。
func (tx *Tx) Transaction(fn func() error) error {
	return tx.db.Transaction(func(*gorm.DB) error { return fn() })
}

// databaseDSN 构造独占打开参数。
//
// locking_mode(EXCLUSIVE) 使数据库文件无法被第二个实例打开，落实并发启动边界。
func databaseDSN(path string, busyTimeout time.Duration) string {
	milliseconds := busyTimeout.Milliseconds()
	if milliseconds <= 0 {
		milliseconds = defaultBusyTimeout.Milliseconds()
	}
	return fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)"+
			"&_pragma=locking_mode(EXCLUSIVE)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)",
		filepath.ToSlash(path),
		milliseconds,
	)
}

func openDatabase(cfg Config) (*gorm.DB, *sql.DB, error) {
	timeout := cfg.BusyTimeout
	if timeout <= 0 {
		timeout = defaultBusyTimeout
	}
	database, err := gorm.Open(sqlite.Open(databaseDSN(cfg.Path, timeout)), &gorm.Config{Logger: logger.Discard})
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

// LoadRevisionForRecovery 读取重启恢复所需输入。
//
// 返回的 active 与 last-good 是上一次运行留下的 Apply 结果记录，只用于展示与比对；
// 恢复流程必须以 desired 为输入重新走完整应用流程，不得把它当作 active 使用。
//
// 若落库的 active/last-good 找不到对应的成功 publish 结果，说明存在绕过 publish 的写入，
// 此时拒绝恢复并报告不一致。
func (s *Store) LoadRevisionForRecovery(ctx context.Context) (RecoveryInput, error) {
	var input RecoveryInput
	err := s.View(ctx, func(tx *Tx) error {
		latest, err := tx.LatestDesired()
		if errors.Is(err, ErrNoDesired) {
			return nil
		}
		if err != nil {
			return err
		}
		state, err := tx.RevisionState()
		if err != nil {
			return err
		}
		if err := verifyActiveRecord(tx, state.ActiveRevision); err != nil {
			return err
		}
		if err := verifyActiveRecord(tx, state.LastGoodRevision); err != nil {
			return err
		}
		input.HasDesired = true
		input.DesiredRevision = latest.Revision
		input.DesiredContent = latest.Content
		input.RecordedActiveRevision = state.ActiveRevision
		input.RecordedLastGoodRevision = state.LastGoodRevision
		return nil
	})
	if err != nil {
		return RecoveryInput{}, err
	}
	return input, nil
}

// verifyActiveRecord 校验 active 类记录必须来自一次成功的 publish。
func verifyActiveRecord(tx *Tx, revision uint64) error {
	if revision == 0 {
		return nil
	}
	var count int64
	err := tx.db.Model(&ApplyResult{}).
		Where("revision = ? AND phase = ? AND succeeded = ?", revision, PhasePublish, true).
		Count(&count).Error
	if err != nil {
		return fmt.Errorf("校验 active 记录失败：%w", translateSQLError(err))
	}
	if count == 0 {
		return fmt.Errorf(
			"数据库中的 active/last-good 记录（版本 %d）没有对应的成功 publish 结果，"+
				"存在绕过 publish 的写入，拒绝恢复", revision)
	}
	return nil
}

// RecoveryInput 是重启恢复的输入。
type RecoveryInput struct {
	HasDesired               bool
	DesiredRevision          uint64
	DesiredContent           string
	RecordedActiveRevision   uint64
	RecordedLastGoodRevision uint64
}

// DatabasePath 返回数据目录下的默认数据库文件路径。
func DatabasePath(dataDirectory string) string {
	return filepath.Join(dataDirectory, "jrpc.db")
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
		return fmt.Errorf("数据库架构版本 %d 高于当前程序支持的 %d，请使用更新的 jrpc", s.schemaVersion, currentSchemaVersion)
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
	if record.Value != RoleClient {
		return fmt.Errorf("数据库文件归属为 %s，jrpc 与 jrps 必须使用各自独立的 SQLite 文件", record.Value)
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
		return writeRoleMarker(tx, RoleClient)
	})
	if err != nil {
		s.logger.Error("数据库迁移失败，拒绝启动", "错误", err)
		return fmt.Errorf("数据库迁移失败，拒绝启动：%w", err)
	}
	return nil
}

func (s *Store) ensureRoleMarker() error {
	err := s.db.Exec(
		"INSERT INTO store_meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO NOTHING",
		metaKeyRole, RoleClient,
	).Error
	if err != nil {
		return fmt.Errorf("写入数据库归属标记失败：%w", translateSQLError(err))
	}
	return nil
}

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
