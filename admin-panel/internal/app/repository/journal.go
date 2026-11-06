package repository

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"guard.local/admin/internal/app/domain"
)

const journalLimit = 5000
const rotateBytes = 5 * 1024 * 1024

// Journal is a bounded in-memory index backed by three rotating JSONL files.
// No file descriptor is kept open, which also permits rotation on Windows.
type Journal struct {
	mu     sync.RWMutex
	path   string
	events []domain.Event
	next   uint64
}

func NewJournal(dir string) (*Journal, error) {
	j := &Journal{path: filepath.Join(dir, "events.jsonl"), events: make([]domain.Event, 0), next: 1}
	for _, suffix := range []string{".2", ".1", ""} {
		f, err := os.Open(j.path + suffix)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		scan := bufio.NewScanner(f)
		scan.Buffer(make([]byte, 4096), 65536)
		for scan.Scan() {
			var event domain.Event
			// A partial final line after a power failure is ignored, valid lines remain.
			if json.Unmarshal(scan.Bytes(), &event) == nil && event.ID > 0 {
				j.keep(event)
				if event.ID >= j.next {
					j.next = event.ID + 1
				}
			}
		}
		err = scan.Err()
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("read journal: %w", err)
		}
	}
	return j, nil
}

func (j *Journal) keep(event domain.Event) {
	j.events = append(j.events, event)
	if len(j.events) > journalLimit {
		j.events = append([]domain.Event(nil), j.events[len(j.events)-journalLimit:]...)
	}
}

func (j *Journal) Append(event domain.Event) (domain.Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	event.ID, event.Time = j.next, time.Now().UTC()
	data, err := json.Marshal(event)
	if err != nil {
		return event, err
	}
	if info, statErr := os.Stat(j.path); statErr == nil && info.Size()+int64(len(data))+1 > rotateBytes {
		if err = os.Remove(j.path + ".2"); err != nil && !os.IsNotExist(err) {
			return event, err
		}
		if err = os.Rename(j.path+".1", j.path+".2"); err != nil && !os.IsNotExist(err) {
			return event, err
		}
		if err = os.Rename(j.path, j.path+".1"); err != nil {
			return event, err
		}
	}
	f, err := os.OpenFile(j.path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0600)
	if err != nil {
		return event, err
	}
	// Isolate an incomplete record left by a power failure. Otherwise the next
	// valid event would be appended to that fragment and lost on the next restart.
	if info, statErr := f.Stat(); statErr == nil && info.Size() > 0 {
		var last [1]byte
		if _, readErr := f.ReadAt(last[:], info.Size()-1); readErr != nil {
			f.Close()
			return event, readErr
		}
		if last[0] != '\n' {
			data = append([]byte{'\n'}, data...)
		}
	}
	_, err = f.Write(append(data, '\n'))
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return event, err
	}
	if closeErr != nil {
		return event, closeErr
	}
	j.next++
	j.keep(event)
	return event, nil
}

// List returns newest first. Callers receive a copy, so SSE/HTTP never race writes.
func (j *Journal) List(filter domain.EventFilter) []domain.Event {
	j.mu.RLock()
	defer j.mu.RUnlock()
	limit := filter.Limit
	if limit <= 0 || limit > journalLimit {
		limit = 200
	}
	out := make([]domain.Event, 0)
	for i := len(j.events) - 1; i >= 0 && len(out) < limit; i-- {
		if filter.Match(j.events[i]) {
			out = append(out, j.events[i])
		}
	}
	return out
}

func (j *Journal) Size() int { j.mu.RLock(); defer j.mu.RUnlock(); return len(j.events) }
