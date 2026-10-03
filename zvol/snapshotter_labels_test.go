package zvol

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mistifyio/go-zfs/v3"
)

// Label values must survive the command boundary, and names that normalise
// to the same ZFS property must not produce a rejected duplicate assignment.
func TestBatchedLabelValuesAndCollisions(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "arguments")
	script := "#!/bin/sh\nprintf '%s\\000' \"$@\" > \"$ZVOL_ARGS_TEST_FILE\"\n"
	if err := os.WriteFile(filepath.Join(dir, "zfs"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("ZVOL_ARGS_TEST_FILE", output)
	value := "spaces = literal; $(never-run)\nsecond line"
	err := setZfsLabelProperties(context.Background(), &zfs.Dataset{Name: "pool/snapshots/1"}, map[string]string{
		"containerd.io/snapshot/Slicer/Image":    "upper",
		"containerd.io/snapshot/slicer/image":    value,
		"containerd.io/snapshot/slicer/hostname": "vm-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
	if len(args) != 4 || args[0] != "set" || args[3] != "pool/snapshots/1" {
		t.Fatalf("unexpected assignments: %#v", args)
	}
	properties := map[string]string{}
	for _, arg := range args[1:3] {
		pair := strings.SplitN(arg, "=", 2)
		properties[pair[0]] = pair[1]
	}
	if properties[zfsLabelPropertyPrefix+"slicer_image"] != value || properties[zfsLabelPropertyPrefix+"slicer_hostname"] != "vm-1" {
		t.Fatalf("labels changed: %#v", properties)
	}
}
