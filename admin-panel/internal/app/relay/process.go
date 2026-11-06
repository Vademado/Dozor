package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func healthy(ctx context.Context, base string) bool {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api", nil)
	response, err := (&http.Client{Timeout: 500 * time.Millisecond}).Do(req)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	var info struct {
		Version string `json:"version"`
	}
	return response.StatusCode == 200 && json.NewDecoder(io.LimitReader(response.Body, 16384)).Decode(&info) == nil && info.Version != ""
}

// EnsureProcess reuses a running local go2rtc or starts our bundled executable.
// Only the child created here is stopped; no global taskkill/killall is used.
func EnsureProcess(ctx context.Context, base, binary, config string, log *slog.Logger) (func(), error) {
	if healthy(ctx, base) {
		log.Info("using existing go2rtc", "url", base)
		return func() {}, nil
	}
	if _, err := os.Stat(binary); err != nil {
		return nil, fmt.Errorf("go2rtc executable missing: %s", binary)
	}
	absoluteConfig, err := filepath.Abs(config)
	if err != nil {
		return nil, err
	}
	if _, err = os.Stat(absoluteConfig); err != nil {
		return nil, fmt.Errorf("go2rtc config missing: %s", absoluteConfig)
	}
	childCtx, cancel := context.WithCancel(ctx)
	command := exec.CommandContext(childCtx, binary, "-config", absoluteConfig)
	command.Dir = filepath.Dir(absoluteConfig)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	hideWindow(command)
	if err = command.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start go2rtc: %w", err)
	}
	done := make(chan struct{})
	go func() { _ = command.Wait(); close(done) }()
	stop := func() { cancel(); <-done }
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			stop()
			return nil, ctx.Err()
		case <-done:
			cancel()
			return nil, errors.New("go2rtc exited during startup; see its log above")
		case <-timer.C:
			stop()
			return nil, errors.New("go2rtc API did not become ready within 10 seconds")
		case <-ticker.C:
			if healthy(ctx, base) {
				log.Info("started bundled go2rtc", "pid", command.Process.Pid, "url", base)
				return stop, nil
			}
		}
	}
}
