//go:build (windows && (amd64 || 386 || arm64)) || (linux && amd64 && (!jadeview_cgo || cgo))

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	jade "github.com/luoxueyousheng/JadeViewGo/v3"
)

const title = "JadeView Playground"
const signature = "jadeview-go-playground"

func main() {
	runtime.LockOSThread()
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type config struct {
	root, frontend, dev, data, example string
	hidden, smoke                      bool
}

func parseArgs(args []string) (config, error) {
	args = append([]string(nil), args...)
	for i := 0; i < len(args); i++ {
		if args[i] == "--dev" || args[i] == "-dev" {
			value := "http://127.0.0.1:5173"
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				value = args[i+1]
				args = append(args[:i+1], args[i+2:]...)
			}
			args[i] = "--dev=" + value
		}
	}
	var c config
	f := flag.NewFlagSet("playground-go", flag.ContinueOnError)
	f.StringVar(&c.dev, "dev", "", "启用 Vite（可指定环回 URL）")
	f.StringVar(&c.root, "root", "", "Playground 仓库根目录")
	f.StringVar(&c.frontend, "frontend", "", "前端目录或 .asar/.japk 文件")
	f.StringVar(&c.data, "data", "", "应用数据目录")
	f.StringVar(&c.example, "example", "", "启动后打开示例")
	f.BoolVar(&c.hidden, "hidden", false, "隐藏原生窗口，便于自动化验证")
	f.BoolVar(&c.smoke, "smoke", false, "运行隐藏窗口 IPC 冒烟验证后退出")
	if err := f.Parse(args); err != nil {
		return c, err
	}
	if f.NArg() != 0 {
		return c, fmt.Errorf("未知参数：%v", f.Args())
	}
	if c.smoke {
		c.hidden = true
		if c.data == "" {
			return c, errors.New("--smoke 需要独立的 --data 目录")
		}
	}
	if c.dev != "" && c.frontend != "" {
		return c, errors.New("--dev 与 --frontend 互斥")
	}
	return c, nil
}

func findRoot(path string) (string, error) {
	explicit := path != ""
	if !explicit {
		path = "."
	}
	dir, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	for {
		for _, entry := range []string{"web/index.html", "web/dist/index.html"} {
			if info, e := os.Stat(filepath.Join(dir, entry)); e == nil && !info.IsDir() {
				return dir, nil
			}
		}
		parent := filepath.Dir(dir)
		if explicit || parent == dir {
			return "", errors.New("找不到 Playground 根目录，请指定 --root")
		}
		dir = parent
	}
}

func run(args []string) error {
	c, err := parseArgs(args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if c.root, err = findRoot(c.root); err != nil {
		return err
	}
	if c.data == "" {
		c.data = filepath.Join(c.root, ".data", "go")
	}
	if c.data, err = filepath.Abs(c.data); err != nil {
		return err
	}
	if err = os.MkdirAll(c.data, 0700); err != nil {
		return err
	}
	if c.smoke {
		entries, e := os.ReadDir(c.data)
		if e != nil {
			return e
		}
		if len(entries) > 0 {
			return errors.New("--smoke 的 --data 目录必须为空")
		}
	}
	if c.dev == "" {
		if c.frontend == "" {
			c.frontend = filepath.Join(c.root, "web", "dist")
		}
		if c.frontend, err = filepath.Abs(c.frontend); err != nil {
			return err
		}
		info, e := os.Stat(c.frontend)
		if e != nil {
			return fmt.Errorf("前端未就绪（先执行 npm run build）：%w", e)
		}
		if info.IsDir() {
			for _, f := range []string{"index.html", "pane.html"} {
				if _, err = os.Stat(filepath.Join(c.frontend, f)); err != nil {
					return err
				}
			}
		} else {
			switch strings.ToLower(filepath.Ext(c.frontend)) {
			case ".asar", ".japk":
			default:
				return errors.New("前端必须是目录、.asar 或 .japk")
			}
		}
	}
	// go run 的临时可执行文件不在 runtime/，显式选用共享的 Linux SO。
	if runtime.GOOS == "linux" && os.Getenv("JADEVIEW_LIBRARY") == "" {
		path := filepath.Join(c.root, "runtime", "libJadeView.so")
		if info, statErr := os.Stat(path); statErr == nil && info.Mode().IsRegular() {
			if err = os.Setenv("JADEVIEW_LIBRARY", path); err != nil {
				return fmt.Errorf("设置 JadeView 运行库: %w", err)
			}
		}
	}
	if err = jade.Preload(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if c.dev != "" {
		origin, cleanup, e := vite(ctx, c.root, c.dev)
		if e != nil {
			return e
		}
		defer cleanup()
		c.dev = origin
	}
	h := newHost(c)
	ready := make(chan error, 1)
	if _, ok := jade.On(jade.EventAppReady, func(_ uint32, data string) string {
		var r struct{ OK bool }
		var err error
		if json.Unmarshal([]byte(data), &r) != nil || !r.OK {
			err = fmt.Errorf("JadeView 初始化失败：%s", data)
		}
		select {
		case ready <- err:
		default:
		}
		return ""
	}); !ok {
		return errors.New("注册 app-ready 失败")
	}
	if !jade.Init(c.dev != "", filepath.Join(c.data, "jadeview.log"), c.data, title, signature, false) {
		return errors.New("JadeView 初始化未受理")
	}
	defer jade.ExitWait(5000)
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	select {
	case err = <-ready:
		if err != nil {
			return err
		}
	case <-startup.Done():
		return startup.Err()
	}
	if err = h.start(startup); err != nil {
		return err
	}
	if c.smoke {
		result := make(chan error, 1)
		go func() {
			result <- h.smoke()
			if !jade.Close(h.main) {
				jade.Exit()
			}
		}()
		jade.RunMessageLoop()
		return <-result
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			jade.Close(h.main)
		case <-done:
		}
	}()
	if c.example == "" {
		c.example = os.Getenv("PLAYGROUND_OPEN")
	}
	if c.example != "" {
		h.mu.Lock()
		_, err = h.spawn(c.example, launch{})
		h.mu.Unlock()
		if err != nil {
			return err
		}
		fmt.Println("EXAMPLE_OPENED", c.example)
	}
	jade.RunMessageLoop()
	return nil
}
