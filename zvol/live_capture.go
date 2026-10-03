package zvol

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/mistifyio/go-zfs/v3"
)

// LabelLiveOrigin opts into this spike's active-volume checkpoint protocol.
// The caller pauses the VM before Prepare and resumes it after publication.
const LabelLiveOrigin = "containerd.io/snapshot/zvol/live-origin"

func runZFS(ctx context.Context, args ...string) ([]byte, error) {
	b, e := exec.CommandContext(ctx, "zfs", args...).CombinedOutput()
	if e != nil {
		return b, fmt.Errorf("zfs %s: %w: %s", strings.Join(args, " "), e, strings.TrimSpace(string(b)))
	}
	return b, nil
}

func (s *snapshotter) captureLiveVolume(ctx context.Context, sourceDevice, target, parent string, volumeSize uint64) (dataset *zfs.Dataset, retErr error) {
	// Resolve only an active volume in this snapshotter's configured dataset.
	sourceDevice = filepath.Clean(sourceDevice)
	prefix := filepath.Join(zfsDevicePath, s.dataset.Name) + "/"
	if !strings.HasPrefix(sourceDevice, prefix) {
		return nil, fmt.Errorf("live origin must belong to the configured dataset")
	}
	id := strings.TrimPrefix(sourceDevice, prefix)
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return nil, fmt.Errorf("invalid live origin volume ID")
	}
	found := false
	err := storage.WalkInfo(ctx, func(ctx context.Context, info snapshots.Info) error {
		if info.Kind != snapshots.KindActive || info.Parent != parent {
			return nil
		}
		actual, _, _, err := storage.GetInfo(ctx, info.Name)
		if err != nil {
			return err
		}
		if actual == id {
			found = true
		}
		return nil
	})
	if err != nil || !found {
		return nil, fmt.Errorf("live origin is not an active snapshot with the requested parent: %v", err)
	}
	// Refuse stale or foreign backing at the reserved metadata ID.
	existing, listErr := exec.CommandContext(ctx, "zfs", "list", "-H", "-o", "name", target).CombinedOutput()
	if listErr == nil {
		return nil, fmt.Errorf("capture destination already exists")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !strings.Contains(string(existing), "dataset does not exist") {
		return nil, fmt.Errorf("check capture destination: %w: %s", listErr, existing)
	}
	source := filepath.Join(s.dataset.Name, id)
	origin, err := zfs.GetDataset(source)
	if err != nil {
		return nil, err
	}
	if origin.Type != zfs.DatasetVolume || origin.Volsize != volumeSize {
		return nil, fmt.Errorf("live capture volume type/size mismatch")
	}
	if err := syncVolume(sourceDevice); err != nil {
		return nil, err
	}
	captureName := "slicer-live-" + filepath.Base(target)
	sourceSnapshot := source + "@" + captureName
	if _, err := runZFS(ctx, "snapshot", sourceSnapshot); err != nil {
		return nil, err
	}
	received := false
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := runZFS(cleanup, "destroy", sourceSnapshot)
		if err != nil {
			retErr = errors.Join(retErr, err)
		}
		if retErr != nil && received {
			_, err = runZFS(cleanup, "destroy", "-r", target)
			retErr = errors.Join(retErr, err)
		}
	}()
	// A full, independent receive avoids keeping the running source alive through
	// clone ancestry. This is paid once per checkpoint; subsequent forks clone.
	sender := exec.CommandContext(ctx, "zfs", "send", sourceSnapshot)
	receiver := exec.CommandContext(ctx, "zfs", "receive", "-u", "-o", "volmode=full", "-o", "refreservation=none", target+"@"+captureName)
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	defer writer.Close()
	sender.Stdout = writer
	receiver.Stdin = reader
	var sendErr, recvErr bytes.Buffer
	sender.Stderr = &sendErr
	receiver.Stderr = &recvErr
	if err := receiver.Start(); err != nil {
		return nil, err
	}
	received = true
	if err := sender.Start(); err != nil {
		reader.Close()
		writer.Close()
		_ = receiver.Process.Kill()
		_ = receiver.Wait()
		return nil, err
	}
	// Children have their own descriptors; close the parent's copies so EOF
	// reaches receive and Wait cannot close a pipe still being consumed.
	reader.Close()
	writer.Close()
	senderResult := sender.Wait()
	receiverResult := receiver.Wait()
	if senderResult != nil || receiverResult != nil {
		return nil, fmt.Errorf("live send/receive: %w; send=%s receive=%s", errors.Join(senderResult, receiverResult), sendErr.String(), recvErr.String())
	}
	// Test-only failure after receiving a private volume exercises rollback.
	if marker := os.Getenv("ZVOL_LIVE_RECEIVE_FAIL_TEST"); marker != "" {
		if _, err := os.Stat(marker); err == nil {
			return nil, fmt.Errorf("injected live receive finalisation failure")
		}
	}
	if _, err := runZFS(ctx, "destroy", target+"@"+captureName); err != nil {
		return nil, err
	}
	dataset, err = zfs.GetDataset(target)
	if err != nil {
		return nil, err
	}
	// Bounded readiness and cancellation, unlike the legacy unbounded helper.
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(getDevicePath(dataset)); err == nil {
			return dataset, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("live capture device did not appear")
		case <-ticker.C:
		}
	}
}
