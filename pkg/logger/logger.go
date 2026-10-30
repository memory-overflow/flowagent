// Package logger 提供分级日志（debug / info / warn / error）与日志文件滚动。
//
// 用法：
//   - 进程启动时调用 logger.Init(...) 一次，配置级别与文件滚动。
//   - 业务代码用 logger.Debugf / Infof / Warnf / Errorf 记录不同级别日志。
//   - 不调 Init 时默认输出到 stdout、级别 info，行为与标准 log 一致。
//
// 与标准库 log 的兼容：Init 会把 log.SetOutput 指向同一输出（stdout + 滚动文件），
// 因此代码里既有的 log.Printf / log.Println 仍照常工作，语义等同 info 级别，
// 且与 logger.* 走同一个底层输出（单写锁串行，避免多行交错）。
package logger

import (
	"io"
	"log"
	"os"
	"strings"
)

// Level 日志级别。数值越小越「啰嗦」。
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// ParseLevel 把配置字符串解析为级别，未知值回落 info。
func ParseLevel(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug
	case "warn", "warning":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

// current 当前生效级别，默认 info（debug 不输出）。
var current = LevelInfo

// enabled 判断级别 l 的消息在当前级别下是否应输出。
func enabled(l Level) bool { return l >= current }

// CurrentLevel 返回当前级别。
func CurrentLevel() Level { return current }

// SetLevel 运行时调整级别（供未来热切换，当前仅启动时由 Init 设置）。
func SetLevel(l Level) { current = l }

// Options 初始化参数。
type Options struct {
	Level      string // debug / info / warn / error，空则 info
	File       string // 日志文件路径，空则仅 stdout
	MaxSizeMB  int    // 单文件大小上限（MB），<=0 时默认 1024（1GB）
	MaxBackups int    // 保留的滚动备份数（不含当前文件），<=0 时默认 4
}

// Init 初始化日志：设置级别与输出（stdout + 滚动文件）。
//
// 返回 error 仅发生在「日志文件/目录创建失败」这类初始化阶段，调用方应视为致命。
func Init(opts Options) error {
	current = ParseLevel(opts.Level)

	if opts.File == "" {
		log.SetOutput(os.Stdout)
		return nil
	}

	maxSizeMB := opts.MaxSizeMB
	if maxSizeMB <= 0 {
		maxSizeMB = 1024
	}
	maxBackups := opts.MaxBackups
	if maxBackups <= 0 {
		maxBackups = 4
	}

	rw, err := NewRotatingWriter(opts.File, int64(maxSizeMB)*1024*1024, maxBackups)
	if err != nil {
		return err
	}
	log.SetOutput(io.MultiWriter(os.Stdout, rw))
	return nil
}

// Writer 返回当前底层输出（供 gin.DefaultWriter / DefaultErrorWriter 复用）。
func Writer() io.Writer { return log.Writer() }

// Debug / Debugf 记录 debug 级日志：默认级别（info）下不输出。
func Debug(args ...interface{}) {
	if enabled(LevelDebug) {
		log.Print(append([]interface{}{"[DEBUG] "}, args...)...)
	}
}

func Debugf(format string, args ...interface{}) {
	if enabled(LevelDebug) {
		log.Printf("[DEBUG] "+format, args...)
	}
}

// Info / Infof 记录 info 级日志（无级别前缀，与既有 log.Printf 观感一致）。
func Info(args ...interface{}) {
	if enabled(LevelInfo) {
		log.Print(args...)
	}
}

func Infof(format string, args ...interface{}) {
	if enabled(LevelInfo) {
		log.Printf(format, args...)
	}
}

// Warn / Warnf 记录 warn 级日志。
func Warn(args ...interface{}) {
	if enabled(LevelWarn) {
		log.Print(append([]interface{}{"[WARN] "}, args...)...)
	}
}

func Warnf(format string, args ...interface{}) {
	if enabled(LevelWarn) {
		log.Printf("[WARN] "+format, args...)
	}
}

// Error / Errorf 记录 error 级日志。
func Error(args ...interface{}) {
	if enabled(LevelError) {
		log.Print(append([]interface{}{"[ERROR] "}, args...)...)
	}
}

func Errorf(format string, args ...interface{}) {
	if enabled(LevelError) {
		log.Printf("[ERROR] "+format, args...)
	}
}
