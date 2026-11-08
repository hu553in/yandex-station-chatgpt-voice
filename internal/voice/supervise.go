package voice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const logFileBytes = 512 << 10
const exitConfig = 78

type configError struct{ error }

func (e configError) Unwrap() error { return e.error }

func ExitCode(err error) int {
	if err == nil || err == context.Canceled {
		return 0
	}
	if _, ok := errors.AsType[configError](err); ok {
		return exitConfig
	}
	return 1
}

type supervisor struct {
	child  func(context.Context) (int, error)
	ready  func(context.Context) error
	pause  func(context.Context) error
	report *log.Logger
}

func (s supervisor) run(ctx context.Context) error {
	for attempt := range 3 {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if err := s.ready(ctx); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		code, err := s.child(ctx)
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if err != nil {
			return err
		}
		if code == 0 {
			return nil
		}
		if code == exitConfig {
			return configError{errors.New("Station client rejected its saved session; inspect the log and run make login on the computer")}
		}
		if attempt == 2 {
			return errors.New("Station client stopped after two recovery attempts; inspect the log and run make restart")
		}
		s.report.Printf("Client worker exited with code %d; recovery attempt %d of 2 in 5 seconds.", code, attempt+1)
		if err := s.pause(ctx); err != nil {
			return err
		}
	}
	return nil
}

func supervise(ctx context.Context, args []string, path string) error {
	if _, err := parseRunSettings(args); err != nil {
		return configError{err}
	}
	if _, err := readSessionFile(credentialPath()); err != nil {
		return configError{err}
	}
	dir := filepath.Join(filepath.Dir(path), "yandex-station-chatgpt-voice")
	lock, err := supervisorLock(dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	output, err := openCappedLog(path, logFileBytes)
	if err != nil {
		return err
	}
	defer output.Close()
	report := log.New(output, "", log.LstdFlags|log.LUTC)
	executable, err := os.Executable()
	if err != nil {
		return errors.New("cannot resolve the installed client executable")
	}
	s := supervisor{
		report: report,
		ready:  waitNativeState,
		pause: func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return context.Cause(ctx)
			case <-time.After(5 * time.Second):
				return nil
			}
		},
		child: func(ctx context.Context) (int, error) {
			return runChild(ctx, executable, append([]string{"run"}, args...), lock, output, report)
		},
	}
	err = s.run(ctx)
	if ctx.Err() != nil && errors.Is(err, context.Canceled) {
		err = context.Canceled
	}
	if err != nil && err != context.Canceled {
		report.Print("Supervisor stopped: ", err)
	}
	return err
}

func supervisorLock(dir string) (*os.File, error) {
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, errors.New("cannot create supervisor runtime directory")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, errors.New("supervisor runtime must be a private directory")
	}
	if info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return nil, errors.New("supervisor runtime has another owner")
	}
	f, err := openLockFile(filepath.Join(dir, "supervisor.lock"), "supervisor lock")
	if err != nil {
		return nil, err
	}
	if unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		f.Close()
		return nil, errors.New("another client instance is still running")
	}
	return f, nil
}

func openLockFile(path, label string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, errors.New("cannot open " + label)
	}
	f := os.NewFile(uintptr(fd), filepath.Base(path))
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		f.Close()
		return nil, errors.New(label + " must be a regular file")
	}
	return f, nil
}

func runChild(ctx context.Context, executable string, args []string, lock *os.File, output io.Writer, report *log.Logger) (int, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if ctx.Err() != nil {
		return 0, context.Cause(ctx)
	}
	cmd := exec.CommandContext(ctx, executable, args...)
	configureChild(cmd)
	cmd.ExtraFiles = []*os.File{lock}
	cmd.Stdout, cmd.Stderr = output, output
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Start(); err != nil {
		return 0, errors.New("cannot start Station client worker")
	}
	report.Printf("Client worker started with PID %d.", cmd.Process.Pid)
	if err := cmd.Wait(); err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			return exit.ExitCode(), nil
		}
		if ctx.Err() != nil {
			return 0, context.Cause(ctx)
		}
		return 0, errors.New("cannot reap Station client worker")
	}
	return 0, nil
}

