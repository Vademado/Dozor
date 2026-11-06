// Package repository implements filesystem persistence.
package repository

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"guard.local/admin/internal/app/domain"
)

type SettingsFile struct{ path string }

func NewSettingsFile(dir string) *SettingsFile {
	return &SettingsFile{filepath.Join(dir, "settings.json")}
}

func (s *SettingsFile) Load() (domain.Settings, error) {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return domain.DefaultSettings(), nil
	}
	if err != nil {
		return domain.Settings{}, fmt.Errorf("read settings: %w", err)
	}
	var settings domain.Settings
	if err = json.Unmarshal(data, &settings); err != nil {
		return settings, fmt.Errorf("decode settings: %w", err)
	}
	if err = settings.Validate(); err != nil {
		return settings, fmt.Errorf("invalid stored settings: %w", err)
	}
	return settings, nil
}

// Save writes to a temporary file on the same volume before replacing settings.
// The caller serializes writes and changes in-memory state only after success.
func (s *SettingsFile) Save(settings domain.Settings) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".settings-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(data, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, s.path)
}
