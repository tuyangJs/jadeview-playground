package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Only loopback Vite origins may supply pages with access to native actions.
func devURL(raw string) (*url.URL, error) {
	u, e := url.Parse(raw)
	if e != nil {
		return nil, e
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "http" || (ip == nil || !ip.IsLoopback()) ||
		u.User != nil || u.Port() == "" || (u.Path != "" && u.Path != "/") ||
		u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("--dev 仅接受 HTTP 环回地址及端口，例如 http://127.0.0.1:5173")
	}
	u.Path = ""
	return u, nil
}

func vite(ctx context.Context, root, raw string) (string, func(), error) {
	u, err := devURL(raw)
	if err != nil {
		return "", nil, err
	}
	client := &http.Client{Timeout: 750 * time.Millisecond, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	ready := func() bool {
		req, e := http.NewRequestWithContext(ctx, "GET", u.String()+"/@vite/client", nil)
		if e != nil {
			return false
		}
		response, e := client.Do(req)
		if e != nil {
			return false
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return false
		}
		body, _ := io.ReadAll(io.LimitReader(response.Body, 512<<10))
		return strings.Contains(string(body), "createHotContext")
	}
	if ready() {
		return u.String(), func() {}, nil
	}
	script := filepath.Join(root, "node_modules", "vite", "bin", "vite.js")
	if _, err = os.Stat(script); err != nil {
		return "", nil, fmt.Errorf("找不到 Vite，请在仓库根目录执行 npm ci：%w", err)
	}
	cmd := exec.CommandContext(ctx, "node", script, "--host", u.Hostname(), "--port", u.Port(), "--strictPort")
	cmd.Dir = root
	hideViteConsole(cmd)
	// A Windows GUI executable launched from Explorer may not have stdio handles.
	if _, err := os.Stdout.Stat(); err == nil {
		cmd.Stdout = os.Stdout
	}
	if _, err := os.Stderr.Stat(); err == nil {
		cmd.Stderr = os.Stderr
	}
	if err = cmd.Start(); err != nil {
		return "", nil, err
	}
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(done) }()
	cleanup := func() { _ = cmd.Process.Kill(); <-done }
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-done:
			return "", nil, fmt.Errorf("Vite 未就绪，已退出：%v", waitErr)
		case <-ctx.Done():
			cleanup()
			return "", nil, ctx.Err()
		case <-deadline.C:
			cleanup()
			return "", nil, errors.New("等待 Vite 就绪超时")
		case <-ticker.C:
			if ready() {
				return u.String(), cleanup, nil
			}
		}
	}
}
