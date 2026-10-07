package zvol

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mistifyio/go-zfs/v3"
)

func installLabelCommand(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	output := filepath.Join(dir, "arguments")
	if err := os.WriteFile(filepath.Join(dir, "zfs"), []byte("#!/bin/sh\n"+script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("ZVOL_ARGS_TEST_FILE", output)
	return output
}

func TestBatchedLabelValuesAndCollisions(t *testing.T) {
	output := installLabelCommand(t, "printf '%s\\000' \"$@\" > \"$ZVOL_ARGS_TEST_FILE\"\n")
	value := "spaces = literal; $(never-run)\nsecond line ' \" `never-run`"
	longKey := strings.Repeat("a", zfsLabelPropertyMaxLength)
	err := setZfsLabelProperties(context.Background(), &zfs.Dataset{Name: "pool/snapshots/1"}, map[string]string{
		"containerd.io/snapshot/Slicer/Image":    "upper",
		"containerd.io/snapshot/slicer/image":    value,
		"containerd.io/snapshot/slicer/hostname": "vm-1",
		"containerd.io/snapshot/":                "skipped",
		longKey + "a":                            "first",
		longKey + "z":                            "last",
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
	want := []string{"set", (zfsLabelPropertyPrefix + longKey)[:zfsLabelPropertyMaxLength] + "=last", zfsLabelPropertyPrefix + "slicer_hostname=vm-1", zfsLabelPropertyPrefix + "slicer_image=" + value, "pool/snapshots/1"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("command arguments = %#v, want %#v", args, want)
	}
}

func TestBatchedLabelsEmpty(t *testing.T) {
	output := installLabelCommand(t, "printf called > \"$ZVOL_ARGS_TEST_FILE\"\nexit 1\n")
	for _, labels := range []map[string]string{nil, {}, {"containerd.io/snapshot/": "skipped"}} {
		if err := setZfsLabelProperties(context.Background(), &zfs.Dataset{Name: "pool/1"}, labels); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty labels invoked zfs: %v", err)
	}
}

func TestBatchedLabelsCommandError(t *testing.T) {
	output := installLabelCommand(t, "printf '%s\\000' \"$@\" >> \"$ZVOL_ARGS_TEST_FILE\"\nprintf 'invalid property\\n' >&2\nexit 2\n")
	err := setZfsLabelProperties(context.Background(), &zfs.Dataset{Name: "pool/1"}, map[string]string{"a": "first", "b": "second"})
	var zfsErr *zfs.Error
	if !errors.As(err, &zfsErr) || zfsErr.Stderr != "invalid property\n" {
		t.Fatalf("missing ZFS error diagnostics: %v", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(zfsErr.Err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("missing process exit status: %v", zfsErr.Err)
	}
	b, readErr := os.ReadFile(output)
	if readErr != nil || strings.Count(string(b), "pool/1\x00") != 1 {
		t.Fatalf("command was retried after failure: %q, %v", b, readErr)
	}
}

func TestBatchedLabelsCancellation(t *testing.T) {
	output := installLabelCommand(t, "printf started > \"$ZVOL_ARGS_TEST_FILE\"\nexec /bin/sleep 30\n")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	result := make(chan error, 1)
	go func() {
		result <- setZfsLabelProperties(ctx, &zfs.Dataset{Name: "pool/1"}, map[string]string{"a": "value"})
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(output); err == nil {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("zfs command did not start")
		case <-tick.C:
		}
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v", err)
		}
		var zfsErr *zfs.Error
		if !errors.As(err, &zfsErr) {
			t.Fatalf("missing process error on cancellation: %v", err)
		}
	case <-deadline.C:
		t.Fatal("cancelled zfs command did not exit")
	}
	if err := os.Remove(output); err != nil {
		t.Fatal(err)
	}
	err := setZfsLabelProperties(ctx, &zfs.Dataset{Name: "pool/1"}, map[string]string{"a": "value"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("already cancelled context error = %v", err)
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("already cancelled context launched zfs: %v", err)
	}
}

// Exercise the kernel's real exec argument limit. The script can only observe
// successful launches, which must each contain one assignment after E2BIG.
func TestBatchedLabelsArgumentLimitFallback(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux exec argument limit control")
	}
	var stack syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_STACK, &stack); err != nil {
		t.Fatal(err)
	}
	limit := stack.Cur / 4
	if limit > 6*1024*1024 {
		limit = 6 * 1024 * 1024
	}
	if limit < 128*1024 {
		limit = 128 * 1024
	}
	output := installLabelCommand(t, "[ \"$#\" -eq 3 ] || exit 3\nprintf '%s\\n' \"${2%%=*}\" >> \"$ZVOL_ARGS_TEST_FILE\"\n")
	labels := make(map[string]string)
	value := strings.Repeat("v", 4000)
	count := int(limit/4000) + 64
	for i := 0; i < count; i++ {
		labels["label_"+strconv.Itoa(i)] = value
	}
	if err := setZfsLabelProperties(context.Background(), &zfs.Dataset{Name: "pool/1"}, labels); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, property := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if !strings.HasPrefix(property, zfsLabelPropertyPrefix+"label_") || seen[property] {
			t.Fatalf("unexpected or repeated fallback assignment: %q", property)
		}
		seen[property] = true
	}
	if len(seen) != count {
		t.Fatalf("fallback applied %d labels, want %d", len(seen), count)
	}
}
