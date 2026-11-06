//go:build darwin

package launchagent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestReadInstalled(t *testing.T) {
	fakeHome(t)
	path, err := PlistPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	want := Spec{Label: Label, ProgramArguments: []string{"/Applications/clipd & tools/clipd", "serve"}, StandardErrPath: "/private/service.log", EnvironmentVariables: map[string]string{"CLIPD_CONFIG": "/private/custom config.json"}}
	data, err := want.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadInstalled(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.ProgramArguments[0] != want.ProgramArguments[0] || got.StandardErrPath != want.StandardErrPath || got.EnvironmentVariables["CLIPD_CONFIG"] != want.EnvironmentVariables["CLIPD_CONFIG"] {
		t.Fatalf("read: %+v", got)
	}
	if err := os.WriteFile(path, []byte("broken plist"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInstalled(context.Background()); err == nil {
		t.Fatal("accepted broken plist")
	}
}
