//go:build darwin

package launchagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// ReadInstalled reads the on-disk job, including its pinned configuration.
func ReadInstalled(ctx context.Context) (Spec, error) {
	path, err := PlistPath()
	if err != nil {
		return Spec{}, err
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return Spec{}, fmt.Errorf("clipd login service is not installed; run 'clipd install'")
		}
		return Spec{}, fmt.Errorf("read installed service %s: %w", path, err)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/plutil", "-convert", "json", "-o", "-", path)
	cmd.WaitDelay = time.Second
	data, err := cmd.Output()
	if err != nil {
		return Spec{}, fmt.Errorf("read installed service %s: %w", path, err)
	}
	var spec Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return Spec{}, fmt.Errorf("read installed service: %w", err)
	}
	if spec.Label != Label || len(spec.ProgramArguments) == 0 {
		return Spec{}, fmt.Errorf("invalid clipd service in %s", path)
	}
	return spec, nil
}
