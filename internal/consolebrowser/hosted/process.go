package hosted

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

type ProcessController struct {
	root                string
	mu                  sync.Mutex
	slots               [MaxSessions]bool
	xvfb, chromium, vnc string
}

func NewProcessController(root string) (*ProcessController, error) {
	if root == "" {
		root = "/run/console-browser"
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, errors.New("browser profile storage unavailable")
	}
	return &ProcessController{root: root, xvfb: "Xvfb", chromium: "chromium", vnc: "x11vnc"}, nil
}

type childProcess struct {
	cmd    *exec.Cmd
	exited chan struct{}
}
type processInstance struct {
	root, vncAddr string
	display       int
	cdpPort       int
	procs         []childProcess
	once          sync.Once
	done          chan struct{}
	release       func()
}

func (p *processInstance) VNCAddr() string       { return p.vncAddr }
func (p *processInstance) Done() <-chan struct{} { return p.done }
func (p *processInstance) start(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return err
	}
	child := childProcess{cmd: cmd, exited: make(chan struct{})}
	p.procs = append(p.procs, child)
	go func() { _ = cmd.Wait(); close(child.exited) }()
	return nil
}
func (p *processInstance) Close() {
	p.once.Do(func() {
		for _, child := range p.procs {
			_ = syscall.Kill(-child.cmd.Process.Pid, syscall.SIGKILL)
		}
		for _, child := range p.procs {
			<-child.exited
		}
		_ = os.RemoveAll(p.root)
		if p.display > 0 {
			_ = os.Remove("/tmp/.X" + strconv.Itoa(p.display) + "-lock")
			_ = os.Remove("/tmp/.X11-unix/X" + strconv.Itoa(p.display))
		}
		if p.release != nil {
			p.release()
		}
		close(p.done)
	})
}
func (p *processInstance) monitor() {
	exited := make(chan struct{}, len(p.procs))
	for _, child := range p.procs {
		go func(c childProcess) { <-c.exited; exited <- struct{}{} }(child)
	}
	select {
	case <-exited:
		p.Close()
	case <-p.done:
	}
}
func (c *ProcessController) Start(ctx context.Context, provider string) (Instance, error) {
	if provider != "claude" && provider != "openai" {
		return nil, errors.New("invalid provider")
	}
	c.mu.Lock()
	slot := -1
	for i, used := range c.slots {
		if !used {
			slot = i
			c.slots[i] = true
			break
		}
	}
	c.mu.Unlock()
	if slot < 0 {
		return nil, errors.New("browser capacity exhausted")
	}
	release := func() { c.mu.Lock(); c.slots[slot] = false; c.mu.Unlock() }
	root, err := os.MkdirTemp(c.root, "browser-")
	if err != nil {
		release()
		return nil, errors.New("browser storage unavailable")
	}
	p := &processInstance{root: root, vncAddr: fmt.Sprintf("127.0.0.1:%d", 5900+slot), cdpPort: 9222 + slot, display: 100 + slot, done: make(chan struct{}), release: release}
	ok := false
	defer func() {
		if !ok {
			p.Close()
		}
	}()
	display := ":" + strconv.Itoa(100+slot)
	x := exec.Command(c.xvfb, display, "-screen", "0", "1280x800x24", "-nolisten", "tcp", "-ac")
	if err = p.start(x); err != nil {
		return nil, errors.New("browser display unavailable")
	}
	if err = waitReady(ctx, p, func() bool { _, e := os.Stat("/tmp/.X11-unix/X" + strconv.Itoa(100+slot)); return e == nil }); err != nil {
		return nil, err
	}
	initialURL := "https://platform.claude.com/"
	if provider == "openai" {
		initialURL = "https://platform.openai.com/settings/organization/billing/overview"
	}
	chrome := exec.Command(c.chromium, "--no-sandbox", "--disable-dev-shm-usage", "--no-first-run", "--no-default-browser-check", "--user-data-dir="+root, "--remote-debugging-address=127.0.0.1", "--remote-debugging-port="+strconv.Itoa(p.cdpPort), "--window-size=1280,800", "--window-position=0,0", initialURL)
	chrome.Env = append(os.Environ(), "DISPLAY="+display, "HOME="+root, "TMPDIR="+root)
	if err = p.start(chrome); err != nil {
		return nil, errors.New("browser unavailable")
	}
	vnc := exec.Command(c.vnc, "-display", display, "-localhost", "-rfbport", strconv.Itoa(5900+slot), "-forever", "-shared", "-nopw", "-quiet", "-noxdamage")
	if err = p.start(vnc); err != nil {
		return nil, errors.New("viewer unavailable")
	}
	if err = waitReady(ctx, p, func() bool {
		conn, e := net.DialTimeout("tcp", p.vncAddr, 100*time.Millisecond)
		if e != nil {
			return false
		}
		_ = conn.Close()
		return true
	}); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, errors.New("browser startup cancelled")
	}
	ok = true
	go p.monitor()
	return p, nil
}
func waitReady(ctx context.Context, p *processInstance, ready func() bool) error {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		for _, child := range p.procs {
			select {
			case <-child.exited:
				return errors.New("browser process exited")
			default:
			}
		}
		if ready() {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("browser startup cancelled")
		case <-tick.C:
		}
	}
}
func (p *processInstance) Finish(ctx context.Context, provider string) (string, error) {
	return extractSession(ctx, p.cdpPort, provider)
}