func waitNativeState(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	for {
		conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", "127.0.0.1:9875")
		if err == nil {
			return conn.Close()
		}
		select {
		case <-ctx.Done():
			if parent.Err() != nil {
				return context.Cause(parent)
			}
			return errors.New("native Alice state was unavailable for 60 seconds")
		case <-time.After(time.Second):
		}
	}
}

type cappedLog struct {
	mu        sync.Mutex
	path      string
	max       int64
	f         *os.File
	size      int64
	moved     bool
	discarded bool
}

func openCappedLog(path string, limit int64) (*cappedLog, error) {
	if limit <= 0 || !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return nil, errors.New("invalid bounded log path or size")
	}
	for _, name := range []string{path, path + ".1"} {
		if info, err := os.Lstat(name); err == nil && !info.Mode().IsRegular() {
			return nil, errors.New("log paths must be regular files")
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("cannot inspect log paths")
		}
	}
	if _, err := os.Lstat(path + ".1"); err == nil {
		previous, _, err := openLogSegment(path+".1", limit)
		if err != nil {
			return nil, err
		}
		if err := previous.Close(); err != nil {
			return nil, err
		}
	}
	f, size, err := openLogSegment(path, limit)
	if err != nil {
		return nil, err
	}
	return &cappedLog{path: path, max: limit, f: f, size: size}, nil
}

func openLogSegment(path string, limit int64) (*os.File, int64, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_APPEND|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, 0, errors.New("cannot open bounded client log")
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) || info.Sys().(*syscall.Stat_t).Nlink != 1 || f.Chmod(0600) != nil {
		f.Close()
		return nil, 0, errors.New("cannot protect bounded client log")
	}
	if info.Size() > limit {
		tail := make([]byte, int(limit))
		_, readErr := f.ReadAt(tail, info.Size()-limit)
		if readErr != nil || f.Truncate(0) != nil {
			f.Close()
			return nil, 0, errors.New("cannot bound existing client log")
		}
		if _, err := f.Write(tail); err != nil {
			f.Close()
			return nil, 0, errors.New("cannot retain recent client log")
		}
	}
	return f, min(info.Size(), limit), nil
}

func (l *cappedLog) Write(p []byte) (n int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n = len(p)
	if l.discarded {
		prefix := time.Now().UTC().Format("2006/01/02 15:04:05") + " Client log output was discarded while storage was unavailable.\n"
		p = append([]byte(prefix), p...)
	}
	l.discarded = l.write(p) != nil
	return n, nil
}

func (l *cappedLog) rotate() error {
	next, err := os.CreateTemp(filepath.Dir(l.path), "."+filepath.Base(l.path)+"-*")
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			next.Close()
			os.Remove(next.Name())
		}
	}()
	if !l.moved {
		if err := os.Rename(l.path, l.path+".1"); err != nil {
			return err
		}
		l.moved = true
	}
	if err := os.Rename(next.Name(), l.path); err != nil {
		return err
	}
	previous := l.f
	l.f, l.size, l.moved = next, 0, false
	published = true
	previous.Close()
	return nil
}

func (l *cappedLog) write(p []byte) error {
	for len(p) > 0 {
		if l.size == l.max {
			if err := l.rotate(); err != nil {
				return err
			}
		}
		part := min(len(p), int(l.max-l.size))
		written, writeErr := l.f.Write(p[:part])
		l.size += int64(written)
		if writeErr != nil {
			return writeErr
		}
		if written != part {
			return io.ErrShortWrite
		}
		p = p[written:]
	}
	return nil
}

func (l *cappedLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

func superviseCommand(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, err := fmt.Fprintln(out, "Usage: yandex-station-chatgpt-voice supervise --log=FILE [run options]")
		return err
	}
	if len(args) == 0 {
		return configError{errors.New("supervise requires --log=FILE and optional run settings")}
	}
	path, ok := strings.CutPrefix(args[0], "--log=")
	if !ok || path == "" {
		return configError{errors.New("supervise requires --log=FILE as its first option")}
	}
	return supervise(ctx, args[1:], path)
}
