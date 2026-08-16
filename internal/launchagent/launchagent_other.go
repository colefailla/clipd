//go:build !darwin

package launchagent

import "context"

// Options controls installation. See the darwin implementation.
type Options struct {
	ExecutablePath string
	ConfigPath     string
}

// Result reports what Install did. See the darwin implementation.
type Result struct {
	PlistPath      string
	ExecutablePath string
	LogPath        string
}

// State describes the installed agent. See the darwin implementation.
type State struct {
	PlistInstalled bool
	PlistPath      string
	Loaded         bool
	PID            int
	LastExitStatus int
	LogPath        string
}

// LogPath is unavailable off macOS.
func LogPath() (string, error) { return "", ErrUnsupported }

// Install is unavailable off macOS.
func Install(context.Context, Options) (Result, error) { return Result{}, ErrUnsupported }

// Uninstall is unavailable off macOS.
func Uninstall(context.Context) (string, error) { return "", ErrUnsupported }

// Status is unavailable off macOS.
func Status(context.Context) (State, error) { return State{}, ErrUnsupported }
