// Package mobile is the gomobile bridge used by the FreeProxy iOS app.
package mobile

import (
	"io"
	"os"
	"path/filepath"
	"sync"

	"freeproxy/core"
)

// Logger receives output text from the proxy.
type Logger interface {
	Output(text string)
}

type logWriter struct{ l Logger }

func (w logWriter) Write(b []byte) (int, error) { w.l.Output(string(b)); return len(b), nil }

var (
	mu      sync.Mutex
	inW     *io.PipeWriter
	running bool
)

// Start runs the proxy in the background. dir should be the app's Documents folder.
func Start(dir string, port int, l Logger) {
	mu.Lock()
	if running {
		mu.Unlock()
		return
	}
	r, w := io.Pipe()
	inW, running = w, true
	mu.Unlock()
	go func() {
		err := core.Run(dir, r, logWriter{l}, nil, "127.0.0.1", port)
		mu.Lock()
		running = false
		mu.Unlock()
		if err != nil {
			l.Output("\n[stopped] tap Start to try again\n")
		}
	}()
}

// Send types a line of input (used for picking a Realm).
func Send(line string) {
	mu.Lock()
	w := inW
	mu.Unlock()
	if w != nil {
		go w.Write([]byte(line + "\n"))
	}
}

// Running reports whether the proxy is running.
func Running() bool { mu.Lock(); defer mu.Unlock(); return running }

// ResetTarget forgets the chosen Realm/server.
func ResetTarget(dir string) { _ = os.Remove(filepath.Join(dir, "config.json")) }

// Logout forgets the login and target.
func Logout(dir string) {
	_ = os.Remove(filepath.Join(dir, "token.json"))
	_ = os.Remove(filepath.Join(dir, "config.json"))
}
