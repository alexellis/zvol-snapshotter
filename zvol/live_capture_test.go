package zvol

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/mistifyio/go-zfs/v3"
)

// Validate ownership from real containerd metadata before any ZFS command.
// Committed volumes, views, another image's active volume, and paths outside
// this snapshotter's dataset must never become capture sources.
func TestLiveCaptureRejectsUnownedOrigins(t *testing.T) {
	store, err := storage.NewMetaStore(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &snapshotter{dataset: &zfs.Dataset{Name: "test/snapshots"}, store: store}
	ids := map[string]string{}
	err = store.WithTransaction(context.Background(), true, func(ctx context.Context) error {
		for _, name := range []string{"parent-a", "parent-b"} {
			if _, err := storage.CreateSnapshot(ctx, snapshots.KindActive, name+"-temporary", ""); err != nil {
				return err
			}
			id, err := storage.CommitActive(ctx, name+"-temporary", name, snapshots.Usage{})
			if err != nil {
				return err
			}
			ids[name] = id
		}
		for _, row := range []struct {
			name, parent string
			kind         snapshots.Kind
		}{
			{"active-b", "parent-b", snapshots.KindActive},
			{"view-a", "parent-a", snapshots.KindView},
		} {
			snap, err := storage.CreateSnapshot(ctx, row.kind, row.name, row.parent)
			if err != nil {
				return err
			}
			ids[row.name] = snap.ID
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, path, want string }{
		{"foreign dataset", "/dev/zvol/other/snapshots/1", "configured dataset"},
		{"traversal", "/dev/zvol/test/snapshots/../1", "configured dataset"},
		{"nested ID", "/dev/zvol/test/snapshots/1/2", "invalid live origin"},
		{"committed", "/dev/zvol/test/snapshots/" + ids["parent-a"], "not an active snapshot"},
		{"view", "/dev/zvol/test/snapshots/" + ids["view-a"], "not an active snapshot"},
		{"different parent", "/dev/zvol/test/snapshots/" + ids["active-b"], "not an active snapshot"},
		{"missing ID", "/dev/zvol/test/snapshots/9999", "not an active snapshot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := store.WithTransaction(context.Background(), true, func(ctx context.Context) error {
				_, err := s.captureLiveVolume(ctx, tc.path, "test/snapshots/999", "parent-a", 1)
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					return fmt.Errorf("unexpected capture result: %v", err)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
