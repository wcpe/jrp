package store

import (
	"fmt"
	"path/filepath"
	"time"
)

const (
	// 忙等待默认超时：SQLite 写竞争时等待而不是立刻失败。
	defaultBusyTimeout = 5 * time.Second
	// 数据库文件后缀：与 OPERATIONS 数据目录约定一致。
	databaseFileSuffix = ".db"
)

// databaseDSN 构造独占打开参数。
//
// 关键点是 locking_mode(EXCLUSIVE)：首个读取即取得排他锁，使同一数据库文件
// 无法被第二个进程或第二个连接打开（FR-09 规格 §2 与 §5 的并发启动边界）。
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

// DatabasePath 返回数据目录下的默认数据库文件路径。
func DatabasePath(dataDirectory string) string {
	return filepath.Join(dataDirectory, "jrps"+databaseFileSuffix)
}
