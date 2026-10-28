package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// RotatingWriter 是带滚动能力的 io.Writer：当前文件超过 maxSize 字节时，
// 把当前文件滚动为 .1，已有 .1→.2 … 依次后移，最多保留 maxBackups 个备份，
// 然后新建当前文件继续写。备份命名约定：<file>.1 为最近一次滚动，数字越大越旧。
//
// 并发安全：Write 内部加锁，可被多个 goroutine 安全调用；但不同 goroutine
// 的写不会互相穿插（单次 Write 原子）。
type RotatingWriter struct {
	mu         sync.Mutex
	path       string
	maxSize    int64
	maxBackups int

	file *os.File
	size int64
}

// NewRotatingWriter 创建滚动写入器并打开（或创建）目标文件。
func NewRotatingWriter(path string, maxSize int64, maxBackups int) (*RotatingWriter, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, err
		}
	}
	w := &RotatingWriter{path: path, maxSize: maxSize, maxBackups: maxBackups}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

// open 打开目标文件并记录当前大小（追加模式）。
func (w *RotatingWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.file = f
	w.size = info.Size()
	return nil
}

// Write 写入数据，超过上限时先滚动再写。
func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	if w.maxSize > 0 && w.size+int64(len(p)) > w.maxSize {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}

	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// rotate 关闭当前文件，滚动历史备份，重开新文件。
func (w *RotatingWriter) rotate() error {
	if w.file != nil {
		if err := w.file.Close(); err != nil {
			return err
		}
		w.file = nil
		w.size = 0
	}

	if w.maxBackups > 0 {
		// 删除最旧的备份，为整体后移腾位置。
		_ = os.Remove(w.backupPath(w.maxBackups))
		for i := w.maxBackups - 1; i >= 1; i-- {
			_ = os.Rename(w.backupPath(i), w.backupPath(i+1))
		}
		_ = os.Rename(w.path, w.backupPath(1))
	}

	return w.open()
}

// backupPath 返回第 i 个滚动备份的路径。
func (w *RotatingWriter) backupPath(i int) string {
	return fmt.Sprintf("%s.%d", w.path, i)
}
