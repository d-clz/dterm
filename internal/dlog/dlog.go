package dlog

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	mu sync.Mutex
	f  *os.File
)

func Init() {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".dterm")
	os.MkdirAll(dir, 0700)
	path := filepath.Join(dir, "debug.log")
	var err error
	f, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return
	}
	Log("=== dterm started ===")
}

func Log(format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	if f == nil {
		return
	}
	fmt.Fprintf(f, "[%s] %s\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, args...))
}
